package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/defect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated DEF/DSP pipeline: finding ingest idempotency, fingerprint
// upsert, occurrence growth, reopen-on-recurrence — the DEF-INV-002
// invariants over the real tables.

func newDefectFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_dsp_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_dsp_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_dsp_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_dsp_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'dsp')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'dsp', 'DSP', 'active')`, m3Project)
	require.NoError(t, err)
	return pg, m3Project
}

func dspFinding(severity defect.Severity, eventID string) defect.Finding {
	return defect.Finding{
		ProjectID:      m3Project,
		SourceType:     defect.SourceJUnit,
		SourceEventID:  eventID,
		Severity:       severity,
		EvidenceRefs:   []string{"junit-report"},
		Repro:          "expected 200 got 500",
		AdapterVersion: "junit-adapter-v1",
	}
}

func TestDefectUpsertLifecycle(t *testing.T) {
	pg, projectID := newDefectFixture(t)
	ctx := context.Background()
	fingerprint := defect.Fingerprint(defect.FingerprintInput{
		ProjectID: projectID, Repository: "acme/orders", Branch: "main",
		CheckID: "pkg/checkout_test.TestRetry", ErrorSignature: "expected 200 got 500",
	})

	// First sighting creates the defect at the finding severity.
	defectID, created, err := pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityMedium, "evt-1"), fingerprint)
	require.NoError(t, err)
	require.True(t, created)

	state, severity, occurrence, found, err := pg.Defects().Defect(ctx, projectID, defectID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "detected", state)
	assert.Equal(t, string(defect.SeverityMedium), severity)
	assert.Equal(t, 1, occurrence)

	// The exact same delivery replays as a duplicate sentinel — no
	// second occurrence, no double counting.
	_, _, err = pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityMedium, "evt-1"), fingerprint)
	assert.ErrorIs(t, err, ErrFindingDuplicate)

	// A NEW event with the same fingerprint grows the occurrence and
	// RISES the severity to the worst finding.
	defectID2, created, err := pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityCritical, "evt-2"), fingerprint)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, defectID, defectID2, "the same fingerprint stays one defect")

	_, severity, occurrence, _, err = pg.Defects().Defect(ctx, projectID, defectID)
	require.NoError(t, err)
	assert.Equal(t, 2, occurrence, "duplicates only grow history")
	assert.Equal(t, string(defect.SeverityCritical), severity, "severity rises to the worst finding")

	// Resolve, then recur: occurrence wins and the defect reopens to
	// assigned (the frozen reopen semantics).
	_, err = pg.DB().ExecContext(ctx, `UPDATE defects SET state = 'resolved', resolved_at = now() WHERE id = $1`, defectID)
	require.NoError(t, err)
	_, created, err = pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityCritical, "evt-3"), fingerprint)
	require.NoError(t, err)
	assert.False(t, created)
	state, _, occurrence, _, err = pg.Defects().Defect(ctx, projectID, defectID)
	require.NoError(t, err)
	assert.Equal(t, "assigned", state, "a resolved defect reopens on recurrence")
	assert.Equal(t, 3, occurrence)

	// Occurrence links are append-only history: three findings linked.
	var links int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM defect_occurrences WHERE defect_id = $1`, defectID).Scan(&links))
	assert.Equal(t, 3, links)
}

func TestDifferentFingerprintsForkDefects(t *testing.T) {
	pg, projectID := newDefectFixture(t)
	ctx := context.Background()

	first := defect.Fingerprint(defect.FingerprintInput{
		ProjectID: projectID, Repository: "r", Branch: "main",
		CheckID: "TestA", ErrorSignature: "sig-a",
	})
	second := defect.Fingerprint(defect.FingerprintInput{
		ProjectID: projectID, Repository: "r", Branch: "main",
		CheckID: "TestB", ErrorSignature: "sig-b",
	})

	idA, createdA, err := pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityLow, "evt-a"), first)
	require.NoError(t, err)
	require.True(t, createdA)
	idB, createdB, err := pg.Defects().RecordFinding(ctx, projectID, dspFinding(defect.SeverityLow, "evt-b"), second)
	require.NoError(t, err)
	require.True(t, createdB)
	assert.NotEqual(t, idA, idB, "different fingerprints are different defects")
}

// Package m4drill is the M4 drill registry: executable anchors for
// the four runbook exercises (M4-RBK-001) and the V4 rehearsal
// inputs, mirroring internal/m2drill (V2) and internal/m3drill (V3).
// This first anchor proves the database-backup-restore runbook's
// restore side against real PostgreSQL — an isolated restore (a
// template-database copy of a seeded source, no external binaries),
// the checksum and business validation the runbook §4.1 requires,
// the measured RPO/RTO, and the REL-RULE-004 lifecycle: the drill's
// backup only turns 'verified' once the off-host restore evidence
// exists. The remaining anchors (runner-offline, webhook-pipeline
// failure, emergency stop) land with their owning streams.
package m4drill

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

const (
	drillSourceDB  = "maestro_drill_source"
	drillRestoreDB = "maestro_drill_restore"
	drillTeam      = "018f7e00-0000-7000-8000-00000000f001"
	drillProject   = "018f7e00-0000-7000-8000-00000000f002"
)

// TestRunbookDatabaseBackupRestore is the TC-DB-RESTORE-001 anchor:
// runbook §4.1 steps 2–4 against a real isolated restore — pick the
// verified backup, validate the checksum, restore into isolation,
// validate data/business/constraints, and record the measured
// RPO/RTO. Point-in-time WAL replay and the GitLab read-only
// reconciliation stay with the full infra drill (registered as the
// remaining exercise scope).
func TestRunbookDatabaseBackupRestore(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, drillRestoreDB))
		_, _ = admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, drillSourceDB))
		_ = admin.Close()
	})
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, drillSourceDB))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, drillSourceDB))
	require.NoError(t, err)

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	source, err := store.OpenPostgres(ctx, dsn[:strings.LastIndex(dsn, "/")+1]+drillSourceDB)
	require.NoError(t, err)
	_, err = store.ApplyPostgresMigrations(ctx, source)
	require.NoError(t, err)
	seedDrillData(t, ctx, source)

	// §4.1.3 requires recording the actual restore point — the
	// fingerprint taken on the live source is it.
	sourceFingerprint, err := databaseFingerprint(ctx, source)
	require.NoError(t, err)

	var sizeBytes int64
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT pg_database_size($1)::bigint`, drillSourceDB).Scan(&sizeBytes))
	assert.Greater(t, sizeBytes, int64(0))

	sourceStore, err := store.NewPostgresStore(source)
	require.NoError(t, err)
	rel := sourceStore.Reliability()
	startedAt := time.Now().UTC()
	backupID, err := rel.RecordBackupStart(ctx, "full", startedAt)
	require.NoError(t, err)
	finishedAt := time.Now().UTC()
	require.NoError(t, rel.CompleteBackup(ctx, backupID, finishedAt, sizeBytes,
		sourceFingerprint.digest, "env:MAESTRO_BACKUP_DRILL"))
	// The template copy needs exclusive access: close every pooled
	// connection to the source before creating the restore database.
	require.NoError(t, source.Close())

	// §4.1.3: restore into the isolated cluster. The elapsed time of
	// copy-plus-validation below is the measured RTO evidence.
	restoreStarted := time.Now().UTC()
	_, err = admin.ExecContext(ctx,
		fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, drillRestoreDB, drillSourceDB))
	require.NoError(t, err, "the isolated restore copy")

	restored, err := store.OpenPostgres(ctx, dsn[:strings.LastIndex(dsn, "/")+1]+drillRestoreDB)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })

	// §4.1.2: the checksum — the restored fingerprint must equal the
	// recorded restore point byte for byte.
	restoredFingerprint, err := databaseFingerprint(ctx, restored)
	require.NoError(t, err)
	assert.Equal(t, sourceFingerprint, restoredFingerprint,
		"checksum validation against the recorded restore point")

	// §4.1.4: data and business validation — row counts, the migration
	// catalog parity and the surviving constraints.
	assert.Equal(t, int64(1), restoredFingerprint.teams)
	assert.Equal(t, int64(1), restoredFingerprint.projects)
	assert.Equal(t, int64(3), restoredFingerprint.auditEvents)
	assert.GreaterOrEqual(t, restoredFingerprint.migrationCount, int64(13))
	_, err = restored.ExecContext(ctx, `UPDATE audit_events SET decision = 'allow' WHERE id = 1`)
	require.Error(t, err, "the immutability trigger survived the restore")

	// REL-RULE-004: the backup reaches 'verified' only through the
	// off-host restore evidence recorded on the restored ledger.
	restoredStore, err := store.NewPostgresStore(restored)
	require.NoError(t, err)
	restoredRel := restoredStore.Reliability()
	verifiedAt := time.Now().UTC()
	require.NoError(t, restoredRel.RecordRestoreVerification(ctx, backupID, verifiedAt,
		"m4drill-restore-host", true, true))
	latest, err := restoredRel.LatestVerifiedBackup(ctx, "full", time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, latest, "the drill backup is now the RPO anchor")
	require.NotNil(t, latest.FinishedAt)
	rpo := verifiedAt.Sub(*latest.FinishedAt)
	rto := verifiedAt.Sub(restoreStarted)
	assert.LessOrEqual(t, rpo, 15*time.Minute, "measured RPO within the objective")
	assert.LessOrEqual(t, rto, 4*time.Hour, "measured RTO within the objective")
	t.Logf("drill evidence: rpo=%s rto=%s size=%d fingerprint=%s",
		rpo.Truncate(time.Millisecond), rto.Truncate(time.Millisecond), sizeBytes, sourceFingerprint.digest)
}

func seedDrillData(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO teams (id, name) VALUES ($1, 'm4drill')`, drillTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'm4drill', 'M4DRILL', 'active')`,
		drillProject, drillTeam)
	require.NoError(t, err)
	for index := 1; index <= 3; index++ {
		_, err = db.ExecContext(ctx, `INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, correlation_id)
			VALUES ($1, $2, $3, 'work_item', $4, $5, $6)`,
			fmt.Sprintf("drill-user-%d", index), drillProject, "work_item.read",
			fmt.Sprintf("drill-w-%d", index), "allow", fmt.Sprintf("drill-corr-%d", index))
		require.NoError(t, err)
	}
}

type fingerprint struct {
	digest          string
	migrationCount  int64
	migrationLatest int64
	teams           int64
	projects        int64
	auditEvents     int64
}

// databaseFingerprint is the checksum input the runbook §7 requires:
// the migration catalog identity plus the business row counts,
// excluding the backup ledger itself (the ledger is asserted
// separately after the restore).
func databaseFingerprint(ctx context.Context, db *sql.DB) (fingerprint, error) {
	result := fingerprint{}
	err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM maestro_meta.schema_migrations),
			(SELECT COALESCE(max(version), 0) FROM maestro_meta.schema_migrations),
			(SELECT count(*) FROM teams),
			(SELECT count(*) FROM projects),
			(SELECT count(*) FROM audit_events)`).
		Scan(&result.migrationCount, &result.migrationLatest, &result.teams, &result.projects, &result.auditEvents)
	if err != nil {
		return fingerprint{}, err
	}
	material := fmt.Sprintf("migrations=%d/%d teams=%d projects=%d audit=%d",
		result.migrationCount, result.migrationLatest, result.teams, result.projects, result.auditEvents)
	sum := sha256.Sum256([]byte(material))
	result.digest = "sha256:" + hex.EncodeToString(sum[:])
	return result, nil
}

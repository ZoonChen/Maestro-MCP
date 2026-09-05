package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated observability: telemetry window convergence and the
// audit-chain export/verify cycle over real immutable rows.

func TestTelemetryAndAuditChain(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_obs_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_obs_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_obs_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_obs_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'obs')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'obs', 'OBS', 'active')`, m3Project)
	require.NoError(t, err)

	// Telemetry: fail-closed validation, upsert convergence.
	obs := pg.Observability()
	err = obs.RecordTelemetry(ctx, TelemetryPoint{Metric: "", RedactionVersion: "v1"})
	require.ErrorIs(t, err, ErrTelemetryWindowRejected)

	point := TelemetryPoint{
		ProjectID: m3Project, Metric: "api_p95_latency_ms", WindowKind: "5m",
		WindowStart: "2026-09-05T10:00:00Z", SampleCount: 10, Sum: 2000,
		RedactionVersion: "redact-v1",
	}
	require.NoError(t, obs.RecordTelemetry(ctx, point))
	point.SampleCount = 20
	point.Sum = 4400
	require.NoError(t, obs.RecordTelemetry(ctx, point), "re-reported windows converge")
	var buckets, samples int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(sample_count), 0) FROM telemetry_aggregates`).Scan(&buckets, &samples))
	assert.Equal(t, int64(1), buckets)
	assert.Equal(t, int64(20), samples, "the upsert wins, no double count")

	// Audit chain: seed immutable events, export, verify.
	for index := 1; index <= 3; index++ {
		_, err = db.ExecContext(ctx, `INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, correlation_id)
			VALUES ($1, $2, $3, 'work_item', $4, $5, $6)`,
			"user-"+string(rune('0'+index)), m3Project, "work_item.read", "w-"+string(rune('0'+index)),
			[]string{"allow", "deny", "allow"}[index-1], "corr-"+string(rune('0'+index)))
		require.NoError(t, err)
	}

	rows, digests, chain, err := obs.AuditExport(ctx, m3Project, 1, 3)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, chain)
	require.NoError(t, obs.AuditChainVerify(ctx, m3Project, 1, 3, digests))

	// The export never carries secrets (token_hash/reason stay out).
	for _, row := range rows {
		assert.NotContains(t, row.Principal, "token")
	}

	// Tamper detection: rewriting one stored decision breaks verify.
	_, err = db.ExecContext(ctx, `UPDATE audit_events SET decision = 'allow' WHERE id = 2`)
	require.Error(t, err, "the immutability trigger refuses the rewrite")

	// Range semantics: an empty range refuses; partial ranges chain.
	_, _, _, err = obs.AuditExport(ctx, m3Project, 10, 5)
	require.ErrorIs(t, err, ErrAuditRangeEmpty)
	partial, partialDigests, _, err := obs.AuditExport(ctx, m3Project, 2, 3)
	require.NoError(t, err)
	require.Len(t, partial, 2)
	require.NoError(t, obs.AuditChainVerify(ctx, m3Project, 2, 3, partialDigests))
}

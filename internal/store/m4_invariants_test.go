package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated M4 migration round trip: forward to the governance schema,
// assert the load-bearing invariants, full revert leaves nothing,
// re-apply reaches target.

func TestM4GovernanceMigrationRoundTrip(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m4_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_m4_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m4_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	isolated, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_m4_test")
	require.NoError(t, err)
	t.Cleanup(func() { isolated.Close() })

	ctx := context.Background()
	applied, err := ApplyPostgresMigrations(ctx, isolated)
	require.NoError(t, err)
	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	require.Equal(t, len(migrations), applied)

	for _, table := range []string{"telemetry_aggregates", "evaluation_records", "pilot_flags"} {
		var exists int
		require.NoError(t, isolated.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`,
			table).Scan(&exists), table)
		assert.Equal(t, 1, exists, "table %s must exist", table)
	}

	seedM3Project(t, isolated) // reuses the team/project fixture

	// Telemetry window identity: (project, metric, window) is unique.
	_, err = isolated.ExecContext(ctx, `INSERT INTO telemetry_aggregates
		(id, project_id, metric, window_kind, window_start, sample_count, sum_value, redaction_version)
		VALUES ($1, $2, 'api_p95_latency_ms', '5m', '2026-09-05T10:00:00Z', 42, 8400, 'redact-v1')`,
		"00000000-0000-7000-8000-0000000000a1", m3Project)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO telemetry_aggregates
		(id, project_id, metric, window_kind, window_start, sample_count, sum_value, redaction_version)
		VALUES ($1, $2, 'api_p95_latency_ms', '5m', '2026-09-05T10:00:00Z', 1, 1, 'redact-v1')`,
		"00000000-0000-7000-8000-0000000000a2", m3Project)
	require.Error(t, err, "one window per metric per bucket")

	// Evaluation record identity: (run, case, layer) unique; security
	// layer requires risk (CHECK).
	digest := "sha256:" + strings.Repeat("e", 64)
	_, err = isolated.ExecContext(ctx, `INSERT INTO evaluation_records
		(id, layer, case_id, run_id, verdict, score, scorer_kind, scorer_version, dataset_digest)
		VALUES ($1, 'quality', 'case-1', $2, 'pass', 92, 'deterministic', 'sc-v1', $3)`,
		"00000000-0000-7000-8000-0000000000b1", "00000000-0000-7000-8000-0000000000c1", digest)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO evaluation_records
		(id, layer, case_id, run_id, verdict, score, scorer_kind, scorer_version, dataset_digest)
		VALUES ($1, 'quality', 'case-1', $2, 'fail', 10, 'rule_based', 'sc-v1', $3)`,
		"00000000-0000-7000-8000-0000000000b2", "00000000-0000-7000-8000-0000000000c1", digest)
	require.Error(t, err, "one record per run/case/layer")
	_, err = isolated.ExecContext(ctx, `INSERT INTO evaluation_records
		(id, layer, case_id, run_id, verdict, score, scorer_kind, scorer_version, dataset_digest, risk)
		VALUES ($1, 'security', 'inj-1', $2, 'fail', 0, 'rule_based', 'sc-v1', $3, NULL)`,
		"00000000-0000-7000-8000-0000000000b3", "00000000-0000-7000-8000-0000000000c2", digest)
	require.Error(t, err, "security layer records carry risk")

	// Pilot flag semantics: gray-only percentage; one flag per project.
	_, err = isolated.ExecContext(ctx, `INSERT INTO pilot_flags
		(id, project_id, flag, stage, changed_by, reason)
		VALUES ($1, $2, 'agent_autofix', 'shadow', 'po-1', 'shadow first')`,
		"00000000-0000-7000-8000-0000000000d1", m3Project)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO pilot_flags
		(id, project_id, flag, stage, gray_percent, changed_by, reason)
		VALUES ($1, $2, 'agent_autofix', 'shadow', 25, 'po-1', 'percent only in gray')`,
		"00000000-0000-7000-8000-0000000000d2", m3Project)
	require.Error(t, err, "shadow carries no percentage")
	_, err = isolated.ExecContext(ctx, `UPDATE pilot_flags SET stage = 'gray', gray_percent = 25 WHERE id = $1`,
		"00000000-0000-7000-8000-0000000000d1")
	require.NoError(t, err)

	// Full revert leaves nothing; re-apply reaches target.
	reverted, err := RevertPostgresMigrations(ctx, isolated, len(migrations))
	require.NoError(t, err)
	require.Equal(t, len(migrations), reverted)
	var objects int
	require.NoError(t, isolated.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&objects))
	assert.Zero(t, objects)

	_, err = ApplyPostgresMigrations(ctx, isolated)
	require.NoError(t, err)
	require.NoError(t, ValidatePostgresSchema(ctx, isolated))
}

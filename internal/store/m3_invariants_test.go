package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated M3 migration round trip: forward to the full defect/agent
// schema, assert the load-bearing invariants (the anchor-card tables
// exist with their uniqueness and append-only shapes), full revert
// leaves nothing, re-apply reaches target.

const m3Project = "018f7e00-0000-7000-8000-000000000002"

func TestM3DefectAgentMigrationRoundTrip(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m3_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_m3_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m3_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	isolated, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_m3_test")
	require.NoError(t, err)
	t.Cleanup(func() { isolated.Close() })

	ctx := context.Background()
	applied, err := ApplyPostgresMigrations(ctx, isolated)
	require.NoError(t, err)
	migrations, err := ParsePostgresMigrations()
	require.NoError(t, err)
	require.Equal(t, len(migrations), applied)

	// Every anchor-card table exists.
	for _, table := range []string{"api_contracts", "integration_runs", "findings",
		"defects", "defect_occurrences", "defect_task_links", "budget_ledgers",
		"budget_entries", "agent_runs"} {
		var exists int
		require.NoError(t, isolated.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`,
			table).Scan(&exists), table)
		assert.Equal(t, 1, exists, "table %s must exist", table)
	}

	seedM3Project(t, isolated)

	// Contract identity: same project+service+version cannot carry two hashes.
	_, err = isolated.ExecContext(ctx, `INSERT INTO api_contracts
		(id, project_id, service, format, version, canonical_hash, spec_digest, source_sha)
		VALUES ($1, $2, 'orders', 'openapi3-json', '1.2.0', $3, $4, $5)`,
		"00000000-0000-7000-8000-0000000000c1", m3Project, "sha256:"+strings.Repeat("a", 64),
		"sha256:"+strings.Repeat("b", 64), strings.Repeat("1", 40))
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO api_contracts
		(id, project_id, service, format, version, canonical_hash, spec_digest, source_sha)
		VALUES ($1, $2, 'orders', 'openapi3-json', '1.2.0', $3, $4, $5)`,
		"00000000-0000-7000-8000-0000000000c2", m3Project, "sha256:"+strings.Repeat("c", 64),
		"sha256:"+strings.Repeat("d", 64), strings.Repeat("2", 40))
	require.Error(t, err, "same contract version cannot carry two hashes")

	// IntegrationRun manifest identity: replays collapse.
	_, err = isolated.ExecContext(ctx, `INSERT INTO integration_runs
		(id, project_id, manifest_hash, revisions, combination_digest, status)
		VALUES ($1, $2, $3, $4::jsonb, $5, 'waiting')`,
		"00000000-0000-7000-8000-0000000000a1", m3Project, "sha256:"+strings.Repeat("e", 64),
		`[{"repository_mapping_id":"m1","sha":"`+strings.Repeat("3", 40)+`"},{"repository_mapping_id":"m2","sha":"`+strings.Repeat("4", 40)+`"}]`,
		"sha256:"+strings.Repeat("f", 64))
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO integration_runs
		(id, project_id, manifest_hash, revisions, combination_digest, status)
		VALUES ($1, $2, $3, '[]'::jsonb, $4, 'waiting')`,
		"00000000-0000-7000-8000-0000000000a2", m3Project,
		"sha256:"+strings.Repeat("e", 64), "sha256:"+strings.Repeat("f", 64))
	require.Error(t, err, "integration run identity is the manifest hash")

	// Finding ingest idempotency: (project, source_type, source_event_id).
	_, err = isolated.ExecContext(ctx, `INSERT INTO findings
		(id, project_id, source_type, source_event_id, severity, adapter_version, payload_digest)
		VALUES ($1, $2, 'pipeline', 'evt-1', 'high', 'v1', $3)`,
		"00000000-0000-7000-8000-0000000000e1", m3Project, "sha256:"+strings.Repeat("1", 64))
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO findings
		(id, project_id, source_type, source_event_id, severity, adapter_version, payload_digest)
		VALUES ($1, $2, 'pipeline', 'evt-1', 'critical', 'v1', $3)`,
		"00000000-0000-7000-8000-0000000000e2", m3Project, "sha256:"+strings.Repeat("2", 64))
	require.Error(t, err, "source event identity is the ingest dedup key")

	// Defect fingerprint uniqueness and the one-active-fix guard.
	_, err = isolated.ExecContext(ctx, `INSERT INTO defects
		(id, project_id, fingerprint_version, fingerprint_hash, state, severity, title)
		VALUES ($1, $2, 1, $3, 'detected', 'high', 'flaky checkout')`,
		"00000000-0000-7000-8000-0000000000d1", m3Project, "sha256:"+strings.Repeat("5", 64))
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO defects
		(id, project_id, fingerprint_version, fingerprint_hash, state, severity, title)
		VALUES ($1, $2, 1, $3, 'detected', 'high', 'duplicate')`,
		"00000000-0000-7000-8000-0000000000d2", m3Project, "sha256:"+strings.Repeat("5", 64))
	require.Error(t, err, "a fingerprint versions the defect space")

	_, err = isolated.ExecContext(ctx, `INSERT INTO defect_task_links (id, project_id, defect_id, work_item_id, link_kind, active)
		VALUES ('00000000-0000-7000-8000-0000000000b1', $1, '00000000-0000-7000-8000-0000000000d1', '018f7e00-0000-7000-8000-000000000003', 'fix', true)`, m3Project)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO defect_task_links (id, project_id, defect_id, work_item_id, link_kind, active)
		VALUES ('00000000-0000-7000-8000-0000000000b2', $1, '00000000-0000-7000-8000-0000000000d1', '018f7e00-0000-7000-8000-000000000004', 'fix', true)`, m3Project)
	require.Error(t, err, "one active remediation per defect")
	// Deactivating the first frees the slot (explicit takeover).
	_, err = isolated.ExecContext(ctx, `UPDATE defect_task_links SET active = false WHERE id = '00000000-0000-7000-8000-0000000000b1'`)
	require.NoError(t, err)
	_, err = isolated.ExecContext(ctx, `INSERT INTO defect_task_links (id, project_id, defect_id, work_item_id, link_kind, active)
		VALUES ('00000000-0000-7000-8000-0000000000b2', $1, '00000000-0000-7000-8000-0000000000d1', '018f7e00-0000-7000-8000-000000000004', 'fix', true)`, m3Project)
	require.NoError(t, err, "takeover after explicit deactivation is legal")

	// Full revert leaves nothing behind; re-apply reaches target.
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

func seedM3Project(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'm3 invariants')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'm3-inv', 'M3 Invariants', 'active')`, m3Project, "018f7e00-0000-7000-8000-000000000001")
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ('018f7e00-0000-7000-8000-000000000003', $1, 'fix task one', 'queued')`, m3Project)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO work_items (id, project_id, title, status) VALUES ('018f7e00-0000-7000-8000-000000000004', $1, 'fix task two', 'queued')`, m3Project)
	require.NoError(t, err)
}

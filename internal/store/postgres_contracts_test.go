package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/contract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated contract registry: versioned hash identity across both
// source forms — the CTR anchor's "no silent swap" invariant.

const ctrFixture = `{"openapi":"3.0.0","info":{"title":"svc","version":"1"},
"paths":{"/orders":{"get":{"responses":{"200":{"description":"ok"}}}}}}`

func newContractFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_ctr_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_ctr_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_ctr_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_ctr_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7f00-0000-7000-8000-000000000001', 'ctr')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7f00-0000-7000-8000-000000000001', 'ctr', 'CTR', 'active')`, m3Project)
	require.NoError(t, err)
	return pg, m3Project
}

func TestContractRegistryHashIdentity(t *testing.T) {
	pg, projectID := newContractFixture(t)
	ctx := context.Background()

	doc, err := contract.ParseDocument([]byte(ctrFixture))
	require.NoError(t, err)
	require.NoError(t, pg.Contracts().RegisterContract(ctx, projectID, doc,
		contract.FormatJSON, "orders", "1.0.0", "sha256:"+strings.Repeat("a", 64), strings.Repeat("1", 40)))

	// Identical replay is a no-op.
	require.NoError(t, pg.Contracts().RegisterContract(ctx, projectID, doc,
		contract.FormatJSON, "orders", "1.0.0", "sha256:"+strings.Repeat("a", 64), strings.Repeat("1", 40)))

	// The YAML rendering of the SAME contract carries the same hash and
	// therefore also replays cleanly — source form is not identity.
	yamlDoc, err := contract.ParseDocument([]byte("openapi: 3.0.0\ninfo:\n  title: svc\n  version: \"1\"\npaths:\n  /orders:\n    get:\n      responses:\n        \"200\":\n          description: ok\n"))
	require.NoError(t, err)
	require.Equal(t, doc.CanonicalHash, yamlDoc.CanonicalHash)
	require.NoError(t, pg.Contracts().RegisterContract(ctx, projectID, yamlDoc,
		contract.FormatYAML, "orders", "1.0.0", "sha256:"+strings.Repeat("a", 64), strings.Repeat("1", 40)))

	// A genuinely different contract under the same version is the swap
	// the registry exists to refuse.
	swapped := strings.Replace(ctrFixture, `"description":"ok"`, `"description":"changed"`, 1)
	swapDoc, err := contract.ParseDocument([]byte(swapped))
	require.NoError(t, err)
	err = pg.Contracts().RegisterContract(ctx, projectID, swapDoc,
		contract.FormatJSON, "orders", "1.0.0", "sha256:"+strings.Repeat("b", 64), strings.Repeat("2", 40))
	assert.ErrorIs(t, err, ErrContractHashConflict)

	// A NEW version registers and resolves back.
	require.NoError(t, pg.Contracts().RegisterContract(ctx, projectID, swapDoc,
		contract.FormatJSON, "orders", "1.1.0", "sha256:"+strings.Repeat("b", 64), strings.Repeat("2", 40)))
	hash, found, err := pg.Contracts().ContractVersion(ctx, projectID, "orders", "1.1.0")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, swapDoc.CanonicalHash, hash)

	_, found, err = pg.Contracts().ContractVersion(ctx, projectID, "orders", "9.9.9")
	require.NoError(t, err)
	assert.False(t, found)
}

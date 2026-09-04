package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/budget"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated budget ledger: durable open, append-only entries with the
// in-transaction totals, the pre-call ceiling, and the stop boundary.

func newBudgetFixture(t *testing.T) (*PostgresStore, string) {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_bud_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_bud_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_bud_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_bud_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'bud')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'bud', 'BUD', 'active')`, m3Project)
	require.NoError(t, err)
	return pg, m3Project
}

func TestBudgetLedgerLifecycle(t *testing.T) {
	pg, projectID := newBudgetFixture(t)
	ctx := context.Background()
	limits := budget.Limits{BudgetUnits: 1000, MaxAttempts: 3, WallTimeLimit: time.Hour}

	ledgerID, err := pg.Budgets().OpenLedger(ctx, projectID, "agent_run", "018f7e00-0000-7000-8000-0000000000a1", "pol-v1", limits)
	require.NoError(t, err)

	// Reopening the same scope replays onto the same ledger.
	replayed, err := pg.Budgets().OpenLedger(ctx, projectID, "agent_run", "018f7e00-0000-7000-8000-0000000000a1", "pol-v1", limits)
	require.NoError(t, err)
	assert.Equal(t, ledgerID, replayed)

	// Reserve -> true usage -> ceiling accounting.
	require.NoError(t, pg.Budgets().AppendEntry(ctx, ledgerID, 1, budget.Reserve, 600, "model-a"))
	require.NoError(t, pg.Budgets().AppendEntry(ctx, ledgerID, 2, budget.Release, 600, "model-a"))
	require.NoError(t, pg.Budgets().AppendEntry(ctx, ledgerID, 3, budget.Spend, 420, "model-a"))

	state, spent, reserved, _, found, err := pg.Budgets().BudgetSnapshot(ctx, ledgerID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "open", state)
	assert.Equal(t, int64(420), spent)
	assert.Equal(t, int64(0), reserved)

	// The durable pre-call gate: a reservation beyond the ceiling
	// refuses; the entry never lands.
	err = pg.Budgets().AppendEntry(ctx, ledgerID, 4, budget.Reserve, 600, "model-b")
	require.ErrorIs(t, err, budget.ErrInsufficient)

	// Stop with a reason; entries refuse afterwards; same-reason
	// re-stop is idempotent, different reason conflicts.
	require.NoError(t, pg.Budgets().StopLedger(ctx, ledgerID, budget.StopBudgetExhausted))
	require.NoError(t, pg.Budgets().StopLedger(ctx, ledgerID, budget.StopBudgetExhausted))
	require.ErrorIs(t, pg.Budgets().StopLedger(ctx, ledgerID, budget.StopManual), ErrBudgetConflict)
	require.ErrorIs(t, pg.Budgets().AppendEntry(ctx, ledgerID, 5, budget.Reserve, 10, "late"), ErrBudgetLedgerStopped)

	state, _, _, stoppedFor, _, err := pg.Budgets().BudgetSnapshot(ctx, ledgerID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", state)
	assert.Equal(t, string(budget.StopBudgetExhausted), stoppedFor)

	// Unknown ledgers answer the miss sentinel.
	_, _, _, _, found, err = pg.Budgets().BudgetSnapshot(ctx, "00000000-0000-7000-8000-00000000dead")
	require.NoError(t, err)
	assert.False(t, found)
	require.ErrorIs(t, pg.Budgets().StopLedger(ctx, "00000000-0000-7000-8000-00000000dead", budget.StopManual), ErrBudgetNotFound)
}

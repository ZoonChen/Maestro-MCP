package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/budget"
)

// PostgreSQL persistence for M3-BUD-001: the durable budget ledger.
// Entries are append-only; the pre-call gate and stop boundary live in
// internal/budget (the deterministic core) — this store persists the
// entries and the ledger state transition under the same row lock.

// BudgetSentinels.
var (
	ErrBudgetLedgerStopped = errors.New("budget ledger is stopped")
	ErrBudgetNotFound      = errors.New("budget ledger not found")
	ErrBudgetConflict      = errors.New("budget ledger state changed concurrently")
)

type pgBudgetStore struct{ db *sql.DB }

// Budgets returns the budget ledger store.
func (s *PostgresStore) Budgets() pgBudgetStore { return pgBudgetStore{db: s.DB()} }

// OpenLedger creates-or-reuses the ledger for one scope: the first
// caller provisions (defect/work_item/agent_run + id) with the budget
// ceilings; later callers replay onto the same row.
func (s pgBudgetStore) OpenLedger(ctx context.Context, projectID, scopeKind, scopeID, budgetVersion string, limits budget.Limits) (ledgerID string, err error) {
	var existing string
	err = s.db.QueryRowContext(ctx, `
		SELECT id::text FROM budget_ledgers
		WHERE scope_kind = $1 AND scope_id = $2`, scopeKind, scopeID).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case errors.Is(err, sql.ErrNoRows):
		id := pgNewUUID()
		if _, err = s.db.ExecContext(ctx, `
			INSERT INTO budget_ledgers (id, project_id, scope_kind, scope_id, budget_version,
				budget_units, max_attempts, wall_time_limit_seconds)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING`,
			id, projectID, scopeKind, scopeID, budgetVersion,
			limits.BudgetUnits, limits.MaxAttempts, int(limits.WallTimeLimit.Seconds())); err != nil {
			return "", fmt.Errorf("budgets: open: %w", err)
		}
		return id, nil
	default:
		return "", fmt.Errorf("budgets: lookup: %w", err)
	}
}

// AppendEntry appends one reserve/release/spend line under the row
// lock; a stopped ledger refuses everything.
func (s pgBudgetStore) AppendEntry(ctx context.Context, ledgerID string, seq int64, direction budget.Direction, units int64, toolRef string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("budgets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var state string
	if err = tx.QueryRowContext(ctx,
		`SELECT state FROM budget_ledgers WHERE id = $1 FOR UPDATE`, ledgerID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBudgetNotFound
		}
		return fmt.Errorf("budgets: lock: %w", err)
	}
	if state != "open" {
		return ErrBudgetLedgerStopped
	}

	var spentUnits, reservedUnits int64
	if err = tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(units) FILTER (WHERE direction = 'spend'), 0),
			COALESCE(SUM(units) FILTER (WHERE direction = 'reserve'), 0) -
			COALESCE(SUM(units) FILTER (WHERE direction = 'release'), 0)
		FROM budget_entries WHERE ledger_id = $1`, ledgerID).Scan(&spentUnits, &reservedUnits); err != nil {
		return fmt.Errorf("budgets: totals: %w", err)
	}
	var ceiling int64
	if err = tx.QueryRowContext(ctx,
		`SELECT budget_units FROM budget_ledgers WHERE id = $1`, ledgerID).Scan(&ceiling); err != nil {
		return fmt.Errorf("budgets: ceiling: %w", err)
	}
	if direction == budget.Reserve && spentUnits+reservedUnits+units > ceiling {
		return budget.ErrInsufficient
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO budget_entries (id, ledger_id, entry_seq, direction, units, tool_ref)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		pgNewUUID(), ledgerID, seq, string(direction), units, toolRef); err != nil {
		return fmt.Errorf("budgets: append: %w", err)
	}

	// The entry and the running totals commit together.
	newSpent, newReserved := spentUnits, reservedUnits
	switch direction {
	case budget.Spend:
		newSpent += units
	case budget.Reserve:
		newReserved += units
	case budget.Release:
		newReserved -= units
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE budget_ledgers SET spent_units = $2, reserved_units = $3, updated_at = now()
		WHERE id = $1`, ledgerID, newSpent, newReserved); err != nil {
		return fmt.Errorf("budgets: totals update: %w", err)
	}
	return tx.Commit()
}

// StopLedger moves the ledger to stopped with its reason; stopping an
// already-stopped ledger is idempotent only for the SAME reason.
func (s pgBudgetStore) StopLedger(ctx context.Context, ledgerID string, reason budget.StopReason) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE budget_ledgers SET state = 'stopped', stop_reason = $2, stopped_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'open'`, ledgerID, string(reason))
	if err != nil {
		return fmt.Errorf("budgets: stop: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var existing sql.NullString
		if qErr := s.db.QueryRowContext(ctx,
			`SELECT stop_reason FROM budget_ledgers WHERE id = $1`, ledgerID).Scan(&existing); qErr != nil {
			if errors.Is(qErr, sql.ErrNoRows) {
				return ErrBudgetNotFound
			}
			return qErr
		}
		if existing.Valid && existing.String == string(reason) {
			return nil
		}
		return ErrBudgetConflict
	}
	return nil
}

// BudgetSnapshot reads one ledger's persisted state.
func (s pgBudgetStore) BudgetSnapshot(ctx context.Context, ledgerID string) (state string, spent int64, reserved int64, stoppedFor string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT state, spent_units, reserved_units, COALESCE(stop_reason, '')
		FROM budget_ledgers WHERE id = $1`, ledgerID).
		Scan(&state, &spent, &reserved, &stoppedFor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, "", false, nil
	}
	if err != nil {
		return "", 0, 0, "", false, fmt.Errorf("budgets: snapshot: %w", err)
	}
	return state, spent, reserved, stoppedFor, true, nil
}

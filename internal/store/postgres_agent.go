package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/agent"
)

// PostgreSQL persistence for agent remediation runs (M3-AGT-001):
// durable state with version guards so the orchestrator's validated
// transitions land exactly once and crash recovery resumes from the
// last recorded state instead of replaying side effects.

// AgentSentinels.
var (
	ErrAgentRunConflict = errors.New("agent run state changed concurrently")
	ErrAgentRunNotFound = errors.New("agent run not found in this project")
)

type pgAgentStore struct{ db *sql.DB }

// AgentRuns returns the remediation-run store.
func (s *PostgresStore) AgentRuns() pgAgentStore { return pgAgentStore{db: s.DB()} }

// CreateRun opens (or resumes) the run for one defect under one
// attempt: first creation lands at eligibility_check; replays answer
// the durable state.
func (s pgAgentStore) CreateRun(ctx context.Context, run agent.RunContext, attempt int, budgetTokens int64) (state agent.State, created bool, err error) {
	var existing string
	err = s.db.QueryRowContext(ctx, `
		SELECT state FROM agent_runs
		WHERE project_id = $1 AND defect_id = $2 AND attempt = $3`,
		run.ProjectID, run.DefectID, attempt).Scan(&existing)
	switch {
	case err == nil:
		return agent.State(existing), false, nil
	case errors.Is(err, sql.ErrNoRows):
		if _, err = s.db.ExecContext(ctx, `
			INSERT INTO agent_runs (id, project_id, defect_id, attempt, state, config_digest, budget_tokens)
			VALUES ($1, $2, $3, $4, 'eligibility_check', $5, $6)
			ON CONFLICT DO NOTHING`,
			run.RunID, run.ProjectID, run.DefectID, attempt, "orchestrator-v1", budgetTokens); err != nil {
			return "", false, fmt.Errorf("agent runs: create: %w", err)
		}
		return agent.StateEligibilityCheck, true, nil
	default:
		return "", false, fmt.Errorf("agent runs: lookup: %w", err)
	}
}

// LoadState reads the durable state.
func (s pgAgentStore) LoadState(ctx context.Context, run agent.RunContext) (state agent.State, attempt int, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT state, attempt FROM agent_runs
		WHERE project_id = $1 AND defect_id = $2 AND id = $3`,
		run.ProjectID, run.DefectID, run.RunID).Scan(&state, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("agent runs: load: %w", err)
	}
	return state, attempt, true, nil
}

// Settle records one validated transition under the version guard.
func (s pgAgentStore) Settle(ctx context.Context, run agent.RunContext, from, to agent.State, reason agent.HandoffReason) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE agent_runs SET
			state = $4,
			handoff_reason = $5,
			updated_at = now()
		WHERE project_id = $1 AND defect_id = $2 AND id = $3 AND state = $6`,
		run.ProjectID, run.DefectID, run.RunID, string(to), nullableText(string(reason)), string(from))
	if err != nil {
		return fmt.Errorf("agent runs: settle: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		current, _, found, qErr := s.LoadState(ctx, run)
		if qErr != nil {
			return qErr
		}
		if !found {
			return ErrAgentRunNotFound
		}
		if current == to {
			return nil // idempotent re-settle of the same transition
		}
		return fmt.Errorf("%w: run is %s, expected %s", ErrAgentRunConflict, current, from)
	}
	return nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/integration"
)

// PostgreSQL persistence for M3-INT-001: IntegrationRuns keyed by the
// manifest's combination digest. The same exact combination replays
// onto the same run (E2E-RULE-001's converse); a new combination is a
// new run; passed is only ever read against the exact digest
// (E2E-RULE-003 — enforced by callers resolving by digest).

// IntegrationRunSentinels for the run lifecycle.
var (
	ErrRunStateConflict = errors.New("integration run state transition refused")
	ErrRunNotFound      = errors.New("integration run not found in this project")
)

type pgIntegrationStore struct{ db *sql.DB }

// IntegrationRuns returns the run store.
func (s *PostgresStore) IntegrationRuns() pgIntegrationStore {
	return pgIntegrationStore{db: s.DB()}
}

// StartRun finds-or-creates the run for an exact combination and moves
// a waiting/expired run into running under the given phase. An existing
// terminal run for the SAME digest is returned as-is: identical
// combinations replay, they never restart (the digest IS the identity).
func (s pgIntegrationStore) StartRun(ctx context.Context, projectID string, manifest integration.Manifest, manifestHash string) (runID string, state string, created bool, err error) {
	digest, err := manifest.CombinationDigest()
	if err != nil {
		return "", "", false, err
	}
	revisionsJSON, err := revisionsJSON(manifest)
	if err != nil {
		return "", "", false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", false, fmt.Errorf("integration runs: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	var existingState string
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, status FROM integration_runs
		WHERE project_id = $1 AND manifest_hash = $2`, projectID, manifestHash).
		Scan(&existing, &existingState)
	switch {
	case err == nil:
		// Replay: only a waiting run may enter running; terminal states
		// answer their recorded verdict for this exact combination.
		if existingState == "waiting" {
			if _, uErr := tx.ExecContext(ctx, `
				UPDATE integration_runs SET status = 'running', phase = 'provisioning', updated_at = now()
				WHERE id = $1 AND status = 'waiting'`, existing); uErr != nil {
				return "", "", false, fmt.Errorf("integration runs: start replay: %w", uErr)
			}
			existingState = "running"
		}
		if cErr := tx.Commit(); cErr != nil {
			return "", "", false, fmt.Errorf("integration runs: commit replay: %w", cErr)
		}
		return existing, existingState, false, nil
	case errors.Is(err, sql.ErrNoRows):
		runID = pgNewUUID()
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO integration_runs (id, project_id, manifest_hash, revisions,
				environment_profile_id, combination_digest, status, phase)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, 'running', 'provisioning')`,
			runID, projectID, manifestHash, revisionsJSON,
			manifest.EnvironmentProfile, digest); err != nil {
			return "", "", false, fmt.Errorf("integration runs: create: %w", err)
		}
		if cErr := tx.Commit(); cErr != nil {
			return "", "", false, fmt.Errorf("integration runs: commit: %w", cErr)
		}
		return runID, "running", true, nil
	default:
		return "", "", false, fmt.Errorf("integration runs: lookup: %w", err)
	}
}

// SettleRun moves a running run to a terminal state. Terminal states
// are immutable: settling an already-settled run refuses with
// ErrRunStateConflict.
func (s pgIntegrationStore) SettleRun(ctx context.Context, projectID, runID, state, phase string) error {
	switch state {
	case "passed", "failed", "cancelled", "expired":
	default:
		return fmt.Errorf("integration runs: %q is not a terminal state", state)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE integration_runs SET status = $3, phase = $4, updated_at = now()
		WHERE project_id = $1 AND id = $2 AND status IN ('waiting', 'running')`,
		projectID, runID, state, phase)
	if err != nil {
		return fmt.Errorf("integration runs: settle: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		current, _, _, found, qErr := s.Run(ctx, projectID, runID)
		if qErr != nil {
			return qErr
		}
		if !found {
			return ErrRunNotFound
		}
		if current == state {
			return nil // idempotent re-settle of the SAME terminal verdict
		}
		return fmt.Errorf("%w: run is %s, refusing %s", ErrRunStateConflict, current, state)
	}
	return nil
}

// Run resolves one run's status.
func (s pgIntegrationStore) Run(ctx context.Context, projectID, runID string) (state string, phase string, digest string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT status, phase, combination_digest FROM integration_runs
		WHERE project_id = $1 AND id = $2`, projectID, runID).
		Scan(&state, &phase, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("integration runs: get: %w", err)
	}
	return state, phase, digest, true, nil
}

func revisionsJSON(manifest integration.Manifest) (string, error) {
	type flat struct {
		RepositoryMappingID string `json:"repository_mapping_id"`
		SHA                 string `json:"sha"`
		ContractHash        string `json:"contract_hash,omitempty"`
	}
	out := make([]flat, 0, len(manifest.Revisions))
	for _, revision := range manifest.Revisions {
		out = append(out, flat{
			RepositoryMappingID: revision.RepositoryMappingID,
			SHA:                 revision.SHA,
			ContractHash:        revision.ContractHash,
		})
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("integration runs: revisions encode: %w", err)
	}
	return string(encoded), nil
}

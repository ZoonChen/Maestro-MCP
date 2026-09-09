package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PostgreSQL persistence for M4-PILOT-001 rollout flags over the
// migration 0012 pilot_flags table. Every recorded stage decision and
// its pilot.decision.recorded audit row land in ONE transaction (state
// change + audit are atomic); the pure transition algebra lives in
// pilot.go and the table CHECKs remain the structural backstop.

// ErrPilotFlagNotFound is returned for unknown (project, flag) pairs.
var ErrPilotFlagNotFound = errors.New("pilot flag not found")

type pgPilotStore struct{ db *sql.DB }

// Pilot returns the rollout-flag store.
func (s *PostgresStore) Pilot() pgPilotStore {
	return pgPilotStore{db: s.DB()}
}

// PilotFlag is one stored rollout flag (the migration 0012 row).
type PilotFlag struct {
	ProjectID   string
	Flag        string
	Stage       string
	GrayPercent int
	ChangedBy   string
	Reason      string
	CreatedAt   string
	UpdatedAt   string
}

const pilotFlagColumns = `project_id::text, flag, stage, gray_percent, changed_by, reason,
	created_at, updated_at`

func scanPilotFlag(scanner interface{ Scan(dest ...any) error }) (PilotFlag, error) {
	flag := PilotFlag{}
	var createdAt, updatedAt time.Time
	if err := scanner.Scan(&flag.ProjectID, &flag.Flag, &flag.Stage, &flag.GrayPercent,
		&flag.ChangedBy, &flag.Reason, &createdAt, &updatedAt); err != nil {
		return flag, err
	}
	flag.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	flag.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
	return flag, nil
}

// ListFlags returns the project's rollout flags in flag order.
func (s pgPilotStore) ListFlags(ctx context.Context, projectID string) ([]PilotFlag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pilotFlagColumns+` FROM pilot_flags WHERE project_id = $1 ORDER BY flag`, projectID)
	if err != nil {
		return nil, fmt.Errorf("pilot store: list: %w", err)
	}
	defer rows.Close()

	flags := []PilotFlag{}
	for rows.Next() {
		flag, scanErr := scanPilotFlag(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("pilot store: scan: %w", scanErr)
		}
		flags = append(flags, flag)
	}
	return flags, rows.Err()
}

// Flag returns one rollout flag.
func (s pgPilotStore) Flag(ctx context.Context, projectID, flag string) (PilotFlag, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+pilotFlagColumns+` FROM pilot_flags WHERE project_id = $1 AND flag = $2`,
		projectID, flag)
	stored, err := scanPilotFlag(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PilotFlag{}, ErrPilotFlagNotFound
	}
	if err != nil {
		return PilotFlag{}, fmt.Errorf("pilot store: get: %w", err)
	}
	return stored, nil
}

// Effect resolves the effect one feature face must apply for a flag; a
// flag without a row is inert (off), never an error.
func (s pgPilotStore) Effect(ctx context.Context, projectID, flag string) (PilotEffect, error) {
	var stage string
	err := s.db.QueryRowContext(ctx,
		`SELECT stage FROM pilot_flags WHERE project_id = $1 AND flag = $2`,
		projectID, flag).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return PilotEffectOff, nil
	}
	if err != nil {
		return PilotEffectOff, fmt.Errorf("pilot store: effect: %w", err)
	}
	return PilotEffectOf(stage), nil
}

// PutFlag records one rollout decision under the pure lifecycle guard:
// the current row is locked, the decision checked, and the state change
// plus the pilot.decision.recorded audit row are committed in ONE
// transaction. An identical target is an idempotent no-op — the flag
// returns unchanged and nothing is audited. created reports whether the
// decision registered a new flag.
func (s pgPilotStore) PutFlag(ctx context.Context, projectID, flag string, decision PilotDecision) (stored PilotFlag, created bool, err error) {
	if len([]rune(flag)) < 1 || len([]rune(flag)) > 128 {
		return PilotFlag{}, false, fmt.Errorf("%w: flag name must be 1-128 characters", ErrPilotDecisionInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PilotFlag{}, false, fmt.Errorf("pilot store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current := PilotFlagState{}
	row := tx.QueryRowContext(ctx,
		`SELECT stage, gray_percent FROM pilot_flags WHERE project_id = $1 AND flag = $2 FOR UPDATE`,
		projectID, flag)
	if err := row.Scan(&current.Stage, &current.GrayPercent); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return PilotFlag{}, false, fmt.Errorf("pilot store: lock: %w", err)
		}
		current = PilotFlagState{Stage: PilotStageOff, GrayPercent: 0, Exists: false}
	} else {
		current.Exists = true
	}

	check, validateErr := PilotValidateDecision(current, decision)
	switch {
	case validateErr != nil:
		return PilotFlag{}, false, validateErr
	case check == PilotDecisionNoop:
		stored, err = s.readFlag(ctx, tx, projectID, flag)
		if err != nil {
			return PilotFlag{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return PilotFlag{}, false, fmt.Errorf("pilot store: commit: %w", err)
		}
		return stored, false, nil
	}

	if current.Exists {
		if _, err := tx.ExecContext(ctx, `
			UPDATE pilot_flags
			SET stage = $3, gray_percent = $4, changed_by = $5, reason = $6, updated_at = now()
			WHERE project_id = $1 AND flag = $2`,
			projectID, flag, decision.Stage, decision.GrayPercent, decision.Actor, decision.Reason); err != nil {
			return PilotFlag{}, false, fmt.Errorf("pilot store: update: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pilot_flags
				(id, project_id, flag, stage, gray_percent, changed_by, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			pgNewUUID(), projectID, flag, decision.Stage, decision.GrayPercent, decision.Actor, decision.Reason); err != nil {
			return PilotFlag{}, false, fmt.Errorf("pilot store: insert: %w", err)
		}
	}
	// The recorded decision and its audit row are atomic: one commit
	// carries both or neither (pilot.decision.recorded, M4-PILOT-001).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, $2, 'pilot.decision.recorded', 'pilot_flag', $3, 'allow', $4, $3)`,
		decision.Actor, projectID, flag, decision.Reason); err != nil {
		return PilotFlag{}, false, fmt.Errorf("pilot store: audit: %w", err)
	}

	stored, err = s.readFlag(ctx, tx, projectID, flag)
	if err != nil {
		return PilotFlag{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PilotFlag{}, false, fmt.Errorf("pilot store: commit: %w", err)
	}
	return stored, !current.Exists, nil
}

func (s pgPilotStore) readFlag(ctx context.Context, tx *sql.Tx, projectID, flag string) (PilotFlag, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+pilotFlagColumns+` FROM pilot_flags WHERE project_id = $1 AND flag = $2`,
		projectID, flag)
	stored, err := scanPilotFlag(row)
	if err != nil {
		return PilotFlag{}, fmt.Errorf("pilot store: read back: %w", err)
	}
	return stored, nil
}

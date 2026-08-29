package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// PostgreSQL implementation of RunnerRegistryStore (ADR-001 /
// SEC-RUNNER-SECURITY): device identities, one-time project-bound
// enrollment codes, project bindings and connection-generation fencing.

const pgRunnerColumns = `id, display_name, device_key_hash, status, generation,
	capabilities, last_heartbeat_at, created_at, updated_at, revoked_at`

func scanPGRunner(row interface{ Scan(...any) error }) (*model.RunnerDevice, error) {
	var runner model.RunnerDevice
	var capabilities []byte
	var lastHeartbeat, revokedAt sql.NullTime
	var createdAt, updatedAt time.Time
	if err := row.Scan(&runner.ID, &runner.DisplayName, &runner.DeviceKeyHash, &runner.Status,
		&runner.Generation, &capabilities, &lastHeartbeat, &createdAt, &updatedAt, &revokedAt); err != nil {
		return nil, err
	}
	runner.ID = pgStr(runner.ID)
	if capabilities != nil {
		runner.Capabilities = capabilities
	}
	runner.CreatedAt = pgTimeString(createdAt)
	runner.UpdatedAt = pgTimeString(updatedAt)
	if lastHeartbeat.Valid {
		at := pgTimeString(lastHeartbeat.Time)
		runner.LastHeartbeatAt = &at
	}
	if revokedAt.Valid {
		at := pgTimeString(revokedAt.Time)
		runner.RevokedAt = &at
	}
	return &runner, nil
}

type pgRunnerRegistryStore struct{ q pgExecer }

func (s pgRunnerRegistryStore) CreateEnrollment(ctx context.Context, e *model.RunnerEnrollment) error {
	if e.ID == "" {
		e.ID = pgNewUUID()
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO runner_enrollments (id, project_id, code_hash, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5)`,
		pgArg(e.ID), pgArg(e.ProjectID), e.CodeHash, pgTimeArg(e.ExpiresAt), pgArg(e.CreatedBy))
	if err != nil {
		return fmt.Errorf("runner registry: create enrollment: %w", err)
	}
	return nil
}

// ConsumeEnrollment atomically burns a one-time code (compare-and-swap).
// Unknown, expired and already-consumed codes map to distinct sentinels so
// enrollment cannot be retried by replaying a captured code.
func (s pgRunnerRegistryStore) ConsumeEnrollment(ctx context.Context, enrollmentID, codeHash string) error {
	var expiresAt, consumedAt sql.NullTime
	var storedHash string
	err := s.q.QueryRowContext(ctx, `
		SELECT code_hash, expires_at, consumed_at
		FROM runner_enrollments
		WHERE id = $1
		FOR UPDATE`,
		pgArg(enrollmentID)).Scan(&storedHash, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEnrollmentInvalid
	}
	if err != nil {
		return fmt.Errorf("runner registry: lock enrollment: %w", err)
	}
	if storedHash != codeHash {
		return ErrEnrollmentInvalid
	}
	if consumedAt.Valid {
		return ErrEnrollmentConsumed
	}
	if !expiresAt.Valid || expiresAt.Time.Before(time.Now().UTC()) {
		return ErrEnrollmentExpired
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE runner_enrollments SET consumed_at = now() WHERE id = $1`,
		pgArg(enrollmentID))
	if err != nil {
		return fmt.Errorf("runner registry: consume enrollment: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrEnrollmentInvalid
	}
	return nil
}

func (s pgRunnerRegistryStore) CreateRunner(ctx context.Context, runner *model.RunnerDevice, binding *model.RunnerBinding) error {
	if runner.ID == "" {
		runner.ID = pgNewUUID()
	}
	if runner.Generation < 1 {
		runner.Generation = 1
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status, generation, capabilities)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::jsonb, '[]'::jsonb))`,
		pgArg(runner.ID), runner.DisplayName, runner.DeviceKeyHash,
		pgRunnerInitialStatus(runner.Status), runner.Generation, pgOptionalJSON(runner.Capabilities)); err != nil {
		return fmt.Errorf("runner registry: create runner: %w", err)
	}
	if binding != nil {
		if _, err := s.q.ExecContext(ctx, `
			INSERT INTO runner_bindings (project_id, runner_id) VALUES ($1, $2)
			ON CONFLICT (project_id, runner_id) DO NOTHING`,
			pgArg(binding.ProjectID), pgArg(runner.ID)); err != nil {
			return fmt.Errorf("runner registry: bind runner: %w", err)
		}
	}
	return nil
}

// pgRunnerInitialStatus guards insertable device states: devices enter the
// lifecycle at pending_approval or approved (admin pre-approval); online and
// beyond are runtime transitions, revoked is terminal-only via RevokeRunner.
func pgRunnerInitialStatus(status string) string {
	switch status {
	case "", model.RunnerStatusPendingApproval:
		return model.RunnerStatusPendingApproval
	case model.RunnerStatusApproved:
		return model.RunnerStatusApproved
	default:
		return model.RunnerStatusPendingApproval
	}
}

func (s pgRunnerRegistryStore) GetRunner(ctx context.Context, id string) (*model.RunnerDevice, error) {
	row := s.q.QueryRowContext(ctx, `SELECT `+pgRunnerColumns+` FROM runners WHERE id = $1`, pgArg(id))
	runner, err := scanPGRunner(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunnerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runner registry: get runner: %w", err)
	}
	return runner, nil
}

func (s pgRunnerRegistryStore) UpdateRunnerStatus(ctx context.Context, id, expectedStatus, newStatus string) error {
	if newStatus == model.RunnerStatusRevoked {
		return fmt.Errorf("runner registry: %w: revoked is terminal and must go through RevokeRunner", ErrRunnerStatusInvalid)
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE runners SET status = $3, updated_at = now()
		WHERE id = $1 AND status = $2 AND status <> 'revoked'`,
		pgArg(id), expectedStatus, newStatus)
	if err != nil {
		return fmt.Errorf("runner registry: update status: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		if _, getErr := s.GetRunner(ctx, id); getErr != nil {
			return getErr
		}
		return ErrRunnerStatusInvalid
	}
	return nil
}

// BumpRunnerGeneration rotates the fencing generation; messages carrying
// an older generation must be rejected from this point on.
func (s pgRunnerRegistryStore) BumpRunnerGeneration(ctx context.Context, id string) (int64, error) {
	var generation int64
	err := s.q.QueryRowContext(ctx, `
		UPDATE runners SET generation = generation + 1, updated_at = now()
		WHERE id = $1 AND status <> 'revoked'
		RETURNING generation`,
		pgArg(id)).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		if _, getErr := s.GetRunner(ctx, id); getErr != nil {
			return 0, getErr
		}
		return 0, ErrRunnerRevoked
	}
	if err != nil {
		return 0, fmt.Errorf("runner registry: bump generation: %w", err)
	}
	return generation, nil
}

func (s pgRunnerRegistryStore) UpdateRunnerHeartbeat(ctx context.Context, id string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE runners SET last_heartbeat_at = now(), updated_at = now()
		WHERE id = $1 AND status IN ('approved', 'online', 'suspect')`,
		pgArg(id))
	if err != nil {
		return fmt.Errorf("runner registry: update heartbeat: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		if _, getErr := s.GetRunner(ctx, id); getErr != nil {
			return getErr
		}
		return ErrRunnerStatusInvalid
	}
	return nil
}

func (s pgRunnerRegistryStore) ListRunnersByProject(ctx context.Context, projectID string) ([]*model.RunnerDevice, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT r.id, r.display_name, r.device_key_hash, r.status, r.generation,
		       r.capabilities, r.last_heartbeat_at, r.created_at, r.updated_at, r.revoked_at
		FROM runners r
		JOIN runner_bindings b ON b.runner_id = r.id
		WHERE b.project_id = $1
		ORDER BY r.created_at, r.id`,
		pgArg(projectID))
	if err != nil {
		return nil, fmt.Errorf("runner registry: list runners: %w", err)
	}
	defer rows.Close()
	runners := []*model.RunnerDevice{}
	for rows.Next() {
		runner, scanErr := scanPGRunner(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("runner registry: scan runner: %w", scanErr)
		}
		runners = append(runners, runner)
	}
	return runners, rows.Err()
}

// RevokeRunner is the only path into the terminal revoked state and stamps
// the revocation time for audit and fencing checks.
func (s pgRunnerRegistryStore) RevokeRunner(ctx context.Context, id string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE runners SET status = 'revoked', revoked_at = now(), updated_at = now()
		WHERE id = $1 AND status <> 'revoked'`,
		pgArg(id))
	if err != nil {
		return fmt.Errorf("runner registry: revoke runner: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		runner, getErr := s.GetRunner(ctx, id)
		if getErr != nil {
			return getErr
		}
		if runner.Status == model.RunnerStatusRevoked {
			return nil // idempotent revoke on the terminal state
		}
		return ErrRunnerStatusInvalid
	}
	return nil
}

// EnrollmentByCodeHash resolves a live enrollment by its storage hash.
func (s pgRunnerRegistryStore) EnrollmentByCodeHash(ctx context.Context, codeHash string) (*model.RunnerEnrollment, string, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, project_id, code_hash, expires_at, consumed_at, created_by, created_at
		FROM runner_enrollments
		WHERE code_hash = $1`, codeHash)
	enrollment, projectID, err := scanPGEnrollmentWithProject(row.Scan)
	if err != nil {
		return nil, "", fmt.Errorf("runner registry: enrollment lookup: %w", err)
	}
	return enrollment, projectID, nil
}

// ProjectOfRunner resolves the runner's single project binding.
func (s pgRunnerRegistryStore) ProjectOfRunner(ctx context.Context, runnerID string) (string, error) {
	var projectID string
	err := s.q.QueryRowContext(ctx, `
		SELECT project_id FROM runner_bindings WHERE runner_id = $1`, pgArg(runnerID)).Scan(&projectID)
	if err != nil {
		return "", fmt.Errorf("runner registry: project binding: %w", err)
	}
	return projectID, nil
}

func scanPGEnrollmentWithProject(scan func(...any) error) (*model.RunnerEnrollment, string, error) {
	var enrollment model.RunnerEnrollment
	var expiresAt, createdAt time.Time
	var consumedAt sql.NullTime
	if err := scan(&enrollment.ID, &enrollment.ProjectID, &enrollment.CodeHash,
		&expiresAt, &consumedAt, &enrollment.CreatedBy, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", ErrEnrollmentInvalid
		}
		return nil, "", err
	}
	enrollment.ID = pgArg(enrollment.ID)
	enrollment.ProjectID = pgArg(enrollment.ProjectID)
	enrollment.CreatedBy = pgArg(enrollment.CreatedBy)
	enrollment.ExpiresAt = pgTimeString(expiresAt)
	enrollment.CreatedAt = pgTimeString(createdAt)
	if consumedAt.Valid {
		at := pgTimeString(consumedAt.Time)
		enrollment.ConsumedAt = &at
	}
	return &enrollment, enrollment.ProjectID, nil
}

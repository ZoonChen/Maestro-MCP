package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Revocation surface of both runbooks (M4-RBK-001 slice C2): a
// revoked device is terminal — every later device-authenticated call
// is refused (the handler answers 410 RUNNER_REVOKED; enrollment
// needs a fresh one-time code, never in-place revival). Revocation
// disposes of the device's in-flight leases — cancelled, executions
// cancelled, orphaned work items requeued for a healthy runner with a
// fresh epoch — and lands the `runner.revoke` audit row. The device
// key is referenced by its stored hash only; no key material, token
// or Keychain secret ever enters the audit trail or a log line.

// RevokeReport is the revocation outcome for the incident record.
type RevokeReport struct {
	RunnerID        string
	ProjectID       string
	AlreadyRevoked  bool
	CancelledLeases []LeaseExpiry
	Redispatched    []Redispatch
	FrozenWaiting   int
	RevokedAt       time.Time
}

// ErrRevocationRequiresIncident pins the runbook's event discipline:
// no revocation without an incident correlation ID.
var ErrRevocationRequiresIncident = errors.New("runner revocation requires an incident correlation id")

// RevokeRunner terminally revokes a device and disposes of its
// leases. Idempotent per the runbook (section 8): a repeat on an
// already-revoked device returns the same business result — leases
// already cancelled, no second audit transition — so operators can
// retry safely mid-incident.
func (o *Ops) RevokeRunner(ctx context.Context, runnerID, actor, incident string) (*RevokeReport, error) {
	if incident == "" {
		return nil, ErrRevocationRequiresIncident
	}
	if actor == "" {
		actor = "security_owner"
	}
	tx, err := o.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("runner revoke: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	var keyHash string
	err = tx.QueryRowContext(ctx, `
		SELECT status, device_key_hash FROM runners WHERE id = $1 FOR UPDATE`, runnerID).Scan(&status, &keyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrRunnerNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runner revoke: lookup: %w", err)
	}
	report := &RevokeReport{RunnerID: runnerID, CancelledLeases: []LeaseExpiry{}, Redispatched: []Redispatch{}}

	projectID, err := o.projectOfRunner(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	report.ProjectID = projectID

	if status == model.RunnerStatusRevoked {
		// Idempotent replay: no state change, no duplicate audit row.
		// Any lease that appeared since (none can — revoked devices
		// fail requireDevice) would still be cancelled here.
		cancelled, err := cancelRunnerLeasesTx(ctx, tx, runnerID, incident)
		if err != nil {
			return nil, err
		}
		report.AlreadyRevoked = true
		report.CancelledLeases = cancelled
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("runner revoke: commit replay: %w", err)
		}
		return report, nil
	}

	revokedAt := o.Now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE runners SET status = 'revoked', revoked_at = now(), updated_at = now()
		WHERE id = $1 AND status <> 'revoked'`, runnerID); err != nil {
		return nil, fmt.Errorf("runner revoke: transition: %w", err)
	}

	cancelled, err := cancelRunnerLeasesTx(ctx, tx, runnerID, incident)
	if err != nil {
		return nil, err
	}
	report.CancelledLeases = cancelled

	redispatched, frozenWaiting, err := o.redispatchQueued(ctx, tx, incident)
	if err != nil {
		return nil, err
	}
	report.Redispatched = redispatched
	report.FrozenWaiting = frozenWaiting

	cancelledLeaseIDs := make([]string, 0, len(cancelled))
	for _, expiry := range cancelled {
		cancelledLeaseIDs = append(cancelledLeaseIDs, expiry.LeaseID)
	}
	if err := recordOpsEvent(ctx, tx, opsEvent{
		Actor:         actor,
		ProjectID:     projectID,
		Action:        AuditActionRunnerRevoked,
		ResourceType:  "runner",
		ResourceID:    runnerID,
		CorrelationID: incident,
		Reason: map[string]any{
			"incident_id":      incident,
			"runner_id":        runnerID,
			"device_key_hash":  keyHash, // hash reference only — the Keychain red line
			"cancelled_leases": cancelledLeaseIDs,
			"terminal":         true,
			"rejoin":           "fresh enrollment + approval only; no in-place revival",
			"revoked_at":       revokedAt.Format(time.RFC3339Nano),
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("runner revoke: commit: %w", err)
	}
	report.RevokedAt = revokedAt
	return report, nil
}

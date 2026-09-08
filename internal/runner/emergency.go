package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Emergency stop controller (M4-RBK-001 slice C3,
// RUNBOOK-EMERGENCY-STOP-CREDENTIAL-REVOKE): the P0 write freeze.
//
// Triggering stops new work issuance on the surfaces this package
// owns — redispatch is refused fail-closed and GuardClaim answers
// ErrWriteFrozen for the claim wiring (see the registered change
// request for the handler mount) — and fences everything in flight:
// every active lease in scope expires immediately, so late runner
// results hit the store's generation/status fences (late evidence
// only). Revocation linkage disposes of compromised device identities
// in the same transaction. The stop itself is an append-only
// `security.emergency_stop` audit row: the durable freeze record the
// controller replays on startup, so a restart cannot silently lose
// the freeze. Lifting requires the same incident, two approvers, and
// never narrows a wider freeze; staged recovery then requeues the
// waiting work items with fresh epochs.

// EmergencyScopeGlobal is the widest freeze scope; any other scope
// string is a project ID.
const EmergencyScopeGlobal = "global"

var (
	// ErrWriteFrozen is the fail-closed answer for every new write
	// while a freeze covers the scope (maps to HTTP 503 once the
	// handler wiring lands).
	ErrWriteFrozen = errors.New("emergency write freeze is active")
	// ErrFreezeHierarchy rejects a scope change that would narrow or
	// shadow an active wider freeze.
	ErrFreezeHierarchy = errors.New("emergency freeze scope conflict: a narrower scope cannot override a wider freeze")
	// ErrEmergencyApproval enforces the two-person rule.
	ErrEmergencyApproval = errors.New("emergency stop and recovery require two distinct approvers")
	// ErrEmergencyRequiresIncident pins the event discipline.
	ErrEmergencyRequiresIncident = errors.New("emergency stop requires an incident correlation id")
)

// EmergencyFreeze is one active stop.
type EmergencyFreeze struct {
	Scope     string // EmergencyScopeGlobal or a project ID
	Incident  string
	Approvers []string
	StoppedAt time.Time
	Actor     string
}

// EmergencyController owns the freeze set. Reads are lock-guarded so
// GuardClaim stays cheap on the dispatch path.
type EmergencyController struct {
	store *store.PostgresStore

	mu        sync.RWMutex
	global    *EmergencyFreeze
	byProject map[string]*EmergencyFreeze
}

// NewEmergencyController binds the controller to an open, migrated
// store. Call LoadFreezeState after construction (and before serving
// traffic) to replay any durable stop.
func NewEmergencyController(st *store.PostgresStore) (*EmergencyController, error) {
	if st == nil {
		return nil, fmt.Errorf("emergency controller: nil store")
	}
	return &EmergencyController{store: st, byProject: map[string]*EmergencyFreeze{}}, nil
}

// StopReport is the stop outcome for the incident record.
type StopReport struct {
	Scope          string
	Incident       string
	Approvers      []string
	AlreadyStopped bool
	ExpiredLeases  []LeaseExpiry
	RevokedRunners []RevocationOutcome
	StoppedAt      time.Time
}

// RevocationOutcome records one linked credential disposal.
type RevocationOutcome struct {
	RunnerID       string
	AlreadyRevoked bool
}

// TriggerEmergencyStop freezes the scope, fences in-flight leases and
// optionally revokes compromised devices — all in one transaction
// with the audit row (state change and audit are atomic). Idempotent
// per (incident, scope): a replayed trigger returns the current
// freeze without touching leases again.
func (e *EmergencyController) TriggerEmergencyStop(ctx context.Context, scope, incident, actor string, approvers []string, revokeRunnerIDs []string) (*StopReport, error) {
	if scope == "" || incident == "" {
		return nil, ErrEmergencyRequiresIncident
	}
	if len(distinct(approvers)) < 2 {
		return nil, ErrEmergencyApproval
	}
	if actor == "" {
		actor = "security_owner"
	}

	// Hierarchy: a project trigger under an active global freeze adds
	// nothing; reject rather than shadow the wider record.
	e.mu.RLock()
	conflict := e.global != nil && scope != EmergencyScopeGlobal
	e.mu.RUnlock()
	if conflict {
		return nil, ErrFreezeHierarchy
	}

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("emergency stop: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency: the stop event is the durable record; a replayed
	// (incident, scope) pair must not fence leases twice.
	var exists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM audit_events
		WHERE action = $1 AND correlation_id = $2 AND resource_id = $3`,
		AuditActionEmergencyStop, incident, scope).Scan(&exists)
	if err == nil {
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("emergency stop: rollback replay: %w", err)
		}
		return e.replayStopReport(scope, incident), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("emergency stop: idempotency probe: %w", err)
	}

	report := &StopReport{Scope: scope, Incident: incident, Approvers: distinct(approvers)}

	// Credential revocation linkage first: leases of revoked devices
	// are cancelled (cause runner_revoked); the freeze expiry below
	// then catches everything else active in scope.
	report.RevokedRunners = []RevocationOutcome{}
	for _, runnerID := range revokeRunnerIDs {
		outcome, err := revokeRunnerInTx(ctx, tx, runnerID, incident, actor)
		if err != nil {
			return nil, err
		}
		report.RevokedRunners = append(report.RevokedRunners, outcome)
	}

	expired, err := expireAllActiveLeases(ctx, tx, scope, incident)
	if err != nil {
		return nil, err
	}
	report.ExpiredLeases = expired
	report.StoppedAt = time.Now().UTC()

	revokedIDs := make([]string, 0, len(report.RevokedRunners))
	for _, outcome := range report.RevokedRunners {
		revokedIDs = append(revokedIDs, outcome.RunnerID)
	}
	expiredIDs := make([]string, 0, len(expired))
	for _, expiry := range expired {
		expiredIDs = append(expiredIDs, expiry.LeaseID)
	}
	if err := recordOpsEvent(ctx, tx, opsEvent{
		Actor:         actor,
		ProjectID:     projectScopeOrNil(scope),
		Action:        AuditActionEmergencyStop,
		ResourceType:  "security",
		ResourceID:    scope,
		CorrelationID: incident,
		Reason: map[string]any{
			"incident_id":     incident,
			"freeze_scope":    scope,
			"approvers":       report.Approvers,
			"expired_leases":  expiredIDs,
			"revoked_runners": revokedIDs,
			"stopped_at":      report.StoppedAt.Format(time.RFC3339Nano),
			"freeze_effect":   "no new lease dispatch or redispatch; in-flight results are late evidence only",
			"recovery":        "same-incident lift with two approvers; staged requeue follows",
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("emergency stop: commit: %w", err)
	}

	freeze := &EmergencyFreeze{Scope: scope, Incident: incident, Approvers: report.Approvers, StoppedAt: report.StoppedAt, Actor: actor}
	e.mu.Lock()
	if scope == EmergencyScopeGlobal {
		e.global = freeze
	} else {
		e.byProject[scope] = freeze
	}
	e.mu.Unlock()
	return report, nil
}

// LiftEmergencyStop ends the freeze for the incident. The two-person
// rule applies; a project lift under a global freeze fails closed
// (runbook section 8: the later write may only keep or strengthen).
// Waiting work items return to the queue on the next monitor sweep
// with fresh epochs — the staged recovery.
func (e *EmergencyController) LiftEmergencyStop(ctx context.Context, scope, incident, actor string, approvers []string) error {
	if scope == "" || incident == "" {
		return ErrEmergencyRequiresIncident
	}
	if len(distinct(approvers)) < 2 {
		return ErrEmergencyApproval
	}
	if actor == "" {
		actor = "operations_owner"
	}

	e.mu.Lock()
	var active *EmergencyFreeze
	if scope == EmergencyScopeGlobal {
		active = e.global
	} else {
		active = e.byProject[scope]
	}
	if active == nil || active.Incident != incident {
		e.mu.Unlock()
		return fmt.Errorf("emergency lift: no active freeze for scope %s incident %s", scope, incident)
	}
	if scope != EmergencyScopeGlobal && e.global != nil {
		e.mu.Unlock()
		return ErrFreezeHierarchy
	}
	e.mu.Unlock()

	tx, err := e.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("emergency lift: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordOpsEvent(ctx, tx, opsEvent{
		Actor:         actor,
		ProjectID:     projectScopeOrNil(scope),
		Action:        AuditActionEmergencyRecovered,
		ResourceType:  "security",
		ResourceID:    scope,
		CorrelationID: incident,
		Reason: map[string]any{
			"incident_id":  incident,
			"freeze_scope": scope,
			"approvers":    distinct(approvers),
			"recovery":     "redispatch re-enabled; monitor sweep requeues waiting items with fresh epochs",
		},
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("emergency lift: commit: %w", err)
	}

	e.mu.Lock()
	if scope == EmergencyScopeGlobal {
		e.global = nil
	} else {
		delete(e.byProject, scope)
	}
	e.mu.Unlock()
	return nil
}

// GuardClaim is the claim-path freeze check: ErrWriteFrozen while the
// project (or the whole plane) is frozen. The registered handler
// wiring turns this into the protocol's stable refusal.
func (e *EmergencyController) GuardClaim(projectID string) error {
	if e.FrozenProject(projectID) {
		return fmt.Errorf("%w (project %s)", ErrWriteFrozen, projectID)
	}
	return nil
}

// FrozenProject reports whether any freeze covers the project.
func (e *EmergencyController) FrozenProject(projectID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.global != nil || e.byProject[projectID] != nil
}

// FrozenScopes snapshots the active freezes (diagnostics and drills).
func (e *EmergencyController) FrozenScopes() []EmergencyFreeze {
	e.mu.RLock()
	defer e.mu.RUnlock()
	scopes := []EmergencyFreeze{}
	if e.global != nil {
		scopes = append(scopes, *e.global)
	}
	for _, freeze := range e.byProject {
		scopes = append(scopes, *freeze)
	}
	return scopes
}

// LoadFreezeState replays the durable stop records from the
// append-only audit chain: every security.emergency_stop whose
// (scope, incident) has no matching recover re-freezes the
// controller. A restart therefore cannot silently drop a freeze.
func (e *EmergencyController) LoadFreezeState(ctx context.Context) error {
	rows, err := e.store.DB().QueryContext(ctx, `
		SELECT a.resource_id, a.correlation_id, a.actor_principal, a.reason, a.created_at,
		       EXISTS (SELECT 1 FROM audit_events r
		               WHERE r.action = $2 AND r.resource_id = a.resource_id
		                 AND r.correlation_id = a.correlation_id) AS recovered
		FROM audit_events a
		WHERE a.action = $1
		ORDER BY a.created_at, a.id`, AuditActionEmergencyStop, AuditActionEmergencyRecovered)
	if err != nil {
		return fmt.Errorf("emergency load: %w", err)
	}
	defer rows.Close()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.global = nil
	e.byProject = map[string]*EmergencyFreeze{}
	for rows.Next() {
		var scope, incident, actor, reason string
		var stoppedAt time.Time
		var recovered bool
		if err := rows.Scan(&scope, &incident, &actor, &reason, &stoppedAt, &recovered); err != nil {
			return fmt.Errorf("emergency load: scan: %w", err)
		}
		if recovered || scope == "" {
			continue
		}
		freeze := &EmergencyFreeze{Scope: scope, Incident: incident, Actor: actor, StoppedAt: stoppedAt.UTC()}
		if scope == EmergencyScopeGlobal {
			e.global = freeze
		} else {
			e.byProject[scope] = freeze
		}
	}
	return rows.Err()
}

func (e *EmergencyController) replayStopReport(scope, incident string) *StopReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	report := &StopReport{Scope: scope, Incident: incident, AlreadyStopped: true, ExpiredLeases: []LeaseExpiry{}, RevokedRunners: []RevocationOutcome{}}
	if scope == EmergencyScopeGlobal && e.global != nil && e.global.Incident == incident {
		report.Approvers = e.global.Approvers
		report.StoppedAt = e.global.StoppedAt
	}
	if freeze, ok := e.byProject[scope]; ok && freeze.Incident == incident {
		report.Approvers = freeze.Approvers
		report.StoppedAt = freeze.StoppedAt
	}
	return report
}

// expireAllActiveLeases fences every active lease in scope
// immediately — unlike the monitor's TTL sweep this does not wait for
// expires_at: the emergency stop invalidates all active leases
// (runbook section 4.1.1 / 6).
func expireAllActiveLeases(ctx context.Context, tx *sql.Tx, scope, correlationID string) ([]LeaseExpiry, error) {
	query := `
		WITH fenced AS (
			SELECT l.id, l.project_id, l.work_item_id, l.runner_id, l.epoch, l.connection_generation
			FROM leases l
			WHERE l.status = 'active'
			%s
			FOR UPDATE OF l SKIP LOCKED
		), marked AS (
			UPDATE leases l SET status = 'expired', updated_at = now()
			FROM fenced f WHERE l.id = f.id
			RETURNING f.id, f.project_id, f.work_item_id, f.runner_id, f.epoch, f.connection_generation
		), interrupted AS (
			UPDATE executions e SET status = 'interrupted', ended_at = now()
			FROM marked m WHERE e.lease_id = m.id AND e.status = 'running'
			RETURNING e.id
		)
		SELECT id, project_id, work_item_id, runner_id, epoch, connection_generation FROM marked`
	var rows *sql.Rows
	var err error
	if scope == EmergencyScopeGlobal {
		rows, err = tx.QueryContext(ctx, fmt.Sprintf(query, ""))
	} else {
		rows, err = tx.QueryContext(ctx, fmt.Sprintf(query, "AND l.project_id = $1"), scope)
	}
	if err != nil {
		return nil, fmt.Errorf("emergency stop: fence leases: %w", err)
	}
	defer rows.Close()
	expiries := []LeaseExpiry{}
	for rows.Next() {
		expiry := LeaseExpiry{CorrelationID: correlationID, Reason: "emergency_stop"}
		if err := rows.Scan(&expiry.LeaseID, &expiry.ProjectID, &expiry.WorkItemID,
			&expiry.RunnerID, &expiry.Epoch, &expiry.ConnectionGeneration); err != nil {
			return nil, fmt.Errorf("emergency stop: scan fenced lease: %w", err)
		}
		expiries = append(expiries, expiry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("emergency stop: fence leases rows: %w", err)
	}
	for _, expiry := range expiries {
		if err := recordOpsEvent(ctx, tx, opsEvent{
			Actor:         "control-plane:emergency-stop",
			ProjectID:     expiry.ProjectID,
			Action:        AuditActionLeaseExpired,
			ResourceType:  "lease",
			ResourceID:    expiry.LeaseID,
			CorrelationID: correlationID,
			Reason: map[string]any{
				"work_item_id":          expiry.WorkItemID,
				"runner_id":             expiry.RunnerID,
				"lease_epoch":           expiry.Epoch,
				"connection_generation": expiry.ConnectionGeneration,
				"cause":                 "emergency_stop",
				"late_evidence_only":    true,
			},
		}); err != nil {
			return nil, err
		}
	}
	return expiries, nil
}

// revokeRunnerInTx is the emergency-linked revocation: same terminal
// transition and audit discipline as Ops.RevokeRunner, inside the
// caller's stop transaction. Lease disposal for these devices rides
// the cause-specific cancel above.
func revokeRunnerInTx(ctx context.Context, tx *sql.Tx, runnerID, incident, actor string) (RevocationOutcome, error) {
	outcome := RevocationOutcome{RunnerID: runnerID}
	var status, keyHash string
	err := tx.QueryRowContext(ctx, `
		SELECT status, device_key_hash FROM runners WHERE id = $1 FOR UPDATE`, runnerID).Scan(&status, &keyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return outcome, store.ErrRunnerNotFound
	}
	if err != nil {
		return outcome, fmt.Errorf("emergency revoke: lookup: %w", err)
	}
	if status == model.RunnerStatusRevoked {
		outcome.AlreadyRevoked = true
		return outcome, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runners SET status = 'revoked', revoked_at = now(), updated_at = now()
		WHERE id = $1 AND status <> 'revoked'`, runnerID); err != nil {
		return outcome, fmt.Errorf("emergency revoke: transition: %w", err)
	}
	cancelled, err := cancelRunnerLeasesTx(ctx, tx, runnerID, incident)
	if err != nil {
		return outcome, err
	}
	cancelledIDs := make([]string, 0, len(cancelled))
	for _, expiry := range cancelled {
		cancelledIDs = append(cancelledIDs, expiry.LeaseID)
	}
	if err := recordOpsEvent(ctx, tx, opsEvent{
		Actor:         actor,
		ProjectID:     projectBindingQuery(ctx, tx, runnerID),
		Action:        AuditActionRunnerRevoked,
		ResourceType:  "runner",
		ResourceID:    runnerID,
		CorrelationID: incident,
		Reason: map[string]any{
			"incident_id":      incident,
			"runner_id":        runnerID,
			"device_key_hash":  keyHash, // hash reference only — the Keychain red line
			"cancelled_leases": cancelledIDs,
			"terminal":         true,
			"linked_to":        AuditActionEmergencyStop,
		},
	}); err != nil {
		return outcome, err
	}
	return outcome, nil
}

func projectScopeOrNil(scope string) string {
	if scope == EmergencyScopeGlobal {
		return ""
	}
	return scope
}

func distinct(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		if _, dup := seen[value]; dup || value == "" {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

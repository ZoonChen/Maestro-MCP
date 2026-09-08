package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Control-plane side of the two S3-owned runbooks (M4-RBK-001):
// runner-offline (offline determination, lease expiry, safe redispatch)
// and emergency-stop-credential-revoke (write freeze, in-flight fencing,
// revocation linkage). The member-side daemon in this package produces
// the signals (heartbeats, connection generations); this file and its
// siblings consume them server-side. Every state change lands one row
// in the append-only audit_events chain — the durable record the M4
// audit export walks — keyed by the incident's correlation ID.

// Frozen protocol actions recorded in audit_events (runbook sections 9
// of both documents: the audit MUST cover offline determination,
// drain/revoke, fencing and redispatch).
const (
	AuditActionSuspectDetermined  = "runner.suspect_determined"
	AuditActionOfflineDetermined  = "runner.offline_determined"
	AuditActionLeaseExpired       = "runner.lease_expired"
	AuditActionRedispatchQueued   = "work_item.redispatch_queued"
	AuditActionRunnerRevoked      = "runner.revoke"
	AuditActionEmergencyStop      = "security.emergency_stop"
	AuditActionEmergencyRecovered = "security.emergency_recover"
)

// Ops is the runbook operations surface over the PostgreSQL control
// plane: the offline monitor (monitor.go), the revocation path
// (revoke.go) and the emergency stop controller (emergency.go) share
// its audit writer and the store handle. It is safe for concurrent
// use.
type Ops struct {
	store *store.PostgresStore
	// emergency is the freeze authority consulted before any
	// redispatch; see emergency.go.
	emergency *EmergencyController
	now       func() time.Time
}

// NewOps binds the operations surface to an open, migrated store.
func NewOps(st *store.PostgresStore, emergency *EmergencyController) (*Ops, error) {
	if st == nil {
		return nil, fmt.Errorf("runner ops: nil store")
	}
	if emergency == nil {
		return nil, fmt.Errorf("runner ops: an emergency controller is required (freeze must gate redispatch)")
	}
	return &Ops{store: st, emergency: emergency, now: time.Now}, nil
}

// Now is the injected clock (report timing; interval comparisons use
// the database clock so sweeps and TTLs never disagree).
func (o *Ops) Now() time.Time { return o.now().UTC() }

// opsEvent is one immutable audit row. Reason must carry only
// server-issued values (IDs, epochs, generations, timestamps) — never
// credentials, tokens or key material; the Keychain red line applies
// to every runbook surface (CLAUDE.md, SEC-RUNNER-SECURITY section 9).
type opsEvent struct {
	Actor         string
	ProjectID     string // empty for control-plane-wide rows
	Action        string
	ResourceType  string
	ResourceID    string
	Decision      string // allow for executed operations, deny for refusals
	CorrelationID string
	Reason        map[string]any
}

// dbExecer is the minimal write surface shared by pool and
// transaction scopes so state change and audit land atomically.
type dbExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordOpsEvent(ctx context.Context, db dbExecer, event opsEvent) error {
	if event.Actor == "" || event.Action == "" || event.CorrelationID == "" {
		return fmt.Errorf("runner ops: audit event requires actor, action and correlation id")
	}
	reason, err := json.Marshal(event.Reason)
	if err != nil {
		return fmt.Errorf("runner ops: audit reason: %w", err)
	}
	var projectID any
	if event.ProjectID != "" {
		projectID = event.ProjectID
	}
	var resourceID any
	if event.ResourceID != "" {
		resourceID = event.ResourceID
	}
	decision := event.Decision
	if decision == "" {
		decision = "allow"
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		event.Actor, projectID, event.Action, event.ResourceType, resourceID, decision, string(reason), event.CorrelationID); err != nil {
		return fmt.Errorf("runner ops: audit %s: %w", event.Action, err)
	}
	return nil
}

// projectOfRunner resolves the runner's single project binding for
// audit scoping (L4 isolation mirrors the registry contract).
func (o *Ops) projectOfRunner(ctx context.Context, runnerID string) (string, error) {
	projectID, err := o.store.RunnerRegistry().ProjectOfRunner(ctx, runnerID)
	if err != nil {
		return "", fmt.Errorf("runner ops: project binding for %s: %w", runnerID, err)
	}
	return projectID, nil
}

// projectBindingQuery is the transaction-scoped binding lookup.
func projectBindingQuery(ctx context.Context, tx *sql.Tx, runnerID string) string {
	var projectID string
	if err := tx.QueryRowContext(ctx,
		`SELECT project_id FROM runner_bindings WHERE runner_id = $1`, runnerID).Scan(&projectID); err != nil {
		return ""
	}
	return projectID
}

// expireLeases marks the caller-selected active leases expired, their
// running executions interrupted, and leaves the work items untouched
// — redispatch is a separate, freeze-gated decision. scopeProject
// narrows the sweep to one project; nil means every project (the
// emergency stop scope). The reason string rides the audit row.
func (o *Ops) expireLeases(ctx context.Context, tx *sql.Tx, scopeProject *string, correlationID string) ([]LeaseExpiry, error) {
	query := `
		WITH stale AS (
			SELECT l.id, l.project_id, l.work_item_id, l.runner_id, l.epoch, l.connection_generation
			FROM leases l
			WHERE l.status = 'active' AND l.expires_at < now()
			%s
			FOR UPDATE OF l SKIP LOCKED
		), marked AS (
			UPDATE leases l SET status = 'expired', updated_at = now()
			FROM stale s WHERE l.id = s.id
			RETURNING s.id, s.project_id, s.work_item_id, s.runner_id, s.epoch, s.connection_generation
		), interrupted AS (
			UPDATE executions e SET status = 'interrupted', ended_at = now()
			FROM marked m WHERE e.lease_id = m.id AND e.status = 'running'
			RETURNING e.id
		)
		SELECT id, project_id, work_item_id, runner_id, epoch, connection_generation FROM marked`
	if scopeProject != nil {
		query = fmt.Sprintf(query, "AND l.project_id = $1")
	} else {
		query = fmt.Sprintf(query, "")
	}
	var rows *sql.Rows
	var err error
	if scopeProject != nil {
		rows, err = tx.QueryContext(ctx, query, *scopeProject)
	} else {
		rows, err = tx.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, fmt.Errorf("runner ops: expire leases: %w", err)
	}
	defer rows.Close()
	expiries := []LeaseExpiry{}
	for rows.Next() {
		expiry := LeaseExpiry{CorrelationID: correlationID, Reason: "lease_ttl_elapsed"}
		if err := rows.Scan(&expiry.LeaseID, &expiry.ProjectID, &expiry.WorkItemID,
			&expiry.RunnerID, &expiry.Epoch, &expiry.ConnectionGeneration); err != nil {
			return nil, fmt.Errorf("runner ops: scan expiry: %w", err)
		}
		expiries = append(expiries, expiry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runner ops: expire leases rows: %w", err)
	}
	for _, expiry := range expiries {
		if err := recordOpsEvent(ctx, tx, opsEvent{
			Actor:         "control-plane:offline-monitor",
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
				"cause":                 expiry.Reason,
				"late_evidence_only":    true, // RUNNER-RULE-002: stale results never advance state
			},
		}); err != nil {
			return nil, err
		}
	}
	return expiries, nil
}

// redispatchQueued returns executing work items whose lease ended
// (expired or cancelled) back to the queue so a healthy runner can
// claim them with a fresh epoch. The status guard plus the partial
// unique index on active leases make the redispatch exactly-once: a
// replayed sweep finds status='queued' and does nothing, and no
// second active lease can exist while one is live. Frozen projects
// are skipped fail-closed — during a write freeze no new lease may be
// issued, so their items wait (runbook emergency-stop section 4.1.1).
func (o *Ops) redispatchQueued(ctx context.Context, tx *sql.Tx, correlationID string) (redispatched []Redispatch, frozenWaiting int, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT w.id, w.project_id, w.lease_epoch
		FROM work_items w
		WHERE w.status = 'executing'
		  AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.work_item_id = w.id AND l.status = 'active')
		  AND EXISTS (SELECT 1 FROM leases l WHERE l.work_item_id = w.id AND l.status IN ('expired', 'cancelled'))
		ORDER BY w.created_at, w.id
		FOR UPDATE OF w SKIP LOCKED`)
	if err != nil {
		return nil, 0, fmt.Errorf("runner ops: redispatch candidates: %w", err)
	}
	type candidate struct {
		WorkItemID, ProjectID string
		LeaseEpoch            int64
	}
	candidates := []candidate{}
	for rows.Next() {
		c := candidate{}
		if err := rows.Scan(&c.WorkItemID, &c.ProjectID, &c.LeaseEpoch); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("runner ops: scan redispatch candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("runner ops: redispatch rows: %w", err)
	}
	rows.Close()

	redispatched = []Redispatch{}
	for _, c := range candidates {
		if o.emergency.FrozenProject(c.ProjectID) {
			frozenWaiting++
			continue
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE work_items SET status = 'queued', version = version + 1, updated_at = now()
			WHERE id = $1 AND status = 'executing'`, c.WorkItemID)
		if err != nil {
			return nil, 0, fmt.Errorf("runner ops: requeue %s: %w", c.WorkItemID, err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			continue // raced with a completion; nothing to requeue
		}
		// Queue changed: waiters must re-observe the CAS token.
		if _, err := tx.ExecContext(ctx, `
			UPDATE projects SET version = version + 1, updated_at = now() WHERE id = $1`, c.ProjectID); err != nil {
			return nil, 0, fmt.Errorf("runner ops: requeue token %s: %w", c.ProjectID, err)
		}
		if err := recordOpsEvent(ctx, tx, opsEvent{
			Actor:         "control-plane:offline-monitor",
			ProjectID:     c.ProjectID,
			Action:        AuditActionRedispatchQueued,
			ResourceType:  "work_item",
			ResourceID:    c.WorkItemID,
			CorrelationID: correlationID,
			Reason: map[string]any{
				"previous_lease_epoch": c.LeaseEpoch,
				"next_lease_epoch":     c.LeaseEpoch + 1,
				"safe_rejoin":          "new epoch claimed only by an eligible runner",
			},
		}); err != nil {
			return nil, 0, err
		}
		redispatched = append(redispatched, Redispatch{
			WorkItemID: c.WorkItemID, ProjectID: c.ProjectID, NextLeaseEpoch: c.LeaseEpoch + 1,
		})
	}
	return redispatched, frozenWaiting, nil
}

// cancelRunnerLeases disposes of a runner's active leases at
// revocation time: leases become cancelled (terminal for that
// attempt), running executions cancelled, and the orphaned work items
// return to the queue unless their project is frozen. Redispatch to a
// healthy runner gets a fresh epoch; the revoked device is fenced out
// of everything (RUNNER-RULE-002, offline runbook section 6).
func cancelRunnerLeasesTx(ctx context.Context, tx *sql.Tx, runnerID, correlationID string) ([]LeaseExpiry, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH cancelled AS (
			UPDATE leases l SET status = 'cancelled', updated_at = now()
			WHERE l.runner_id = $1 AND l.status = 'active'
			RETURNING l.id, l.project_id, l.work_item_id, l.epoch, l.connection_generation
		), executions_done AS (
			UPDATE executions e SET status = 'cancelled', ended_at = now()
			FROM cancelled c WHERE e.lease_id = c.id AND e.status = 'running'
			RETURNING e.id
		)
		SELECT id, project_id, work_item_id, $1, epoch, connection_generation FROM cancelled`, runnerID)
	if err != nil {
		return nil, fmt.Errorf("runner ops: cancel leases: %w", err)
	}
	defer rows.Close()
	expiries := []LeaseExpiry{}
	for rows.Next() {
		expiry := LeaseExpiry{CorrelationID: correlationID, Reason: "runner_revoked"}
		if err := rows.Scan(&expiry.LeaseID, &expiry.ProjectID, &expiry.WorkItemID,
			&expiry.RunnerID, &expiry.Epoch, &expiry.ConnectionGeneration); err != nil {
			return nil, fmt.Errorf("runner ops: scan cancelled lease: %w", err)
		}
		expiries = append(expiries, expiry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runner ops: cancel leases rows: %w", err)
	}
	for _, expiry := range expiries {
		if err := recordOpsEvent(ctx, tx, opsEvent{
			Actor:         "control-plane:revocation",
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
				"cause":                 "runner_revoked",
				"late_evidence_only":    true,
			},
		}); err != nil {
			return nil, err
		}
	}
	return expiries, nil
}

// Touch stamps device liveness: the claim/heartbeat/complete call
// sites invoke it so the offline monitor sees a true heartbeat (the
// frozen member-side interval is HeartbeatInterval). A pending or
// approved device's first touch promotes it to online (registry
// contract: first valid long-poll enters online); suspect devices
// rejoin the same way. Draining devices stay draining — leaving drain
// is an operations decision, not a side effect of liveness (offline
// runbook section 4.2.4). Revoked devices are refused.
func (o *Ops) Touch(ctx context.Context, runnerID string) error {
	registry := o.store.RunnerRegistry()
	device, err := registry.GetRunner(ctx, runnerID)
	if err != nil {
		return fmt.Errorf("runner ops: touch: %w", err)
	}
	switch device.Status {
	case model.RunnerStatusApproved, model.RunnerStatusSuspect:
		if err := registry.UpdateRunnerStatus(ctx, runnerID, device.Status, model.RunnerStatusOnline); err != nil {
			return fmt.Errorf("runner ops: touch promote: %w", err)
		}
	case model.RunnerStatusOnline:
		// already live
	case model.RunnerStatusDraining:
		return nil //nolint:nilerr // liveness during drain is recorded, scheduling stays off
	case model.RunnerStatusRevoked:
		return store.ErrRunnerRevoked
	default:
		return fmt.Errorf("runner ops: touch: %w: status %s", store.ErrRunnerStatusInvalid, device.Status)
	}
	if err := registry.UpdateRunnerHeartbeat(ctx, runnerID); err != nil {
		return fmt.Errorf("runner ops: touch heartbeat: %w", err)
	}
	return nil
}

package runner

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/config"
)

// OfflineMonitor is the runner-offline runbook's detection and
// recovery engine (M4-RBK-001 slice C1). Each sweep:
//
//  1. determines liveness from the recorded device heartbeats —
//     online→suspect after the frozen 45s window, suspect→offline
//     after 90s (RUNBOOK-RUNNER-OFFLINE section 3; the constants are
//     the frozen config values, never request-tunable);
//  2. expires active leases whose TTL elapsed and marks their
//     executions interrupted (late evidence only, RUNNER-RULE-002);
//  3. requeues orphaned work items so the next eligible claim
//     re-dispatches them with a fresh epoch — exactly once, and
//     never while the owning project is write-frozen.
//
// Determinations, expiries and redispatches each land an audit row;
// the sweep report carries the measured identification latency for
// the 90s objective (runner-security section 11).

// OfflineMonitor cadence. The tick is internal engine timing, not a
// protocol constant: it bounds how far past the frozen thresholds a
// determination can lag (worst case threshold + tick).
const defaultSweepTick = 5 * time.Second

// SweepReport is one sweep's outcome and drill evidence.
type SweepReport struct {
	Suspected    []StatusDetermination
	Offline      []StatusDetermination
	Expired      []LeaseExpiry
	Redispatched []Redispatch
	// FrozenWaiting counts work items ready for redispatch but held
	// back by an active emergency write freeze.
	FrozenWaiting int
}

// StatusDetermination records one runner state transition with the
// evidence the runbook's event record requires (section 7).
type StatusDetermination struct {
	RunnerID        string
	ProjectID       string
	FromStatus      string
	ToStatus        string
	LastHeartbeatAt time.Time
	DeterminedAt    time.Time
	// IdentificationLatency is DeterminedAt - LastHeartbeatAt: the
	// measured detection time against the 90s objective.
	IdentificationLatency time.Duration
	// Generation is the registry connection-generation counter (the
	// fencing epoch of the device channel).
	Generation       int64
	ActiveLeaseID    string
	ActiveLeaseEpoch int64
	CorrelationID    string
}

// LeaseExpiry records one fenced lease.
type LeaseExpiry struct {
	LeaseID              string
	ProjectID            string
	WorkItemID           string
	RunnerID             string
	Epoch                int64
	ConnectionGeneration string
	Reason               string
	CorrelationID        string
}

// Redispatch records one work item returned to the queue.
type Redispatch struct {
	WorkItemID     string
	ProjectID      string
	NextLeaseEpoch int64
}

// OfflineMonitor sweeps on a ticker until the context ends.
type OfflineMonitor struct {
	ops  *Ops
	tick time.Duration
}

// NewOfflineMonitor validates the wiring; tick <= 0 selects the
// default cadence.
func NewOfflineMonitor(ops *Ops, tick time.Duration) (*OfflineMonitor, error) {
	if ops == nil {
		return nil, fmt.Errorf("runner monitor: nil ops")
	}
	if tick <= 0 {
		tick = defaultSweepTick
	}
	return &OfflineMonitor{ops: ops, tick: tick}, nil
}

// Run sweeps every tick until the context is cancelled; a sweep error
// is logged and retried on the next tick (fail-closed determination
// never advances state on error).
func (m *OfflineMonitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil //nolint:nilerr // cancellation is the intended stop
		case <-ticker.C:
			if _, err := m.SweepOnce(ctx, fmt.Sprintf("sweep-%d", m.ops.Now().UnixNano())); err != nil {
				slog.Error("runner offline monitor sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce performs one detection-and-recovery pass. Every step is
// guarded (status CAS, partial unique index, FOR UPDATE SKIP LOCKED)
// so concurrent sweeps and API traffic converge instead of
// double-firing.
func (m *OfflineMonitor) SweepOnce(ctx context.Context, correlationID string) (*SweepReport, error) {
	report := &SweepReport{}
	tx, err := m.ops.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("runner monitor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	suspected, err := m.determineStale(ctx, tx, "online", "suspect",
		config.RunnerSuspectSec, correlationID)
	if err != nil {
		return nil, err
	}
	report.Suspected = suspected

	offlined, err := m.determineStale(ctx, tx, "suspect", "offline",
		config.RunnerOfflineSec, correlationID)
	if err != nil {
		return nil, err
	}
	report.Offline = offlined

	expired, err := m.ops.expireLeases(ctx, tx, nil, correlationID)
	if err != nil {
		return nil, err
	}
	report.Expired = expired

	redispatched, frozenWaiting, err := m.ops.redispatchQueued(ctx, tx, correlationID)
	if err != nil {
		return nil, err
	}
	report.Redispatched = redispatched
	report.FrozenWaiting = frozenWaiting

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("runner monitor: commit: %w", err)
	}
	return report, nil
}

// determineStale runs one guarded liveness transition over the
// registry: rows in fromStatus whose liveness anchor (heartbeat, or
// updated_at for devices promoted without a heartbeat yet) is older
// than the frozen window move to toStatus. Each transition lands the
// runbook's audit row with the active lease snapshot.
func (m *OfflineMonitor) determineStale(ctx context.Context, tx *sql.Tx, fromStatus, toStatus string, windowSec int, correlationID string) ([]StatusDetermination, error) {
	determinedAt := m.ops.Now()
	rows, err := tx.QueryContext(ctx, `
		WITH stale AS (
			SELECT r.id, r.generation, r.last_heartbeat_at, COALESCE(r.last_heartbeat_at, r.updated_at) AS liveness
			FROM runners r
			WHERE r.status = $1
			  AND COALESCE(r.last_heartbeat_at, r.updated_at) < now() - make_interval(secs => $2)
			FOR UPDATE OF r SKIP LOCKED
		), transitioned AS (
			UPDATE runners r SET status = $3, updated_at = now()
			FROM stale s WHERE r.id = s.id
			RETURNING r.id, r.generation, s.last_heartbeat_at, s.liveness
		)
		SELECT t.id, t.generation, t.liveness,
		       COALESCE(l.id::text, ''), COALESCE(l.epoch, 0)
		FROM transitioned t
		LEFT JOIN LATERAL (
			SELECT l.id, l.epoch FROM leases l
			WHERE l.runner_id = t.id AND l.status = 'active'
			ORDER BY l.expires_at LIMIT 1
		) l ON true`,
		fromStatus, windowSec, toStatus)
	if err != nil {
		return nil, fmt.Errorf("runner monitor: %s: %w", toStatus, err)
	}
	defer rows.Close()
	determinations := []StatusDetermination{}
	for rows.Next() {
		determination := StatusDetermination{
			FromStatus:    fromStatus,
			ToStatus:      toStatus,
			DeterminedAt:  determinedAt,
			CorrelationID: correlationID,
		}
		if err := rows.Scan(&determination.RunnerID, &determination.Generation,
			&determination.LastHeartbeatAt,
			&determination.ActiveLeaseID, &determination.ActiveLeaseEpoch); err != nil {
			return nil, fmt.Errorf("runner monitor: scan %s: %w", toStatus, err)
		}
		// The liveness anchor is the last recorded heartbeat, falling
		// back to the promotion time for devices that never heartbeated.
		determination.IdentificationLatency = determinedAt.Sub(determination.LastHeartbeatAt)
		determinations = append(determinations, determination)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runner monitor: %s rows: %w", toStatus, err)
	}

	for index := range determinations {
		projectID, err := m.ops.projectOfRunner(ctx, determinations[index].RunnerID)
		if err != nil {
			return nil, err
		}
		determinations[index].ProjectID = projectID
		if err := recordOpsEvent(ctx, tx, opsEvent{
			Actor:         "control-plane:offline-monitor",
			ProjectID:     projectID,
			Action:        auditActionForStatus(toStatus),
			ResourceType:  "runner",
			ResourceID:    determinations[index].RunnerID,
			CorrelationID: correlationID,
			Reason: map[string]any{
				"incident_id":               correlationID,
				"runner_id":                 determinations[index].RunnerID,
				"connection_generation":     determinations[index].Generation,
				"last_heartbeat_at":         determinations[index].LastHeartbeatAt.UTC().Format(time.RFC3339Nano),
				"lease_id":                  determinations[index].ActiveLeaseID,
				"lease_epoch":               determinations[index].ActiveLeaseEpoch,
				"window_seconds":            windowSec,
				"identification_latency_ms": determinations[index].IdentificationLatency.Milliseconds(),
				"new_leases_stopped":        true, // offline runbook section 6: offline runners get no lease
			},
		}); err != nil {
			return nil, err
		}
	}
	return determinations, nil
}

func auditActionForStatus(status string) string {
	switch status {
	case "suspect":
		return AuditActionSuspectDetermined
	case "offline":
		return AuditActionOfflineDetermined
	default:
		return "runner.status_determined"
	}
}

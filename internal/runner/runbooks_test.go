package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// PG-gated tests for the two S3-owned runbook surfaces (M4-RBK-001):
// offline determination with exactly-once redispatch, terminal
// revocation with lease disposal, and the emergency write freeze with
// in-flight fencing. All against a real, migrated PostgreSQL.

const (
	opsTeam    = "018f7f00-0000-7000-8000-00000000aa01"
	opsProject = "018f7f00-0000-7000-8000-00000000aa02"
	otherTeam  = "018f7f00-0000-7000-8000-00000000bb01"
	otherProj  = "018f7f00-0000-7000-8000-00000000bb02"
)

type opsFixture struct {
	t       *testing.T
	db      *sql.DB
	store   *store.PostgresStore
	ops     *Ops
	ctrl    *EmergencyController
	monitor *OfflineMonitor
}

func newOpsFixture(t *testing.T) *opsFixture {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the compose postgres to include this test")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	require.NoError(t, err)
	const dbName = "maestro_runner_ops_test"
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, dbName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, dbName))
		_ = admin.Close()
	})

	db, err := store.OpenPostgres(ctx, dsn[:strings.LastIndex(dsn, "/")+1]+dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.ApplyPostgresMigrations(ctx, db)
	require.NoError(t, err)

	st, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	ctrl, err := NewEmergencyController(st)
	require.NoError(t, err)
	ops, err := NewOps(st, ctrl)
	require.NoError(t, err)
	monitor, err := NewOfflineMonitor(ops, time.Second)
	require.NoError(t, err)

	fixture := &opsFixture{t: t, db: db, store: st, ops: ops, ctrl: ctrl, monitor: monitor}
	fixture.seed(ctx)
	return fixture
}

func (f *opsFixture) seed(ctx context.Context) {
	_, err := f.db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'runner-ops')`, opsTeam)
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'runner-ops', 'Runner Ops', 'active')`,
		opsProject, opsTeam)
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'runner-ops-other')`, otherTeam)
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'runner-ops-b', 'Runner Ops B', 'active')`,
		otherProj, otherTeam)
	require.NoError(f.t, err)
}

// addRunner creates an approved, project-bound device and promotes it
// online with one heartbeat.
func (f *opsFixture) addRunner(ctx context.Context, name, projectID string) *model.RunnerDevice {
	f.t.Helper()
	device := &model.RunnerDevice{
		DisplayName:   name,
		DeviceKeyHash: "sha256:" + name,
		Status:        model.RunnerStatusApproved,
		Capabilities:  []byte(`["rootless_oci","no_new_privileges","resource_limits"]`),
	}
	require.NoError(f.t, f.store.RunnerRegistry().CreateRunner(ctx, device, &model.RunnerBinding{ProjectID: projectID}))
	require.NoError(f.t, f.ops.Touch(ctx, device.ID))
	return device
}

// addQueuedWorkItem seeds one queued work item.
func (f *opsFixture) addQueuedWorkItem(ctx context.Context, projectID, title string) string {
	f.t.Helper()
	id := "018f7f10-0000-7000-8000-" + fmt.Sprintf("%012d", time.Now().UnixNano()%1e12)
	_, err := f.db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, $3, 'queued')`,
		id, projectID, title)
	require.NoError(f.t, err)
	return id
}

// queueVersion reads the current CAS token the next claim must present.
func (f *opsFixture) queueVersion(ctx context.Context, projectID string) int64 {
	var version int64
	require.NoError(f.t, f.db.QueryRowContext(ctx,
		`SELECT version FROM projects WHERE id = $1`, projectID).Scan(&version))
	return version
}

// claim drives the real dispatch core for a runner.
func (f *opsFixture) claim(ctx context.Context, runnerID, generation string, ttl time.Duration) *store.WorkItemClaim {
	f.t.Helper()
	projectID, err := f.store.RunnerRegistry().ProjectOfRunner(ctx, runnerID)
	require.NoError(f.t, err)
	claim, err := f.store.ClaimNextWorkItem(ctx, runnerID, generation, f.queueVersion(ctx, projectID), ttl)
	require.NoError(f.t, err)
	return claim
}

func (f *opsFixture) runnerStatus(ctx context.Context, runnerID string) string {
	var status string
	require.NoError(f.t, f.db.QueryRowContext(ctx,
		`SELECT status FROM runners WHERE id = $1`, runnerID).Scan(&status))
	return status
}

func (f *opsFixture) workItemStatus(ctx context.Context, workItemID string) string {
	var status string
	require.NoError(f.t, f.db.QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE id = $1`, workItemID).Scan(&status))
	return status
}

func (f *opsFixture) auditRows(ctx context.Context, action string) []map[string]any {
	rows, err := f.db.QueryContext(ctx,
		`SELECT actor_principal, action, resource_type, resource_id, correlation_id, reason FROM audit_events WHERE action = $1 ORDER BY id`, action)
	require.NoError(f.t, err)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var actor, act, rtype, rid, corr, reason string
		require.NoError(f.t, rows.Scan(&actor, &act, &rtype, &rid, &corr, &reason))
		var parsed any
		require.NoError(f.t, json.Unmarshal([]byte(reason), &parsed))
		out = append(out, map[string]any{
			"actor": actor, "action": act, "resource_type": rtype,
			"resource_id": rid, "correlation_id": corr, "reason": parsed,
		})
	}
	require.NoError(f.t, rows.Err())
	return out
}

// TestRunbookOfflineDeterminationAndRedispatch is the C1 core: a
// silent device passes suspect → offline on the frozen windows, its
// expired lease is fenced, the orphaned work item requeues exactly
// once, and the next eligible claim re-dispatches with a fresh epoch.
func TestRunbookOfflineDeterminationAndRedispatch(t *testing.T) {
	fixture := newOpsFixture(t)
	ctx := context.Background()

	silent := fixture.addRunner(ctx, "silent", opsProject)
	healthy := fixture.addRunner(ctx, "healthy", opsProject)
	workItem := fixture.addQueuedWorkItem(ctx, opsProject, "c1-drill")

	claim := fixture.claim(ctx, silent.ID, "gen-silent-1", 90*time.Second)
	require.Equal(t, workItem, claim.WorkItemID)
	require.EqualValues(t, 1, claim.LeaseEpoch)

	// The device goes silent; both the heartbeat and the lease TTL
	// age past their windows.
	_, err := fixture.db.ExecContext(ctx, `
		UPDATE runners SET last_heartbeat_at = now() - interval '100 seconds' WHERE id = $1`, silent.ID)
	require.NoError(t, err)
	_, err = fixture.db.ExecContext(ctx, `
		UPDATE leases SET expires_at = now() - interval '1 second' WHERE id = $1`, claim.LeaseID)
	require.NoError(t, err)

	report, err := fixture.monitor.SweepOnce(ctx, "c1-compressed")
	require.NoError(t, err)

	// 45s window: online → suspect; 90s window: suspect → offline —
	// both fire in one sweep for a 100s-silent device.
	require.Len(t, report.Suspected, 1)
	require.Equal(t, silent.ID, report.Suspected[0].RunnerID)
	require.Len(t, report.Offline, 1)
	assert.Equal(t, silent.ID, report.Offline[0].RunnerID)
	assert.Equal(t, "suspect", report.Offline[0].FromStatus)
	assert.GreaterOrEqual(t, report.Offline[0].IdentificationLatency, 90*time.Second)
	assert.Equal(t, "offline", fixture.runnerStatus(ctx, silent.ID))

	// The stale lease expired with its execution interrupted and the
	// work item back in the queue — exactly once.
	require.Len(t, report.Expired, 1)
	assert.Equal(t, claim.LeaseID, report.Expired[0].LeaseID)
	assert.Equal(t, "queued", fixture.workItemStatus(ctx, workItem))
	require.Len(t, report.Redispatched, 1)
	assert.EqualValues(t, 2, report.Redispatched[0].NextLeaseEpoch)

	// A replayed sweep changes nothing.
	second, err := fixture.monitor.SweepOnce(ctx, "c1-compressed-2")
	require.NoError(t, err)
	assert.Empty(t, second.Suspected)
	assert.Empty(t, second.Offline)
	assert.Empty(t, second.Expired)
	assert.Empty(t, second.Redispatched)

	// The offline device is barred from new leases (stable sentinel
	// behind the handler's 403).
	_, err = fixture.store.ClaimNextWorkItem(ctx, silent.ID, "gen-silent-2",
		fixture.queueVersion(ctx, opsProject), 90*time.Second)
	assert.ErrorIs(t, err, store.ErrRunnerStatusInvalid)

	// The healthy runner re-claims with a fresh epoch; nobody else
	// can claim the same item afterwards.
	reclaim := fixture.claim(ctx, healthy.ID, "gen-healthy-1", 90*time.Second)
	require.Equal(t, workItem, reclaim.WorkItemID)
	require.EqualValues(t, 2, reclaim.LeaseEpoch)
	var activeLeases int
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT count(*) FROM leases WHERE work_item_id = $1 AND status = 'active'`, workItem).Scan(&activeLeases))
	assert.Equal(t, 1, activeLeases, "exactly one active lease after redispatch")

	// The silent generation's late result is fenced out (late
	// evidence only, RUNNER-RULE-002).
	err = fixture.store.CompleteExecution(ctx, claim.ExecutionID, silent.ID, "gen-silent-1", "completed", nil, "")
	assert.Error(t, err, "a late result from the offline generation must not advance the work item")

	// Audit coverage: determination, expiry and redispatch all landed.
	require.Len(t, fixture.auditRows(ctx, AuditActionSuspectDetermined), 1)
	offlineRows := fixture.auditRows(ctx, AuditActionOfflineDetermined)
	require.Len(t, offlineRows, 1)
	assert.Equal(t, "c1-compressed", offlineRows[0]["correlation_id"])
	require.Len(t, fixture.auditRows(ctx, AuditActionLeaseExpired), 1)
	require.Len(t, fixture.auditRows(ctx, AuditActionRedispatchQueued), 1)
}

// TestRunbookRevocationDisposesLeasesAndRefusesEverything is the C2
// core: revocation is terminal and audited, in-flight leases are
// cancelled with their work items requeued, and every later device
// action hits a stable sentinel (the handler's 410/403/409 codes).
func TestRunbookRevocationDisposesLeasesAndRefusesEverything(t *testing.T) {
	fixture := newOpsFixture(t)
	ctx := context.Background()

	compromised := fixture.addRunner(ctx, "compromised", opsProject)
	healthy := fixture.addRunner(ctx, "healthy-after-revoke", opsProject)
	workItem := fixture.addQueuedWorkItem(ctx, opsProject, "c2-drill")
	claim := fixture.claim(ctx, compromised.ID, "gen-bad-1", 90*time.Second)

	report, err := fixture.ops.RevokeRunner(ctx, compromised.ID, "security_owner:drill", "INC-C2")
	require.NoError(t, err)
	require.False(t, report.AlreadyRevoked)
	assert.Equal(t, "revoked", fixture.runnerStatus(ctx, compromised.ID))
	require.Len(t, report.CancelledLeases, 1)
	assert.Equal(t, claim.LeaseID, report.CancelledLeases[0].LeaseID)
	assert.Equal(t, "queued", fixture.workItemStatus(ctx, workItem))

	// Every later device action is refused with the stable sentinels
	// the handler maps to protocol codes.
	_, err = fixture.store.ClaimNextWorkItem(ctx, compromised.ID, "gen-bad-2",
		fixture.queueVersion(ctx, opsProject), 90*time.Second)
	assert.ErrorIs(t, err, store.ErrRunnerStatusInvalid)
	_, err = fixture.store.RunnerLeaseHeartbeat(ctx, claim.LeaseID, compromised.ID, "gen-bad-1", claim.LeaseVersion, 90*time.Second)
	assert.Error(t, err)
	err = fixture.store.CompleteExecution(ctx, claim.ExecutionID, compromised.ID, "gen-bad-1", "completed", nil, "")
	assert.Error(t, err)
	_, err = fixture.store.RunnerRegistry().BumpRunnerGeneration(ctx, compromised.ID)
	assert.ErrorIs(t, err, store.ErrRunnerRevoked)
	err = fixture.ops.Touch(ctx, compromised.ID)
	assert.ErrorIs(t, err, store.ErrRunnerRevoked)

	// The revoked identity cannot be re-inserted as a device: unknown
	// or terminal statuses coerce to pending_approval (a fresh one-time
	// code plus approval is the only revival path), and the terminal
	// row itself stays revoked whatever the operator sends.
	device := &model.RunnerDevice{DisplayName: "revived", DeviceKeyHash: "sha256:x",
		Status: model.RunnerStatusRevoked, Capabilities: []byte(`["a","b","c"]`)}
	require.NoError(t, fixture.store.RunnerRegistry().CreateRunner(ctx, device, &model.RunnerBinding{ProjectID: opsProject}))
	assert.Equal(t, model.RunnerStatusPendingApproval, fixture.runnerStatus(ctx, device.ID),
		"a device can never enter the registry as revoked")

	// A healthy runner takes over the requeued item with a fresh epoch.
	reclaim := fixture.claim(ctx, healthy.ID, "gen-good-1", 90*time.Second)
	require.Equal(t, workItem, reclaim.WorkItemID)
	require.EqualValues(t, 2, reclaim.LeaseEpoch)

	// The audit row landed with the incident correlation and only the
	// key-hash reference (Keychain red line).
	rows := fixture.auditRows(ctx, AuditActionRunnerRevoked)
	require.Len(t, rows, 1)
	assert.Equal(t, "INC-C2", rows[0]["correlation_id"])
	reason := rows[0]["reason"].(map[string]any)
	assert.Equal(t, "sha256:compromised", reason["device_key_hash"])

	// Idempotent replay: same business result, no second audit row.
	replay, err := fixture.ops.RevokeRunner(ctx, compromised.ID, "security_owner:drill", "INC-C2")
	require.NoError(t, err)
	assert.True(t, replay.AlreadyRevoked)
	assert.Len(t, fixture.auditRows(ctx, AuditActionRunnerRevoked), 1)

	// Revocation requires an incident correlation ID.
	_, err = fixture.ops.RevokeRunner(ctx, healthy.ID, "security_owner:drill", "")
	assert.ErrorIs(t, err, ErrRevocationRequiresIncident)
}

// TestRunbookEmergencyStopFreezeAndRecovery is the C3 core: the stop
// freezes dispatch fail-closed, fences every active lease in scope
// (late results rejected), links credential revocation, replays from
// the durable audit record on restart, and staged recovery requeues
// the waiting items with fresh epochs.
func TestRunbookEmergencyStopFreezeAndRecovery(t *testing.T) {
	fixture := newOpsFixture(t)
	ctx := context.Background()

	compromised := fixture.addRunner(ctx, "compromised-p0", opsProject)
	bystander := fixture.addRunner(ctx, "bystander", opsProject)
	otherProjectRunner := fixture.addRunner(ctx, "other-project", otherProj)
	workItemMain := fixture.addQueuedWorkItem(ctx, opsProject, "c3-drill-main")
	workItemOther := fixture.addQueuedWorkItem(ctx, otherProj, "c3-drill-other")

	badClaim := fixture.claim(ctx, compromised.ID, "gen-p0-1", 90*time.Second)
	otherClaim := fixture.claim(ctx, otherProjectRunner.ID, "gen-other-1", 90*time.Second)
	require.Equal(t, workItemMain, badClaim.WorkItemID)
	require.Equal(t, workItemOther, otherClaim.WorkItemID)

	// The two-person rule and the incident discipline.
	_, err := fixture.ctrl.TriggerEmergencyStop(ctx, EmergencyScopeGlobal, "INC-RULES", "security_owner:drill",
		[]string{"approver-1"}, nil)
	assert.ErrorIs(t, err, ErrEmergencyApproval)
	_, err = fixture.ctrl.TriggerEmergencyStop(ctx, EmergencyScopeGlobal, "", "security_owner:drill",
		[]string{"approver-1", "approver-2"}, nil)
	assert.ErrorIs(t, err, ErrEmergencyRequiresIncident)

	report, err := fixture.ctrl.TriggerEmergencyStop(ctx, EmergencyScopeGlobal, "INC-P0",
		"security_owner:drill", []string{"approver-1", "approver-2"}, []string{compromised.ID})
	require.NoError(t, err)
	require.False(t, report.AlreadyStopped)

	// In-flight fencing across the whole plane: the linked revocation
	// cancelled the compromised device's lease (cause runner_revoked)
	// and the freeze expired every other active lease regardless of
	// TTL; late results are rejected on both.
	require.Len(t, report.ExpiredLeases, 1, "only the non-revoked runner's lease rides the freeze expiry")
	assert.Equal(t, "revoked", fixture.runnerStatus(ctx, compromised.ID))
	assert.Len(t, report.RevokedRunners, 1)
	var badLeaseStatus, otherLeaseStatus string
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT status FROM leases WHERE id = $1`, badClaim.LeaseID).Scan(&badLeaseStatus))
	require.NoError(t, fixture.db.QueryRowContext(ctx,
		`SELECT status FROM leases WHERE id = $1`, otherClaim.LeaseID).Scan(&otherLeaseStatus))
	assert.Equal(t, "cancelled", badLeaseStatus)
	assert.Equal(t, "expired", otherLeaseStatus)
	err = fixture.store.CompleteExecution(ctx, badClaim.ExecutionID, compromised.ID, "gen-p0-1", "completed", nil, "")
	assert.Error(t, err, "the revoked device's late result must not advance state")
	err = fixture.store.CompleteExecution(ctx, otherClaim.ExecutionID, otherProjectRunner.ID, "gen-other-1", "completed", nil, "")
	assert.Error(t, err, "an in-flight result after the stop must not advance state")

	// New writes are frozen: the claim guard refuses, the sweep holds
	// the waiting items instead of requeueing.
	assert.ErrorIs(t, fixture.ctrl.GuardClaim(opsProject), ErrWriteFrozen)
	assert.ErrorIs(t, fixture.ctrl.GuardClaim(otherProj), ErrWriteFrozen)
	sweep, err := fixture.monitor.SweepOnce(ctx, "c3-frozen-sweep")
	require.NoError(t, err)
	assert.Empty(t, sweep.Redispatched, "no redispatch while frozen")
	assert.Equal(t, 2, sweep.FrozenWaiting)
	assert.Equal(t, "executing", fixture.workItemStatus(ctx, workItemMain))
	assert.Equal(t, "executing", fixture.workItemStatus(ctx, workItemOther))

	// The durable stop survives a restart: a fresh controller replays
	// the freeze from the append-only audit chain.
	reloaded, err := NewEmergencyController(fixture.store)
	require.NoError(t, err)
	require.NoError(t, reloaded.LoadFreezeState(ctx))
	assert.True(t, reloaded.FrozenProject(opsProject), "the freeze must survive a restart")
	assert.ErrorIs(t, reloaded.GuardClaim(otherProj), ErrWriteFrozen)

	// Idempotent replay of the same incident: no second fencing, no
	// second stop row.
	replay, err := fixture.ctrl.TriggerEmergencyStop(ctx, EmergencyScopeGlobal, "INC-P0",
		"security_owner:drill", []string{"approver-1", "approver-2"}, nil)
	require.NoError(t, err)
	assert.True(t, replay.AlreadyStopped)
	require.Len(t, fixture.auditRows(ctx, AuditActionEmergencyStop), 1)

	// Recovery keeps the discipline: wrong incident refused, the
	// two-person rule applies.
	assert.Error(t, fixture.ctrl.LiftEmergencyStop(ctx, EmergencyScopeGlobal, "INC-WRONG",
		"operations_owner:drill", []string{"approver-1", "approver-2"}))
	assert.ErrorIs(t, fixture.ctrl.LiftEmergencyStop(ctx, EmergencyScopeGlobal, "INC-P0",
		"operations_owner:drill", []string{"solo"}), ErrEmergencyApproval)
	require.NoError(t, fixture.ctrl.LiftEmergencyStop(ctx, EmergencyScopeGlobal, "INC-P0",
		"operations_owner:drill", []string{"approver-1", "approver-3"}))
	assert.False(t, fixture.ctrl.FrozenProject(opsProject))

	// Staged recovery: the next sweep requeues the waiting items and a
	// healthy runner claims with a fresh epoch.
	recovery, err := fixture.monitor.SweepOnce(ctx, "c3-recovery-sweep")
	require.NoError(t, err)
	require.Len(t, recovery.Redispatched, 2)
	reclaim := fixture.claim(ctx, bystander.ID, "gen-bystander-1", 90*time.Second)
	require.Equal(t, workItemMain, reclaim.WorkItemID)
	require.EqualValues(t, 2, reclaim.LeaseEpoch)

	// Audit chain: stop and recover rows share the incident
	// correlation; the stop reason carries scope and approvers.
	stopRows := fixture.auditRows(ctx, AuditActionEmergencyStop)
	require.Len(t, stopRows, 1)
	assert.Equal(t, "INC-P0", stopRows[0]["correlation_id"])
	stopReason := stopRows[0]["reason"].(map[string]any)
	assert.Equal(t, EmergencyScopeGlobal, stopReason["freeze_scope"])
	assert.Equal(t, []any{"approver-1", "approver-2"}, stopReason["approvers"])
	recoverRows := fixture.auditRows(ctx, AuditActionEmergencyRecovered)
	require.Len(t, recoverRows, 1)
	assert.Equal(t, "INC-P0", recoverRows[0]["correlation_id"])
}

// TestRunbookEmergencyScopeHierarchy proves the freeze lattice: a
// project freeze does not shadow the plane, and a narrower lift
// cannot end a wider freeze.
func TestRunbookEmergencyScopeHierarchy(t *testing.T) {
	fixture := newOpsFixture(t)
	ctx := context.Background()

	_, err := fixture.ctrl.TriggerEmergencyStop(ctx, opsProject, "INC-PROJ",
		"security_owner:drill", []string{"approver-1", "approver-2"}, nil)
	require.NoError(t, err)
	assert.True(t, fixture.ctrl.FrozenProject(opsProject))
	assert.False(t, fixture.ctrl.FrozenProject(otherProj), "a project freeze leaves other projects dispatching")

	// The global stop strengthens; the earlier project freeze rides
	// along until its own incident is lifted.
	_, err = fixture.ctrl.TriggerEmergencyStop(ctx, EmergencyScopeGlobal, "INC-GLOBAL",
		"security_owner:drill", []string{"approver-1", "approver-2"}, nil)
	require.NoError(t, err)
	_, err = fixture.ctrl.TriggerEmergencyStop(ctx, otherProj, "INC-OTHER",
		"security_owner:drill", []string{"approver-1", "approver-2"}, nil)
	assert.ErrorIs(t, err, ErrFreezeHierarchy, "a project trigger under a global freeze adds nothing")
	assert.ErrorIs(t, fixture.ctrl.LiftEmergencyStop(ctx, opsProject, "INC-PROJ",
		"operations_owner:drill", []string{"approver-1", "approver-2"}), ErrFreezeHierarchy,
		"a project lift cannot end the global freeze")
	require.NoError(t, fixture.ctrl.LiftEmergencyStop(ctx, EmergencyScopeGlobal, "INC-GLOBAL",
		"operations_owner:drill", []string{"approver-1", "approver-2"}))
	assert.True(t, fixture.ctrl.FrozenProject(opsProject), "the project freeze outlives the global lift")
	require.NoError(t, fixture.ctrl.LiftEmergencyStop(ctx, opsProject, "INC-PROJ",
		"operations_owner:drill", []string{"approver-1", "approver-2"}))
	assert.False(t, fixture.ctrl.FrozenProject(opsProject))
}

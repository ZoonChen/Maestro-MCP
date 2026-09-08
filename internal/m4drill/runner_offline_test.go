package m4drill

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/audit"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/runner"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The runner-offline drill anchor (M4-RBK-001, TC-RBKRUN-001): the
// runbook's §3 liveness windows and §4 recovery executed against real
// PostgreSQL and the real Runner protocol endpoints — a device goes
// silent, the monitor determines suspect/offline on the frozen 45/90s
// windows (identification latency measured against the 90s objective,
// runner-security §11), the expired lease is fenced, and the orphaned
// work item re-dispatches exactly once with a fresh epoch to a
// healthy runner. The §9 audit chain covers determination, expiry and
// redispatch and verifies end to end.

const (
	offlineTeam    = "018f7f20-0000-7000-8000-00000000cc01"
	offlineProject = "018f7f20-0000-7000-8000-00000000cc02"
)

type offlineDrill struct {
	t       *testing.T
	db      *sql.DB
	store   *store.PostgresStore
	ops     *runner.Ops
	monitor *runner.OfflineMonitor
	ctrl    *runner.EmergencyController
	router  *gin.Engine
	tokens  *identity.DeviceTokenMinter
}

func newOfflineDrill(t *testing.T) *offlineDrill {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	require.NoError(t, err)
	const dbName = "maestro_m4drill_offline"
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
	ctrl, err := runner.NewEmergencyController(st)
	require.NoError(t, err)
	require.NoError(t, ctrl.LoadFreezeState(ctx))
	ops, err := runner.NewOps(st, ctrl)
	require.NoError(t, err)
	monitor, err := runner.NewOfflineMonitor(ops, time.Second)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'm4drill-offline')`, offlineTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'm4drill-off', 'M4DRill Offline', 'active')`,
		offlineProject, offlineTeam)
	require.NoError(t, err)

	tokens, err := identity.NewDeviceTokenMinter("drill-secret-0123456789abcdef0123456789", time.Now)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler.RegisterRunnerV3(router, handler.RunnerV3Options{Registry: st, Tokens: tokens})

	// The production detection engine on a compressed 1s tick (the
	// frozen 45/90s thresholds are untouched).
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	t.Cleanup(stopMonitor)
	go func() { _ = monitor.Run(monitorCtx) }()

	return &offlineDrill{t: t, db: db, store: st, ops: ops, monitor: monitor, ctrl: ctrl, router: router, tokens: tokens}
}

// enrollRunner drives the real enrollment endpoint with a fresh
// one-time code, approves the device through the registry and
// promotes it online — the drill's device onboarding.
func (d *offlineDrill) enrollRunner(ctx context.Context, name string) (runnerID, token string) {
	d.t.Helper()
	code, codeHash, err := identity.NewEnrollmentCode()
	require.NoError(d.t, err)
	require.NoError(d.t, d.store.RunnerRegistry().CreateEnrollment(ctx, &model.RunnerEnrollment{
		ProjectID: offlineProject, CodeHash: codeHash,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		CreatedBy: offlineApprover(ctx, d),
	}))
	body := fmt.Sprintf(`{"enrollment_code":%q,"device_public_key":%q,"display_name":%q,"capabilities":["rootless_oci","no_new_privileges","resource_limits"]}`,
		code, "device-key-"+name, name)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v3/runners/enroll", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	d.router.ServeHTTP(response, request)
	require.Equal(d.t, http.StatusCreated, response.Code, "body: %s", response.Body.String())
	var credential struct {
		RunnerID    string `json:"runner_id"`
		AccessToken string `json:"access_token"`
	}
	require.NoError(d.t, json.Unmarshal(response.Body.Bytes(), &credential))
	require.NoError(d.t, d.store.RunnerRegistry().UpdateRunnerStatus(ctx,
		credential.RunnerID, model.RunnerStatusPendingApproval, model.RunnerStatusApproved))
	require.NoError(d.t, d.ops.Touch(ctx, credential.RunnerID))
	return credential.RunnerID, credential.AccessToken
}

// claimHTTP drives the real claim endpoint; the Idempotency-Key
// carries the queue CAS token the daemon shape requires.
func (d *offlineDrill) claimHTTP(token, generation string, queueVersion int64) (int, map[string]any) {
	d.t.Helper()
	body := fmt.Sprintf(`{"protocol_version":"3.0","connection_generation":%q,"capabilities":["rootless_oci","no_new_privileges","resource_limits"],"wait_seconds":5}`, generation)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v3/runner-leases/claim", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", fmt.Sprintf("daemon-%s-claim-000001-q%d", generation, queueVersion))
	d.router.ServeHTTP(response, request)
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		decoded = map[string]any{"raw": response.Body.String()}
	}
	return response.Code, decoded
}

func (d *offlineDrill) completeHTTP(token, executionID, generation string, body map[string]any) int {
	d.t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(d.t, err)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v3/executions/"+executionID+"/complete", strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	d.router.ServeHTTP(response, request)
	return response.Code
}

func (d *offlineDrill) queueVersion() int64 {
	var version int64
	require.NoError(d.t, d.db.QueryRowContext(context.Background(),
		`SELECT version FROM projects WHERE id = $1`, offlineProject).Scan(&version))
	return version
}

func (d *offlineDrill) runnerStatus(runnerID string) string {
	var status string
	require.NoError(d.t, d.db.QueryRowContext(context.Background(),
		`SELECT status FROM runners WHERE id = $1`, runnerID).Scan(&status))
	return status
}

func (d *offlineDrill) workItemStatus(workItemID string) string {
	var status string
	require.NoError(d.t, d.db.QueryRowContext(context.Background(),
		`SELECT status FROM work_items WHERE id = $1`, workItemID).Scan(&status))
	return status
}

func (d *offlineDrill) auditActions() []string {
	rows, err := d.db.QueryContext(context.Background(),
		`SELECT action FROM audit_events ORDER BY id`)
	require.NoError(d.t, err)
	defer rows.Close()
	actions := []string{}
	for rows.Next() {
		var action string
		require.NoError(d.t, rows.Scan(&action))
		actions = append(actions, action)
	}
	require.NoError(d.t, rows.Err())
	return actions
}

// TestRunbookRunnerOffline is the TC-RBKRUN-001 anchor.
func TestRunbookRunnerOffline(t *testing.T) {
	drill := newOfflineDrill(t)
	ctx := context.Background()

	doomedID, doomedToken := drill.enrollRunner(ctx, "doomed")
	rescuerID, rescuerToken := drill.enrollRunner(ctx, "rescuer")

	workItem := "018f7f21-0000-7000-8000-00000000cc10"
	_, err := drill.db.ExecContext(ctx,
		`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'offline-drill', 'queued')`,
		workItem, offlineProject)
	require.NoError(t, err)

	t.Run("section 4.2 dispatch and silence", func(t *testing.T) {
		code, lease := drill.claimHTTP(doomedToken, "gen-doomed-1", drill.queueVersion())
		require.Equal(t, http.StatusOK, code, "claim: %v", lease)
		require.NotNil(t, lease["id"], "the claim must return a lease, not a no-work reply: %v", lease)
		assert.EqualValues(t, 1, lease["epoch"])
		assert.Equal(t, workItem, lease["work_item_id"])

		// The device's last heartbeat is the silence anchor.
		var lastHeartbeat time.Time
		require.NoError(t, drill.db.QueryRowContext(ctx,
			`SELECT last_heartbeat_at FROM runners WHERE id = $1`, doomedID).Scan(&lastHeartbeat))
		startedWaiting := time.Now()
		doomedLeaseID, _ := lease["id"].(string)
		doomedExecutionID, _ := lease["execution_id"].(string)

		// The rescuer keeps heartbeating through the wait — a healthy
		// device's 15s cadence compressed to 10s here; it also proves
		// the suspect → online rejoin path on every touch.
		keeperCtx, stopKeeper := context.WithCancel(ctx)
		defer stopKeeper()
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-keeperCtx.Done():
					return
				case <-ticker.C:
					_ = drill.ops.Touch(keeperCtx, rescuerID)
				}
			}
		}()

		// §3: 45s window → suspect. The monitor runs the production
		// cadence compressed to a 1s tick (internal engine timing, the
		// 45/90s thresholds stay frozen).
		waitForStatus(t, drill, doomedID, "suspect", 60*time.Second)
		suspectSeenAt := time.Now()
		t.Logf("drill evidence: suspect at %s after silence (window 45s)",
			suspectSeenAt.Sub(startedWaiting).Truncate(time.Millisecond))

		// §3: 90s window → offline — the identification objective
		// (runner-security §11: detection within 90 seconds).
		waitForStatus(t, drill, doomedID, "offline", 60*time.Second)
		offlineSeenAt := time.Now()
		identificationLatency := offlineSeenAt.Sub(lastHeartbeat)
		assert.LessOrEqual(t, identificationLatency, 92*time.Second,
			"identification must land within the 90s objective plus the 1s tick")
		t.Logf("drill evidence: identification_latency=%s (objective <= 90s + tick)",
			identificationLatency.Truncate(time.Millisecond))

		// §6: an offline runner MUST NOT get new leases — the protocol
		// answers the stable 403.
		code, reply := drill.claimHTTP(doomedToken, "gen-doomed-2", drill.queueVersion())
		assert.Equal(t, http.StatusForbidden, code, "offline claim: %v", reply)

		// §4.1/§4.2: the lease TTL (90s from claim) lapses with the
		// silence; the sweep fences it and requeues the orphan.
		require.Eventually(t, func() bool {
			return drill.workItemStatus(workItem) == "queued"
		}, 30*time.Second, 500*time.Millisecond, "the expired lease must requeue the work item")
		var leaseStatus string
		require.NoError(t, drill.db.QueryRowContext(ctx,
			`SELECT status FROM leases WHERE id = $1`, doomedLeaseID).Scan(&leaseStatus))
		assert.Equal(t, "expired", leaseStatus)

		// §4.2 redispatch exactly once: the rescuer claims with a fresh
		// epoch; the doomed generation's late result is fenced.
		code, reclaim := drill.claimHTTP(rescuerToken, "gen-rescuer-1", drill.queueVersion())
		require.Equal(t, http.StatusOK, code, "rescue claim: %v", reclaim)
		assert.EqualValues(t, 2, reclaim["epoch"], "redispatch mints a fresh epoch")
		rescueExecutionID, _ := reclaim["execution_id"].(string)
		var activeLeases int
		require.NoError(t, drill.db.QueryRowContext(ctx,
			`SELECT count(*) FROM leases WHERE work_item_id = $1 AND status = 'active'`, workItem).Scan(&activeLeases))
		assert.Equal(t, 1, activeLeases, "exactly one active lease after redispatch")

		lateCode := drill.completeHTTP(doomedToken, doomedExecutionID, "gen-doomed-1", map[string]any{
			"lease_id": doomedLeaseID, "connection_generation": "gen-doomed-1",
			"outcome": "completed", "commit_sha": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		})
		assert.Equal(t, http.StatusGone, lateCode,
			"the silent generation's late result is fenced (late evidence only, LEASE_EXPIRED)")

		// The rescuer finishes cleanly; the work item advances through
		// the normal terminal path.
		finishCode := drill.completeHTTP(rescuerToken, rescueExecutionID, "gen-rescuer-1", map[string]any{
			"lease_id": reclaim["id"], "connection_generation": "gen-rescuer-1",
			"outcome": "completed", "commit_sha": "cafebabecafebabecafebabecafebabecafebabe",
		})
		assert.Equal(t, http.StatusAccepted, finishCode)
		assert.Equal(t, "validating", drill.workItemStatus(workItem))
	})

	t.Run("section 9 audit chain", func(t *testing.T) {
		actions := drill.auditActions()
		for _, required := range []string{
			runner.AuditActionSuspectDetermined, runner.AuditActionOfflineDetermined,
			runner.AuditActionLeaseExpired, runner.AuditActionRedispatchQueued,
		} {
			assert.Contains(t, actions, required)
		}

		// The immutable chain verifies end to end: export, recompute,
		// compare (the M4 audit export contract).
		var maxSeq int64
		require.NoError(t, drill.db.QueryRowContext(ctx,
			`SELECT COALESCE(max(id), 0) FROM audit_events`).Scan(&maxSeq))
		rows, digests, _, err := drill.store.Observability().AuditExport(ctx, offlineProject, 1, maxSeq)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		require.NoError(t, audit.Verify(rows, digests))
		require.NoError(t, drill.store.Observability().AuditChainVerify(ctx, offlineProject, 1, maxSeq, digests))

		// Tampering breaks the chain (append-only discipline).
		if len(rows) > 0 {
			_, err = drill.db.ExecContext(ctx,
				`UPDATE audit_events SET action = 'tampered' WHERE id = $1`, rows[0].Seq)
			assert.Error(t, err, "the immutability trigger guards the audit trail")
		}
	})
}

func waitForStatus(t *testing.T, d *offlineDrill, runnerID, want string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		return d.runnerStatus(runnerID) == want
	}, timeout, 500*time.Millisecond, "runner %s must reach %s", runnerID, want)
}

// drillApprover provisions the enrolling approver identity the
// enrollment's created_by foreign key requires.
func offlineApprover(ctx context.Context, d *offlineDrill) string {
	user, err := d.store.Identities().GetOrCreateUser(ctx,
		"https://idp.example", "offline-drill-approver", "Offline Drill Approver")
	require.NoError(d.t, err)
	return user.ID
}

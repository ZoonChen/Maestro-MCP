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

// The emergency-stop drill anchor (M4-RBK-001, TC-SEC-STOP-001): the
// runbook's §4.1 immediate containment executed against real
// PostgreSQL and the real Runner protocol — the two-person stop
// freezes dispatch fail-closed on every surface this session owns
// (redispatch held, claim guard refused, in-flight leases fenced so
// late results hit the protocol's rejection codes), the credential
// revocation linkage disposes of the compromised device (its token
// answers 410 on every device-authenticated route), the durable stop
// replays from the append-only audit chain on restart, and the
// staged recovery requeues the waiting work with fresh epochs. The
// §9/§10 audit chain covers stop, revocation and recovery and
// verifies end to end, with no key or token material in any row.
//
// Wiring note (registered change request): the v3 claim handler does
// not yet consult EmergencyController.GuardClaim, so a claim on a
// freshly queued item during a freeze is the one write path this
// drill cannot honestly refuse at HTTP level today; every recovery
// and revocation surface it exercises is real.

const (
	stopTeam    = "018f7f30-0000-7000-8000-00000000dd01"
	stopProject = "018f7f30-0000-7000-8000-00000000dd02"
)

type stopDrill struct {
	t       *testing.T
	db      *sql.DB
	store   *store.PostgresStore
	ops     *runner.Ops
	ctrl    *runner.EmergencyController
	monitor *runner.OfflineMonitor
	router  *gin.Engine
}

func newStopDrill(t *testing.T) *stopDrill {
	t.Helper()
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	require.NoError(t, err)
	const dbName = "maestro_m4drill_stop"
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
	monitor, err := runner.NewOfflineMonitor(ops, 250*time.Millisecond)
	require.NoError(t, err)
	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	t.Cleanup(stopMonitor)
	go func() { _ = monitor.Run(monitorCtx) }()

	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'm4drill-stop')`, stopTeam)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'm4drill-stop', 'M4Drill Stop', 'active')`,
		stopProject, stopTeam)
	require.NoError(t, err)

	tokens, err := identity.NewDeviceTokenMinter("drill-stop-secret-0123456789abcdef01234", time.Now)
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler.RegisterRunnerV3(router, handler.RunnerV3Options{Registry: st, Tokens: tokens})
	return &stopDrill{t: t, db: db, store: st, ops: ops, ctrl: ctrl, monitor: monitor, router: router}
}

// enrollRunner is the drill's device onboarding (see the offline
// drill); the returned plaintext public key stays client-side only.
func (d *stopDrill) enrollRunner(ctx context.Context, name string) (runnerID, token, devicePublicKey string) {
	d.t.Helper()
	code, codeHash, err := identity.NewEnrollmentCode()
	require.NoError(d.t, err)
	require.NoError(d.t, d.store.RunnerRegistry().CreateEnrollment(ctx, &model.RunnerEnrollment{
		ProjectID: stopProject, CodeHash: codeHash,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		CreatedBy: stopApprover(ctx, d),
	}))
	devicePublicKey = "device-key-" + name
	body := fmt.Sprintf(`{"enrollment_code":%q,"device_public_key":%q,"display_name":%q,"capabilities":["rootless_oci","no_new_privileges","resource_limits"]}`,
		code, devicePublicKey, name)
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
	return credential.RunnerID, credential.AccessToken, devicePublicKey
}

func (d *stopDrill) claimHTTP(token, generation string) (int, map[string]any) {
	d.t.Helper()
	var queueVersion int64
	require.NoError(d.t, d.db.QueryRowContext(context.Background(),
		`SELECT version FROM projects WHERE id = $1`, stopProject).Scan(&queueVersion))
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

func (d *stopDrill) completeHTTP(token, executionID, generation string) int {
	d.t.Helper()
	body := fmt.Sprintf(`{"connection_generation":%q,"outcome":"completed","commit_sha":"feedfacefeedfacefeedfacefeedfacefeedface"}`, generation)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v3/executions/"+executionID+"/complete", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	d.router.ServeHTTP(response, request)
	return response.Code
}

func (d *stopDrill) meHTTP(token string) int {
	d.t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v3/runners/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	d.router.ServeHTTP(response, request)
	return response.Code
}

func (d *stopDrill) workItemStatus(workItemID string) string {
	var status string
	require.NoError(d.t, d.db.QueryRowContext(context.Background(),
		`SELECT status FROM work_items WHERE id = $1`, workItemID).Scan(&status))
	return status
}

func (d *stopDrill) auditDump(ctx context.Context) string {
	rows, err := d.db.QueryContext(ctx,
		`SELECT action || ' | ' || COALESCE(reason, '') FROM audit_events ORDER BY id`)
	require.NoError(d.t, err)
	defer rows.Close()
	var builder strings.Builder
	for rows.Next() {
		var line string
		require.NoError(d.t, rows.Scan(&line))
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	require.NoError(d.t, rows.Err())
	return builder.String()
}

// TestRunbookEmergencyStop is the TC-SEC-STOP-001 anchor.
func TestRunbookEmergencyStop(t *testing.T) {
	drill := newStopDrill(t)
	ctx := context.Background()

	compromisedID, compromisedToken, compromisedKey := drill.enrollRunner(ctx, "compromised")
	_, bystanderToken, _ := drill.enrollRunner(ctx, "bystander")

	workItem := "018f7f31-0000-7000-8000-00000000dd10"
	_, err := drill.db.ExecContext(ctx,
		`INSERT INTO work_items (id, project_id, title, status) VALUES ($1, $2, 'stop-drill', 'queued')`,
		workItem, stopProject)
	require.NoError(t, err)

	var compromisedExecutionID string
	t.Run("section 3 trigger and section 4.1 containment", func(t *testing.T) {
		code, lease := drill.claimHTTP(compromisedToken, "gen-bad-1")
		require.Equal(t, http.StatusOK, code, "claim: %v", lease)
		compromisedExecutionID, _ = lease["execution_id"].(string)

		// Suspected compromise: contain first, no waiting for
		// forensics — two-person stop with linked revocation.
		_, err := drill.ctrl.TriggerEmergencyStop(ctx, runner.EmergencyScopeGlobal, "INC-DRILL-P0",
			"security_owner:drill", []string{"approver-security", "approver-operations"},
			[]string{compromisedID})
		require.NoError(t, err)

		// The write freeze holds every recovery surface fail-closed.
		assert.ErrorIs(t, drill.ctrl.GuardClaim(stopProject), runner.ErrWriteFrozen)
		require.Eventually(t, func() bool {
			// The sweep must keep the orphaned item waiting, never
			// requeue: no new lease during the freeze.
			return drill.workItemStatus(workItem) == "executing"
		}, 3*time.Second, 100*time.Millisecond)
		// And the queue is empty for any device: no new lease is
		// issued while the freeze holds the work.
		code, reply := drill.claimHTTP(bystanderToken, "gen-good-1")
		require.Equal(t, http.StatusOK, code, "bystander claim: %v", reply)
		assert.Equal(t, false, reply["available"], "the freeze must not issue new leases")

		// §4.1.4 credential revocation: the compromised device is
		// refused on every device-authenticated route — 410 is the
		// contract's stable code for a revoked device.
		assert.Equal(t, http.StatusGone, drill.meHTTP(compromisedToken))
		code, reply = drill.claimHTTP(compromisedToken, "gen-bad-2")
		assert.Equal(t, http.StatusGone, code, "revoked claim: %v", reply)
		assert.Equal(t, http.StatusGone,
			drill.completeHTTP(compromisedToken, compromisedExecutionID, "gen-bad-1"))

		// §5/§8 idempotency: replaying the same incident changes
		// nothing and writes no second stop row.
		replay, err := drill.ctrl.TriggerEmergencyStop(ctx, runner.EmergencyScopeGlobal, "INC-DRILL-P0",
			"security_owner:drill", []string{"approver-security", "approver-operations"}, nil)
		require.NoError(t, err)
		assert.True(t, replay.AlreadyStopped)
	})

	t.Run("section 6 restart durability", func(t *testing.T) {
		reloaded, err := runner.NewEmergencyController(drill.store)
		require.NoError(t, err)
		require.NoError(t, reloaded.LoadFreezeState(ctx))
		assert.True(t, reloaded.FrozenProject(stopProject),
			"the stop must survive a control-plane restart")
		assert.ErrorIs(t, reloaded.GuardClaim(stopProject), runner.ErrWriteFrozen)
	})

	t.Run("section 4.2 staged recovery", func(t *testing.T) {
		// Wrong incident and solo approval refuse.
		assert.Error(t, drill.ctrl.LiftEmergencyStop(ctx, runner.EmergencyScopeGlobal, "INC-WRONG",
			"operations_owner:drill", []string{"a", "b"}))
		assert.ErrorIs(t, drill.ctrl.LiftEmergencyStop(ctx, runner.EmergencyScopeGlobal, "INC-DRILL-P0",
			"operations_owner:drill", []string{"solo"}), runner.ErrEmergencyApproval)

		require.NoError(t, drill.ctrl.LiftEmergencyStop(ctx, runner.EmergencyScopeGlobal, "INC-DRILL-P0",
			"operations_owner:drill", []string{"approver-security", "approver-operations"}))
		assert.False(t, drill.ctrl.FrozenProject(stopProject))

		// The waiting item requeues and re-dispatches exactly once
		// with a fresh epoch to a clean device.
		require.Eventually(t, func() bool {
			return drill.workItemStatus(workItem) == "queued"
		}, 5*time.Second, 100*time.Millisecond, "recovery must requeue the waiting work")
		code, reclaim := drill.claimHTTP(bystanderToken, "gen-good-2")
		require.Equal(t, http.StatusOK, code, "recovery claim: %v", reclaim)
		assert.EqualValues(t, 2, reclaim["epoch"], "recovery mints a fresh epoch")
		recoveryExecutionID, _ := reclaim["execution_id"].(string)
		assert.Equal(t, http.StatusAccepted,
			drill.completeHTTP(bystanderToken, recoveryExecutionID, "gen-good-2"))
		assert.Equal(t, "validating", drill.workItemStatus(workItem))
	})

	t.Run("section 9/10 audit chain and red lines", func(t *testing.T) {
		var maxSeq int64
		require.NoError(t, drill.db.QueryRowContext(ctx,
			`SELECT COALESCE(max(id), 0) FROM audit_events`).Scan(&maxSeq))
		rows, digests, _, err := drill.store.Observability().AuditExport(ctx, stopProject, 1, maxSeq)
		require.NoError(t, err)
		require.NotEmpty(t, rows)
		require.NoError(t, audit.Verify(rows, digests))

		// The project-scoped export slice carries the project-level
		// rows (revocation, lease fencing); the global stop/recover
		// rows are control-plane-wide (project_id NULL) and asserted
		// over the full trail below.
		projectActions := map[string]bool{}
		for _, row := range rows {
			projectActions[row.Action] = true
		}
		for _, required := range []string{
			runner.AuditActionRunnerRevoked, runner.AuditActionLeaseExpired,
		} {
			assert.True(t, projectActions[required], "the project export must cover %s", required)
		}
		allRows, allErr := drill.db.QueryContext(ctx, `SELECT action FROM audit_events`)
		require.NoError(t, allErr)
		actions := map[string]bool{}
		for allRows.Next() {
			var action string
			require.NoError(t, allRows.Scan(&action))
			actions[action] = true
		}
		require.NoError(t, allRows.Err())
		for _, required := range []string{
			runner.AuditActionEmergencyStop, runner.AuditActionEmergencyRecovered,
		} {
			assert.True(t, actions[required], "audit must cover %s", required)
		}

		// The Keychain red line: no device key or token material in
		// any audit row (hash references only).
		dump := drill.auditDump(ctx)
		assert.NotContains(t, dump, compromisedKey)
		assert.NotContains(t, dump, compromisedToken)
		assert.NotContains(t, dump, bystanderToken)

		// Tamper evidence: the append-only trigger guards the chain.
		_, err = drill.db.ExecContext(ctx,
			`UPDATE audit_events SET decision = 'deny' WHERE id = $1`, rows[0].Seq)
		assert.Error(t, err, "the immutability trigger guards the audit trail")
	})
}

// drillApprover provisions the enrolling approver identity the
// enrollment's created_by foreign key requires.
func stopApprover(ctx context.Context, d *stopDrill) string {
	user, err := d.store.Identities().GetOrCreateUser(ctx,
		"https://idp.example", "stop-drill-approver", "Stop Drill Approver")
	require.NoError(d.t, err)
	return user.ID
}

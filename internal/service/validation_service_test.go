package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTaskTestRequirementsRejectsArbitraryCommand(t *testing.T) {
	err := ValidateTaskTestRequirements([]byte(`{"command":"go test ./...","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":80}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command is forbidden")
}

func TestResolveValidationPolicyRequiresCompleteTrustedPolicy(t *testing.T) {
	profile := testCommandProfile(t, "coverage")
	digest, err := profile.Digest()
	require.NoError(t, err)
	minCoverage := 80.0
	task := &model.Task{TestRequirements: validationRequirementsJSON(t, profile, digest, "coverage.out", minCoverage)}
	cfg := TestExecutionConfig{PolicyVersion: "3.0.0", PolicyDigest: "sha256:" + strings.Repeat("c", 64)}

	policy, err := resolveValidationPolicy(task, &model.Project{}, cfg)
	require.NoError(t, err)
	assert.Equal(t, profile.ID, policy.ProfileID)

	_, err = resolveValidationPolicy(&model.Task{}, &model.Project{Config: json.RawMessage(`{}`)}, cfg)
	require.Error(t, err)
	_, err = resolveValidationPolicy(task, &model.Project{}, TestExecutionConfig{})
	require.Error(t, err)
}

func TestSubmitAndValidateStatusAndOwnershipFailBeforeExecution(t *testing.T) {
	svc := setupTestEnv(t)
	ctx := context.Background()

	pending := newTestTask("T-sa-pending")
	mustCreateTask(t, svc.stores.taskStore, pending)
	err := svc.validSvc.SubmitAndValidate(ctx, testProjectID, pending.ID, "session-1", "worker-1", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrTaskStateInvalid)

	seedTestSession(t, svc.stores, "session-owner")
	sid := "session-owner"
	owned := newTestTask("T-sa-wrong")
	owned.Status = model.TaskStatusInProgress
	owned.AssignedSessionID = &sid
	mustCreateTask(t, svc.stores.taskStore, owned)
	err = svc.validSvc.SubmitAndValidate(ctx, testProjectID, owned.ID, "session-impostor", "worker-1", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrTaskNotOwned)
}

func TestSubmitAndValidateSuccessUsesApprovedProfile(t *testing.T) {
	fixture := newValidationFixture(t, "coverage", `["src"]`)

	err := fixture.services.validSvc.SubmitAndValidate(context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil)
	require.NoError(t, err)

	task, err := fixture.services.stores.taskStore.GetByID(context.Background(), testProjectID, fixture.taskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusSubmitted, task.Status)
	runs, err := fixture.services.validSvc.GetValidationHistory(context.Background(), testProjectID, fixture.taskID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "passed", runs[0].Result)
	assert.Equal(t, model.EvidenceAuthorityDiagnostic, runs[0].Authority)
	assert.Equal(t, model.EvidenceProducerMaestroLocal, runs[0].Producer)
	assert.Nil(t, runs[0].PipelineID)
	assert.Nil(t, runs[0].JobID)
	assert.Contains(t, runs[0].TestCommand, "go-unit@3.0.0")
	assert.Equal(t, runs[0].TestCommand, runs[0].ProfileRef)
	assert.Equal(t, "3.0.0", runs[0].PolicyVersion)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, runs[0].PolicyDigest)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, runs[0].EvidenceDigest)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, runs[0].WorkspaceDigest)
	assert.Regexp(t, `^[0-9a-f]{40}$`, runs[0].BaseCommit)
	assert.Regexp(t, `^[0-9a-f]{40}$`, runs[0].SourceCommit)
	require.NotNil(t, runs[0].Coverage)
	assert.Equal(t, 100.0, *runs[0].Coverage)

	var leaseStatus, workerStatus, worktreeStatus string
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT status FROM task_leases WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&leaseStatus))
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT status FROM agent_workers WHERE project_id = ? AND id = ?`, testProjectID, "worker-1").Scan(&workerStatus))
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT status FROM worktrees WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&worktreeStatus))
	assert.Equal(t, model.LeaseStatusCompleted, leaseStatus)
	assert.Equal(t, model.WorkerStatusIdle, workerStatus)
	assert.Equal(t, model.WorktreeStatusSealed, worktreeStatus)
	var physicalSessionID, leaseID string
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`, testProjectID, fixture.sessionID).Scan(&physicalSessionID))
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT id FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&leaseID))
	for _, expected := range []struct {
		typeName, id string
		count        int
	}{
		{"task", fixture.taskID, 2},
		{"lease", leaseID, 2},
		{"worker", physicalSessionID + "/worker-1", 2},
		{"session", physicalSessionID, 1},
	} {
		var count int
		require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
			WHERE project_id = ? AND aggregate_type = ? AND aggregate_id = ?`,
			testProjectID, expected.typeName, expected.id).Scan(&count))
		assert.Equal(t, expected.count, count, "%s/%s", expected.typeName, expected.id)
	}
	var worktreeHistories, invalidHistories int
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_type = 'worktree' AND to_status = 'sealed'`,
		testProjectID).Scan(&worktreeHistories))
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND (to_version <> from_version + 1 OR COALESCE(actor_id, '') = '' OR
		  reason = '' OR COALESCE(causation_id, '') = '')`, testProjectID).Scan(&invalidHistories))
	assert.Equal(t, 1, worktreeHistories)
	assert.Zero(t, invalidHistories)
}

func TestSubmitAndValidatePersistsOnlyRedactedDiagnosticOutput(t *testing.T) {
	fixture := newValidationFixture(t, "secret-coverage", `["src"]`)
	require.NoError(t, fixture.services.validSvc.SubmitAndValidate(
		context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil,
	))

	runs, err := fixture.services.validSvc.GetValidationHistory(context.Background(), testProjectID, fixture.taskID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "passed", runs[0].Result)
	require.NotNil(t, runs[0].TestOutput)
	redactedOutput := *runs[0].TestOutput
	assert.Contains(t, redactedOutput, "[REDACTED]")
	assert.Contains(t, redactedOutput, "[REDACTED PRIVATE KEY]")
	for _, secret := range testDiagnosticSecretCanaries().values() {
		assert.NotContains(t, redactedOutput, secret)
	}

	var submittedOutput string
	require.NoError(t, fixture.services.stores.db.QueryRowContext(context.Background(),
		`SELECT test_output FROM task_results WHERE project_id = ? AND task_id = ?`,
		testProjectID, fixture.taskID,
	).Scan(&submittedOutput))
	for _, secret := range testDiagnosticSecretCanaries().values() {
		assert.NotContains(t, submittedOutput, secret)
	}
}

func TestSubmitAndValidateRenewsShortLeaseAndAcceptsSameLeaseHeartbeat(t *testing.T) {
	fixture := newValidationFixture(t, "sleep-coverage", `["src"]`)
	fixture.services.validSvc.testExecConfig.DefaultTimeout = 5 * time.Second
	ctx := context.Background()
	leaseID := uuid.NewString()
	expiresAt := time.Now().UTC().Add(3 * time.Second).Format("2006-01-02 15:04:05")
	var oldLeaseID string
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT active_lease_id
		FROM tasks WHERE project_id = ? AND id = ?`, testProjectID, fixture.taskID).Scan(&oldLeaseID))
	_, err := fixture.services.stores.db.ExecContext(ctx, `UPDATE task_leases
		SET id = ?, expires_at = ? WHERE project_id = ? AND id = ?`,
		leaseID, expiresAt, testProjectID, oldLeaseID)
	require.NoError(t, err)
	_, err = fixture.services.stores.db.ExecContext(ctx, `UPDATE tasks
		SET active_lease_id = ?, lease_expires_at = ? WHERE project_id = ? AND id = ?`,
		leaseID, expiresAt, testProjectID, fixture.taskID)
	require.NoError(t, err)

	validationDone := make(chan error, 1)
	go func() {
		validationDone <- fixture.services.validSvc.SubmitAndValidate(
			ctx, testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil)
	}()

	// The service reserves the bounded profile window before it starts the host
	// diagnostic. Observe that reservation, then renew the same Lease through
	// the public heartbeat path while the profile is still executing.
	deadline := time.Now().Add(2 * time.Second)
	var leaseVersion int64
	var renewedExpiry string
	for time.Now().Before(deadline) {
		err = fixture.services.stores.db.QueryRowContext(ctx, `SELECT version, expires_at
			FROM task_leases WHERE project_id = ? AND id = ?`, testProjectID, leaseID).
			Scan(&leaseVersion, &renewedExpiry)
		require.NoError(t, err)
		if leaseVersion >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int64(2), leaseVersion, "validation must reserve one new Lease version")
	renewedDeadline, err := time.Parse("2006-01-02 15:04:05", renewedExpiry)
	require.NoError(t, err)
	assert.Greater(t, renewedDeadline.Unix(), time.Now().UTC().Add(2*time.Second).Unix())
	lease, err := fixture.services.taskSvc.HeartbeatTask(ctx, testProjectID, fixture.taskID,
		fixture.sessionID, "worker-1", leaseID, leaseVersion, "validation-heartbeat-0001")
	require.NoError(t, err)
	assert.Equal(t, int64(3), lease.Version)

	select {
	case err := <-validationDone:
		require.NoError(t, err)
	case <-time.After(7 * time.Second):
		t.Fatal("validation did not finish after the bounded helper exited")
	}
	task, err := fixture.services.stores.taskStore.GetByID(ctx, testProjectID, fixture.taskID)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatusValidating, task.Status)
	var finalLeaseStatus string
	var finalLeaseVersion int64
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT status, version
		FROM task_leases WHERE project_id = ? AND id = ?`, testProjectID, leaseID).
		Scan(&finalLeaseStatus, &finalLeaseVersion))
	assert.Equal(t, model.LeaseStatusCompleted, finalLeaseStatus)
	assert.Equal(t, int64(4), finalLeaseVersion)
}

func TestSubmitAndValidateMissingWorktreePersistsFailureAndReleasesAuthority(t *testing.T) {
	svc := setupTestEnv(t)
	seedTestSession(t, svc.stores, "session-owner")
	sid := "session-owner"
	task := newTestTask("T-no-worktree")
	prepareValidationLease(t, svc.stores, task, sid, "worker-1")

	err := svc.validSvc.SubmitAndValidate(context.Background(), testProjectID, task.ID, sid, "worker-1", nil)
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "WORKTREE_ERROR", validationErr.Code)
	runs, historyErr := svc.validSvc.GetValidationHistory(context.Background(), testProjectID, task.ID)
	require.NoError(t, historyErr)
	require.Len(t, runs, 1)
	assert.Equal(t, "WORKTREE_ERROR", *runs[0].ErrorCode)
	unchanged, getErr := svc.stores.taskStore.GetByID(context.Background(), testProjectID, task.ID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusNeedsHuman, unchanged.Status)
	assert.Nil(t, unchanged.ActiveLeaseID)
	var leaseStatus, workerStatus string
	require.NoError(t, svc.stores.db.QueryRow(`SELECT status FROM task_leases WHERE project_id = ? AND task_id = ?`, testProjectID, task.ID).Scan(&leaseStatus))
	require.NoError(t, svc.stores.db.QueryRow(`SELECT status FROM agent_workers WHERE project_id = ? AND id = ?`, testProjectID, "worker-1").Scan(&workerStatus))
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	assert.Equal(t, model.WorkerStatusIdle, workerStatus)
}

func TestSubmitAndValidateBoundaryCoverageOutputAndTimeoutFailClosed(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		allowedDirs string
		prepare     func(t *testing.T, fixture *validationFixture)
		configure   func(cfg *TestExecutionConfig)
		wantCode    string
		wantStatus  string
	}{
		{
			name:        "boundary violation",
			mode:        "coverage",
			allowedDirs: `["src"]`,
			prepare: func(t *testing.T, fixture *validationFixture) {
				require.NoError(t, os.MkdirAll(fixture.worktreePath+"/outside", 0o755))
				require.NoError(t, os.WriteFile(fixture.worktreePath+"/outside/file.go", []byte("package outside\n"), 0o600))
			},
			wantCode: "BOUNDARY_VIOLATION", wantStatus: model.TaskStatusFailed,
		},
		{name: "coverage missing", mode: "success", allowedDirs: `["src"]`, wantCode: "COVERAGE_MISSING", wantStatus: model.TaskStatusNeedsHuman},
		{name: "output truncated", mode: "output", allowedDirs: `["src"]`, wantCode: "OUTPUT_TRUNCATED", wantStatus: model.TaskStatusNeedsHuman},
		{
			name:        "timeout",
			mode:        "sleep",
			allowedDirs: `["src"]`,
			configure: func(cfg *TestExecutionConfig) {
				cfg.DefaultTimeout = 50 * time.Millisecond
			},
			wantCode: "PROFILE_TIMEOUT", wantStatus: model.TaskStatusNeedsHuman,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newValidationFixture(t, tt.mode, tt.allowedDirs)
			if tt.prepare != nil {
				tt.prepare(t, fixture)
			}
			if tt.configure != nil {
				cfg := fixture.services.validSvc.testExecConfig
				tt.configure(&cfg)
				fixture.services.validSvc.testExecConfig = cfg
			}
			err := fixture.services.validSvc.SubmitAndValidate(context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assert.Equal(t, tt.wantCode, validationErr.Code)
			task, getErr := fixture.services.stores.taskStore.GetByID(context.Background(), testProjectID, fixture.taskID)
			require.NoError(t, getErr)
			assert.Equal(t, tt.wantStatus, task.Status)
			assert.Nil(t, task.ActiveLeaseID)
			runs, historyErr := fixture.services.validSvc.GetValidationHistory(context.Background(), testProjectID, fixture.taskID)
			require.NoError(t, historyErr)
			require.Len(t, runs, 1)
			assert.Equal(t, tt.wantCode, *runs[0].ErrorCode)
			if tt.wantCode == "OUTPUT_TRUNCATED" {
				assert.True(t, runs[0].OutputTruncated)
			}
		})
	}
}

func TestSubmitAndValidateEvidencePersistenceFailureBlocksState(t *testing.T) {
	fixture := newValidationFixture(t, "coverage", `["src"]`)
	_, dropErr := fixture.services.stores.db.ExecContext(context.Background(), `DROP TABLE validation_runs`)
	require.NoError(t, dropErr)

	err := fixture.services.validSvc.SubmitAndValidate(context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil)
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "EVIDENCE_PERSIST_FAILED", validationErr.Code)
	task, getErr := fixture.services.stores.taskStore.GetByID(context.Background(), testProjectID, fixture.taskID)
	require.NoError(t, getErr)
	assert.Equal(t, model.TaskStatusInProgress, task.Status)
}

func TestSubmitAndValidateExpiredOrMismatchedLeaseFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		mutate  string
		wantErr error
	}{
		{name: "expired", mutate: `UPDATE task_leases SET expires_at = '2000-01-01 00:00:00' WHERE task_id = ?`, wantErr: store.ErrValidationFailed},
		{name: "wrong epoch", mutate: `UPDATE task_leases SET epoch = epoch + 1 WHERE task_id = ?`, wantErr: store.ErrValidationFailed},
		{name: "worker not reserved", mutate: `UPDATE agent_workers SET current_task_id = NULL, status = 'idle' WHERE current_task_id = ?`, wantErr: store.ErrValidationFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newValidationFixture(t, "coverage", `["src"]`)
			_, err := fixture.services.stores.db.Exec(tt.mutate, fixture.taskID)
			require.NoError(t, err)
			err = fixture.services.validSvc.SubmitAndValidate(context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assert.Equal(t, "LEASE_INVALID", validationErr.Code)
			assert.ErrorIs(t, err, tt.wantErr)
			task, getErr := fixture.services.stores.taskStore.GetByID(context.Background(), testProjectID, fixture.taskID)
			require.NoError(t, getErr)
			assert.Equal(t, model.TaskStatusNeedsHuman, task.Status)
			assert.Nil(t, task.ActiveLeaseID)
		})
	}
}

func TestSubmitVerificationRequiresLatestUntamperedPassedEvidence(t *testing.T) {
	for _, tt := range []struct {
		name          string
		authoritative bool
		tamper        bool
		wantStatus    string
		wantError     bool
	}{
		{name: "local diagnostic cannot approve", wantStatus: model.TaskStatusValidating, wantError: true},
		{name: "exact merge gate evidence passes", authoritative: true, wantStatus: model.TaskStatusReadyForHumanMerge},
		{name: "filesystem drift blocks merge gate", authoritative: true, tamper: true, wantStatus: model.TaskStatusValidating, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newValidationFixture(t, "coverage", `["src"]`)
			ctx := context.Background()
			require.NoError(t, fixture.services.validSvc.SubmitAndValidate(ctx, testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil))

			verifierSessionID := "verifier-session"
			seedScopedValidationSession(t, fixture.services.stores, "physical-verifier-session", verifierSessionID, model.RoleVerifier)
			seedTestWorker(t, fixture.services.stores, verifierSessionID, "verifier-worker")
			claimed, err := fixture.services.taskSvc.GetVerificationTask(ctx, testProjectID, verifierSessionID, "verifier-worker")
			require.NoError(t, err)
			require.Equal(t, fixture.taskID, claimed.ID)
			require.NotNil(t, claimed.ActiveLeaseID)
			claimedLeaseID := *claimed.ActiveLeaseID
			claimedVersion := claimed.Version

			if tt.authoritative {
				seedMergeGateValidationEvidence(t, fixture.services.stores, fixture.taskID)
			}

			if tt.tamper {
				require.NoError(t, os.WriteFile(fixture.worktreePath+"/src/main.go", []byte("package main\n// tampered after validation\n"), 0o600))
			}
			err = fixture.services.taskSvc.SubmitVerification(ctx, testProjectID, verifierSessionID, "verifier-worker", fixture.taskID, true, "reviewed")
			if tt.wantError {
				require.Error(t, err)
				assert.ErrorIs(t, err, store.ErrValidationFailed)
			} else {
				require.NoError(t, err)
			}
			task, getErr := fixture.services.stores.taskStore.GetByID(ctx, testProjectID, fixture.taskID)
			require.NoError(t, getErr)
			assert.Equal(t, tt.wantStatus, task.Status)
			if tt.wantError {
				assert.Equal(t, claimedVersion, task.Version, "rejected pass must not consume the task CAS")
				require.NotNil(t, task.ActiveLeaseID)
				assert.Equal(t, claimedLeaseID, *task.ActiveLeaseID)
				var leaseStatus, workerStatus string
				var currentTaskID *string
				require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT status FROM task_leases
					WHERE project_id = ? AND id = ?`, testProjectID, claimedLeaseID).Scan(&leaseStatus))
				require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT status, current_task_id FROM agent_workers
					WHERE project_id = ? AND id = ?`, testProjectID, "verifier-worker").Scan(&workerStatus, &currentTaskID))
				assert.Equal(t, model.LeaseStatusActive, leaseStatus)
				assert.Equal(t, model.WorkerStatusBusy, workerStatus)
				require.NotNil(t, currentTaskID)
				assert.Equal(t, fixture.taskID, *currentTaskID)
			}
		})
	}
}

// seedMergeGateValidationEvidence is deliberately test-only. M0 has no
// production path capable of assigning merge_gate authority; M2 will replace
// this fixture with authenticated GitLab Pipeline/Job ingestion.
func seedMergeGateValidationEvidence(t *testing.T, stores *testStores, taskID string) {
	t.Helper()
	result, err := stores.db.ExecContext(context.Background(), `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, source_commit, changed_files,
		test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
		authority, producer, pipeline_id, job_id,
		test_exit_code, test_output, output_truncated, coverage,
		boundary_ok, test_ok, coverage_ok, summary, result, error_code,
		duration_ms, log_path, created_at
	)
	SELECT local.task_id, local.project_id, local.attempt + 1,
		local.base_commit, local.source_commit, local.changed_files,
		local.test_command, local.profile_ref, local.policy_version, local.policy_digest,
		?, local.workspace_digest,
		?, 'gitlab-ci:test-fixture', NULL, NULL,
		local.test_exit_code, local.test_output, local.output_truncated, local.coverage,
		local.boundary_ok, local.test_ok, local.coverage_ok, local.summary,
		local.result, local.error_code, local.duration_ms, local.log_path, datetime('now')
	FROM validation_runs AS local
	WHERE local.id = (
		SELECT id FROM validation_runs WHERE project_id = ? AND task_id = ?
		ORDER BY attempt DESC, id DESC LIMIT 1
	)`,
		"sha256:"+strings.Repeat("f", 64), model.EvidenceAuthorityMergeGate,
		testProjectID, taskID,
	)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
}

type validationFixture struct {
	services     *testServices
	taskID       string
	sessionID    string
	worktreePath string
}

func newValidationFixture(t *testing.T, helperMode, allowedDirs string) *validationFixture {
	t.Helper()
	svc := setupTestEnv(t)
	workspace, baseCommit := createTestGitRepository(t)
	worktreePath, err := createWorktree(context.Background(), workspace, "T-fixture")
	require.NoError(t, err)
	_, err = svc.stores.db.ExecContext(context.Background(), `UPDATE projects SET workspace_path = ? WHERE id = ?`, workspace, testProjectID)
	require.NoError(t, err)

	profile := testCommandProfile(t, helperMode)
	digest, err := profile.Digest()
	require.NoError(t, err)
	registry, err := NewCommandProfileRegistry([]CommandProfile{profile})
	require.NoError(t, err)
	svc.validSvc.testExecConfig = TestExecutionConfig{
		Profiles:           registry,
		PolicyVersion:      "3.0.0",
		PolicyDigest:       "sha256:" + strings.Repeat("c", 64),
		AllowHostExecution: true,
	}

	sessionID := "session-owner"
	seedScopedValidationSession(t, svc.stores, "physical-session-owner", sessionID, model.RoleBackend)
	taskID := "T-validation"
	task := newTestTask(taskID)
	task.AllowedDirectories = allowedDirs
	task.TestRequirements = validationRequirementsJSON(t, profile, digest, "coverage.out", 80)
	prepareValidationLease(t, svc.stores, task, sessionID, "worker-1")
	_, err = svc.stores.worktreeStore.Create(context.Background(), testProjectID, &model.Worktree{
		TaskID:       taskID,
		ProjectID:    testProjectID,
		SessionID:    &sessionID,
		WorktreePath: worktreePath,
		BranchName:   "task/T-fixture",
		BaseCommit:   baseCommit,
		Status:       model.WorktreeStatusActive,
		Generation:   task.LeaseEpoch,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	return &validationFixture{services: svc, taskID: taskID, sessionID: sessionID, worktreePath: worktreePath}
}

func seedScopedValidationSession(t *testing.T, stores *testStores, physicalID, externalID, role string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := stores.db.ExecContext(context.Background(), `
		INSERT INTO agent_sessions
		(id, project_id, role, client_type, capacity, status, version, external_id, last_heartbeat, created_at)
		VALUES (?, ?, ?, 'test', 5, ?, 0, ?, ?, ?)`,
		physicalID, testProjectID, role, model.SessionStatusOnline, externalID, now, now)
	require.NoError(t, err)
}

func prepareValidationLease(t *testing.T, stores *testStores, task *model.Task, externalSessionID, workerID string) {
	t.Helper()
	var physicalSessionID string
	require.NoError(t, stores.db.QueryRowContext(context.Background(), `
		SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		testProjectID, externalSessionID,
	).Scan(&physicalSessionID))
	seedTestWorker(t, stores, externalSessionID, workerID)
	expiresAt := time.Now().UTC().Add(10 * time.Minute).Format("2006-01-02 15:04:05")
	leaseID := "lease-" + task.ID
	task.Status = model.TaskStatusExecuting
	task.AssignedSessionID = &externalSessionID
	task.AssignedWorkerID = &workerID
	task.Version = 2
	task.LeaseEpoch = 1
	task.ActiveLeaseID = &leaseID
	task.LeaseExpiresAt = &expiresAt
	mustCreateTask(t, stores.taskStore, task)
	result, err := stores.db.ExecContext(context.Background(), `
		UPDATE agent_workers SET current_task_id = ?, status = ?, version = version + 1
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = ?`,
		task.ID, model.WorkerStatusBusy, testProjectID, physicalSessionID, workerID, model.WorkerStatusIdle)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	_, err = stores.db.ExecContext(context.Background(), `
		INSERT INTO task_leases
		(id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, 1, ?, datetime('now'), datetime('now'))`,
		leaseID, testProjectID, task.ID, physicalSessionID, workerID, model.LeaseStatusActive, expiresAt)
	require.NoError(t, err)
}

func validationRequirementsJSON(t *testing.T, profile CommandProfile, digest, coveragePath string, minCoverage float64) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"profile_id":      profile.ID,
		"profile_version": profile.Version,
		"profile_digest":  digest,
		"coverage_format": "go-cover",
		"coverage_path":   coveragePath,
		"min_coverage":    minCoverage,
	})
	require.NoError(t, err)
	return data
}

func TestSubmitAndValidateNonexistentTask(t *testing.T) {
	svc := setupTestEnv(t)
	err := svc.validSvc.SubmitAndValidate(context.Background(), testProjectID, "T-NONEXIST", "session-1", "worker-1", nil)
	require.Error(t, err)
	assert.False(t, errors.Is(err, store.ErrValidationFailed))
}

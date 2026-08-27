package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidationErrorPreservesStableCodeAndCause(t *testing.T) {
	withoutCause := &ValidationError{Code: "POLICY_INVALID", Message: "policy rejected"}
	assert.Equal(t, "POLICY_INVALID: policy rejected", withoutCause.Error())

	cause := errors.New("database unavailable")
	withCause := &ValidationError{Code: "EVIDENCE_PERSIST_FAILED", Message: "evidence rejected", Cause: cause}
	assert.Equal(t, "EVIDENCE_PERSIST_FAILED: evidence rejected: database unavailable", withCause.Error())
	assert.ErrorIs(t, withCause, cause)
}

func TestSubmitAndValidateRejectsWorkerProfileAndExecutionFailure(t *testing.T) {
	t.Run("forged worker", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		err := fixture.services.validSvc.SubmitAndValidate(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "forged-worker", nil,
		)
		require.ErrorIs(t, err, store.ErrTaskNotOwned)
		assertValidationAuthorityUnchanged(t, fixture)
	})

	t.Run("profile no longer approved", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		registry, err := NewCommandProfileRegistry(nil)
		require.NoError(t, err)
		fixture.services.validSvc.testExecConfig.Profiles = registry

		err = fixture.services.validSvc.SubmitAndValidate(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil,
		)
		assertValidationFailureCode(t, err, "PROFILE_NOT_APPROVED")
		assertValidationFailureReleasedAuthority(t, fixture, model.TaskStatusNeedsHuman)
	})

	t.Run("approved profile returns nonzero", func(t *testing.T) {
		fixture := newValidationFixture(t, "failure", `["src"]`)
		err := fixture.services.validSvc.SubmitAndValidate(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil,
		)
		assertValidationFailureCode(t, err, "TEST_FAILED")
		assert.ErrorIs(t, err, store.ErrTestExecutionFailed)
		assertValidationFailureReleasedAuthority(t, fixture, model.TaskStatusFailed)
	})

	t.Run("host execution disabled", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		fixture.services.validSvc.testExecConfig.AllowHostExecution = false
		err := fixture.services.validSvc.SubmitAndValidate(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil,
		)
		assertValidationFailureCode(t, err, "PROFILE_EXEC_ERROR")
		assertValidationFailureReleasedAuthority(t, fixture, model.TaskStatusNeedsHuman)
	})
}

func TestSubmitAndValidateLeaseRenewalFailureHasNoExecutionSideEffects(t *testing.T) {
	fixture := newValidationFixture(t, "coverage", `["src"]`)
	_, err := fixture.services.stores.db.Exec(`CREATE TRIGGER fail_validation_lease_renewal
		BEFORE UPDATE ON task_leases BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_LEASE_RENEWAL'); END`)
	require.NoError(t, err)

	err = fixture.services.validSvc.SubmitAndValidate(
		context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil,
	)
	assertValidationFailureCode(t, err, "LEASE_RENEWAL_FAILED")
	assert.Contains(t, err.Error(), "FAIL_VALIDATION_LEASE_RENEWAL")
	assertValidationAuthorityUnchanged(t, fixture)
	assertValidationRunCount(t, fixture, 0)
}

func TestPersistFailedValidationRollsBackEveryWriteStage(t *testing.T) {
	stages := []struct {
		name    string
		trigger string
	}{
		{"evidence", `CREATE TRIGGER fail_validation_failure BEFORE INSERT ON validation_runs
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"task", `CREATE TRIGGER fail_validation_failure BEFORE UPDATE ON tasks
			WHEN NEW.status = 'failed' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"task history", `CREATE TRIGGER fail_validation_failure BEFORE INSERT ON state_history
			WHEN NEW.aggregate_type = 'task' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"lease", `CREATE TRIGGER fail_validation_failure BEFORE UPDATE ON task_leases
			WHEN NEW.status = 'released' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"worker", `CREATE TRIGGER fail_validation_failure BEFORE UPDATE ON agent_workers
			WHEN NEW.status = 'idle' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"worktree", `CREATE TRIGGER fail_validation_failure BEFORE UPDATE ON worktrees
			WHEN NEW.status = 'quarantined' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"activity", `CREATE TRIGGER fail_validation_failure BEFORE INSERT ON activity_log
			WHEN NEW.action = 'validation_rejected' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
		{"audit", `CREATE TRIGGER fail_validation_failure BEFORE INSERT ON audit_log
			WHEN NEW.action = 'validation.evaluate' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_FAILURE_STAGE'); END`},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			fixture := newValidationFixture(t, "coverage", `["src"]`)
			evidence := validationEvidenceSnapshot(t, fixture)
			evidence.Result = "failed"
			evidence.ErrorCode = "TEST_FAILED"
			require.NoError(t, sealValidationEvidence(testProjectID, fixture.taskID, &evidence))
			_, err := fixture.services.stores.db.Exec(stage.trigger)
			require.NoError(t, err)

			err = fixture.services.validSvc.persistFailedValidation(
				context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil, &evidence,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_VALIDATION_FAILURE_STAGE")
			assertValidationAuthorityUnchanged(t, fixture)
			assertValidationRunCount(t, fixture, 0)
			assertValidationHistoryCount(t, fixture, 0)
		})
	}
}

func TestPersistSuccessfulValidationRollsBackEveryWriteStage(t *testing.T) {
	stages := []struct {
		name    string
		trigger string
	}{
		{"evidence", `CREATE TRIGGER fail_validation_success BEFORE INSERT ON validation_runs
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"result", `CREATE TRIGGER fail_validation_success BEFORE INSERT ON task_results
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"task", `CREATE TRIGGER fail_validation_success BEFORE UPDATE ON tasks
			WHEN NEW.status = 'validating' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"lease", `CREATE TRIGGER fail_validation_success BEFORE UPDATE ON task_leases
			WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"worker", `CREATE TRIGGER fail_validation_success BEFORE UPDATE ON agent_workers
			WHEN OLD.status = 'busy' AND NEW.status = 'idle' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"worktree", `CREATE TRIGGER fail_validation_success BEFORE UPDATE ON worktrees
			WHEN NEW.status = 'sealed' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"history", `CREATE TRIGGER fail_validation_success BEFORE INSERT ON state_history
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"activity", `CREATE TRIGGER fail_validation_success BEFORE INSERT ON activity_log
			WHEN NEW.action = 'submitted' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
		{"audit", `CREATE TRIGGER fail_validation_success BEFORE INSERT ON audit_log
			WHEN NEW.action = 'validation.evaluate' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_SUCCESS_STAGE'); END`},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			fixture := newValidationFixture(t, "coverage", `["src"]`)
			evidence := validationEvidenceSnapshot(t, fixture)
			evidence.Result = "passed"
			evidence.BoundaryOK = true
			evidence.TestOK = true
			evidence.CoverageOK = true
			coverage := 100.0
			evidence.Coverage = &coverage
			require.NoError(t, sealValidationEvidence(testProjectID, fixture.taskID, &evidence))
			_, err := fixture.services.stores.db.Exec(stage.trigger)
			require.NoError(t, err)

			err = fixture.services.validSvc.persistSuccessfulValidation(
				context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil, &evidence,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_VALIDATION_SUCCESS_STAGE")
			assertValidationAuthorityUnchanged(t, fixture)
			assertValidationRunCount(t, fixture, 0)
			assertValidationHistoryCount(t, fixture, 0)
		})
	}
}

func TestExtendValidationLeaseRejectsStaleAuthority(t *testing.T) {
	t.Run("invalid evidence", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", nil, time.Minute,
		)
		require.ErrorIs(t, err, store.ErrInvalidParameter)
	})

	t.Run("missing task", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, "missing-task", fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "extend validation lease task")
	})

	t.Run("task version changed", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		evidence.TaskVersion++
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.ErrorIs(t, err, store.ErrConcurrentConflict)
	})

	t.Run("lease expired", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		expired := "2000-01-01 00:00:00"
		_, err := fixture.services.stores.db.Exec(`UPDATE task_leases SET expires_at = ? WHERE project_id = ? AND task_id = ?`,
			expired, testProjectID, fixture.taskID)
		require.NoError(t, err)
		_, err = fixture.services.stores.db.Exec(`UPDATE tasks SET lease_expires_at = ? WHERE project_id = ? AND id = ?`,
			expired, testProjectID, fixture.taskID)
		require.NoError(t, err)
		evidence.LeaseExpiresAt = expired
		_, _, _, err = fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.ErrorIs(t, err, store.ErrLeaseExpired)
	})

	t.Run("lease version changed", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		evidence.LeaseVersion++
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.ErrorIs(t, err, store.ErrLeaseVersionMismatch)
	})

	t.Run("worker reservation missing", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		_, err := fixture.services.stores.db.Exec(`UPDATE agent_workers SET status = 'lost', current_task_id = NULL
			WHERE project_id = ? AND id = ?`, testProjectID, "worker-1")
		require.NoError(t, err)
		_, _, _, err = fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "extend validation lease worker")
	})

	t.Run("worker version changed", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		evidence.WorkerVersion++
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.ErrorIs(t, err, store.ErrConcurrentConflict)
	})

	t.Run("database closed", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		require.NoError(t, fixture.services.stores.db.Close())
		_, _, _, err := fixture.services.validSvc.extendValidationLease(
			context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "extend validation lease begin")
	})
}

func TestExtendValidationLeaseRollsBackEveryWriteStage(t *testing.T) {
	stages := []struct {
		name    string
		trigger string
	}{
		{"lease", `CREATE TRIGGER fail_validation_extend BEFORE UPDATE ON task_leases
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_EXTEND_STAGE'); END`},
		{"task", `CREATE TRIGGER fail_validation_extend BEFORE UPDATE ON tasks
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_EXTEND_STAGE'); END`},
		{"worker", `CREATE TRIGGER fail_validation_extend BEFORE UPDATE ON agent_workers
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_EXTEND_STAGE'); END`},
		{"session", `CREATE TRIGGER fail_validation_extend BEFORE UPDATE ON agent_sessions
			BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_EXTEND_STAGE'); END`},
		{"audit", `CREATE TRIGGER fail_validation_extend BEFORE INSERT ON audit_log
			WHEN NEW.action = 'validation.lease_extend' BEGIN SELECT RAISE(ABORT, 'FAIL_VALIDATION_EXTEND_STAGE'); END`},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			fixture := newValidationFixture(t, "coverage", `["src"]`)
			evidence := validationEvidenceSnapshot(t, fixture)
			before := validationAuthoritySnapshotFromDB(t, fixture)
			_, err := fixture.services.stores.db.Exec(stage.trigger)
			require.NoError(t, err)

			_, _, _, err = fixture.services.validSvc.extendValidationLease(
				context.Background(), testProjectID, fixture.taskID, fixture.sessionID, "worker-1", &evidence, time.Minute,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "FAIL_VALIDATION_EXTEND_STAGE")
			assert.Equal(t, before, validationAuthoritySnapshotFromDB(t, fixture))
		})
	}
}

func TestValidationEvidencePersistenceAndSnapshotHelpersFailClosed(t *testing.T) {
	t.Run("nil evidence cannot be sealed", func(t *testing.T) {
		require.Error(t, sealValidationEvidence(testProjectID, "task", nil))
	})

	t.Run("local insert rejects forged authority", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		tx, err := fixture.services.stores.db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		evidence.Authority = model.EvidenceAuthorityMergeGate
		_, err = insertValidationRun(context.Background(), tx, testProjectID, fixture.taskID, nil, &evidence)
		require.ErrorIs(t, err, store.ErrInvalidParameter)
	})

	t.Run("attempt query failure is surfaced", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		evidence := validationEvidenceSnapshot(t, fixture)
		_, err := fixture.services.stores.db.Exec(`DROP TABLE validation_runs`)
		require.NoError(t, err)
		tx, err := fixture.services.stores.db.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = insertValidationRun(context.Background(), tx, testProjectID, fixture.taskID, nil, &evidence)
		require.Error(t, err)
	})

	t.Run("history driver failure is wrapped", func(t *testing.T) {
		fixture := newValidationFixture(t, "coverage", `["src"]`)
		require.NoError(t, fixture.services.stores.db.Close())
		_, err := fixture.services.validSvc.GetValidationHistory(context.Background(), testProjectID, fixture.taskID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get validation history")
	})

	t.Run("snapshot rejects escape directory and oversized file", func(t *testing.T) {
		root := t.TempDir()
		_, err := digestWorkspaceSnapshot(root, []string{"../outside"})
		require.Error(t, err)

		big := filepath.Join(root, "large.bin")
		file, err := os.Create(big)
		require.NoError(t, err)
		require.NoError(t, file.Truncate(maxValidationFileBytes+1))
		require.NoError(t, file.Close())
		_, err = digestWorkspaceSnapshot(root, []string{"large.bin"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bounded regular file")
	})

	t.Run("snapshot records deleted files and rejects directories", func(t *testing.T) {
		root := t.TempDir()
		digest, err := digestWorkspaceSnapshot(root, []string{"deleted.go"})
		require.NoError(t, err)
		assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)

		require.NoError(t, os.Mkdir(filepath.Join(root, "directory"), 0o700))
		_, err = digestWorkspaceSnapshot(root, []string{"directory"})
		require.Error(t, err)
	})
}

type validationAuthoritySnapshot struct {
	TaskStatus      string
	TaskVersion     int64
	TaskLeaseID     sql.NullString
	TaskLeaseExpiry sql.NullString
	LeaseStatus     string
	LeaseVersion    int64
	LeaseExpiry     string
	WorkerStatus    string
	WorkerVersion   int64
	WorkerTaskID    sql.NullString
	WorktreeStatus  string
	WorktreeVersion int64
}

func validationEvidenceSnapshot(t *testing.T, fixture *validationFixture) validationEvidence {
	t.Helper()
	ctx := context.Background()
	task, err := fixture.services.stores.taskStore.GetByID(ctx, testProjectID, fixture.taskID)
	require.NoError(t, err)
	physicalSessionID, leaseVersion, workerVersion, leaseExpiresAt, err := fixture.services.validSvc.loadActiveValidationAuthority(
		ctx, testProjectID, fixture.taskID, fixture.sessionID, "worker-1", task,
	)
	require.NoError(t, err)
	worktree, err := fixture.services.stores.worktreeStore.GetByTaskID(ctx, testProjectID, fixture.taskID)
	require.NoError(t, err)
	project, err := fixture.services.stores.projectStore.GetByID(ctx, testProjectID)
	require.NoError(t, err)
	policy, err := resolveValidationPolicy(task, project, fixture.services.validSvc.testExecConfig)
	require.NoError(t, err)
	policyJSON, err := json.Marshal(policy)
	require.NoError(t, err)

	evidence := validationEvidence{
		Authority:          model.EvidenceAuthorityDiagnostic,
		Producer:           model.EvidenceProducerMaestroLocal,
		BaseCommit:         worktree.BaseCommit,
		SourceCommit:       worktree.BaseCommit,
		ChangedFiles:       `[]`,
		ProfileRef:         validationProfileReference(policy),
		PolicyVersion:      fixture.services.validSvc.testExecConfig.PolicyVersion,
		PolicyDigest:       fixture.services.validSvc.testExecConfig.PolicyDigest,
		Result:             "error",
		TaskVersion:        task.Version,
		LeaseID:            *task.ActiveLeaseID,
		LeaseEpoch:         task.LeaseEpoch,
		LeaseVersion:       leaseVersion,
		LeaseExpiresAt:     leaseExpiresAt,
		PhysicalSessionID:  physicalSessionID,
		WorkerVersion:      workerVersion,
		WorktreeID:         worktree.ID,
		WorktreeGeneration: worktree.Generation,
		WorktreeVersion:    worktree.Version,
		WorktreePath:       worktree.WorktreePath,
		WorkspacePath:      project.WorkspacePath,
		ResolvedPolicyJSON: string(policyJSON),
		WorkspaceDigest:    "sha256:" + strings.Repeat("a", 64),
	}
	require.NoError(t, sealValidationEvidence(testProjectID, fixture.taskID, &evidence))
	return evidence
}

func validationAuthoritySnapshotFromDB(t *testing.T, fixture *validationFixture) validationAuthoritySnapshot {
	t.Helper()
	ctx := context.Background()
	var snapshot validationAuthoritySnapshot
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT
		t.status, t.version, t.active_lease_id, t.lease_expires_at,
		l.status, l.version, l.expires_at,
		w.status, w.version, w.current_task_id,
		wt.status, wt.version
		FROM tasks AS t
		JOIN task_leases AS l ON l.project_id = t.project_id AND l.task_id = t.id
		JOIN agent_workers AS w ON w.project_id = t.project_id AND w.id = t.assigned_worker_id
		JOIN worktrees AS wt ON wt.project_id = t.project_id AND wt.task_id = t.id
		WHERE t.project_id = ? AND t.id = ?`, testProjectID, fixture.taskID).Scan(
		&snapshot.TaskStatus, &snapshot.TaskVersion, &snapshot.TaskLeaseID, &snapshot.TaskLeaseExpiry,
		&snapshot.LeaseStatus, &snapshot.LeaseVersion, &snapshot.LeaseExpiry,
		&snapshot.WorkerStatus, &snapshot.WorkerVersion, &snapshot.WorkerTaskID,
		&snapshot.WorktreeStatus, &snapshot.WorktreeVersion,
	))
	return snapshot
}

func assertValidationAuthorityUnchanged(t *testing.T, fixture *validationFixture) {
	t.Helper()
	snapshot := validationAuthoritySnapshotFromDB(t, fixture)
	assert.Equal(t, model.TaskStatusExecuting, snapshot.TaskStatus)
	assert.True(t, snapshot.TaskLeaseID.Valid)
	assert.True(t, snapshot.TaskLeaseExpiry.Valid)
	assert.Equal(t, model.LeaseStatusActive, snapshot.LeaseStatus)
	assert.Equal(t, model.WorkerStatusBusy, snapshot.WorkerStatus)
	assert.True(t, snapshot.WorkerTaskID.Valid)
	assert.Equal(t, fixture.taskID, snapshot.WorkerTaskID.String)
	assert.Equal(t, model.WorktreeStatusActive, snapshot.WorktreeStatus)
}

func assertValidationFailureReleasedAuthority(t *testing.T, fixture *validationFixture, wantTaskStatus string) {
	t.Helper()
	ctx := context.Background()
	var taskStatus, leaseStatus, workerStatus, worktreeStatus string
	var activeLeaseID sql.NullString
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT status, active_lease_id
		FROM tasks WHERE project_id = ? AND id = ?`, testProjectID, fixture.taskID).Scan(&taskStatus, &activeLeaseID))
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&leaseStatus))
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT status FROM agent_workers
		WHERE project_id = ? AND id = ?`, testProjectID, "worker-1").Scan(&workerStatus))
	require.NoError(t, fixture.services.stores.db.QueryRowContext(ctx, `SELECT status FROM worktrees
		WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&worktreeStatus))
	assert.Equal(t, wantTaskStatus, taskStatus)
	assert.False(t, activeLeaseID.Valid)
	assert.Equal(t, model.LeaseStatusReleased, leaseStatus)
	assert.Equal(t, model.WorkerStatusIdle, workerStatus)
	assert.Equal(t, model.WorktreeStatusQuarantined, worktreeStatus)
}

func assertValidationFailureCode(t *testing.T, err error, code string) {
	t.Helper()
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, code, validationErr.Code)
}

func assertValidationRunCount(t *testing.T, fixture *validationFixture, want int) {
	t.Helper()
	var count int
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT COUNT(*) FROM validation_runs
		WHERE project_id = ? AND task_id = ?`, testProjectID, fixture.taskID).Scan(&count))
	assert.Equal(t, want, count)
}

func assertValidationHistoryCount(t *testing.T, fixture *validationFixture, want int) {
	t.Helper()
	var count int
	require.NoError(t, fixture.services.stores.db.QueryRow(`SELECT COUNT(*) FROM state_history
		WHERE project_id = ? AND aggregate_id = ?`, testProjectID, fixture.taskID).Scan(&count))
	assert.Equal(t, want, count)
}

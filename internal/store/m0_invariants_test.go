package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

func TestScopedSessionIdentityRoundTripAndCrossProjectForgery(t *testing.T) {
	db, taskStore := setupTestDB(t)
	seedProjectAndFeature(t, db)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	const projectB, featureB = "proj-test-002", "feat-test-002"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects(id, name, workspace_path, status, config, created_at, updated_at)
		VALUES (?, 'Project B', ?, 'active', '{}', ?, ?)`,
		projectB, t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO features(id, project_id, title, description, status, created_at, updated_at)
		VALUES (?, ?, 'Feature B', 'Feature B', 'active', ?, ?)`,
		featureB, projectB, now, now,
	); err != nil {
		t.Fatalf("seed second feature: %v", err)
	}

	sessions := NewSessionStore(db)
	for _, projectID := range []string{testProjectID, projectB} {
		session := &model.AgentSession{
			ID: "same-client-session", ProjectID: projectID, Role: model.RoleBackend,
			ClientType: "test", Capacity: 1, Status: model.SessionStatusOnline,
			LastHeartbeat: now, CreatedAt: now,
		}
		if err := sessions.Create(ctx, projectID, session); err != nil {
			t.Fatalf("create scoped session %s: %v", projectID, err)
		}
	}
	var physicalA, physicalB string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE project_id = ?`, testProjectID).Scan(&physicalA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT id FROM agent_sessions WHERE project_id = ?`, projectB).Scan(&physicalB); err != nil {
		t.Fatal(err)
	}
	if physicalA == physicalB || physicalA == "same-client-session" || physicalB == "same-client-session" {
		t.Fatalf("physical keys must be project-scoped and opaque: A=%q B=%q", physicalA, physicalB)
	}
	for _, projectID := range []string{testProjectID, projectB} {
		got, err := sessions.GetByID(ctx, projectID, "same-client-session")
		if err != nil || got.ID != "same-client-session" {
			t.Fatalf("logical session round trip %s: got=%+v err=%v", projectID, got, err)
		}
	}

	taskA := newTestTask("T-scope-A", testProjectID, testFeatureID)
	logicalID := "same-client-session"
	taskA.AssignedSessionID = &logicalID
	if err := taskStore.Create(ctx, testProjectID, taskA); err != nil {
		t.Fatalf("create task with logical session: %v", err)
	}
	got, err := taskStore.GetByID(ctx, testProjectID, taskA.ID)
	if err != nil || got.AssignedSessionID == nil || *got.AssignedSessionID != logicalID {
		t.Fatalf("task leaked physical session: task=%+v err=%v", got, err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET assigned_session_id = ? WHERE project_id = ? AND id = ?`,
		physicalB, testProjectID, taskA.ID,
	); err == nil || !strings.Contains(err.Error(), "PROJECT_SCOPE_SESSION") {
		t.Fatalf("cross-project forged task session must fail with scope trigger, got %v", err)
	}

	workers := NewWorkerStore(db)
	worker := &model.AgentWorker{ID: "worker-1", Status: model.WorkerStatusIdle, LastActive: now}
	if err := workers.Create(ctx, testProjectID, logicalID, worker); err != nil {
		t.Fatalf("create worker with logical session: %v", err)
	}
	workerGot, err := workers.GetByID(ctx, testProjectID, logicalID, worker.ID)
	if err != nil || workerGot.SessionID != logicalID {
		t.Fatalf("worker leaked physical session: worker=%+v err=%v", workerGot, err)
	}
}

func TestSecurityHistoriesAreDatabaseAppendOnly(t *testing.T) {
	db, taskStore := setupTestDB(t)
	seedProjectAndFeature(t, db)
	ctx := context.Background()
	task := newTestTask("T-append-only", testProjectID, testFeatureID)
	mustCreateTask(t, taskStore, task)

	result, err := db.ExecContext(ctx, `INSERT INTO validation_runs(
		task_id, project_id, attempt, base_commit, changed_files, test_command,
		boundary_ok, test_ok, coverage_ok, result, duration_ms, created_at,
		source_commit, policy_version, policy_digest, profile_ref, evidence_digest, workspace_digest)
		VALUES (?, ?, 1, ?, '[]', 'profile@1', 1, 1, 1, 'passed', 1, datetime('now'),
		        ?, '3.0.0', ?, 'profile@1', ?, ?)`,
		task.ID, testProjectID, strings.Repeat("a", 40), strings.Repeat("b", 40),
		"sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64),
		"sha256:"+strings.Repeat("e", 64),
	)
	if err != nil {
		t.Fatalf("seed validation evidence: %v", err)
	}
	validationID, _ := result.LastInsertId()
	if _, err := db.ExecContext(ctx, `UPDATE validation_runs SET result = 'failed' WHERE id = ?`, validationID); err == nil || !strings.Contains(err.Error(), "APPEND_ONLY_VALIDATION_EVIDENCE") {
		t.Fatalf("validation evidence update must fail, got %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM validation_runs WHERE id = ?`, validationID); err == nil || !strings.Contains(err.Error(), "APPEND_ONLY_VALIDATION_EVIDENCE") {
		t.Fatalf("validation evidence delete must fail, got %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO state_history(
		project_id, aggregate_type, aggregate_id, from_status, to_status,
		from_version, to_version, reason) VALUES (?, 'task', ?, 'queued', 'leased', 0, 1, 'test')`,
		testProjectID, task.ID,
	); err != nil {
		t.Fatal(err)
	}
	assertAppendOnlyFailure(t, db, "state_history", "APPEND_ONLY_STATE_HISTORY")

	if _, err := db.ExecContext(ctx, `INSERT INTO audit_log(
		bound_project, action, result, created_at) VALUES (?, 'test', 'ALLOWED', datetime('now'))`,
		testProjectID,
	); err != nil {
		t.Fatal(err)
	}
	assertAppendOnlyFailure(t, db, "audit_log", "APPEND_ONLY_AUDIT_LOG")
}

func TestTaskTransitionTriggersRejectIllegalOrUnversionedWrites(t *testing.T) {
	db, taskStore := setupTestDB(t)
	seedProjectAndFeature(t, db)
	ctx := context.Background()
	task := newTestTask("T-db-transition", testProjectID, testFeatureID)
	mustCreateTask(t, taskStore, task)

	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = 'done', version = version + 1,
		 merged_fact_id = 'gitlab:event:1', merge_commit = ?
		 WHERE project_id = ? AND id = ?`,
		strings.Repeat("a", 40), testProjectID, task.ID,
	); err == nil || !strings.Contains(err.Error(), "ILLEGAL_TASK_TRANSITION") {
		t.Fatalf("queued -> done must be rejected at the database boundary, got %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = 'leased' WHERE project_id = ? AND id = ?`,
		testProjectID, task.ID,
	); err == nil || !strings.Contains(err.Error(), "TASK_VERSION_NOT_INCREMENTED") {
		t.Fatalf("unversioned status update must be rejected, got %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = 'leased', version = version + 1 WHERE project_id = ? AND id = ?`,
		testProjectID, task.ID,
	); err != nil {
		t.Fatalf("canonical versioned queued -> leased should succeed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tasks SET status = 'executing', version = version + 2 WHERE project_id = ? AND id = ?`,
		testProjectID, task.ID,
	); err == nil || !strings.Contains(err.Error(), "TASK_VERSION_NOT_INCREMENTED") {
		t.Fatalf("status update must advance version exactly once, got %v", err)
	}
}

func assertAppendOnlyFailure(t *testing.T, db *sql.DB, table, marker string) {
	t.Helper()
	var updateStatement, deleteStatement string
	switch table {
	case "state_history":
		updateStatement = `UPDATE state_history SET created_at = created_at`
		deleteStatement = `DELETE FROM state_history`
	case "audit_log":
		updateStatement = `UPDATE audit_log SET created_at = created_at`
		deleteStatement = `DELETE FROM audit_log`
	default:
		t.Fatalf("append-only assertion does not allow table %q", table)
	}
	if _, err := db.Exec(updateStatement); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("%s update must fail with %s, got %v", table, marker, err)
	}
	if _, err := db.Exec(deleteStatement); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("%s delete must fail with %s, got %v", table, marker, err)
	}
}

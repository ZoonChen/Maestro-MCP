package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

func TestValidationRunStoreCreatesOnlyLocalDiagnosticEvidence(t *testing.T) {
	db, taskStore := setupTestDB(t)
	seedProjectAndFeature(t, db)
	task := newTestTask("T-evidence-authority", testProjectID, testFeatureID)
	mustCreateTask(t, taskStore, task)
	store := NewSQLiteValidationRunStore(db)

	run := localValidationRunFixture(task.ID)
	id, err := store.Create(context.Background(), testProjectID, run)
	if err != nil {
		t.Fatalf("create local diagnostic: %v", err)
	}
	if id == 0 {
		t.Fatal("expected persisted validation id")
	}
	if run.Authority != model.EvidenceAuthorityDiagnostic || run.Producer != model.EvidenceProducerMaestroLocal {
		t.Fatalf("store did not assign local identity: authority=%q producer=%q", run.Authority, run.Producer)
	}
	latest, err := store.LatestByTask(context.Background(), testProjectID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Authority != model.EvidenceAuthorityDiagnostic || latest.Producer != model.EvidenceProducerMaestroLocal {
		t.Fatalf("local identity did not round trip: %+v", latest)
	}
	if latest.PipelineID != nil || latest.JobID != nil {
		t.Fatalf("local evidence must not carry CI coordinates: %+v", latest)
	}

	forged := localValidationRunFixture(task.ID)
	forged.Attempt = 2
	forged.Authority = model.EvidenceAuthorityMergeGate
	forged.Producer = "gitlab-ci"
	if _, err := store.Create(context.Background(), testProjectID, forged); !errors.Is(err, ErrInvalidParameter) {
		t.Fatalf("public M0 Store must reject forged merge authority, got %v", err)
	}
	latest, err = store.LatestByTask(context.Background(), testProjectID, task.ID)
	if err != nil || latest.Attempt != 1 {
		t.Fatalf("forged append changed history: latest=%+v err=%v", latest, err)
	}
}

func TestValidationRunAuthorityDatabaseConstraintsAndOptionalCIIDs(t *testing.T) {
	db, taskStore := setupTestDB(t)
	seedProjectAndFeature(t, db)
	task := newTestTask("T-evidence-db-authority", testProjectID, testFeatureID)
	mustCreateTask(t, taskStore, task)
	store := NewSQLiteValidationRunStore(db)
	if _, err := store.Create(context.Background(), testProjectID, localValidationRunFixture(task.ID)); err != nil {
		t.Fatal(err)
	}

	copyLatest := func(authority, producer string) error {
		_, err := db.Exec(`INSERT INTO validation_runs (
			task_id, project_id, attempt, base_commit, source_commit, changed_files,
			test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
			authority, producer, pipeline_id, job_id,
			test_exit_code, test_output, output_truncated, coverage,
			boundary_ok, test_ok, coverage_ok, summary, result, error_code,
			duration_ms, log_path, created_at
		)
		SELECT task_id, project_id, attempt + 1, base_commit, source_commit, changed_files,
			test_command, profile_ref, policy_version, policy_digest, ?, workspace_digest,
			?, ?, NULL, NULL,
			test_exit_code, test_output, output_truncated, coverage,
			boundary_ok, test_ok, coverage_ok, summary, result, error_code,
			duration_ms, log_path, datetime('now')
		FROM validation_runs WHERE project_id = ? AND task_id = ?
		ORDER BY attempt DESC, id DESC LIMIT 1`,
			"sha256:"+strings.Repeat("f", 64), authority, producer, testProjectID, task.ID)
		return err
	}
	if err := copyLatest(model.EvidenceAuthorityDiagnostic, "forged-producer"); err == nil ||
		!strings.Contains(err.Error(), "INVALID_EVIDENCE_AUTHORITY") {
		t.Fatalf("diagnostic producer forgery must fail, got %v", err)
	}
	if err := copyLatest(model.EvidenceAuthorityMergeGate, model.EvidenceProducerMaestroLocal); err == nil ||
		!strings.Contains(err.Error(), "INVALID_EVIDENCE_AUTHORITY") {
		t.Fatalf("local producer must not claim merge authority, got %v", err)
	}
	if err := copyLatest(model.EvidenceAuthorityMergeGate, "gitlab-ci:test"); err != nil {
		t.Fatalf("trusted fixture with nullable pipeline/job should be accepted: %v", err)
	}
	latest, err := store.LatestByTask(context.Background(), testProjectID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Authority != model.EvidenceAuthorityMergeGate || latest.Producer != "gitlab-ci:test" {
		t.Fatalf("merge authority did not round trip: %+v", latest)
	}
	if latest.PipelineID != nil || latest.JobID != nil {
		t.Fatalf("pipeline/job are optional in M0 fixture: %+v", latest)
	}
	if _, err := db.Exec(`UPDATE validation_runs SET authority = 'diagnostic' WHERE id = ?`, latest.ID); err == nil ||
		!strings.Contains(err.Error(), "APPEND_ONLY_VALIDATION_EVIDENCE") {
		t.Fatalf("authority must be immutable, got %v", err)
	}
	if _, err := db.Exec(`UPDATE validation_runs SET producer = 'other' WHERE id = ?`, latest.ID); err == nil ||
		!strings.Contains(err.Error(), "APPEND_ONLY_VALIDATION_EVIDENCE") {
		t.Fatalf("producer must be immutable, got %v", err)
	}
}

func TestSchemaV4MigratesExistingEvidenceAsDiagnostic(t *testing.T) {
	database, err := NewSQLiteDB(t.TempDir() + "/evidence-v3.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	for _, migration := range migrations {
		if migration.version > 3 {
			break
		}
		if _, err := database.db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply legacy migration v%d: %v", migration.version, err)
		}
	}
	if _, err := database.db.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	seedProjectAndFeature(t, database.db)
	taskStore := NewSQLiteTaskStore(database.db)
	task := newTestTask("T-evidence-v3", testProjectID, testFeatureID)
	mustCreateTask(t, taskStore, task)
	if _, err := database.db.ExecContext(ctx, `INSERT INTO validation_runs (
		task_id, project_id, attempt, base_commit, source_commit, changed_files,
		test_command, profile_ref, policy_version, policy_digest, evidence_digest, workspace_digest,
		boundary_ok, test_ok, coverage_ok, result, duration_ms, created_at
	) VALUES (?, ?, 1, ?, ?, '[]', 'profile@1', 'profile@1', '3.0.0', ?, ?, ?, 1, 1, 1, 'passed', 1, datetime('now'))`,
		task.ID, testProjectID, strings.Repeat("a", 40), strings.Repeat("b", 40),
		"sha256:"+strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64),
		"sha256:"+strings.Repeat("e", 64)); err != nil {
		t.Fatalf("seed v3 evidence: %v", err)
	}
	if err := database.Init(ctx); err != nil {
		t.Fatalf("migrate v3 evidence to current schema: %v", err)
	}
	var version int
	if err := database.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() {
		t.Fatalf("schema version=%d, want %d", version, CurrentSchemaVersion())
	}
	run, err := NewSQLiteValidationRunStore(database.db).LatestByTask(ctx, testProjectID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Authority != model.EvidenceAuthorityDiagnostic || run.Producer != model.EvidenceProducerMaestroLocal {
		t.Fatalf("legacy evidence gained authority during migration: %+v", run)
	}
	if run.PipelineID != nil || run.JobID != nil {
		t.Fatalf("legacy evidence gained external CI identity: %+v", run)
	}
}

func localValidationRunFixture(taskID string) *model.ValidationRun {
	exitCode := 0
	return &model.ValidationRun{
		TaskID: taskID, Attempt: 1,
		BaseCommit: strings.Repeat("a", 40), SourceCommit: strings.Repeat("b", 40), ChangedFiles: "[]",
		TestCommand: "profile@1", ProfileRef: "profile@1", PolicyVersion: "3.0.0",
		PolicyDigest:   "sha256:" + strings.Repeat("c", 64),
		EvidenceDigest: "sha256:" + strings.Repeat("d", 64), WorkspaceDigest: "sha256:" + strings.Repeat("e", 64),
		TestExitCode: &exitCode, BoundaryOK: true, TestOK: true, CoverageOK: true,
		Result: "passed", DurationMs: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

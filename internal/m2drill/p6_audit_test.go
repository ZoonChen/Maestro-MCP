package m2drill

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/ZoonChen/Maestro-MCP/internal/gitlab"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P6 V2 convergence audits (m2 plan P6): the three stage-specific
// audits as EXECUTABLE evidence — SQL invariants over a populated
// scenario plus behavioral re-assertions. Each test names the audit
// line it proves; the audit record (plans/prep/m2/p6-convergence-audit.md)
// maps every line to this suite and the live drills.

const (
	auditProject = drillProject // the fixture's seeded project
	auditWork    = "018f7a00-0000-7000-8000-000000000003"
	auditBranch  = "maestro/audit/" + auditWork
	auditSource  = "1111111111111111111111111111111111111111"
	auditTarget  = "2222222222222222222222222222222222222222"
)

// newAuditFixture builds a populated scenario: verified evidence on a
// complete tuple, an approved waiver with separation of duties, and a
// diagnostic record that must never satisfy its gate.
func newAuditFixture(t *testing.T) *drillFixture {
	t.Helper()
	fixture := newDrillFixture(t)
	ctx := context.Background()

	// The audit's own work item (the fixture's drill item stays separate).
	_, err := fixture.db.ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'audit work item', 'validating', 1)`, auditWork, auditProject)
	require.NoError(t, err)

	// Complete tuple (MR projection) so evidence can bind.
	_, err = fixture.db.ExecContext(ctx, `
		INSERT INTO merge_requests (id, project_id, gitlab_instance_id, gitlab_project_id, mr_iid,
			work_item_id, state, source_branch, target_branch, source_sha, target_sha)
		VALUES ($1, $2, $3, 9500, 60, $4, 'opened', $5, 'main', $6, $7)`,
		"018f7a00-0000-7000-8000-0000000000aa", auditProject, drillInstance, auditWork,
		auditBranch, auditSource, auditTarget)
	require.NoError(t, err)

	// A pipeline projection for the jobs.
	_, err = fixture.db.ExecContext(ctx, `
		INSERT INTO pipelines (id, project_id, gitlab_instance_id, gitlab_project_id, gitlab_pipeline_id, sha, ref, status)
		VALUES ($1, $2, $3, 9500, 700, $4, $5, 'success')`,
		"018f7a00-0000-7000-8000-0000000000bb", auditProject, drillInstance, auditSource, auditBranch)
	require.NoError(t, err)

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	eval := &evidence.Service{Company: company, Store: fixture.pg.Quality()}
	ingest := &gitlab.EvidenceIngestor{
		Eval:          eval,
		PolicyVersion: company.Version,
		Append:        fixture.pg.Quality(),
		Tuples:        fixture.pg.GitLab(),
	}

	// Verified merge_gate evidence for unit, plus a diagnostic record
	// for sast that must never satisfy its gate.
	applied, err := ingest.IngestJob(ctx, auditProject, gitlab.JobRecord{
		InstanceID: drillInstance, GitlabProject: 9500, PipelineID: 700, JobID: 7001,
		Name: "unit", Status: "success", Ref: auditBranch,
	}, auditSource)
	require.NoError(t, err)
	require.True(t, applied, "the verified unit job must produce evidence")
	require.NoError(t, fixture.pg.Quality().AppendEvidence(ctx, &evidence.Record{
		EvidenceID: "018f7a00-0000-7000-8000-0000000000cc", ProjectID: auditProject,
		WorkItemID: auditWork, Kind: evidence.GateSAST, Authority: evidence.AuthorityDiagnostic,
		Status: evidence.EvidencePassed, SourceSHA: auditSource, TargetSHA: auditTarget,
		PolicyVersion: company.Version, Attempt: 1,
		Producer: evidence.Producer{Type: "runner_profile", ID: "local", Version: "1"},
	}))

	// An approved waiver with separation of duties and a bounded life.
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)
	// Engine-level waiver construction MUST mint the gate identity from
	// the VERSIONED tuple (the HTTP path binds the id from the gate row
	// directly) — otherwise the waiver floats free of every snapshot.
	tuple := evidence.Tuple{ProjectID: auditProject, WorkItemID: auditWork,
		SourceSHA: auditSource, TargetSHA: auditTarget, PolicyVersion: resolved.Policy.Version}
	waiver, err := evidence.NewWaiver(resolved, evidence.WaiverRequestInput{
		GateID: evidence.StableGateID(tuple, evidence.GateLintTypecheck),
		Check:  evidence.GateLintTypecheck, SourceSHA: auditSource,
		MergeRequestIID: 60, Requester: "audit-requester",
		Reason:    "audit fixture waiver, ticket-000",
		ExpiresAt: time.Now().Add(48 * time.Hour),
	}, time.Now())
	require.NoError(t, err)
	_, err = fixture.pg.Quality().CreateWaiver(ctx, waiver, auditProject, auditWork)
	require.NoError(t, err)
	waivers, err := fixture.pg.Quality().ListWaiversForWorkItem(ctx, auditProject, auditWork)
	require.NoError(t, err)
	require.Len(t, waivers, 1)
	require.NoError(t, fixture.pg.Quality().ApproveWaiver(ctx, waivers[0].ID, "audit-approver"))

	// Evaluate so gate snapshots exist.
	_, err = eval.EvaluateWorkItem(ctx, tuple)
	require.NoError(t, err)
	return fixture
}

// TestAuditEvidenceAuthority — 审计一：diagnostic 与 merge_gate 无混淆。
func TestAuditEvidenceAuthority(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// SQL invariant 1: every merge_gate row carries the provider ids and
	// a gitlab_job producer (a diagnostic row can never masquerade).
	var confused int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM evidence
		WHERE authority = 'merge_gate'
		  AND (gitlab_pipeline_id IS NULL OR gitlab_job_id IS NULL
		       OR producer::jsonb->>'type' <> 'gitlab_job'
		       OR source_sha = '' OR target_sha = '')`).Scan(&confused))
	assert.Zero(t, confused, "merge_gate rows must carry provider lineage")

	// SQL invariant 2: diagnostic rows never carry provider ids.
	var leak int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM evidence
		WHERE authority = 'diagnostic'
		  AND (gitlab_pipeline_id IS NOT NULL OR gitlab_job_id IS NOT NULL)`).Scan(&leak))
	assert.Zero(t, leak, "diagnostic rows carry no provider lineage")

	// Behavioral: the diagnostic PASS on sast leaves the gate pending.
	snapshots, err := f.pg.Quality().ListGateSnapshots(ctx, auditProject, auditWork)
	require.NoError(t, err)
	for _, snapshot := range snapshots {
		if snapshot.Check == evidence.GateSAST && snapshot.SourceSHA == auditSource {
			assert.Equal(t, "pending", snapshot.Status,
				"diagnostic evidence never satisfies a required gate")
		}
		if snapshot.Check == evidence.GateUnit && snapshot.SourceSHA == auditSource {
			assert.Equal(t, "passed", snapshot.Status, "verified evidence passes")
		}
	}
}

// TestAuditWaiverProcess — 审计二：审批人隔离 / 期限 / SHA 绑定。
func TestAuditWaiverProcess(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// SQL invariant 1: separation of duties on every decided waiver.
	var selfApproved int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM waivers
		WHERE state IN ('approved', 'active')
		  AND (approver_principal IS NULL OR approver_principal = requester_principal)`).Scan(&selfApproved))
	assert.Zero(t, selfApproved, "no approved waiver lacks a distinct approver")

	// SQL invariant 2: bounded lifetime measured from approval.
	var overlong int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM waivers
		WHERE state IN ('approved', 'active')
		  AND expires_at > approved_at + interval '7 days'`).Scan(&overlong))
	assert.Zero(t, overlong, "no waiver outlives the seven-day ceiling")

	// SQL invariant 3: every waiver binds a real gate snapshot row.
	var unbound int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM waivers w
		WHERE NOT EXISTS (
			SELECT 1 FROM gate_snapshots g
			WHERE g.project_id = w.project_id AND g.id::text = w.gate_id
			  AND g.source_sha = w.source_sha)`).Scan(&unbound))
	assert.Zero(t, unbound, "every waiver binds its gate's exact SHA")

	// Behavioral: the store refuses self-approval outright.
	waivers, err := f.pg.Quality().ListWaiversForWorkItem(ctx, auditProject, auditWork)
	require.NoError(t, err)
	require.Len(t, waivers, 1)
	err = f.pg.Quality().ApproveWaiver(ctx, waivers[0].ID, waivers[0].Requester)
	assert.ErrorIs(t, err, store.ErrWaiverSelfApprove, "self-approval is structurally refused")

	// Behavioral: the approved waiver actually waives its gate.
	approved, err := f.pg.Quality().ListWaiversForWorkItem(ctx, auditProject, auditWork)
	require.NoError(t, err)
	assert.Equal(t, "approved", approved[0].State)
	snapshots, err := f.pg.Quality().ListGateSnapshots(ctx, auditProject, auditWork)
	require.NoError(t, err)
	for _, snapshot := range snapshots {
		if snapshot.Check == evidence.GateLintTypecheck && snapshot.SourceSHA == auditSource {
			assert.Equal(t, "waived", snapshot.Status, "the validly approved waiver applies")
		}
	}
}

// TestAuditWebhookSecretManagement — 审计三：Secret 只以引用存在、
// 永不外泄到投影或日志路径。
func TestAuditWebhookSecretManagement(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	// SQL invariant 1: every secret field is an env-scoped reference.
	var badRefs int
	require.NoError(t, f.db.QueryRowContext(ctx, `
		SELECT count(*) FROM gitlab_instances
		WHERE webhook_secret_ref !~ '^env:MAESTRO_[A-Z0-9_]+$'
		   OR bot_credential_ref !~ '^env:MAESTRO_[A-Z0-9_]+$'`).Scan(&badRefs))
	assert.Zero(t, badRefs, "secrets exist only as env-scoped references")

	// Behavioral: the sanitized registry view carries no secret fields.
	instances, err := f.pg.Instances().ListInstances(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, instances)
	for _, view := range instances {
		assert.NotContains(t, fmt.Sprintf("%v", view), "secret_ref", "sanitized views leak nothing")
	}

	// Behavioral: a wrong token is denied without any business row and
	// without echoing the presented credential.
	router := gin.New()
	router.Use(func(c *gin.Context) { gin.New().ServeHTTP(c.Writer, c.Request) })
	request := httptest.NewRequest(http.MethodPost,
		"/api/v3/webhooks/gitlab/"+drillInstance, strings.NewReader(`{"project_id":9500}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Gitlab-Token", "audit-candidate-secret")
	request.Header.Set("X-Gitlab-Event", "Push Hook")
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.NotContains(t, response.Body.String(), "audit-candidate-secret", "denials never echo credentials")

	var inboxRows int
	require.NoError(t, f.db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_inbox`).Scan(&inboxRows))
	assert.Zero(t, inboxRows, "a denied delivery leaves no business rows")
}

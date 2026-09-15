package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated integration tests for the D1 control-plane self-attesting
// gates: the full service path (facts load from persisted records,
// judgments mint control_plane evidence, verdicts compose with CI
// producers), the negative cases the brief demands (broken policy
// chain, unregistered runner, version drift) and the A1-4-shaped
// historical tuple replay.

func newControlPlaneFixture(t *testing.T) (*PostgresStore, string, string) {
	t.Helper()
	pg, projectID, workItemID := newQualityFixture(t)
	ctx := context.Background()
	_, err := pg.DB().ExecContext(ctx, `
		INSERT INTO work_items (id, project_id, title, status, version)
		VALUES ($1, $2, 'd1 control-plane item', 'validating', 2)`, workItemID, projectID)
	require.NoError(t, err)
	return pg, projectID, workItemID
}

func seedRunnerExecution(t *testing.T, pg *PostgresStore, projectID, workItemID, runnerStatus string, profileRef string) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.DB().ExecContext(ctx, `
		INSERT INTO runners (id, display_name, device_key_hash, status)
		VALUES ('018f7400-0000-7000-8000-0000000000b1', 'd1 runner', 'hash-d1', $1)`, runnerStatus)
	require.NoError(t, err)
	_, err = pg.DB().ExecContext(ctx, `
		INSERT INTO executions (id, project_id, work_item_id, runner_id, status)
		VALUES ('018f7400-0000-7000-8000-0000000000c1', $1, $2,
			'018f7400-0000-7000-8000-0000000000b1', 'completed')`, projectID, workItemID)
	require.NoError(t, err)
	if profileRef != "" {
		_, err = pg.DB().ExecContext(ctx, `
			INSERT INTO validation_runs (project_id, work_item_id, attempt, profile_ref, result)
			VALUES ($1, $2, 1, $3, 'passed')`, projectID, workItemID, profileRef)
		require.NoError(t, err)
	}
}

func controlPlaneTuple(projectID, workItemID string) evidence.Tuple {
	return evidence.Tuple{
		ProjectID:  projectID,
		WorkItemID: workItemID,
		SourceSHA:  strings.Repeat("7", 40),
		TargetSHA:  strings.Repeat("8", 40),
	}
}

// ciEvidence appends a merge_gate record for one CI-named gate.
func ciEvidence(t *testing.T, pg *PostgresStore, tup evidence.Tuple, check string, index int) {
	t.Helper()
	pipeline, job := int64(30), int64(300+index)
	record := &evidence.Record{
		EvidenceID: fmt.Sprintf("018f7500-0000-7000-8000-%012d", index),
		ProjectID:  tup.ProjectID, WorkItemID: tup.WorkItemID,
		Kind: check, Authority: evidence.AuthorityMergeGate, Status: evidence.EvidencePassed,
		SourceSHA: tup.SourceSHA, TargetSHA: tup.TargetSHA,
		PipelineID: &pipeline, JobID: &job,
		PolicyVersion: "3.0.0", Attempt: 1,
		Producer: evidence.Producer{Type: "gitlab_job", ID: check, Version: "gitlab-ci"},
	}
	require.NoError(t, pg.Quality().AppendEvidence(context.Background(), record))
}

func gateStatus(t *testing.T, pg *PostgresStore, projectID, workItemID string, tup evidence.Tuple, check string) string {
	t.Helper()
	var status string
	require.NoError(t, pg.DB().QueryRow(`
		SELECT status FROM gate_snapshots
		WHERE project_id = $1 AND work_item_id = $2 AND gate_id = $3
		  AND source_sha = $4 AND target_sha = $5 AND policy_version = $6`,
		projectID, workItemID, check, tup.SourceSHA, tup.TargetSHA, tup.PolicyVersion).Scan(&status))
	return status
}

func TestControlPlaneSelfAttestationThroughService(t *testing.T) {
	pg, projectID, workItemID := newControlPlaneFixture(t)
	ctx := context.Background()
	seedRunnerExecution(t, pg, projectID, workItemID, "approved",
		"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	service := &evidence.Service{Company: company, Store: pg.Quality()}

	tup := controlPlaneTuple(projectID, workItemID)
	ciEvidence(t, pg, tup, evidence.GateBuild, 1)
	ciEvidence(t, pg, tup, evidence.GateUnit, 2)

	verdict, err := service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)

	// The three engine-oracle gates self-attested green alongside the
	// CI gates that have producers; the CI gates without producers
	// honestly stay pending, so the verdict is not ready.
	for _, check := range []string{evidence.GatePolicyIntegrity, evidence.GateBaselineFreshness, evidence.GateBoundary} {
		assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, check), check)
	}
	assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBuild))
	assert.False(t, verdict.Ready, "CI gates without producers still block")
	assert.Equal(t, "3.1.0", verdict.Tuple.PolicyVersion, "the live path binds the active version")

	// The self-attestation landed as append-only evidence rows.
	var controlPlaneRows int
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM evidence WHERE authority = 'control_plane'`).Scan(&controlPlaneRows))
	assert.Equal(t, 3, controlPlaneRows)

	// Re-running the same tuple is idempotent: no new rows, no attempt
	// bump, snapshots stay green.
	_, err = service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM evidence WHERE authority = 'control_plane'`).Scan(&controlPlaneRows))
	assert.Equal(t, 3, controlPlaneRows)
}

func TestControlPlaneNegativeJudgments(t *testing.T) {
	t.Run("unregistered runner fails boundary", func(t *testing.T) {
		pg, projectID, workItemID := newControlPlaneFixture(t)
		seedRunnerExecution(t, pg, projectID, workItemID, "revoked",
			"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))
		company, err := evidence.CompanyPolicy()
		require.NoError(t, err)
		verdict, err := (&evidence.Service{Company: company, Store: pg.Quality()}).
			EvaluateWorkItem(context.Background(), controlPlaneTuple(projectID, workItemID))
		require.NoError(t, err)
		assert.Equal(t, evidence.GateFailed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBoundary))
	})

	t.Run("execution without profile fails boundary", func(t *testing.T) {
		pg, projectID, workItemID := newControlPlaneFixture(t)
		seedRunnerExecution(t, pg, projectID, workItemID, "online", "")
		company, err := evidence.CompanyPolicy()
		require.NoError(t, err)
		verdict, err := (&evidence.Service{Company: company, Store: pg.Quality()}).
			EvaluateWorkItem(context.Background(), controlPlaneTuple(projectID, workItemID))
		require.NoError(t, err)
		assert.Equal(t, evidence.GateFailed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBoundary))
	})

	t.Run("broken stored policy digest fails policy_integrity", func(t *testing.T) {
		pg, projectID, workItemID := newControlPlaneFixture(t)
		seedRunnerExecution(t, pg, projectID, workItemID, "approved",
			"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))
		overlay := qualityOverlay("d1-overlay", nil)
		_, err := pg.Quality().PutProjectPolicy(context.Background(), projectID, overlay, 0)
		require.NoError(t, err)
		// Tamper the stored digest column: the recomputed document no
		// longer matches what the row claims.
		_, err = pg.DB().ExecContext(context.Background(),
			`UPDATE quality_policies SET policy_digest = 'sha256:' || repeat('9', 64) WHERE project_id = $1`, projectID)
		require.NoError(t, err)

		company, err := evidence.CompanyPolicy()
		require.NoError(t, err)
		verdict, err := (&evidence.Service{Company: company, Store: pg.Quality()}).
			EvaluateWorkItem(context.Background(), controlPlaneTuple(projectID, workItemID))
		require.NoError(t, err)
		assert.Equal(t, evidence.GateFailed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GatePolicyIntegrity))
		assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBoundary),
			"one broken judgment does not poison the others")
	})

	t.Run("replay pin against drifted policy fails baseline_freshness", func(t *testing.T) {
		pg, projectID, workItemID := newControlPlaneFixture(t)
		seedRunnerExecution(t, pg, projectID, workItemID, "approved",
			"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))
		company, err := evidence.CompanyPolicy()
		require.NoError(t, err)

		pinned := controlPlaneTuple(projectID, workItemID)
		pinned.PolicyVersion = "2.9.0" // the historical tuple's version
		verdict, err := (&evidence.Service{Company: company, Store: pg.Quality()}).
			EvaluateWorkItem(context.Background(), pinned)
		require.NoError(t, err)
		assert.Equal(t, "2.9.0", verdict.Tuple.PolicyVersion, "the replay pin is honored verbatim")
		assert.Equal(t, evidence.GateFailed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBaselineFreshness))
		assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GatePolicyIntegrity))
	})
}

// TestControlPlaneA14TupleReplay is the D1-4 acceptance: an A1-4-shaped
// historical tuple — a validating work item whose CI evidence covers
// build/unit (the pilot's honest ceiling) plus governed execution facts
// — re-runs evaluation and the three engine-oracle gates go green; with
// the full CI gate set the same replay drives validating →
// ready_for_human_merge through the existing guarded writer.
func TestControlPlaneA14TupleReplay(t *testing.T) {
	pg, projectID, workItemID := newControlPlaneFixture(t)
	ctx := context.Background()
	seedRunnerExecution(t, pg, projectID, workItemID, "approved",
		"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	service := &evidence.Service{Company: company, Store: pg.Quality()}

	// The A1-4 reality at replay time: CI evidence for build/unit only.
	tup := controlPlaneTuple(projectID, workItemID)
	ciEvidence(t, pg, tup, evidence.GateBuild, 1)
	ciEvidence(t, pg, tup, evidence.GateUnit, 2)

	verdict, err := service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)
	for _, check := range []string{evidence.GatePolicyIntegrity, evidence.GateBaselineFreshness, evidence.GateBoundary} {
		assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, check), check)
	}
	assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateBuild))
	assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, evidence.GateUnit))
	assert.False(t, verdict.Ready, "honest ceiling: CI gates without producers still block")

	// The remaining CI producers arrive (the capability catch-up D2
	// governs); the same tuple re-runs and completes the done-chain
	// precondition: validating → ready_for_human_merge.
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)
	index := 10
	for _, check := range resolved.Policy.RequiredGates {
		switch check {
		case evidence.GateBuild, evidence.GateUnit:
			continue // already present
		case evidence.GatePolicyIntegrity, evidence.GateBaselineFreshness, evidence.GateBoundary:
			continue // self-attested
		default:
			ciEvidence(t, pg, tup, check, index)
			index++
		}
	}
	verdict, err = service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)
	require.True(t, verdict.Ready, "full CI set + self-attested gates compose to ready")

	var status string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE project_id = $1 AND id = $2`, projectID, workItemID).Scan(&status))
	assert.Equal(t, "ready_for_human_merge", status, "the guarded ready writer fired on the Ready verdict")

	for _, check := range []string{evidence.GatePolicyIntegrity, evidence.GateBaselineFreshness, evidence.GateBoundary} {
		assert.Equal(t, evidence.GatePassed, gateStatus(t, pg, projectID, workItemID, verdict.Tuple, check), check)
	}
}

// TestCapabilityBaselinePilotShape is the D2-4 acceptance: a
// zero-declaration project (the peixun pilot shape) resolves to exactly
// the six core gates, all six go green from three CI producers plus the
// three control-plane self-attestations, and the Ready verdict drives
// validating → ready_for_human_merge. Declaring a capability widens the
// snapshot set to exactly core ∪ declared — the noise-free contrast the
// two-tier baseline exists for.
func TestCapabilityBaselinePilotShape(t *testing.T) {
	pg, projectID, workItemID := newControlPlaneFixture(t)
	ctx := context.Background()
	seedRunnerExecution(t, pg, projectID, workItemID, "approved",
		"maven-build@1.0.0@sha256:"+strings.Repeat("ab", 32))

	company, err := evidence.CompanyPolicy()
	require.NoError(t, err)
	service := &evidence.Service{Company: company, Store: pg.Quality()}

	// Zero declarations: effective = core six.
	resolved, err := evidence.ResolveEffective(company, nil)
	require.NoError(t, err)
	require.Len(t, resolved.Policy.RequiredGates, 6)

	// The pilot reality: CI evidence for the three CI core gates; the
	// three engine-oracle gates self-attest from the execution facts.
	tup := controlPlaneTuple(projectID, workItemID)
	ciEvidence(t, pg, tup, evidence.GateBuild, 1)
	ciEvidence(t, pg, tup, evidence.GateUnit, 2)
	ciEvidence(t, pg, tup, evidence.GateSecretScan, 3)

	verdict, err := service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)
	require.True(t, verdict.Ready, "six core gates green: 3 CI + 3 self-attested")

	snapshots, err := pg.Quality().ListGateSnapshots(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.Len(t, snapshots, 6, "undeclared capability gates never become snapshots")
	seen := map[string]string{}
	for _, snapshot := range snapshots {
		seen[snapshot.Check] = snapshot.Status
	}
	assert.Equal(t, map[string]string{
		evidence.GateBuild:             evidence.GatePassed,
		evidence.GateUnit:              evidence.GatePassed,
		evidence.GateSecretScan:        evidence.GatePassed,
		evidence.GatePolicyIntegrity:   evidence.GatePassed,
		evidence.GateBaselineFreshness: evidence.GatePassed,
		evidence.GateBoundary:          evidence.GatePassed,
	}, seen)

	var status string
	require.NoError(t, pg.DB().QueryRowContext(ctx,
		`SELECT status FROM work_items WHERE project_id = $1 AND id = $2`, projectID, workItemID).Scan(&status))
	assert.Equal(t, "ready_for_human_merge", status, "A1-4 shape: the ready writer completed the done-chain precondition")

	// Contrast: a project that declares quality.coverage (with its
	// producer anchor) adds exactly one gate — nothing else changes.
	declared := qualityOverlay("d2-capable", func(p *evidence.Policy) {
		p.Capabilities = []evidence.CapabilityDeclaration{{
			Capability: "quality.coverage",
			Producer:   evidence.CapabilityProducer{Repo: "peixun/backend", Job: "coverage"},
		}}
		p.RequiredGates = append(p.RequiredGates, evidence.GateCoverage)
	})
	_, err = pg.Quality().PutProjectPolicy(ctx, projectID, declared, 0)
	require.NoError(t, err)

	declaredVerdict, err := service.EvaluateWorkItem(ctx, tup)
	require.NoError(t, err)
	require.Len(t, declaredVerdict.Gates, 7, "core six plus the declared capability gate")
	assert.False(t, declaredVerdict.Ready, "the declared coverage gate honestly blocks until its producer reports")
	coverage := func() string {
		for _, gate := range declaredVerdict.Gates {
			if gate.Check == evidence.GateCoverage {
				return gate.State
			}
		}
		return ""
	}()
	assert.Equal(t, evidence.GatePending, coverage)
}

func TestControlPlaneFactsStoreSurface(t *testing.T) {
	pg, projectID, workItemID := newControlPlaneFixture(t)
	ctx := context.Background()

	// No execution records: nil Boundary, no overlay row: digest match
	// vacuously true.
	facts, err := pg.Quality().ControlPlaneFacts(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.NotNil(t, facts)
	assert.True(t, facts.StoredDigestMatch)
	assert.Nil(t, facts.Boundary)

	// Governed execution present: the fact names runner and profile.
	seedRunnerExecution(t, pg, projectID, workItemID, "approved",
		"npm-build@2.0.0@sha256:"+strings.Repeat("cd", 32))
	facts, err = pg.Quality().ControlPlaneFacts(ctx, projectID, workItemID)
	require.NoError(t, err)
	require.NotNil(t, facts.Boundary)
	assert.True(t, facts.Boundary.RunnerRegistered)
	assert.Equal(t, "approved", facts.Boundary.RunnerStatus)
	assert.Equal(t, "npm-build@2.0.0@sha256:"+strings.Repeat("cd", 32), facts.Boundary.ProfileRef)
}

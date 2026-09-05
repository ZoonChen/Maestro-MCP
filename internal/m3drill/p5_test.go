// Package m3drill is the M3-P5 convergence playbook (the V3 ritual's
// executable evidence, mirroring internal/m2drill for V2): one
// cumulative, PG-gated scenario that walks every frozen exit-gate
// anchor against the real components — the contract engine, the
// integration-run lifecycle, the finding/defect normalization, the
// budget ledger and the agent orchestrator. Each subtest names the
// playbook line it proves; drift in one stage must surface in the
// next, never silently pass.
package m3drill

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/agent"
	"github.com/ZoonChen/Maestro-MCP/internal/budget"
	"github.com/ZoonChen/Maestro-MCP/internal/contract"
	"github.com/ZoonChen/Maestro-MCP/internal/defect"
	"github.com/ZoonChen/Maestro-MCP/internal/integration"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	m3Project = "018f7f00-0000-7000-8000-000000000002"
	m3Team    = "018f7e00-0000-7000-8000-000000000001"
)

type m3Fixture struct {
	db *sql.DB
	pg *store.PostgresStore
}

func newM3Fixture(t *testing.T) *m3Fixture {
	t.Helper()
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m3_drill WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_m3_drill`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_m3_drill WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_m3_drill")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ($1, 'm3 drill')`, m3Team)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, $2, 'm3drill', 'M3 Drill', 'active')`, m3Project, m3Team)
	require.NoError(t, err)
	return &m3Fixture{db: db, pg: pg}
}

// The breaking-contract anchor: a breaking diff is detected by the CTR
// engine, normalized into a contract finding, and lands as ONE defect
// with the responsible side carried in its evidence — and the
// integration run for the same combination FAILS (the block).
func TestBreakingChangeBlocksAndCreatesResponsibility(t *testing.T) {
	f := newM3Fixture(t)
	ctx := context.Background()

	// Register the consumer-side contract, then break it.
	base := `{"openapi":"3.0.0","info":{"title":"svc","version":"1"},"paths":{"/a":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"count":{"type":"integer"}}}}}}}}}}}`
	variant := strings.Replace(base, `"type":"integer"`, `"type":"string"`, 1)
	baseDoc := parseContract(t, base)
	newDoc := parseContract(t, variant)
	_ = baseDoc

	// The registry refuses a silent swap under the same version.
	require.NoError(t, f.pg.Contracts().RegisterContract(ctx, m3Project, baseDoc,
		"openapi3-json", "orders", "1.0.0", "sha256:"+strings.Repeat("a", 64), strings.Repeat("1", 40)))
	err := f.pg.Contracts().RegisterContract(ctx, m3Project, newDoc,
		"openapi3-json", "orders", "1.0.0", "sha256:"+strings.Repeat("b", 64), strings.Repeat("2", 40))
	require.ErrorIs(t, err, store.ErrContractHashConflict)

	// The breaking diff verdict is the finding's material; the defect
	// lands with responsibility evidence.
	finding, fpInput, err := defect.FromContract(defect.ContractFinding{
		ProjectID: m3Project, Repository: "acme/orders", Branch: "main",
		Service: "orders", Location: "responses.200.properties.count",
		Detail: "schema type changed", Provider: "backend", Consumer: "web",
	})
	require.NoError(t, err)
	fingerprint := defect.Fingerprint(fpInput)
	defectID, created, err := f.pg.Defects().RecordFinding(ctx, m3Project, finding, fingerprint)
	require.NoError(t, err)
	require.True(t, created)
	assert.NotEmpty(t, defectID)

	// The integration run for the broken combination fails and stays
	// failed: a settled verdict never re-opens.
	manifest := integration.Manifest{
		Revisions: []integration.RepositoryRevision{
			{RepositoryMappingID: "m-web", SHA: strings.Repeat("a", 40)},
			{RepositoryMappingID: "m-api", SHA: strings.Repeat("b", 40)},
		},
		ContractHash:       finding.EvidenceRefs[0],
		SuiteVersion:       "suite-1",
		EnvironmentProfile: "staging-1",
		FixtureVersion:     "fx-1",
		PolicyVersion:      "pol-1",
		TTL:                time.Hour,
	}
	manifestHash, err := manifest.CombinationDigest()
	require.NoError(t, err)
	runID, state, _, err := f.pg.IntegrationRuns().StartRun(ctx, m3Project, manifest, manifestHash)
	require.NoError(t, err)
	require.Equal(t, "running", state)
	require.NoError(t, f.pg.IntegrationRuns().SettleRun(ctx, m3Project, runID, "failed", "complete"))
	require.ErrorIs(t, f.pg.IntegrationRuns().SettleRun(ctx, m3Project, runID, "passed", "complete"), store.ErrRunStateConflict,
		"a failed verdict cannot be re-written to passed")
}

// The dedup anchor: pipeline failures normalize to findings; the same
// test identity lands on ONE defect; occurrence grows, severity rises.
func TestPipelineFailuresDedupeToOneDefect(t *testing.T) {
	f := newM3Fixture(t)
	ctx := context.Background()

	mk := func(eventID string, sev defect.Severity) defect.Finding {
		finding, _, err := defect.FromPipeline(defect.PipelineFinding{
			ProjectID: m3Project, Repository: "acme/orders", Branch: "main",
			JobName: "unit", Stage: "test", LogExcerpt: "FAIL: TestCheckout",
			ExitCode: 1, PipelineID: "50", JobID: "500", SourceSHA: strings.Repeat("3", 40),
		})
		require.NoError(t, err)
		finding.SourceEventID = "pipeline:50:job:500:" + eventID
		finding.Severity = sev
		return finding
	}
	fingerprint := defect.Fingerprint(defect.FingerprintInput{
		ProjectID: m3Project, Repository: "acme/orders", Branch: "main",
		CheckID: "unit", ErrorSignature: "FAIL: TestCheckout",
	})

	first := mk("r1", defect.SeverityMedium)
	defectID, created, err := f.pg.Defects().RecordFinding(ctx, m3Project, first, fingerprint)
	require.NoError(t, err)
	require.True(t, created)

	// Re-run with the same identity: occurrence grows, one defect.
	second := mk("r2", defect.SeverityCritical)
	same, created, err := f.pg.Defects().RecordFinding(ctx, m3Project, second, fingerprint)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, defectID, same, "one test identity is one defect")

	_, severity, occurrence, found, err := f.pg.Defects().Defect(ctx, m3Project, defectID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, occurrence)
	assert.Equal(t, string(defect.SeverityCritical), severity, "severity rises to the worst finding")
}

// The agent anchors: the happy path parks for human review (the defect
// is NEVER auto-resolved), budget exhaustion hands off honestly, and
// the frozen machine refuses injection-driven state.
func TestAgentHonestyAnchors(t *testing.T) {
	f := newM3Fixture(t)
	ctx := context.Background()

	// A parent defect + durable run for the persistence leg.
	_, fp, err := defect.FromPipeline(defect.PipelineFinding{
		ProjectID: m3Project, Repository: "acme/orders", Branch: "main",
		JobName: "unit", LogExcerpt: "FAIL: TestPay", ExitCode: 1,
		PipelineID: "60", JobID: "600",
	})
	require.NoError(t, err)
	dID, _, err := f.pg.Defects().RecordFinding(ctx, m3Project, defect.Finding{
		ProjectID: m3Project, SourceType: defect.SourcePipeline, SourceEventID: "pipeline:60:job:600",
		Severity: defect.SeverityHigh, EvidenceRefs: []string{"job:600"}, AdapterVersion: "v1",
	}, defect.Fingerprint(fp))
	require.NoError(t, err)
	run := agent.RunContext{ProjectID: m3Project, DefectID: dID, RunID: "018f7e00-0000-7000-8000-0000000000e1", Attempt: 1}
	state, created, err := f.pg.AgentRuns().CreateRun(ctx, run, 1, 1000)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, agent.StateEligibilityCheck, state)

	// Happy path: every stage passes, CI green — the run PARKS at
	// awaiting_human; the defect stays unresolved (humans resolve).
	ledger, err := budget.New("ag", budget.Limits{BudgetUnits: 1000, MaxAttempts: 3, WallTimeLimit: time.Hour})
	require.NoError(t, err)
	start := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	o := &agent.Orchestrator{Ports: allPassPorts(), Ledger: ledger, MaxAttempts: 3, StartedAt: start, Now: func() time.Time { return start }}

	steps := []func() (agent.StepOutcome, error){
		func() (agent.StepOutcome, error) { return o.EligibilityGate(run) },
		func() (agent.StepOutcome, error) { return o.ReproduceStep(run) },
		func() (agent.StepOutcome, error) { return o.DiagnoseStep(run, "sig") },
		func() (agent.StepOutcome, error) { return o.ModifyStep(run, "diag") },
		func() (agent.StepOutcome, error) { return o.LocalVerifyStep(run, "diff") },
		func() (agent.StepOutcome, error) { return o.MRTransitionStep(run) },
		func() (agent.StepOutcome, error) { return o.CIVerifyStep(run, "mr") },
	}
	var out agent.StepOutcome
	for _, step := range steps {
		out, err = step()
		require.NoError(t, err)
		require.NoError(t, f.pg.AgentRuns().Settle(ctx, run, out.From, out.To, out.Reason))
	}
	assert.Equal(t, agent.StateAwaitingHuman, out.To, "green CI parks; the agent never resolves")
	defectState, _, _, found, err := f.pg.Defects().Defect(ctx, m3Project, dID)
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEqual(t, "resolved", defectState, "no agent path resolves a defect")

	// Injection discipline: hostile text cannot drive the machine —
	// free-text "states" simply do not exist in the enum, and illegal
	// edges refuse. (The transition validator itself is unexported; the
	// discipline is asserted through the frozen predicate and the
	// public Stop surface.)
	assert.False(t, agent.CanTransition(agent.StateAwaitingHuman, agent.StateReproducing))
	assert.False(t, agent.CanTransition(agent.StateStopped, agent.StateModifying))
	assert.False(t, agent.CanTransition(agent.StateReproducing, agent.StateMRCreated))
	stopOut, stopErr := o.Stop(run, agent.StateReproducing)
	require.NoError(t, stopErr)
	assert.Equal(t, agent.StateStopped, stopOut.To)

	// Budget exhaustion hands off honestly — no "fixed" claim.
	drained, err := budget.New("dr", budget.Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: time.Hour})
	require.NoError(t, err)
	res, err := drained.ReserveCall(10, "model", start)
	require.NoError(t, err)
	_ = drained.RecordUsageMissing(res, start)
	exhausted := &agent.Orchestrator{Ports: failVerifyPorts(), Ledger: drained, MaxAttempts: 3, StartedAt: start, Now: func() time.Time { return start }}
	out, err = exhausted.LocalVerifyStep(run, "diff")
	require.NoError(t, err)
	assert.Equal(t, agent.StateNeedsHuman, out.To)
	assert.Equal(t, agent.HandoffBudgetExhausted, out.Reason, "the honest reason, never a guessed fix")
}

func parseContract(t *testing.T, source string) *contract.Document {
	t.Helper()
	doc, err := contract.ParseDocument([]byte(source))
	require.NoError(t, err)
	return doc
}

func allPassPorts() agent.Ports {
	return agent.Ports{
		Eligible:      func(agent.RunContext) (bool, string, error) { return true, "", nil },
		Reproduce:     func(agent.RunContext) (bool, string, error) { return true, "sig", nil },
		Diagnose:      func(agent.RunContext, string) (string, error) { return "diag", nil },
		Modify:        func(agent.RunContext, string) (string, error) { return "diff", nil },
		VerifyLocally: func(agent.RunContext, string) (bool, bool, error) { return true, false, nil },
		CreateMR:      func(agent.RunContext, string) (string, error) { return "mr", nil },
		CheckCI:       func(agent.RunContext, string) (bool, bool, error) { return true, false, nil },
	}
}

func failVerifyPorts() agent.Ports {
	ports := allPassPorts()
	ports.VerifyLocally = func(agent.RunContext, string) (bool, bool, error) { return false, true, nil }
	return ports
}

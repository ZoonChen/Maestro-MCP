package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
)

// PG-gated M4-EVAL-001 persistence: wire validation before the insert,
// the (run, case, layer) uniqueness, the jsonb round trips and the
// 0012 security-layer CHECK as the structural backstop.

func TestEvaluationRecordPersistence(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_eval_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_eval_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_eval_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_eval_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-000000000001', 'eval')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ($1, '018f7e00-0000-7000-8000-000000000001', 'eval', 'EVAL', 'active')`, m3Project)
	require.NoError(t, err)

	storeEval := pg.Evaluation()
	runID := "018f7e00-0000-7000-8000-00000000e001"
	digest := "sha256:" + strings.Repeat("a", 64)

	// The frozen wire's conditional requirements reject before insert.
	security := eval.Record{
		SchemaVersion: eval.SchemaVersion, ProjectID: m3Project,
		Layer: eval.LayerSecurity, CaseID: "inj-001", RunID: runID,
		Verdict: eval.VerdictPass, Score: 100,
		Scorer:        eval.Scorer{Kind: eval.ScorerRuleBased, Version: "redteam-v1"},
		DatasetDigest: digest, Version: 1,
	}
	_, err = storeEval.AppendRecord(ctx, security)
	require.ErrorIs(t, err, eval.ErrRecordRejected, "security without risk never persists")

	failed := eval.Record{
		SchemaVersion: eval.SchemaVersion, ProjectID: m3Project,
		Layer: eval.LayerQuality, CaseID: "case-001", RunID: runID,
		Verdict: eval.VerdictFail, Score: 12,
		Scorer:        eval.Scorer{Kind: eval.ScorerDeterministic, Version: "golden-v3"},
		DatasetDigest: digest, Version: 1,
	}
	_, err = storeEval.AppendRecord(ctx, failed)
	require.ErrorIs(t, err, eval.ErrRecordRejected, "fail without notes never persists")

	// The honest full record round trips every projection column.
	seed, latency := int64(7), int64(4200)
	full := eval.Record{
		SchemaVersion: eval.SchemaVersion, ProjectID: m3Project,
		Layer: eval.LayerQuality, CaseID: "case-001", RunID: runID,
		Category: "unit-regression", Verdict: eval.VerdictFail, Score: 12,
		Scorer:        eval.Scorer{Kind: eval.ScorerLLMJudge, Version: "judge-v2", CalibrationRef: "calib-2026-09"},
		DatasetDigest: digest, Seed: &seed, ModelConfigDigest: digest,
		TrajectoryConstraints:    []string{"no-protected-ref", "budget-capped"},
		ForbiddenActionsObserved: []string{"git.push"},
		TokensUsed:               1500, LatencyMS: &latency,
		ExternalStateDigest: digest,
		EvidenceRefs:        []string{"018f7e00-0000-7000-8000-0000000000bb"},
		Notes:               "two assertions missed the fixed error code",
		Version:             1,
	}
	id, err := storeEval.AppendRecord(ctx, full)
	require.NoError(t, err)
	assert.NotEmpty(t, id, "the store generated the record id")

	readBack, err := storeEval.RecordsByRun(ctx, runID, eval.LayerQuality)
	require.NoError(t, err)
	require.Len(t, readBack, 1)
	got := readBack[0]
	assert.Equal(t, id, got.RecordID)
	assert.Equal(t, m3Project, got.ProjectID)
	assert.Equal(t, "unit-regression", got.Category)
	assert.Equal(t, eval.Scorer{Kind: eval.ScorerLLMJudge, Version: "judge-v2", CalibrationRef: "calib-2026-09"}, got.Scorer)
	assert.Equal(t, digest, got.DatasetDigest)
	assert.Equal(t, digest, got.ModelConfigDigest)
	require.NotNil(t, got.Seed)
	assert.Equal(t, int64(7), *got.Seed)
	require.NotNil(t, got.LatencyMS)
	assert.Equal(t, int64(4200), *got.LatencyMS)
	assert.Equal(t, []string{"no-protected-ref", "budget-capped"}, got.TrajectoryConstraints)
	assert.Equal(t, []string{"git.push"}, got.ForbiddenActionsObserved)
	assert.Equal(t, []string{"018f7e00-0000-7000-8000-0000000000bb"}, got.EvidenceRefs)
	assert.Equal(t, "two assertions missed the fixed error code", got.Notes)

	// The uniqueness key: the same (run, case, layer) is a duplicate.
	_, err = storeEval.AppendRecord(ctx, full)
	require.ErrorIs(t, err, ErrEvaluationDuplicate)

	// The security layer passes with risk; the SQL backstop refuses a
	// direct insert without it.
	security.Risk = eval.RiskElevated
	_, err = storeEval.AppendRecord(ctx, security)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO evaluation_records
		(id, layer, case_id, run_id, verdict, score, scorer_kind, scorer_version, dataset_digest)
		VALUES (gen_random_uuid(), 'security', 'raw-001', $1, 'pass', 100, 'rule_based', 'v1', $2)`,
		"018f7e00-0000-7000-8000-00000000e002", digest)
	require.Error(t, err, "the 0012 CHECK refuses security rows without risk")

	// The harness input path: ten trials, five passing — verdict counts
	// plus the pure pass^k estimator form the report inputs.
	for trial := 2; trial <= 10; trial++ {
		trialRecord := eval.Record{
			SchemaVersion: eval.SchemaVersion, ProjectID: m3Project,
			Layer: eval.LayerQuality, CaseID: "case-" + string(rune('0'+trial)), RunID: runID,
			Verdict: eval.VerdictPass, Score: 88,
			Scorer:        eval.Scorer{Kind: eval.ScorerDeterministic, Version: "golden-v3"},
			DatasetDigest: digest, Version: 1,
		}
		if trial <= 5 {
			trialRecord.Verdict = eval.VerdictFail
			trialRecord.Score = 30
			trialRecord.Notes = "flaky trial " + string(rune('0'+trial))
		}
		_, err = storeEval.AppendRecord(ctx, trialRecord)
		require.NoError(t, err)
	}

	counts, err := storeEval.VerdictCounts(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, int64(6), counts[eval.VerdictPass], "five passing trials plus the security record")
	assert.Equal(t, int64(5), counts[eval.VerdictFail], "case-001 plus four flaky trials")

	// pass^k is computed per case over its own layer's trials — here the
	// ten quality trials with five passes.
	passAtOne, err := eval.PassPowerK(10, 5, 1)
	require.NoError(t, err)
	assert.InDelta(t, 0.5, passAtOne, 1e-12)
}

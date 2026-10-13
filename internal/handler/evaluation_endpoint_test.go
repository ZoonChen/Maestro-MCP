package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
)

// EV-2 / G3-E2: GET /api/v3/projects/:pid/evaluations is the
// evaluation observation read face over the CLI-imported
// evaluation_records, riding the frozen quality.read permission
// through the real authorize tree and the real PostgreSQL store.

func evaluationRecordForTest(layer eval.Layer, caseID string, verdict eval.Verdict, notes string) eval.Record {
	return eval.Record{
		ProjectID:     qProjectID,
		Layer:         layer,
		CaseID:        caseID,
		RunID:         "018f7e00-0000-7000-8000-00000000e201",
		Verdict:       verdict,
		Score:         100,
		Scorer:        eval.Scorer{Kind: eval.ScorerRuleBased, Version: "1.0.0"},
		DatasetDigest: "sha256:" + strings.Repeat("a", 64),
		Notes:         notes,
		Version:       1,
	}
}

func TestProjectEvaluationsList(t *testing.T) {
	f := newQualityFixture(t)
	ctx := context.Background()

	// Seed two runs for the project through the same append-only store
	// the eval-import CLI drives: run A with four trials across layers,
	// run B with one failing trial carrying the ingest projection's
	// disambiguated trial case id.
	runA := evaluationRecordForTest(eval.LayerQuality, "EV-CLAIM-QUAL-01", eval.VerdictPass, "")
	runA.RunID = "018f7e00-0000-7000-8000-00000000e201"
	_, err := f.pg.Evaluation().AppendRecord(ctx, runA)
	require.NoError(t, err)
	for _, layer := range []eval.Layer{eval.LayerTrajectory, eval.LayerSecurity, eval.LayerCapability} {
		record := evaluationRecordForTest(layer, "EV-GATE-TRAJ-01", eval.VerdictPass, "")
		record.RunID = runA.RunID
		switch layer {
		case eval.LayerSecurity:
			record.Risk = eval.RiskElevated
			record.CaseID = "EV-MERGE-SEC-01"
		case eval.LayerCapability:
			record.CaseID = "EV-AUDIT-CAP-01"
			record.Verdict = eval.VerdictError
			record.Notes = "adapter tool error"
		}
		_, err := f.pg.Evaluation().AppendRecord(ctx, record)
		require.NoError(t, err)
	}

	runB := evaluationRecordForTest(eval.LayerQuality, "EV-CLAIM-QUAL-02#t2", eval.VerdictFail, "expected files_changed mismatch")
	runB.RunID = "018f7e00-0000-7000-8000-00000000e202"
	_, err = f.pg.Evaluation().AppendRecord(ctx, runB)
	require.NoError(t, err)

	path := "/api/v3/projects/" + qProjectID + "/evaluations"

	t.Run("developer with quality.read sees the runs with aggregated counts", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		body := response.Body.String()
		// Newest run first (run B), then run A.
		assert.Less(t, strings.Index(body, "00000000e202"), strings.Index(body, "00000000e201"))
		assert.Contains(t, body, `"dataset_digest":"sha256:`)
		assert.Contains(t, body, `"records":1`)
		assert.Contains(t, body, `"records":4`)
		// Map keys marshal alphabetically (blocked/error/fail/pass/skipped).
		assert.Contains(t, body, `"verdict_counts":{"blocked":0,"error":0,"fail":1,"pass":0,"skipped":0}`)
		assert.Contains(t, body, `"verdict_counts":{"blocked":0,"error":1,"fail":0,"pass":3,"skipped":0}`)
		assert.Contains(t, body, `"layer_counts":{"capability":0,"quality":1,"security":0,"trajectory":0}`)
		assert.Contains(t, body, `"layer_counts":{"capability":1,"quality":1,"security":1,"trajectory":1}`)
		assert.Contains(t, body, `"first_record_at"`)
		assert.Contains(t, body, `"last_record_at"`)
		// The ingest projection's disambiguated trial ids are stored
		// truth; the run summary deliberately counts trials rather than
		// re-joining case identities (contract note in the openapi
		// description of this operation).
	})

	t.Run("viewer membership is enough for the read face", func(t *testing.T) {
		response := f.request(t, f.viewerTK, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	})

	t.Run("unauthenticated requests are rejected", func(t *testing.T) {
		response := f.request(t, "", http.MethodGet, path, nil, "")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
	})

	t.Run("membership without quality.read is forbidden", func(t *testing.T) {
		// platform_admin holds no quality.read in the frozen matrix.
		response := f.request(t, f.platTK, http.MethodGet, path, nil, "")
		assert.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("no membership hides the project as 404", func(t *testing.T) {
		// sec-1's membership is viewer in THIS project; give the token
		// a foreign project scope instead by hitting an unknown one —
		// covered below; here an unknown project exercises the same guard.
		response := f.request(t, f.devTK, http.MethodGet,
			"/api/v3/projects/018f7500-0000-7000-8000-00000000dead/evaluations", nil, "")
		assert.Equal(t, http.StatusNotFound, response.Code)
	})
}

func TestProjectEvaluationsEmptyProject(t *testing.T) {
	f := newQualityFixture(t)

	t.Run("a project with no imported runs answers an empty array", func(t *testing.T) {
		response := f.request(t, f.devTK, http.MethodGet,
			"/api/v3/projects/"+qProjectID+"/evaluations", nil, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, "[]", response.Body.String())
	})
}

// The store helper behind the endpoint: a run that mixed dataset
// digests (which violates the run-freeze expectation of authority §3)
// surfaces as a null digest instead of a silently flattened value.
func TestEvaluationRunsByProjectMixedDigest(t *testing.T) {
	f := newQualityFixture(t)
	ctx := context.Background()

	first := evaluationRecordForTest(eval.LayerQuality, "EV-CLAIM-QUAL-01", eval.VerdictPass, "")
	first.RunID = "018f7e00-0000-7000-8000-00000000e301"
	second := evaluationRecordForTest(eval.LayerQuality, "EV-CLAIM-QUAL-03", eval.VerdictPass, "")
	second.RunID = first.RunID
	second.DatasetDigest = "sha256:" + strings.Repeat("b", 64)
	_, err := f.pg.Evaluation().AppendRecord(ctx, first)
	require.NoError(t, err)
	_, err = f.pg.Evaluation().AppendRecord(ctx, second)
	require.NoError(t, err)

	response := f.request(t, f.devTK, http.MethodGet,
		"/api/v3/projects/"+qProjectID+"/evaluations", nil, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"dataset_digest":null`)
	assert.Contains(t, response.Body.String(), `"records":2`)
}

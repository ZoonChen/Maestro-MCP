//go:build p5b

package p5b_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// TestP5bStageTwo registers and walks the FINAL first-run artifacts —
// the test report (real profile numbers), the release note (the D1
// selection conclusion + the evidence summary this brief's DoD names)
// and the retrospective — then re-exports and verifies the whole audit
// chain. Runs AFTER TestP5bStageOne and after the engineering evidence
// (profile A/B, pipelines, merges) has landed in the content files.
func TestP5bStageTwo(t *testing.T) {
	f := p5bOpen(t)
	ctx := context.Background()
	started := time.Now()

	author := p5bStartRunner(t, f.config, p5bPocPID, "p5b-author")
	tech := p5bStartRunner(t, f.config, p5bPocPID, "p5b-tech")
	product := p5bStartRunner(t, f.config, p5bPocPID, "p5b-product")
	qa := p5bStartRunner(t, f.config, p5bPocPID, "p5b-qa")

	register := func(t *testing.T, spec p5bAssetSpec) *store.Asset {
		t.Helper()
		if latest, err := f.pg.Assets().LatestAsset(ctx, spec.assetID); err == nil {
			return latest
		} else if !errors.Is(err, store.ErrAssetNotFound) {
			t.Fatalf("latest asset %s: %v", spec.assetID, err)
		}
		arguments := map[string]any{
			"asset_id": spec.assetID, "asset_type": spec.assetType, "title": spec.title,
			"sensitivity": spec.sensitivity, "source_digest": f.digest(t, spec.contentRef),
			"content_ref": spec.contentRef, "idempotency_key": spec.idem("register"),
		}
		if len(spec.reviewers) > 0 {
			arguments["reviewers"] = spec.reviewers
		}
		text := author.callOK(t, "asset_register", arguments)
		assert.Contains(t, text, `"status":"draft"`)
		latest, err := f.pg.Assets().LatestAsset(ctx, spec.assetID)
		require.NoError(t, err)
		return latest
	}
	moveTo := func(t *testing.T, spec p5bAssetSpec, version int, stage string, runner *p5bStdioRunner) {
		t.Helper()
		asset, err := f.pg.Assets().GetAsset(ctx, spec.assetID, version)
		require.NoError(t, err)
		if asset.Status == store.AssetStatusDraft && stage == "review" {
			runner.callOK(t, "asset_review", map[string]any{
				"asset_id": spec.assetID, "version": float64(version), "idempotency_key": spec.idem("review"),
			})
		}
		if asset.Status == store.AssetStatusReviewed && stage == "approve" {
			runner.callOK(t, "asset_approve", map[string]any{
				"asset_id": spec.assetID, "version": float64(version), "idempotency_key": spec.idem("approve"),
			})
		}
	}

	testReport := p5bAssetSpec{assetID: "ART-test-report-001", assetType: "test-report",
		title: "测试报告：D1 PoC 构建链路 A/B 与双路线冒烟（真实 Profile 实测）", sensitivity: "internal",
		contentRef: "assets/ART-test-report-001/test-report.md",
		reviewers:  []string{"function:qa_owner"}, key: "p5b-tr"}
	releaseNote := p5bAssetSpec{assetID: "ART-release-note-001", assetType: "release-note",
		title: "D1 选型结论与 Evidence 汇总（0→1 全链路首演报告）", sensitivity: "internal",
		contentRef: "assets/ART-release-note-001/release-note.md",
		reviewers:  []string{"function:product_owner", "function:qa_owner", "function:technical_lead"}, key: "p5b-rn"}
	retrospective := p5bAssetSpec{assetID: "ART-retrospective-001", assetType: "retrospective",
		title: "首演复盘：角色工作流摩擦点与工具缺口实单", sensitivity: "internal",
		contentRef: "assets/ART-retrospective-001/retrospective.md",
		reviewers:  []string{"function:technical_lead"}, key: "p5b-rt"}

	t.Run("test-report walk（qa_owner 面板）", func(t *testing.T) {
		register(t, testReport)
		moveTo(t, testReport, 1, "review", qa)
		moveTo(t, testReport, 1, "approve", qa)
		asset, err := f.pg.Assets().GetAsset(ctx, testReport.assetID, 1)
		require.NoError(t, err)
		assert.Equal(t, store.AssetStatusApproved, asset.Status)
	})

	t.Run("release-note walk（product 签评审 + technical 签放行＝决策 Gate 双签）", func(t *testing.T) {
		register(t, releaseNote)
		moveTo(t, releaseNote, 1, "review", product) // product sign
		moveTo(t, releaseNote, 1, "approve", tech)   // technical sign
		asset, err := f.pg.Assets().GetAsset(ctx, releaseNote.assetID, 1)
		require.NoError(t, err)
		assert.Equal(t, store.AssetStatusApproved, asset.Status)
		f.ev.record(t, "decision_gate_dual_sign", map[string]any{
			"asset":        "ART-release-note-001@1",
			"review_actor": "session:p5b-product-session", "approve_actor": "session:p5b-tech-session",
			"functional_basis": "pilot-admin holds product_owner+technical_lead (J1 grants); dual-sign expression = two ledger transitions by two function-named sessions — native multi-sign Gate is a registered gap",
		})
	})

	t.Run("retrospective walk", func(t *testing.T) {
		register(t, retrospective)
		moveTo(t, retrospective, 1, "review", tech)
		moveTo(t, retrospective, 1, "approve", tech)
		asset, err := f.pg.Assets().GetAsset(ctx, retrospective.assetID, 1)
		require.NoError(t, err)
		assert.Equal(t, store.AssetStatusApproved, asset.Status)
	})

	t.Run("final ledger over MCP", func(t *testing.T) {
		text := qa.callOK(t, "asset_query", map[string]any{})
		var payload struct {
			Assets []struct {
				AssetID string `json:"asset_id"`
				Version int    `json:"version"`
				Status  string `json:"status"`
			} `json:"assets"`
		}
		require.NoError(t, json.Unmarshal([]byte(text), &payload))
		approved := map[string]bool{}
		for _, asset := range payload.Assets {
			if asset.Status == store.AssetStatusApproved {
				approved[fmt.Sprintf("%s@%d", asset.AssetID, asset.Version)] = true
			}
		}
		for _, expected := range []string{
			"ART-blueprint-001@1", "ART-detailed-design-001@2", "ART-test-plan-001@1",
			"ART-test-report-001@1", "ART-release-note-001@1", "ART-retrospective-001@1",
			"ART-incident-002@1", "ART-research-001@1",
		} {
			assert.True(t, approved[expected], "ledger must show %s approved", expected)
		}
		f.ev.record(t, "final_ledger", map[string]any{"total_rows": len(payload.Assets), "approved": approved})
	})

	t.Run("final audit export + verify", func(t *testing.T) {
		status, body := f.rest.do(t, "GET", "/api/v3/projects/"+p5bPocPID+"/audit-export?from_seq=1&to_seq=100000", nil, nil)
		require.Equal(t, 200, status, "audit export: %s", body)
		var export struct {
			ChainDigest string `json:"chain_digest"`
			Entries     []struct {
				Action      string `json:"action"`
				EntryDigest string `json:"entry_digest"`
			} `json:"entries"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &export))
		actions := map[string]int{}
		digests := make([]string, 0, len(export.Entries))
		for _, entry := range export.Entries {
			if entry.Action != "" {
				actions[entry.Action]++
			}
			digests = append(digests, entry.EntryDigest)
		}
		assert.GreaterOrEqual(t, actions["asset.approved"], 9, "nine approved walks must be on the chain")
		assert.Greater(t, actions["workgraph.plan.sealed"], 2, "contrast + formal seals")
		f.ev.record(t, "audit_export_final", map[string]any{
			"entries": len(export.Entries), "chain_digest": export.ChainDigest, "actions": actions,
		})
		verifyStatus, verifyBody := f.rest.do(t, "POST", "/api/v3/projects/"+p5bPocPID+"/audit-export/verify",
			map[string]string{"If-Match": export.ChainDigest, "Idempotency-Key": "p5b-audit-verify-0002"},
			mustJSON(t, map[string]any{"from_seq": 1, "to_seq": 100000, "claimed_digests": digests}))
		require.Equal(t, 200, verifyStatus, "audit verify: %s", verifyBody)
		assert.Contains(t, verifyBody, `"verified":true`)
		f.ev.record(t, "stage_two_completed", map[string]any{
			"wall_seconds": time.Since(started).Seconds(), "at": time.Now().Format(time.RFC3339),
			"final_chain_digest": export.ChainDigest,
		})
	})
}

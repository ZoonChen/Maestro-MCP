package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// loadTestManifest reads the committed manifest (the replay source the
// importer itself uses — the test pins the same file).
func loadTestManifest(t *testing.T) BomManifest {
	t.Helper()
	path := filepath.Join("data", "bom-manifest-20260909.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest BomManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return manifest
}

func TestManifestCounts(t *testing.T) {
	manifest := loadTestManifest(t)
	if len(manifest.Items) != 105 {
		t.Fatalf("manifest carries %d items, want 105 (workbook 00-总览 live stats)", len(manifest.Items))
	}
	phase1, p0 := 0, 0
	for _, item := range manifest.Items {
		if item.Phase == "一期" {
			phase1++
			if item.Priority == "P0" {
				p0++
			}
		}
	}
	if phase1 != 44 || p0 != 37 {
		t.Fatalf("phase-1 = %d (P0 %d), want 44 (37 P0 + 7 P1)", phase1, p0)
	}
	if manifest.Source.SHA256 == "" {
		t.Fatal("manifest must anchor the source workbook digest")
	}
}

func TestDerivedIDsStableAndLegal(t *testing.T) {
	manifest := loadTestManifest(t)
	index := codeIndex(manifest)
	if len(index) != 105 {
		t.Fatalf("code index covers %d codes", len(index))
	}
	humanCodes := map[string]bool{}
	uuids := map[string]bool{}
	issueKeys := map[string]bool{}
	uuidForm := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	for _, item := range manifest.Items {
		code := itemHumanCode(manifest, item.Code)
		if !humanCodeForm.MatchString(code) || humanCodes[code] {
			t.Fatalf("human code %q illegal or duplicated", code)
		}
		humanCodes[code] = true

		id := flatItemUUID(manifest, item.Code)
		if !uuidForm.MatchString(id) || uuids[id] {
			t.Fatalf("flat uuid %q illegal or duplicated", id)
		}
		uuids[id] = true

		issue := jiraIssueKey(manifest, item)
		if !issueKeyLegal.MatchString(issue) || issueKeys[issue] {
			t.Fatalf("issue key %q illegal or duplicated", issue)
		}
		issueKeys[issue] = true
		if slot := itemSlotKey(item.Code); !slotKeyLegal.MatchString(slot) {
			t.Fatalf("slot key %q illegal", slot)
		}
	}
	if got := itemHumanCode(manifest, manifest.Items[0].Code); got != "MST-WI-00701" {
		t.Fatalf("first human code = %q, want MST-WI-00701", got)
	}
	if got := jiraIssueKey(manifest, manifest.Items[0]); got != "PXPEIXUN-101" {
		t.Fatalf("first phase-1 issue key = %q, want PXPEIXUN-101", got)
	}
}

func TestM1ProposalStructure(t *testing.T) {
	manifest := loadTestManifest(t)
	proposals := deriveLetterProposals(manifest, proposalOptions{
		Phase: "一期", LetterCodeBase: s2aM1LetterCodeBase, DomainCodeBase: s2aM1DomainCodeBase,
		Repo: "peixun-backend", BaselineSHA: "0123456789012345678901234567890123456789",
		WorkspacePaths: []string{"doc"}, BOMAssetRef: "ART-bom-001@2",
		IncludeFlows: true, IncludeEdges: true,
	})
	if len(proposals) != 5 {
		t.Fatalf("phase-1 splits into %d letter proposals, want 5", len(proposals))
	}
	items, edges, flows := 0, 0, 0
	for _, group := range proposals {
		for _, node := range group.Nodes {
			if node.NodeType == "work_item" {
				items++
			}
		}
		edges += len(group.Edges)
		flows += len(group.Flows)
	}
	if items != 44 {
		t.Fatalf("phase-1 work items = %d, want 44", items)
	}
	if edges != len(m1RequiresEdges) {
		t.Fatalf("requires edges = %d, want %d (all 17 land inside their letter groups)", edges, len(m1RequiresEdges))
	}
	if flows != 44 {
		t.Fatalf("bom consume flows = %d, want 44", flows)
	}
	if err := checkDerived(proposals); err != nil {
		t.Fatalf("structure check: %v", err)
	}
}

func TestM23ProposalStructure(t *testing.T) {
	manifest := loadTestManifest(t)
	proposals := deriveLetterProposals(manifest, proposalOptions{
		Phase: "二三期", LetterCodeBase: s2aM23LetterCodeBase, DomainCodeBase: s2aM23DomainCodeBase,
		Repo: "peixun-backend", BaselineSHA: "0123456789012345678901234567890123456789",
		WorkspacePaths: []string{"doc"},
	})
	if len(proposals) != 5 {
		t.Fatalf("phase-2/3 splits into %d letter proposals, want 5", len(proposals))
	}
	items := 0
	for _, group := range proposals {
		for _, node := range group.Nodes {
			if node.NodeType == "work_item" {
				items++
			}
		}
		if len(group.Edges) != 0 || len(group.Flows) != 0 {
			t.Fatalf("placeholders must stay edge- and flow-free, letter %s", group.Letter)
		}
	}
	if items != 61 {
		t.Fatalf("phase-2/3 placeholder items = %d, want 61 (brief said 46 — BOM errata)", items)
	}
	if err := checkDerived(proposals); err != nil {
		t.Fatalf("structure check: %v", err)
	}
}

func TestBudgetEnvelopes(t *testing.T) {
	manifest := loadTestManifest(t)
	proposals := deriveLetterProposals(manifest, proposalOptions{
		Phase: "一期", LetterCodeBase: s2aM1LetterCodeBase, DomainCodeBase: s2aM1DomainCodeBase,
		Repo: "peixun-backend", BaselineSHA: "0123456789012345678901234567890123456789",
		WorkspacePaths: []string{"doc"}, BOMAssetRef: "ART-bom-001@2", IncludeFlows: true, IncludeEdges: true,
	})
	// Letter package budget must equal its item-day sum (the per-proposal
	// envelope check enforces sum ≤ package).
	wantDays := map[string]int{}
	for _, item := range manifest.Items {
		if item.Phase == "一期" {
			wantDays[strings.ToLower(letterOf(item.Code))] += item.Days
		}
	}
	for _, group := range proposals {
		for _, node := range group.Nodes {
			if node.LocalID != "grp-"+group.Letter {
				continue
			}
			budget := node.Spec["budget_units"].(int)
			if budget != wantDays[group.Letter] {
				t.Fatalf("letter %s budget = %d, want %d", group.Letter, budget, wantDays[group.Letter])
			}
			if budget < 1 {
				t.Fatalf("letter %s budget must be positive", group.Letter)
			}
		}
	}
	// Phase-1 total: the workbook's own arithmetic.
	if total := letterBudget(filterPhase(manifest, "一期")); total != 246 {
		t.Fatalf("phase-1 day total = %d, want 246", total)
	}
}

func filterPhase(manifest BomManifest, phase string) []BomItem {
	var out []BomItem
	for _, item := range manifest.Items {
		if item.Phase == phase {
			out = append(out, item)
		}
	}
	return out
}

func TestFlatItemFields(t *testing.T) {
	manifest := loadTestManifest(t)
	if got := flatPriority("P0"); got != "high" {
		t.Fatalf("P0 → %q, want high", got)
	}
	if got := flatPriority("P2"); got != "low" {
		t.Fatalf("P2 → %q, want low", got)
	}
	// Titles must fit the work_items 200-char CHECK.
	for _, item := range manifest.Items {
		title := fmt.Sprintf("[BOM %s] %s（%s %s）", item.Code, item.Feature, item.Phase, item.Priority)
		if len([]rune(title)) > 200 {
			t.Fatalf("title too long for %s", item.Code)
		}
	}
}

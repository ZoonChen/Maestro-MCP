package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssembleDigestAndPointer locks the S2C-4 registration payload:
// digest covers the file bytes, the content pointer is repo-relative,
// and the summary is exactly one JSON document serialized as a string
// (the frozen asset_register contract).
func TestAssembleDigestAndPointer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy", "gitlab", "peixun", "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(root, "deploy", "gitlab", "peixun", "reports", "shadow-report-W9-20261015.md")
	body := "# 影子期周报 W9\n\nline2\n"
	if err := os.WriteFile(report, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root) // root carries a go.mod below; repoRoot() must resolve to root

	reg, err := assemble(report, 9, "ART-shadow-report-009", "影子期周报 W9（测试）")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if reg.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %s, want sha256 of body", reg.Digest)
	}
	wantRef := "deploy/gitlab/peixun/reports/shadow-report-W9-20261015.md"
	if reg.ContentRef != wantRef {
		t.Fatalf("content_ref = %s, want %s", reg.ContentRef, wantRef)
	}
	if !json.Valid([]byte(reg.SummaryJSON)) {
		t.Fatalf("summary must be a valid JSON document: %q", reg.SummaryJSON)
	}
	var inner string
	if err := json.Unmarshal([]byte(reg.SummaryJSON), &inner); err != nil {
		t.Fatalf("summary must be one JSON document serialized as a string: %v", err)
	}
	summary := map[string]any{}
	if err := json.Unmarshal([]byte(inner), &summary); err != nil {
		t.Fatalf("inner summary document: %v", err)
	}
	if summary["week"].(float64) != 9 {
		t.Fatalf("summary week = %v, want 9", summary["week"])
	}
}

// TestAssembleRejectsBadAssetID pins the frozen store rule (S2B F7) at
// the driver boundary so the failure carries the rule text, not a bare
// store INVALID_PARAMETER.
func TestAssembleRejectsBadAssetID(t *testing.T) {
	report := filepath.Join(t.TempDir(), "r.md")
	if err := os.WriteFile(report, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := assemble(report, 1, "ART-detailed-design-a11", "")
	if err == nil || !strings.Contains(err.Error(), "ART-[a-z-]+-[0-9]{3,}") {
		t.Fatalf("want rule-bearing rejection, got: %v", err)
	}
}

// TestAssembleRejectsOutsideRepo keeps the ledger pointer repo-relative:
// a report that lives outside the resolved repo root must be rejected
// rather than registered with an absolute or escaping pointer.
func TestAssembleRejectsOutsideRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	_, err := assemble(outside, 1, "ART-shadow-report-001", "")
	if err == nil || !strings.Contains(err.Error(), "outside the repo root") {
		t.Fatalf("want outside-repo rejection, got: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Unit-level coverage for the asset-intake command: digest pinning,
// summary resolution, assets-dir copying, version derivation and the
// pointer-only rule for confidential artifacts. The PG-gated ledger
// behavior lives in internal/store (postgres_assets_test.go).

func TestHashFilePinsExactContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "research.md")
	content := "# 调研报告\n\n开源选型结论……\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	// sha256 of the raw bytes, not of a canonical JSON form.
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest shape: %q", digest)
	}
	again, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != again {
		t.Fatal("digest must be deterministic for identical content")
	}

	// A one-byte change must change the digest.
	if err := os.WriteFile(path, []byte(content+"x"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed == digest {
		t.Fatal("digest must change when content changes")
	}
}

func TestLoadSummaryRules(t *testing.T) {
	summary, err := loadSummary("", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(summary) != "[]" {
		t.Fatalf("default summary = %s, want []", summary)
	}

	summary, err = loadSummary("", "会话记录：完成 J2a 台账切片")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "J2a 台账切片") {
		t.Fatalf("note summary = %s", summary)
	}

	file := filepath.Join(t.TempDir(), "sheets.json")
	rows := `[{"sheet":"功能BOM","rows":112},{"sheet":"技术BOM","rows":40}]`
	if err := os.WriteFile(file, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	summary, err = loadSummary(file, "ignored-note")
	if err != nil {
		t.Fatal(err)
	}
	if string(summary) != rows {
		t.Fatalf("summary file wins over note: %s", summary)
	}

	if err := os.WriteFile(file, []byte(`{"not":"an array"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSummary(file, ""); err == nil {
		t.Fatal("non-array summary file must be rejected")
	}
	if err := os.WriteFile(file, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSummary(file, ""); err == nil {
		t.Fatal("empty summary file must be rejected")
	}
}

func TestCopyIntoAssetsDir(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(source, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	assetsDir := filepath.Join(dir, "assets")
	ref, err := copyIntoAssetsDir(assetsDir, "ART-research-001", source)
	if err != nil {
		t.Fatal(err)
	}
	// A relative assets dir yields a relative ledger pointer (the pilot
	// repo layout "assets/<asset_id>/<file>"); an absolute one yields an
	// absolute pointer.
	want := filepath.Join(assetsDir, "ART-research-001", "plan.md")
	if ref != want {
		t.Fatalf("ref = %s, want %s", ref, want)
	}
	copied, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != "body" {
		t.Fatalf("copied body = %q", copied)
	}
}

func TestNextAssetVersion(t *testing.T) {
	lookup := &fakeLatestLookup{asset: nil, err: store.ErrAssetNotFound}
	version, err := nextAssetVersion(context.Background(), lookup, "ART-research-001", "")
	if err != nil || version != 1 {
		t.Fatalf("first version = %d, err = %v", version, err)
	}
	lookup.asset, lookup.err = &store.Asset{Version: 3}, nil
	version, err = nextAssetVersion(context.Background(), lookup, "ART-research-001", "")
	if err != nil || version != 4 {
		t.Fatalf("next version = %d, err = %v", version, err)
	}
	version, err = nextAssetVersion(context.Background(), lookup, "ART-research-001", "ART-research-001@2")
	if err != nil || version != 3 {
		t.Fatalf("supersedes version = %d, err = %v", version, err)
	}
	if _, err := nextAssetVersion(context.Background(), lookup, "ART-research-001", "not-a-ref"); err == nil {
		t.Fatal("malformed supersedes ref must be rejected")
	}
}

func TestConfidentialArtifactsArePointerOnly(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runAssetIntake(context.Background(), []string{
		"--project", "018f7e00-0000-7000-8000-000000000099",
		"--asset-id", "ART-legacy-intake-001", "--type", "legacy-intake",
		"--title", "会话记录", "--owner", "integrator",
		"--mode", "file", "--path", "/tmp/plan.md",
		"--sensitivity", "confidential",
	}, streams{in: nil, out: &out, err: &errOut})
	if err == nil {
		t.Fatal("confidential + file mode must be refused before any IO")
	}
	var cmdErr *commandError
	if !errors.As(err, &cmdErr) || cmdErr.code != "USAGE_ERROR" {
		t.Fatalf("expected USAGE_ERROR, got %v", err)
	}
}

type fakeLatestLookup struct {
	asset *store.Asset
	err   error
}

func (f *fakeLatestLookup) LatestAsset(ctx context.Context, assetID string) (*store.Asset, error) {
	return f.asset, f.err
}

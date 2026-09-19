package store

import (
	"errors"
	"strings"
	"testing"
)

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// TestContentDigestPinsContentAndSize covers the intake wire digest:
// the happy path pins exact bytes and the reader failure surfaces
// verbatim (the ledger must not trust a half-read digest).
func TestContentDigestPinsContentAndSize(t *testing.T) {
	digest, size, err := ContentDigest(strings.NewReader("maestro"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 7 {
		t.Fatalf("size = %d, want 7", size)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest %q is not a sha256 wire digest", digest)
	}
	again, _, err := ContentDigest(strings.NewReader("maestro"))
	if err != nil || again != digest {
		t.Fatalf("digest not deterministic: %q vs %q (err %v)", digest, again, err)
	}

	boom := errors.New("boom")
	if _, _, err := ContentDigest(failingReader{boom}); !errors.Is(err, boom) {
		t.Fatalf("reader error must surface verbatim, got %v", err)
	}
}

// TestSplitAssetRefDisplayLookups covers the constraint-free ref split
// used by display and lookup paths, including the malformed refusal.
func TestSplitAssetRefDisplayLookups(t *testing.T) {
	id, version, err := SplitAssetRef("ART-bom-001@2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "ART-bom-001" || version != 2 {
		t.Fatalf("split = %q@%d, want ART-bom-001@2", id, version)
	}
	if _, _, err := SplitAssetRef("not-a-ref"); err == nil {
		t.Fatal("malformed ref must be refused")
	}
}

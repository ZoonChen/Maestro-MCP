package workgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CanonicalJSON re-encodes any JSON document with sorted object keys
// and no insignificant whitespace, mirroring store.CanonicalJSON: the
// store layer remains the SQL authority, this copy keeps the protocol
// package free of internal imports (digests must agree — both are
// json.Marshal over decoded values, which sorts map keys).
func CanonicalJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("workgraph: canonical json: %w", err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("workgraph: canonical json: %w", err)
	}
	return canonical, nil
}

// SpecDigest returns the sha256:<64hex> digest of the canonical form.
// Digests are for stale judgment only, never entity identity
// (WGM-007).
func SpecDigest(raw []byte) (string, error) {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

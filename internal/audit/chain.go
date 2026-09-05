// Package audit implements the M4-OBS-001 export core: the
// append-only audit chain over the durable audit_events table. Every
// entry's digest chains onto its predecessor, so any mutation of
// history breaks verification — and the export itself carries the
// rolling chain_digest the recipient recomputes (the frozen
// audit-export.schema.json).
//
// The package is pure over the row projection; persistence lives in
// internal/store.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Row is one durable audit event as exported (the wire entry shape).
type Row struct {
	Seq           int64
	EventID       string
	EventType     string
	Principal     string
	OccurredAt    string
	CorrelationID string
	Resource      string
	Action        string
	Decision      string
	EvidenceRefs  []string
}

// EntryDigest computes one row's digest: the canonical join of the
// identity fields plus the PREVIOUS entry's digest. The chain link is
// what makes rewriting history detectable.
func EntryDigest(row Row, prevDigest string) string {
	material := strings.Join([]string{
		strconv.FormatInt(row.Seq, 10),
		nullMark(row.EventID), nullMark(row.EventType), nullMark(row.Principal),
		nullMark(row.OccurredAt), nullMark(row.CorrelationID), nullMark(row.Resource),
		nullMark(row.Action), nullMark(row.Decision), joinRefs(row.EvidenceRefs),
		nullMark(prevDigest),
	}, "\x00")
	digest := sha256.Sum256([]byte(material))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// Chain walks rows in seq order and returns each entry's digest plus
// the rolling chain digest (empty input chains to itself as the empty
// digest sentinel).
func Chain(rows []Row) (entries []string, chain string) {
	prev := ""
	for _, row := range rows {
		next := EntryDigest(row, prev)
		entries = append(entries, next)
		prev = next
	}
	if prev == "" {
		sum := sha256.Sum256(nil)
		prev = "sha256:" + hex.EncodeToString(sum[:])
	}
	return entries, prev
}

// Verify recomputes the chain over rows and compares the claimed
// per-entry digests; the first mismatch reports its position.
func Verify(rows []Row, claimed []string) error {
	if len(rows) != len(claimed) {
		return fmt.Errorf("audit chain: %d rows but %d digests", len(rows), len(claimed))
	}
	recomputed, chain := Chain(rows)
	for index := range recomputed {
		if recomputed[index] != claimed[index] {
			return fmt.Errorf("audit chain: entry %d digest mismatch (chain %s)", index+1, chain)
		}
	}
	return nil
}

func nullMark(value string) string {
	if value == "" {
		return "\x00null"
	}
	return value
}

func joinRefs(refs []string) string {
	out := ""
	for _, ref := range refs {
		out += nullMark(ref) + ","
	}
	return out
}

package audit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleRows() []Row {
	return []Row{
		{Seq: 1, EventID: "e1", EventType: "http.request", Principal: "user-1", OccurredAt: "2026-09-05T10:00:00Z", CorrelationID: "c1", Action: "project.read", Decision: "allow"},
		{Seq: 2, EventID: "e2", EventType: "policy.decision", Principal: "user-2", OccurredAt: "2026-09-05T10:01:00Z", CorrelationID: "c2", Resource: "project:p1", Action: "waiver.approve", Decision: "deny", EvidenceRefs: []string{"ev-1"}},
		{Seq: 3, EventID: "e3", EventType: "tool.call", Principal: "agent-1", OccurredAt: "2026-09-05T10:02:00Z", CorrelationID: "c3", Action: "get_defect", Decision: "allow"},
	}
}

func TestChainVerifiesAndDetectsTampering(t *testing.T) {
	rows := sampleRows()
	entries, chain := Chain(rows)
	require.Len(t, entries, 3)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, chain)
	require.NoError(t, Verify(rows, entries))

	// Mutating ANY field of ANY historical row breaks verification at
	// that position (and everything chained after it).
	tampered := sampleRows()
	tampered[1].Decision = "allow" // history rewrite
	err := Verify(tampered, entries)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry 2")

	// Truncation and extension both mismatch.
	require.Error(t, Verify(rows[:2], entries))
	require.Error(t, Verify(append(sampleRows(), rows[0]), entries))

	// Empty chains verify against the empty sentinel.
	emptyEntries, emptyChain := Chain(nil)
	assert.Empty(t, emptyEntries)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, emptyChain)
	require.NoError(t, Verify(nil, nil))
}

func TestEntryDigestIsOrderAndContentSensitive(t *testing.T) {
	row := sampleRows()[0]

	// The prev link participates: same row, different predecessor.
	a := EntryDigest(row, "")
	b := EntryDigest(row, "sha256:"+repeat("a", 64))
	assert.NotEqual(t, a, b)

	// Seq participates.
	bumped := row
	bumped.Seq = 2
	assert.NotEqual(t, a, EntryDigest(bumped, ""))

	// Evidence refs participate.
	withRefs := row
	withRefs.EvidenceRefs = []string{"ev-9"}
	assert.NotEqual(t, a, EntryDigest(withRefs, ""))

	// Determinism.
	assert.Equal(t, a, EntryDigest(row, ""))
}

func repeat(ch string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ch[0]
	}
	return string(out)
}

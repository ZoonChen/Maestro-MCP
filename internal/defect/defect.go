// Package defect implements the M3 DEF/DSP normalization core
// (M3-DEF-001 + M3-DSP-001): six source adapters normalize findings,
// the versioned fingerprint derives unique defects, and duplicate
// events only grow occurrence history — first facts are never
// overwritten (DEF-INV-001/002).
//
// The package is pure and deterministic: fingerprints, signature
// normalization and severity mapping are functions of their inputs, so
// replay, tests and the shadow pipeline all converge on the same
// defect space. Persistence lives in internal/store.
package defect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SourceType enumerates the six normalized finding sources (the frozen
// finding.schema.json / events.yaml enum).
type SourceType string

const (
	SourcePipeline SourceType = "pipeline"
	SourceJUnit    SourceType = "junit"
	SourceContract SourceType = "contract"
	SourceSAST     SourceType = "sast"
	SourceSecret   SourceType = "secret"
	SourceManualQA SourceType = "manual_qa"
)

// Severity is the normalized severity scale.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// FingerprintAlgorithm versions the fingerprint derivation; any change
// to the composition or normalization bumps this and re-keys the
// defect space (old defects stay under their version).
const FingerprintAlgorithm = 1

// Finding is the normalized shape every adapter emits (the frozen
// finding.schema.json).
type Finding struct {
	FindingID      string
	ProjectID      string
	DefectID       string
	SourceType     SourceType
	SourceEventID  string
	Severity       Severity
	Occurrence     int
	EvidenceRefs   []string
	Environment    string
	Repro          string
	TaskRefs       []string
	AdapterVersion string
}

// FingerprintInput is the raw material DEF-INV-001 hashes: project,
// repository, target branch, test/rule identity and the normalized
// error signature. Paths must be normalized RELATIVE paths.
type FingerprintInput struct {
	ProjectID      string
	Repository     string
	Branch         string
	CheckID        string // test name / rule id / contract location
	ErrorSignature string
}

// Fingerprint derives the versioned defect identity: sha256 over the
// canonical join of the input parts. The composition is frozen by
// DEF-INV-001 — adding a part means a new algorithm version.
func Fingerprint(input FingerprintInput) string {
	parts := []string{
		"v1",
		NormalizePath(input.ProjectID),
		NormalizePath(input.Repository),
		NormalizePath(input.Branch),
		NormalizePath(input.CheckID),
		NormalizeSignature(input.ErrorSignature),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var absolutePath = regexp.MustCompile(`^(?:/|[A-Za-z]:[\\/]|(?:git@[^:]+):)`)

// NormalizePath canonicalizes a path component: forward slashes, no
// leading/trailing separators, and ABSOLUTE PATHS ARE REJECTED — a
// machine-local absolute path would fork the defect space per host.
func NormalizePath(path string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	for strings.HasPrefix(normalized, "./") {
		normalized = strings.TrimPrefix(normalized, "./")
	}
	normalized = strings.Trim(normalized, "/")
	// Reject absolute forms by reducing them to their tail: the frozen
	// rule says paths are relative, so /a/b and a/b are one identity.
	if absolutePath.MatchString(normalized) {
		if idx := strings.LastIndex(normalized, ":"); idx >= 0 && idx < len(normalized)-1 {
			normalized = normalized[idx+1:]
		} else if idx := strings.Index(normalized, "/"); idx >= 0 {
			normalized = normalized[idx+1:]
		}
		normalized = strings.Trim(normalized, "/")
	}
	return normalized
}

var (
	addrHex      = regexp.MustCompile(`\b0x[0-9a-fA-F]{4,}\b`)
	addrDec      = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	uuidLike     = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	timestamped  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ][0-9:.+Z-]+\b`)
	durationLike = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|µs|ms|s)\b`)
	goroutineRef = regexp.MustCompile(`goroutine \d+`)
)

// NormalizeSignature standardizes an error/stack signature: addresses,
// timestamps, durations, UUIDs, IPs and goroutine ids vanish (they are
// run-specific noise); function names, error types and line structure
// stay. The result is line-trimmed and capped.
func NormalizeSignature(signature string) string {
	lines := strings.Split(signature, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = addrHex.ReplaceAllString(line, "0xADDR")
		line = addrDec.ReplaceAllString(line, "IP")
		line = uuidLike.ReplaceAllString(line, "UUID")
		line = timestamped.ReplaceAllString(line, "TS")
		line = durationLike.ReplaceAllString(line, "DUR")
		line = goroutineRef.ReplaceAllString(line, "goroutine N")
		line = strings.TrimRight(line, " \t\r")
		if isPureNoise(line) {
			continue
		}
		if line != "" {
			out = append(out, line)
		}
	}
	normalized := strings.Join(out, "\n")
	if len(normalized) > 4096 {
		normalized = normalized[:4096]
	}
	return normalized
}

// isPureNoise reports a line that is nothing BUT normalized
// placeholders — timestamps/durations/etc. with no surviving
// signal. Dropping them keeps run-log verbosity from forking the
// defect space.
func isPureNoise(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	without := trimmed
	for _, placeholder := range []string{"0xADDR", "IP", "UUID", "TS", "DUR", "N"} {
		without = strings.ReplaceAll(without, placeholder, "")
	}
	return strings.Trim(without, " :.,()[]-\t") == ""
}

// SeverityRank orders the scale for aggregation.
func SeverityRank(severity Severity) int {
	switch severity {
	case SeverityCritical:
		return 5
	case SeverityHigh:
		return 4
	case SeverityMedium:
		return 3
	case SeverityLow:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// AggregateSeverity picks the loudest severity across a defect's
// occurrences: a defect is as severe as its worst finding.
func AggregateSeverity(severities []Severity) (Severity, error) {
	if len(severities) == 0 {
		return "", fmt.Errorf("defect: aggregate severity needs at least one finding")
	}
	best := SeverityInfo
	for _, severity := range severities {
		if !isValidSeverity(severity) {
			return "", fmt.Errorf("defect: unknown severity %q", severity)
		}
		if SeverityRank(severity) > SeverityRank(best) {
			best = severity
		}
	}
	return best, nil
}

func isValidSeverity(severity Severity) bool {
	switch severity {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return true
	}
	return false
}

// SortEvidenceRefs returns a deterministic evidence ordering.
func SortEvidenceRefs(refs []string) []string {
	out := append([]string(nil), refs...)
	sort.Strings(out)
	return out
}

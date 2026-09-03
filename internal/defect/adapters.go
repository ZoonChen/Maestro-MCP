package defect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The six source adapters. Each turns a raw external payload into the
// normalized Finding + FingerprintInput pair; anything the adapter
// cannot resolve fails closed — no auto-passed, half-invented checks
// (the m3 book's "未知字段不得自动映射" discipline).

// PipelineFinding is the raw shape of a failed CI pipeline job report.
type PipelineFinding struct {
	ProjectID  string
	Repository string
	Branch     string
	JobName    string // the check identity
	Stage      string
	LogExcerpt string
	ExitCode   int
	SourceSHA  string
	PipelineID string
	JobID      string
}

// FromPipeline normalizes a failed pipeline job. A zero/non-failing
// exit code is an error: adapters carry failures only.
func FromPipeline(raw PipelineFinding) (Finding, FingerprintInput, error) {
	if raw.JobName == "" || raw.PipelineID == "" || raw.JobID == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter pipeline: job name, pipeline and job ids are required")
	}
	if raw.ExitCode == 0 {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter pipeline: exit code 0 is not a failure")
	}
	finding := Finding{
		ProjectID:      raw.ProjectID,
		SourceType:     SourcePipeline,
		SourceEventID:  fmt.Sprintf("pipeline:%s:job:%s", raw.PipelineID, raw.JobID),
		Severity:       SeverityHigh, // CI failure defaults high; triage may lower
		EvidenceRefs:   []string{"pipeline:" + raw.PipelineID, "job:" + raw.JobID},
		Environment:    raw.Stage,
		Repro:          raw.LogExcerpt,
		AdapterVersion: "pipeline-adapter-v1",
	}
	input := FingerprintInput{
		ProjectID:      raw.ProjectID,
		Repository:     raw.Repository,
		Branch:         raw.Branch,
		CheckID:        raw.JobName,
		ErrorSignature: raw.LogExcerpt,
	}
	return finding, input, nil
}

// JUnitFinding is one failed testcase from a JUnit report.
type JUnitFinding struct {
	ProjectID  string
	Repository string
	Branch     string
	Suite      string
	TestClass  string
	TestName   string
	Message    string
	SourceSHA  string
	ReportRef  string
}

// FromJUnit normalizes a failed testcase; the check identity is
// suite/class/name.
func FromJUnit(raw JUnitFinding) (Finding, FingerprintInput, error) {
	if raw.TestClass == "" || raw.TestName == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter junit: test class and name are required")
	}
	checkID := strings.TrimPrefix(raw.TestClass+"."+raw.TestName, "./")
	finding := Finding{
		ProjectID:      raw.ProjectID,
		SourceType:     SourceJUnit,
		SourceEventID:  "junit:" + checkID + ":" + shortDigest(raw.Message),
		Severity:       SeverityMedium,
		EvidenceRefs:   []string{orDefault(raw.ReportRef, "junit-report")},
		Repro:          raw.Message,
		AdapterVersion: "junit-adapter-v1",
	}
	input := FingerprintInput{
		ProjectID:      raw.ProjectID,
		Repository:     raw.Repository,
		Branch:         raw.Branch,
		CheckID:        checkID,
		ErrorSignature: raw.Message,
	}
	return finding, input, nil
}

// ContractFinding is a breaking-change verdict from the CTR engine.
type ContractFinding struct {
	ProjectID  string
	Repository string
	Branch     string
	Service    string
	Location   string // e.g. responses.200.properties.count
	Detail     string
	Provider   string
	Consumer   string
}

// FromContract normalizes a breaking diff entry; the check identity is
// service+location.
func FromContract(raw ContractFinding) (Finding, FingerprintInput, error) {
	if raw.Service == "" || raw.Location == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter contract: service and location are required")
	}
	checkID := "contract:" + raw.Service + ":" + NormalizePath(raw.Location)
	finding := Finding{
		ProjectID:      raw.ProjectID,
		SourceType:     SourceContract,
		SourceEventID:  checkID + ":" + shortDigest(raw.Detail),
		Severity:       SeverityHigh,
		EvidenceRefs:   []string{"provider:" + orDefault(raw.Provider, "unknown"), "consumer:" + orDefault(raw.Consumer, "unknown")},
		Repro:          raw.Detail,
		AdapterVersion: "contract-adapter-v1",
	}
	input := FingerprintInput{
		ProjectID:      raw.ProjectID,
		Repository:     raw.Repository,
		Branch:         raw.Branch,
		CheckID:        checkID,
		ErrorSignature: raw.Detail,
	}
	return finding, input, nil
}

// ScanFinding is one SAST or secret-scanner hit. Secret payloads carry
// NO raw secret material here — only the rule identity and a masked
// excerpt (content stays behind the evidence reference).
type ScanFinding struct {
	ProjectID  string
	Repository string
	Branch     string
	Tool       string // sast tool or secret scanner name
	RuleID     string
	FilePath   string
	Line       int
	Excerpt    string // already masked by the producer
	IsSecret   bool
}

// FromScan normalizes a SAST/secret hit; the check identity is
// tool+rule(+path for code-local rules).
func FromScan(raw ScanFinding) (Finding, FingerprintInput, error) {
	if raw.Tool == "" || raw.RuleID == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter scan: tool and rule are required")
	}
	checkID := raw.Tool + ":" + raw.RuleID
	if raw.IsSecret {
		// Secret rules key on rule+path: the same leaked rule at a new
		// path is a NEW exposure, not a recurrence.
		checkID += ":" + NormalizePath(raw.FilePath)
		// Defense in depth: the adapter masks credential-shaped
		// prefixes itself — a leaky producer must not leak through.
		raw.Excerpt = maskCredentialPrefixes(raw.Excerpt)
	}
	severity := SeverityHigh
	if raw.IsSecret {
		severity = SeverityCritical
	}
	finding := Finding{
		ProjectID:      raw.ProjectID,
		SourceType:     SourceSAST,
		Severity:       severity,
		SourceEventID:  checkID + ":" + shortDigest(raw.Excerpt),
		EvidenceRefs:   []string{"scan:" + raw.Tool},
		Repro:          raw.Excerpt,
		AdapterVersion: "scan-adapter-v1",
	}
	if raw.IsSecret {
		finding.SourceType = SourceSecret
		finding.AdapterVersion = "secret-adapter-v1"
	}
	input := FingerprintInput{
		ProjectID:      raw.ProjectID,
		Repository:     raw.Repository,
		Branch:         raw.Branch,
		CheckID:        checkID,
		ErrorSignature: raw.Excerpt,
	}
	return finding, input, nil
}

// ManualQAFinding is a human-reported defect; repro is mandatory.
type ManualQAFinding struct {
	ProjectID  string
	Repository string
	Branch     string
	Reporter   string
	Title      string
	Repro      string
	Severity   Severity
}

// FromManualQA normalizes a QA report; the frozen schema requires repro
// for manual findings.
func FromManualQA(raw ManualQAFinding) (Finding, FingerprintInput, error) {
	if strings.TrimSpace(raw.Repro) == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter manual_qa: repro is mandatory")
	}
	if raw.Reporter == "" || raw.Title == "" {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter manual_qa: reporter and title are required")
	}
	severity := raw.Severity
	if !isValidSeverity(severity) {
		return Finding{}, FingerprintInput{}, fmt.Errorf("defect adapter manual_qa: unknown severity %q", raw.Severity)
	}
	finding := Finding{
		ProjectID:      raw.ProjectID,
		SourceType:     SourceManualQA,
		SourceEventID:  "manual:" + raw.Reporter + ":" + shortDigest(raw.Title+"\n"+raw.Repro),
		Severity:       severity,
		EvidenceRefs:   []string{"manual:" + raw.Reporter},
		Repro:          raw.Repro,
		AdapterVersion: "manual-qa-adapter-v1",
	}
	input := FingerprintInput{
		ProjectID:      raw.ProjectID,
		Repository:     raw.Repository,
		Branch:         raw.Branch,
		CheckID:        "manual:" + raw.Title,
		ErrorSignature: raw.Repro,
	}
	return finding, input, nil
}

// credentialPrefixes are provider key shapes the secret adapter masks
// regardless of producer behavior (the first block only — the scanner
// owns full redaction).
var credentialPrefixes = []string{"AKIA", "ASIA", "sk-", "ghp_", "gho_", "xoxb-", "-----BEGIN"}

func maskCredentialPrefixes(excerpt string) string {
	for _, prefix := range credentialPrefixes {
		if idx := strings.Index(excerpt, prefix); idx >= 0 {
			excerpt = excerpt[:idx] + prefix + "[REDACTED]"
		}
	}
	return excerpt
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func shortDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	hexed := hex.EncodeToString(sum[:])
	if len(hexed) > 16 {
		return hexed[:16]
	}
	return hexed
}

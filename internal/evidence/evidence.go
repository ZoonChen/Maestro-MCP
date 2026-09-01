package evidence

import (
	"fmt"
	"regexp"
)

// Evidence statuses from the frozen enum; every record carries one —
// absence is never a pass.
const (
	EvidencePassed    = "passed"
	EvidenceFailed    = "failed"
	EvidenceError     = "error"
	EvidenceCancelled = "cancelled"
	EvidenceSkipped   = "skipped"
)

// Authority levels: merge_gate evidence originates only from verified
// GitLab ingestion; diagnostic evidence (runner profiles, human QA) can
// never satisfy a required gate (EVIDENCE-REQ-002, TC-EVIDENCE-004).
const (
	AuthorityMergeGate  = "merge_gate"
	AuthorityDiagnostic = "diagnostic"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// Record mirrors the frozen evidence.schema.json. PipelineID/JobID are
// the GitLab numeric identifiers; Supersedes implements the append-only
// correction chain (EVIDENCE-RULE-001) carried by the store.
type Record struct {
	EvidenceID    string   `json:"evidence_id"`
	ProjectID     string   `json:"project_id"`
	WorkItemID    string   `json:"work_item_id"`
	Kind          string   `json:"kind"`
	Authority     string   `json:"authority"`
	Status        string   `json:"status"`
	SourceSHA     string   `json:"source_sha"`
	TargetSHA     string   `json:"target_sha"`
	PipelineID    *int64   `json:"pipeline_id"`
	JobID         *int64   `json:"job_id"`
	PolicyVersion string   `json:"policy_version"`
	Producer      Producer `json:"producer"`
	Attempt       int      `json:"attempt"`
	Summary       string   `json:"summary,omitempty"`
	ParserVersion string   `json:"parser_version,omitempty"`
	Supersedes    string   `json:"supersedes_id,omitempty"`
}

// Producer identifies the originating system and its version.
type Producer struct {
	Type    string `json:"type"` // gitlab_job | runner_profile | human_qa
	ID      string `json:"id"`
	Version string `json:"version"`
}

// Validate enforces the frozen schema, including the authority-conditional
// rules: merge_gate requires pipeline+job and a gitlab_job producer;
// diagnostic forbids pipeline/job and requires a runner/human producer.
func (r *Record) Validate() error {
	if r.EvidenceID == "" {
		return fmt.Errorf("evidence: evidence_id must not be empty")
	}
	if _, known := gateKinds[r.Kind]; !known {
		return fmt.Errorf("evidence %s: kind %q is outside the frozen enum", r.EvidenceID, r.Kind)
	}
	switch r.Authority {
	case AuthorityMergeGate, AuthorityDiagnostic:
	default:
		return fmt.Errorf("evidence %s: authority %q is outside the enum", r.EvidenceID, r.Authority)
	}
	switch r.Status {
	case EvidencePassed, EvidenceFailed, EvidenceError, EvidenceCancelled, EvidenceSkipped:
	default:
		return fmt.Errorf("evidence %s: status %q is outside the enum", r.EvidenceID, r.Status)
	}
	if !shaPattern.MatchString(r.SourceSHA) || !shaPattern.MatchString(r.TargetSHA) {
		return fmt.Errorf("evidence %s: source/target SHA must be 40-64 lowercase hex", r.EvidenceID)
	}
	if r.PolicyVersion == "" || len(r.PolicyVersion) > 128 {
		return fmt.Errorf("evidence %s: policy_version must be 1-128 chars", r.EvidenceID)
	}
	if r.Attempt < 1 {
		return fmt.Errorf("evidence %s: attempt must be >= 1", r.EvidenceID)
	}
	switch r.Producer.Type {
	case "gitlab_job", "runner_profile", "human_qa":
	default:
		return fmt.Errorf("evidence %s: producer type %q is outside the enum", r.EvidenceID, r.Producer.Type)
	}
	if r.Producer.ID == "" || r.Producer.Version == "" {
		return fmt.Errorf("evidence %s: producer id and version are required", r.EvidenceID)
	}
	if r.Authority == AuthorityMergeGate {
		if r.PipelineID == nil || *r.PipelineID < 1 || r.JobID == nil || *r.JobID < 1 {
			return fmt.Errorf("evidence %s: merge_gate authority requires pipeline_id and job_id", r.EvidenceID)
		}
		if r.Producer.Type != "gitlab_job" {
			return fmt.Errorf("evidence %s: merge_gate authority requires a gitlab_job producer", r.EvidenceID)
		}
	} else {
		if r.PipelineID != nil || r.JobID != nil {
			return fmt.Errorf("evidence %s: diagnostic authority must not carry pipeline/job ids", r.EvidenceID)
		}
		if r.Producer.Type == "gitlab_job" {
			return fmt.Errorf("evidence %s: diagnostic authority cannot have a gitlab_job producer", r.EvidenceID)
		}
	}
	return nil
}

// MatchesTuple reports whether the record is bound to the exact SHA
// tuple under evaluation (EVIDENCE-RULE-002/004).
func (r *Record) MatchesTuple(sourceSHA, targetSHA string) bool {
	return r.SourceSHA == sourceSHA && r.TargetSHA == targetSHA
}

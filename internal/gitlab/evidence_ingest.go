package gitlab

import (
	"context"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
	"github.com/google/uuid"
)

// Evidence ingestion from provider facts (M2-QG-001 × EVIDENCE §3/§7):
// a CI job whose NAME is exactly one of the frozen gate kinds produces
// merge_gate evidence bound to the MR projection's exact SHA tuple.
// Unknown job names, non-terminal statuses and incomplete tuples
// produce NOTHING — an unrecognized producer never auto-maps to passed
// (m2 delivery book section 7).
type EvidenceIngestor struct {
	// Eval is the evaluation trigger; nil leaves ingestion append-only.
	Eval *evidence.Service
	// PolicyVersion labels ingested records (the company baseline
	// version at wiring time — provenance, the evaluation binds its own
	// resolved version).
	PolicyVersion string
	// Append persists one immutable record.
	Append EvidenceAppender
	// Tuples resolves the work item and SHA tuple for a branch (the MR
	// projection of the same source branch).
	Tuples BranchTupleResolver
	Now    func() time.Time
}

// now defaults to the wall clock (test injectable).

// EvidenceAppender persists evidence records.
type EvidenceAppender interface {
	AppendEvidence(ctx context.Context, record *evidence.Record) error
}

// BranchTupleResolver maps a task branch to its bound work item and
// the MR projection's SHA tuple; complete=false while the projection
// lacks either SHA (out-of-order job-before-MR events wait for the
// tuple, mirroring the job-before-pipeline deferral).
type BranchTupleResolver interface {
	BranchTuple(ctx context.Context, projectID, sourceBranch string) (workItemID, sourceSHA, targetSHA string, complete bool, err error)
}

// terminalJobStatuses maps GitLab job states onto the frozen evidence
// statuses. Running/pending/created/manual are not outcomes and carry
// no evidence.
var jobStatusToEvidence = map[string]string{
	"success":   evidence.EvidencePassed,
	"failed":    evidence.EvidenceFailed,
	"canceled":  evidence.EvidenceCancelled,
	"cancelled": evidence.EvidenceCancelled,
	"skipped":   evidence.EvidenceSkipped,
}

// IngestJob derives and persists evidence for one completed job, then
// re-evaluates the tuple. The returned applied flag reports that the
// job produced evidence (terminal status AND recognized kind AND a
// complete tuple).
func (ing *EvidenceIngestor) IngestJob(ctx context.Context, projectID string, job JobRecord, jobSHA string) (bool, error) {
	status, terminal := jobStatusToEvidence[job.Status]
	if !terminal {
		return false, nil
	}
	if !isGateKind(job.Name) {
		// Unknown producers never become evidence.
		return false, nil
	}
	workItemID, sourceSHA, targetSHA, complete, err := ing.Tuples.BranchTuple(ctx, projectID, jobBranch(job))
	if err != nil {
		return false, err
	}
	if !complete {
		return false, nil
	}
	// Exact-SHA binding (EVIDENCE-RULE-002): a job that ran a different
	// commit than the tuple's source never becomes evidence for it.
	if jobSHA != "" && jobSHA != sourceSHA {
		return false, nil
	}

	pipelineID, jobID := job.PipelineID, job.JobID
	record := &evidence.Record{
		EvidenceID:    stableEvidenceID(projectID, workItemID, job),
		ProjectID:     projectID,
		WorkItemID:    workItemID,
		Kind:          job.Name,
		Authority:     evidence.AuthorityMergeGate,
		Status:        status,
		SourceSHA:     sourceSHA,
		TargetSHA:     targetSHA,
		PipelineID:    &pipelineID,
		JobID:         &jobID,
		PolicyVersion: ing.PolicyVersion,
		Producer:      evidence.Producer{Type: "gitlab_job", ID: job.Name, Version: "gitlab-ci"},
		Attempt:       1,
	}
	if err := ing.Append.AppendEvidence(ctx, record); err != nil {
		return false, fmt.Errorf("evidence ingest: append: %w", err)
	}

	if ing.Eval != nil {
		_, err := ing.Eval.EvaluateWorkItem(ctx, evidence.Tuple{
			ProjectID: projectID, WorkItemID: workItemID,
			SourceSHA: sourceSHA, TargetSHA: targetSHA,
		})
		if err != nil {
			return true, fmt.Errorf("evidence ingest: evaluate: %w", err)
		}
	}
	return true, nil
}

// jobBranch reads the ref the job payload carried (the task branch).
func jobBranch(job JobRecord) string { return job.Ref }

// stableEvidenceID derives the record identity from the external
// facts: the same pipeline+job re-delivered collapses onto one record.
var evidenceNamespace = uuid.MustParse("019207c0-0000-7000-8000-00000000aaa2")

func stableEvidenceID(projectID, workItemID string, job JobRecord) string {
	return uuid.NewSHA1(evidenceNamespace, []byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d",
		projectID, workItemID, job.PipelineID, job.JobID))).String()
}

func isGateKind(name string) bool {
	switch name {
	case evidence.GateBaselineFreshness, evidence.GateBoundary, evidence.GatePolicyIntegrity,
		evidence.GateBuild, evidence.GateUnit, evidence.GateLintTypecheck, evidence.GateCoverage,
		evidence.GateIntegration, evidence.GateContract, evidence.GateSecretScan, evidence.GateSAST,
		evidence.GateDependency, evidence.GateImage, evidence.GateLicense:
		return true
	}
	return false
}

// OnTupleComplete re-evaluates when an MR projection completes the
// tuple (jobs may already have evidence waiting — EVIDENCE §8
// out-of-order convergence).
func (ing *EvidenceIngestor) OnTupleComplete(ctx context.Context, tup evidence.Tuple) error {
	if ing.Eval == nil {
		return nil
	}
	_, err := ing.Eval.EvaluateWorkItem(ctx, tup)
	return err
}

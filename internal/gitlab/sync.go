// Package gitlab implements the M2-GL/MR synchronization half: verified
// webhook bodies become merge-request, pipeline and job projections,
// and a merged merge request — bound to a work item through the frozen
// task-branch naming — drives the fact-bound ready_for_human_merge →
// done edge (GL-INV-003). The connector's outbound API client (bot
// token, reconciliation) lands with S4a part 1; this package consumes
// only already-verified deliveries from the webhook inbox.
package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
)

// MergeRequestRecord is the projection of one merge-request payload.
type MergeRequestRecord struct {
	InstanceID    string
	GitlabProject int64
	IID           int64
	State         string // opened | closed | locked | merged
	SourceBranch  string
	TargetBranch  string
	SourceSHA     string
	TargetSHA     string
	MergeCommit   string
	MergedAt      string // RFC3339, empty when unmerged
}

// PipelineRecord is the projection of one pipeline payload.
type PipelineRecord struct {
	InstanceID    string
	ProjectID     string // resolved Maestro project
	GitlabProject int64
	PipelineID    int64
	SHA           string
	Ref           string
	Status        string
	Source        string
}

// JobRecord is the projection of one job payload.
type JobRecord struct {
	InstanceID    string
	GitlabProject int64
	PipelineID    int64
	JobID         int64
	Name          string
	Status        string
	Stage         string
	Ref           string
}

// SyncStore is the persistence contract for projection writes and the
// fact-bound done transition.
type SyncStore interface {
	// MappingProject resolves the Maestro project for an instance's
	// numeric GitLab project ("" when unmapped).
	MappingProject(ctx context.Context, instanceID string, gitlabProjectID int64) (string, error)

	// ResolveBranchBinding resolves the branch naming contract's project
	// segment (W6-2): maestro/<project-key>/<item> binds against the
	// project whose key matches, scoped to the mapping project's team so
	// shared keys across teams stay unambiguous. bound=false leaves the
	// projection unbound (reconciliation territory) — the composite FK
	// on (project_id, work_item_id) then never rejects the projection.
	ResolveBranchBinding(ctx context.Context, mappingProject, projectKey, workItemID string) (bindProject string, bound bool, err error)

	// UpsertMergeRequest inserts or refreshes the merge-request
	// projection, binding work_item_id through the frozen task-branch
	// naming when the branch resolves to a work item in the project.
	UpsertMergeRequest(ctx context.Context, projectID string, rec MergeRequestRecord, workItemID string) error

	// MarkWorkItemDoneFromMerge applies the fact-bound transition. The
	// first bool reports whether THIS call performed the transition; the
	// second reports a WITHHELD fact (the work item sits outside
	// ready_for_human_merge — nothing regresses, reconciliation owns the
	// drift). An already-done item is an idempotent no-op.
	MarkWorkItemDoneFromMerge(ctx context.Context, projectID, workItemID, mergeCommitSHA, factID string) (transitioned, withheld bool, err error)

	UpsertPipeline(ctx context.Context, rec PipelineRecord) error
	UpsertJob(ctx context.Context, rec JobRecord) error

	// BranchTuple resolves the MR projection's binding and SHA tuple
	// for a source branch (evidence ingestion). The first return is the
	// BRANCH-resolved project the projection lives under (W6-2).
	BranchTuple(ctx context.Context, projectID, sourceBranch string) (resolvedProject, workItemID, sourceSHA, targetSHA string, complete bool, err error)
}

// Syncer applies one verified raw webhook body to the projections.
// Ingest (optional) turns completed gate-named jobs into evidence and
// re-evaluates the bound tuple.
type Syncer struct {
	Store  SyncStore
	Ingest *EvidenceIngestor
}

// ApplyOutcome summarizes one applied delivery.
type ApplyOutcome struct {
	Kind            string
	Transitioned    bool // a work item moved ready_for_human_merge -> done
	Withheld        bool // a merged fact arrived outside ready: recorded, not applied
	EvidenceApplied bool // a completed gate-named job produced evidence
}

// mrPayload reads just the fields the projection needs.
type mrPayload struct {
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	ObjectAttributes struct {
		IID          int64  `json:"iid"`
		State        string `json:"state"`
		Action       string `json:"action"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		MergedAt       string `json:"merged_at"`
		DiffRefs       struct {
			BaseSHA  string `json:"base_sha"`
			HeadSHA  string `json:"head_sha"`
			StartSHA string `json:"start_sha"`
		} `json:"diff_refs"`
	} `json:"object_attributes"`
}

type pipelinePayload struct {
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	ObjectAttributes struct {
		ID     int64  `json:"id"`
		SHA    string `json:"sha"`
		Ref    string `json:"ref"`
		Status string `json:"status"`
		Source string `json:"source"`
	} `json:"object_attributes"`
}

type jobPayload struct {
	ProjectID  int64  `json:"project_id"`
	PipelineID int64  `json:"pipeline_id"`
	JobID      int64  `json:"build_id"`
	Name       string `json:"build_name"`
	Status     string `json:"build_status"`
	Stage      string `json:"stage"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
}

// ApplyBody dispatches one raw body by its kind marker. The caller has
// already verified and decrypted the delivery; unknown shapes are
// errors, never silent skips.
func (s *Syncer) ApplyBody(ctx context.Context, instanceID, kind string, body []byte) (ApplyOutcome, error) {
	switch kind {
	case "merge_request":
		return s.applyMergeRequest(ctx, instanceID, body)
	case "pipeline":
		return s.applyPipeline(ctx, instanceID, body)
	case "job":
		return s.applyJob(ctx, instanceID, body)
	case "push":
		// Branch-head movements are reconciliation territory (the
		// connector pulls ref updates); nothing to project here.
		return ApplyOutcome{Kind: kind}, nil
	default:
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: unknown event kind %q", kind)
	}
}

func (s *Syncer) applyMergeRequest(ctx context.Context, instanceID string, body []byte) (ApplyOutcome, error) {
	var payload mrPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: merge-request body: %w", err)
	}
	attrs := payload.ObjectAttributes
	if payload.Project.ID < 1 || attrs.IID < 1 || attrs.SourceBranch == "" || attrs.TargetBranch == "" {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: merge-request payload lacks project/iid/branches")
	}
	rec := MergeRequestRecord{
		InstanceID:    instanceID,
		GitlabProject: payload.Project.ID,
		IID:           attrs.IID,
		State:         normalizeMRState(attrs.State),
		SourceBranch:  attrs.SourceBranch,
		TargetBranch:  attrs.TargetBranch,
		SourceSHA:     attrs.DiffRefs.HeadSHA,
		TargetSHA:     attrs.DiffRefs.BaseSHA,
		MergeCommit:   attrs.MergeCommitSHA,
		MergedAt:      attrs.MergedAt,
	}
	if rec.SourceSHA == "" {
		rec.SourceSHA = attrs.LastCommit.ID
	}
	projectID, err := s.Store.MappingProject(ctx, instanceID, payload.Project.ID)
	if err != nil {
		return ApplyOutcome{}, err
	}
	return s.ApplyMergeRequestRecord(ctx, projectID, rec, mergeFactID(instanceID, rec))
}

// ApplyMergeRequestRecord applies one MR fact (webhook-shaped or
// provider-pulled) through the single truth path: bind by branch
// marker, upsert the projection, drive the done edge on merged facts.
//
// The projection lands under the BRANCH-resolved project when the
// naming contract's key resolves (W6-2): maestro/<project-key>/<item>
// is authoritative for the (project, work item) binding, so a merged
// MR raised from a repo mapped to another project (the pilot's
// cross-governance-domain flow) still binds and never trips the
// composite foreign key. Unresolvable markers leave the projection
// under the mapping project, unbound.
func (s *Syncer) ApplyMergeRequestRecord(ctx context.Context, projectID string, rec MergeRequestRecord, factID string) (ApplyOutcome, error) {
	workItemID := ""
	bindProject := projectID
	if key, item := BranchMarker(rec.SourceBranch); key != "" {
		resolved, bound, err := s.Store.ResolveBranchBinding(ctx, projectID, key, item)
		if err != nil {
			return ApplyOutcome{}, err
		}
		if bound {
			bindProject, workItemID = resolved, item
		}
	}
	if err := s.Store.UpsertMergeRequest(ctx, bindProject, rec, workItemID); err != nil {
		return ApplyOutcome{}, err
	}
	// Tuple completion re-evaluates: evidence may already be waiting
	// from jobs that ran before the MR projection carried both SHAs.
	if s.Ingest != nil && workItemID != "" && rec.SourceSHA != "" && rec.TargetSHA != "" {
		if err := s.Ingest.OnTupleComplete(ctx, TupleFor(bindProject, workItemID, rec)); err != nil {
			return ApplyOutcome{}, err
		}
	}
	if rec.State != "merged" || rec.MergeCommit == "" || bindProject == "" || workItemID == "" {
		return ApplyOutcome{Kind: "merge_request"}, nil
	}
	transitioned, withheld, err := s.Store.MarkWorkItemDoneFromMerge(ctx, bindProject, workItemID, rec.MergeCommit, factID)
	if err != nil {
		return ApplyOutcome{}, err
	}
	return ApplyOutcome{Kind: "merge_request", Transitioned: transitioned, Withheld: withheld}, nil
}

func (s *Syncer) applyPipeline(ctx context.Context, instanceID string, body []byte) (ApplyOutcome, error) {
	var payload pipelinePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: pipeline body: %w", err)
	}
	if payload.Project.ID < 1 || payload.ObjectAttributes.ID < 1 || payload.ObjectAttributes.SHA == "" {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: pipeline payload lacks project/id/sha")
	}
	projectID, err := s.Store.MappingProject(ctx, instanceID, payload.Project.ID)
	if err != nil {
		return ApplyOutcome{}, err
	}
	if projectID == "" {
		return ApplyOutcome{Kind: "pipeline"}, nil
	}
	if err := s.Store.UpsertPipeline(ctx, PipelineRecord{
		InstanceID:    instanceID,
		ProjectID:     projectID,
		GitlabProject: payload.Project.ID,
		PipelineID:    payload.ObjectAttributes.ID,
		SHA:           payload.ObjectAttributes.SHA,
		Ref:           payload.ObjectAttributes.Ref,
		Status:        payload.ObjectAttributes.Status,
		Source:        payload.ObjectAttributes.Source,
	}); err != nil {
		return ApplyOutcome{}, err
	}
	return ApplyOutcome{Kind: "pipeline"}, nil
}

func (s *Syncer) applyJob(ctx context.Context, instanceID string, body []byte) (ApplyOutcome, error) {
	var payload jobPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: job body: %w", err)
	}
	if payload.ProjectID < 1 || payload.PipelineID < 1 || payload.JobID < 1 {
		return ApplyOutcome{}, fmt.Errorf("gitlab sync: job payload lacks project/pipeline/job")
	}
	job := JobRecord{
		InstanceID:    instanceID,
		GitlabProject: payload.ProjectID,
		PipelineID:    payload.PipelineID,
		JobID:         payload.JobID,
		Name:          payload.Name,
		Status:        payload.Status,
		Stage:         payload.Stage,
		Ref:           payload.Ref,
	}
	if err := s.Store.UpsertJob(ctx, job); err != nil {
		return ApplyOutcome{}, err
	}
	if s.Ingest != nil {
		projectID, err := s.Store.MappingProject(ctx, instanceID, payload.ProjectID)
		if err != nil {
			return ApplyOutcome{}, err
		}
		// W7-5 (F26): a terminal gate-named job on a marker branch whose
		// evidence tuple is not yet complete used to be DROPPED — when
		// its terminal event raced the first reconciliation that builds
		// the tuple, the evidence vanished and only a manual job retry
		// (a brand-new job arriving after the tuple) could recover it
		// (S2B10: one retry round lost per incident). Deferring the
		// whole delivery instead rides the SAME outbox replay path the
		// pipeline-deferral above uses: the event replays after
		// reconciliation completes the tuple and mints its evidence.
		// Deferral is bounded to marker branches (maestro/<key>/<item>):
		// team-natural branches never carry governance evidence.
		if deferred, deferErr := s.jobEvidenceDeferred(ctx, projectID, job); deferErr != nil {
			return ApplyOutcome{}, deferErr
		} else if deferred {
			return ApplyOutcome{}, fmt.Errorf(
				"gitlab sync: job %d terminal on %s but tuple incomplete, evidence deferred", job.JobID, job.Ref)
		}
		if applied, ingestErr := s.Ingest.IngestJob(ctx, projectID, job, payload.SHA); ingestErr != nil {
			return ApplyOutcome{}, ingestErr
		} else if applied {
			return ApplyOutcome{Kind: "job", EvidenceApplied: true}, nil
		}
	}
	return ApplyOutcome{Kind: "job"}, nil
}

// jobEvidenceDeferred reports whether a terminal gate-named job on a
// marker branch must wait for its evidence tuple: the branch resolves
// through the naming contract but the MR projection's SHA tuple is not
// complete yet. Non-terminal jobs, non-gate producers and unmarked
// branches never defer.
func (s *Syncer) jobEvidenceDeferred(ctx context.Context, projectID string, job JobRecord) (bool, error) {
	if _, terminal := jobStatusToEvidence[job.Status]; !terminal {
		return false, nil
	}
	if !isGateKind(job.Name) {
		return false, nil
	}
	key, _ := BranchMarker(jobBranch(job))
	if key == "" {
		return false, nil
	}
	_, _, _, _, complete, err := s.Store.BranchTuple(ctx, projectID, jobBranch(job))
	if err != nil {
		return false, err
	}
	return !complete, nil
}

// TupleFor builds the evaluation tuple from an MR record.
func TupleFor(projectID, workItemID string, rec MergeRequestRecord) evidence.Tuple {
	return evidence.Tuple{
		ProjectID:  projectID,
		WorkItemID: workItemID,
		SourceSHA:  rec.SourceSHA,
		TargetSHA:  rec.TargetSHA,
	}
}

// WorkItemIDFromBranch reads the task marker out of the frozen task
// branch naming maestro/<project-key>/<task-id>. Anything else (target
// branches, manual branches) has no marker and returns "".
func WorkItemIDFromBranch(branch string) string {
	_, taskID := BranchMarker(branch)
	return taskID
}

// BranchMarker splits the frozen task branch naming into its project
// key and work item segments (W6-2): the KEY segment names the project
// the branch's work item belongs to, which is what the binding resolves
// against — not the repository mapping. Empty key means no marker.
func BranchMarker(branch string) (projectKey, workItemID string) {
	prefix, rest, found := strings.Cut(branch, "/")
	if !found || prefix != "maestro" {
		return "", ""
	}
	key, taskID, found := strings.Cut(rest, "/")
	if !found || key == "" || taskID == "" {
		return "", ""
	}
	return key, taskID
}

// mergeFactID is the durable lineage recorded on the work item: the
// instance and external identity of the merged merge request.
func mergeFactID(instanceID string, rec MergeRequestRecord) string {
	return fmt.Sprintf("gitlab:%s:mr:%d", instanceID, rec.IID)
}

// normalizeMRState maps GitLab states onto the frozen projection enum.
func normalizeMRState(state string) string {
	switch strings.TrimSpace(state) {
	case "opened", "closed", "locked", "merged":
		return strings.TrimSpace(state)
	case "":
		return "opened"
	default:
		// Unknown states never masquerade as merged.
		return "locked"
	}
}

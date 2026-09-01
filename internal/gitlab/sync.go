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
}

// SyncStore is the persistence contract for projection writes and the
// fact-bound done transition.
type SyncStore interface {
	// MappingProject resolves the Maestro project for an instance's
	// numeric GitLab project ("" when unmapped).
	MappingProject(ctx context.Context, instanceID string, gitlabProjectID int64) (string, error)

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
}

// Syncer applies one verified raw webhook body to the projections.
type Syncer struct {
	Store SyncStore
}

// ApplyOutcome summarizes one applied delivery.
type ApplyOutcome struct {
	Kind         string
	Transitioned bool // a work item moved ready_for_human_merge -> done
	Withheld     bool // a merged fact arrived outside ready: recorded, not applied
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
	state := normalizeMRState(attrs.State)
	rec := MergeRequestRecord{
		InstanceID:    instanceID,
		GitlabProject: payload.Project.ID,
		IID:           attrs.IID,
		State:         state,
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
	workItemID := WorkItemIDFromBranch(attrs.SourceBranch)
	if err := s.Store.UpsertMergeRequest(ctx, projectID, rec, workItemID); err != nil {
		return ApplyOutcome{}, err
	}

	if state != "merged" || rec.MergeCommit == "" || projectID == "" || workItemID == "" {
		return ApplyOutcome{Kind: "merge_request"}, nil
	}
	transitioned, withheld, err := s.Store.MarkWorkItemDoneFromMerge(ctx, projectID, workItemID, rec.MergeCommit, mergeFactID(instanceID, rec))
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
	if err := s.Store.UpsertJob(ctx, JobRecord{
		InstanceID:    instanceID,
		GitlabProject: payload.ProjectID,
		PipelineID:    payload.PipelineID,
		JobID:         payload.JobID,
		Name:          payload.Name,
		Status:        payload.Status,
		Stage:         payload.Stage,
	}); err != nil {
		return ApplyOutcome{}, err
	}
	return ApplyOutcome{Kind: "job"}, nil
}

// WorkItemIDFromBranch reads the task marker out of the frozen task
// branch naming maestro/<project-key>/<task-id>. Anything else (target
// branches, manual branches) has no marker and returns "".
func WorkItemIDFromBranch(branch string) string {
	prefix, rest, found := strings.Cut(branch, "/")
	if !found || prefix != "maestro" {
		return ""
	}
	marker, taskID, found := strings.Cut(rest, "/")
	if !found || marker == "" {
		return ""
	}
	return taskID
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

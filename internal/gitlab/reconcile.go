package gitlab

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
)

// Reconciliation pulls the provider's facts and refreshes the read
// models through the SAME application path as webhook deliveries —
// one truth path for projections and the done edge (GL-INV-003: the
// merged fact may arrive by webhook or by reconcile).
type Reconciler struct {
	Mapping MappingContext
	Secrets webhook.SecretResolver
	Syncer  *Syncer
	// NewClient is injectable for tests; production pins NewClient.
	NewClient func(baseURL, token string) (*Client, error)
}

// MappingContext resolves a project's integration scope.
type MappingContext interface {
	// ProjectMappingContext returns the instance id, base_url, the bot
	// credential reference, the numeric project id, the target branch
	// and the mapping row version; found=false when unmapped.
	ProjectMappingContext(ctx context.Context, projectID string) (instanceID, baseURL, botSecretRef string, gitlabProjectID int64, targetBranch string, mappingVersion int64, found bool, err error)
}

// ErrUnmappedProject reports reconciliation without a mapping.
var ErrUnmappedProject = errors.New("project has no GitLab mapping")

// ReconcileOutcome summarizes one reconcile operation.
type ReconcileOutcome struct {
	RemoteState  string
	Transitioned bool
	Withheld     bool
}

// ReconcileMergeRequest pulls the provider's MR state and applies it.
// Provider unavailability propagates as ErrProviderUnavailable — the
// cached projection stays untouched and the caller answers 503.
func (r *Reconciler) ReconcileMergeRequest(ctx context.Context, projectID string, mrIID int64) (*ReconcileOutcome, error) {
	instanceID, baseURL, botRef, gitlabProjectID, _, _, found, err := r.Mapping.ProjectMappingContext(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrUnmappedProject
	}
	token, err := r.Secrets.Resolve(ctx, botRef)
	if err != nil {
		return nil, fmt.Errorf("reconcile: bot credential: %w", err)
	}
	constructor := r.NewClient
	if constructor == nil {
		constructor = NewClient
	}
	client, err := constructor(baseURL, token)
	if err != nil {
		return nil, err
	}
	remote, err := client.MergeRequest(ctx, gitlabProjectID, mrIID)
	if err != nil {
		return nil, err
	}

	rec := MergeRequestRecord{
		InstanceID:    instanceID,
		GitlabProject: gitlabProjectID,
		IID:           remote.IID,
		State:         normalizeMRState(remote.State),
		SourceBranch:  remote.SourceBranch,
		TargetBranch:  remote.TargetBranch,
		SourceSHA:     remote.SourceSHA,
		MergeCommit:   remote.MergeCommit,
		MergedAt:      remote.MergedAt,
	}
	outcome, err := r.Syncer.ApplyMergeRequestRecord(ctx, projectID, rec, reconcileFactID(rec))
	if err != nil {
		return nil, err
	}
	return &ReconcileOutcome{
		RemoteState:  rec.State,
		Transitioned: outcome.Transitioned,
		Withheld:     outcome.Withheld,
	}, nil
}

// reconcileFactID marks the done-edge lineage with its source kind.
func reconcileFactID(rec MergeRequestRecord) string {
	return mergeFactID("reconcile", rec)
}

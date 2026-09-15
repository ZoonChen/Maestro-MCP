package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
)

// Control-plane fact source for the D1 self-attesting gates: the
// persisted ground truth the engine's mechanical judgments compare
// against. Digest integrity of the stored project policy row (tamper
// detection) and the execution records backing the boundary judgment
// (registered runner + approved command profile). CompanyDigest and
// ProjectDigest on the returned facts are filled by the evaluation
// service from the documents it actually loaded; this source owns only
// what the store alone knows.

// registeredRunnerStatuses is the runner status set that still counts
// as "registered" for the boundary judgment: approved or online.
// pending_approval never executed legitimately; suspect/draining/
// revoked lost their registration.
var registeredRunnerStatuses = map[string]bool{"approved": true, "online": true}

// ControlPlaneFacts loads the store-side ground truth for one work
// item: whether the stored project policy row's digest still matches
// its document, and the latest execution facts. No execution rows at
// all yields a nil Boundary (the gate mints no self-attestation and
// falls back to CI producers).
func (s pgQualityStore) ControlPlaneFacts(ctx context.Context, projectID, workItemID string) (*evidence.ControlPlaneFacts, error) {
	facts := &evidence.ControlPlaneFacts{StoredDigestMatch: true}

	var storedDigest string
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT policy_digest, policy FROM quality_policies WHERE project_id = $1`, projectID).
		Scan(&storedDigest, &raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No overlay: the company baseline alone is in force and there is
		// no stored row whose digest could drift.
	case err != nil:
		return nil, fmt.Errorf("control-plane facts: policy row: %w", err)
	default:
		policy, parseErr := evidence.ParsePolicy(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("control-plane facts: stored policy is invalid: %w", parseErr)
		}
		recomputed, digestErr := policy.Digest()
		if digestErr != nil {
			return nil, fmt.Errorf("control-plane facts: policy digest: %w", digestErr)
		}
		facts.StoredDigestMatch = recomputed == storedDigest
	}

	boundary, err := s.boundaryFact(ctx, projectID, workItemID)
	if err != nil {
		return nil, err
	}
	facts.Boundary = boundary
	return facts, nil
}

// boundaryFact derives the boundary judgment's facts from execution
// records: the work item's newest execution names the runner; the
// newest profile-bearing validation run names the command profile.
// Absent executions means the work item never executed under lease
// governance — nil, not a failure.
func (s pgQualityStore) boundaryFact(ctx context.Context, projectID, workItemID string) (*evidence.BoundaryFact, error) {
	fact := &evidence.BoundaryFact{}
	var runnerStatus string
	err := s.db.QueryRowContext(ctx, `
		SELECT e.id::text, r.id::text, r.status
		FROM executions e JOIN runners r ON r.id = e.runner_id
		WHERE e.project_id = $1 AND e.work_item_id = $2
		ORDER BY e.created_at DESC, e.id
		LIMIT 1`, projectID, workItemID).
		Scan(&fact.ExecutionID, &fact.RunnerID, &runnerStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("control-plane facts: execution: %w", err)
	}
	fact.RunnerStatus = runnerStatus
	fact.RunnerRegistered = registeredRunnerStatuses[runnerStatus]

	err = s.db.QueryRowContext(ctx, `
		SELECT profile_ref FROM validation_runs
		WHERE project_id = $1 AND work_item_id = $2 AND profile_ref <> ''
		ORDER BY id DESC
		LIMIT 1`, projectID, workItemID).
		Scan(&fact.ProfileRef)
	if errors.Is(err, sql.ErrNoRows) {
		fact.ProfileRef = ""
		return fact, nil
	}
	if err != nil {
		return nil, fmt.Errorf("control-plane facts: validation run: %w", err)
	}
	return fact, nil
}

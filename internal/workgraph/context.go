package workgraph

import "time"

// ContextSet is the ADR-009 §9 minimal execution context an attempt is
// pinned to: the exact repository SHA, the workspace boundary, the
// direct dependency artifacts with their digests, the acceptance
// criteria and the budget. Parent transcripts never leak in.
type ContextSet struct {
	Repo               string         `json:"repo"`
	BaseSHA            string         `json:"base_sha"`
	WorkspacePaths     []string       `json:"workspace_paths"`
	Inputs             []ContextInput `json:"inputs"`
	AcceptanceCriteria []string       `json:"acceptance_criteria"`
	BudgetUnits        int64          `json:"budget_units"`
}

// ContextInput is one direct dependency artifact binding.
type ContextInput struct {
	Port        string `json:"port"`
	AssetRef    string `json:"asset_ref"`
	AssetDigest string `json:"asset_digest"`
}

// ExecutionEnvelope is the unified dispatch result (WGS-REQ-003 /
// WGS §7): everything a worker needs to start, resume or cancel the
// claimed node, and nothing else.
type ExecutionEnvelope struct {
	TaskID              string    `json:"task_id"` // work_nodes.human_code
	NodeID              string    `json:"node_id"`
	NodeRevisionID      string    `json:"node_revision_id"`
	ExecutionAttemptID  string    `json:"execution_attempt_id"`
	LeaseToken          string    `json:"lease_token"`
	LeaseEpoch          int64     `json:"lease_epoch"`
	WorkspacePath       string    `json:"workspace_path"`
	Generation          string    `json:"generation"` // connection_generation
	BaseSHA             string    `json:"base_sha"`
	ContextDigest       string    `json:"context_digest"`
	BudgetUnits         int64     `json:"budget_units"`
	LeaseExpiresAt      time.Time `json:"lease_expires_at"`
	CorrelationID       string    `json:"correlation_id"`
}

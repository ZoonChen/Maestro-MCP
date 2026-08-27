// Package model defines all data structures for the Maestro-MCP server.
// Each struct maps to a SQLite table as defined in docs/technical/data-model.md Section 2.2 DDL.
// DDL is the single source of truth — struct field names and types must match DDL columns.
package model

import "encoding/json"

// ---------------------------------------------------------------------------
// Helper types (parsed from JSON columns)
// ---------------------------------------------------------------------------

// TestRequirements corresponds to tasks.test_requirements JSON field.
type TestRequirements struct {
	ProfileID      string   `json:"profile_id"`
	ProfileVersion string   `json:"profile_version"`
	ProfileDigest  string   `json:"profile_digest"`
	CoverageFormat string   `json:"coverage_format"`
	CoveragePath   string   `json:"coverage_path"`
	MinCoverage    *float64 `json:"min_coverage"`
}

// Dependency corresponds to an element in the tasks.dependencies JSON array.
type Dependency struct {
	TaskID       string `json:"task_id"`
	RequireState string `json:"require_state"`
}

// ProjectConfig corresponds to projects.config JSON field.
type ProjectConfig struct {
	DefaultCommandProfileID      *string  `json:"default_command_profile_id,omitempty"`
	DefaultCommandProfileVersion *string  `json:"default_command_profile_version,omitempty"`
	DefaultCommandProfileDigest  *string  `json:"default_command_profile_digest,omitempty"`
	DefaultCoverageFormat        *string  `json:"default_coverage_format,omitempty"`
	DefaultCoveragePath          *string  `json:"default_coverage_path,omitempty"`
	DefaultMinCoverage           *float64 `json:"default_min_coverage,omitempty"`
	DefaultTestTimeout           *int     `json:"default_test_timeout,omitempty"`
	MaxWorktrees                 *int     `json:"max_worktrees,omitempty"`
	MergeTargetBranch            *string  `json:"merge_target_branch,omitempty"`
	ContractPaths                []string `json:"contract_paths,omitempty"`
	ContractProvider             *string  `json:"contract_provider,omitempty"`
	ContractWatch                *bool    `json:"contract_watch,omitempty"`
}

// ---------------------------------------------------------------------------
// Status constants
// ---------------------------------------------------------------------------

// Canonical WorkItem status constants — used in tasks.status and v3 wire data.
// The deprecated Go names below are aliases only so the M0 migration can be
// landed without an all-at-once transport rewrite. Their values are canonical;
// legacy strings are accepted only by LegacyTaskStatusToCanonical during DB
// migration and must never be persisted by new writes.
const (
	TaskStatusDraft              = "draft"
	TaskStatusQueued             = "queued"
	TaskStatusLeased             = "leased"
	TaskStatusExecuting          = "executing"
	TaskStatusValidating         = "validating"
	TaskStatusReadyForHumanMerge = "ready_for_human_merge"
	TaskStatusDone               = "done"
	TaskStatusBlocked            = "blocked"
	TaskStatusCancelling         = "cancelling"
	TaskStatusCancelled          = "cancelled"
	TaskStatusFailed             = "failed"
	TaskStatusNeedsHuman         = "needs_human"

	// Deprecated compatibility identifiers. Do not use in new code.
	TaskStatusPending         = TaskStatusQueued
	TaskStatusInProgress      = TaskStatusExecuting
	TaskStatusSubmitted       = TaskStatusValidating
	TaskStatusVerifying       = TaskStatusValidating
	TaskStatusReadyToMerge    = TaskStatusReadyForHumanMerge
	TaskStatusMergeConflicted = TaskStatusNeedsHuman
)

// Project status constants.
const (
	ProjectStatusActive   = "active"
	ProjectStatusArchived = "archived"
)

// Feature status constants.
const (
	FeatureStatusPlanning  = "planning"
	FeatureStatusActive    = "active"
	FeatureStatusCompleted = "completed"
	FeatureStatusClosed    = "closed"
)

// AgentSession status constants.
const (
	SessionStatusOnline  = "online"
	SessionStatusOffline = "offline"
)

// AgentWorker status constants.
const (
	WorkerStatusIdle     = "idle"
	WorkerStatusReserved = "reserved"
	WorkerStatusBusy     = "busy"
	WorkerStatusLost     = "lost"
)

// Worktree status constants.
const (
	WorktreeStatusAllocated      = "allocated"
	WorktreeStatusActive         = "active"
	WorktreeStatusSubmitted      = "submitted"
	WorktreeStatusStale          = "stale"
	WorktreeStatusMerged         = "merged"
	WorktreeStatusAbandoned      = "abandoned"
	WorktreeStatusSealed         = "sealed"
	WorktreeStatusQuarantined    = "quarantined"
	WorktreeStatusCleanupPending = "cleanup_pending"
)

// Task lease status constants. A lease is the durable authority for a worker
// to mutate an executing task. Historical leases are never overwritten.
const (
	LeaseStatusActive    = "active"
	LeaseStatusCompleted = "completed"
	LeaseStatusReleased  = "released"
	LeaseStatusExpired   = "expired"
	LeaseStatusCancelled = "cancelled"
)

// Role constants — used in agent_sessions.role and tasks.role.
const (
	RoleBackend     = "backend"
	RoleFrontend    = "frontend"
	RoleDevops      = "devops"
	RoleVerifier    = "verifier"
	RoleCoordinator = "coordinator"
)

// Priority constants — used in tasks.priority.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// RelationType constants — used in tasks.relation_type field.
const (
	RelationFollowup           = "followup"
	RelationRetry              = "retry"
	RelationReplacement        = "replacement"
	RelationConflictResolution = "conflict_resolution"
)

// ActivityLog action constants — used in activity_log.action column.
const (
	ActionCreated         = "created"
	ActionClaimed         = "claimed"
	ActionSubmitted       = "submitted"
	ActionApproved        = "approved"
	ActionRejected        = "rejected"
	ActionBlocked         = "blocked"
	ActionUnblocked       = "unblocked"
	ActionVerifying       = "verifying"
	ActionMergeConflicted = "merge_conflicted"
	ActionMerged          = "merged"
	ActionMergeRequested  = "merge_requested"
	ActionReopened        = "reopened"
	ActionCancelled       = "cancelled"
	ActionFollowupCreated = "followup_created"
	ActionDone            = "done"
	ActionUpdated         = "updated"
)

// ---------------------------------------------------------------------------
// Database-backed structs (one per table, field order matches DDL column order)
// ---------------------------------------------------------------------------

// Project maps to the projects table.
type Project struct {
	ID            string          `db:"id"             json:"id"`
	Name          string          `db:"name"           json:"name"`
	WorkspacePath string          `db:"workspace_path" json:"workspace_path"`
	Description   string          `db:"description"    json:"description"`
	Status        string          `db:"status"         json:"status"`
	Config        json.RawMessage `db:"config"         json:"config"`
	CreatedAt     string          `db:"created_at"     json:"created_at"`
	UpdatedAt     string          `db:"updated_at"     json:"updated_at"`
}

// Feature maps to the features table.
type Feature struct {
	ID            string `db:"id"             json:"id"`
	ProjectID     string `db:"project_id"     json:"project_id"`
	Title         string `db:"title"          json:"title"`
	Description   string `db:"description"    json:"description"`
	ReferenceURLs string `db:"reference_urls" json:"reference_urls"` // JSON array TEXT
	Status        string `db:"status"         json:"status"`
	CreatedAt     string `db:"created_at"     json:"created_at"`
	UpdatedAt     string `db:"updated_at"     json:"updated_at"`
}

// Task maps to the tasks table — the largest struct.
type Task struct {
	ID                 string          `db:"id"                  json:"id"`
	ProjectID          string          `db:"project_id"          json:"project_id"`
	FeatureID          string          `db:"feature_id"          json:"feature_id"`
	Title              string          `db:"title"               json:"title"`
	Description        string          `db:"description"         json:"description"`
	Role               string          `db:"role"                json:"role"`
	Status             string          `db:"status"              json:"status"`
	AllowedDirectories string          `db:"allowed_directories" json:"allowed_directories"`
	ForbiddenPatterns  json.RawMessage `db:"forbidden_patterns"  json:"forbidden_patterns"`
	RequiredAPIs       json.RawMessage `db:"required_apis"       json:"required_apis"`
	Dependencies       json.RawMessage `db:"dependencies"        json:"dependencies"`
	ParentTaskID       *string         `db:"parent_task_id"      json:"parent_task_id,omitempty"`
	RelationType       *string         `db:"relation_type"       json:"relation_type,omitempty"`
	TestRequirements   json.RawMessage `db:"test_requirements"   json:"test_requirements"`
	AssignedSessionID  *string         `db:"assigned_session_id" json:"assigned_session_id"`
	AssignedWorkerID   *string         `db:"assigned_worker_id"  json:"assigned_worker_id"`
	AssignedAt         *string         `db:"assigned_at"         json:"assigned_at"`
	BlockerReason      *string         `db:"blocker_reason"      json:"blocker_reason"`
	CancelReason       *string         `db:"cancel_reason"       json:"cancel_reason"`
	MergeCommit        *string         `db:"merge_commit"        json:"merge_commit"`
	VerifiedBy         *string         `db:"verified_by"         json:"verified_by"`
	VerifiedAt         *string         `db:"verified_at"         json:"verified_at"`
	Priority           string          `db:"priority"            json:"priority"`
	Summary            *string         `db:"summary"             json:"summary"`
	Version            int64           `db:"version"             json:"version"`
	LeaseEpoch         int64           `db:"lease_epoch"         json:"lease_epoch"`
	ActiveLeaseID      *string         `db:"active_lease_id"     json:"active_lease_id,omitempty"`
	LeaseExpiresAt     *string         `db:"lease_expires_at"    json:"lease_expires_at,omitempty"`
	MergedFactID       *string         `db:"merged_fact_id"      json:"merged_fact_id,omitempty"`
	CreatedAt          string          `db:"created_at"          json:"created_at"`
	UpdatedAt          string          `db:"updated_at"          json:"updated_at"`
}

// TaskResult maps to the task_results table.
// Server-side truth: changed_files and test output are populated by the server,
// not submitted by agents.
type TaskResult struct {
	ID               string   `db:"id"                json:"id"`
	TaskID           string   `db:"task_id"           json:"task_id"`
	ProjectID        string   `db:"project_id"        json:"project_id"`
	BaseCommit       string   `db:"base_commit"       json:"base_commit"`
	ChangedFiles     string   `db:"changed_files"     json:"changed_files"` // JSON array TEXT
	TestCommand      string   `db:"test_command"      json:"test_command"`
	TestOutput       string   `db:"test_output"       json:"test_output"`
	Coverage         *float64 `db:"coverage"          json:"coverage,omitempty"`
	Summary          *string  `db:"summary"           json:"summary,omitempty"` // JSON TEXT
	SubmittedAt      string   `db:"submitted_at"      json:"submitted_at"`
	ValidatedAt      *string  `db:"validated_at"      json:"validated_at,omitempty"`
	ValidationErrors *string  `db:"validation_errors" json:"validation_errors,omitempty"`
	VerifierNotes    *string  `db:"verifier_notes"    json:"verifier_notes,omitempty"`
}

// Evidence authority and producer identities are server-generated facts.
// Local Runner validation is diagnostic only; merge_gate is reserved for the
// authenticated CI ingestion path introduced with GitLab integration.
const (
	EvidenceAuthorityDiagnostic  = "diagnostic"
	EvidenceAuthorityMergeGate   = "merge_gate"
	EvidenceProducerMaestroLocal = "maestro-local"
)

// ValidationRun maps to the validation_runs table (append-only history).
type ValidationRun struct {
	ID              int64    `db:"id"              json:"id"`
	TaskID          string   `db:"task_id"         json:"task_id"`
	ProjectID       string   `db:"project_id"      json:"project_id"`
	Attempt         int      `db:"attempt"         json:"attempt"`
	BaseCommit      string   `db:"base_commit"     json:"base_commit"`
	SourceCommit    string   `db:"source_commit"   json:"source_commit"`
	ChangedFiles    string   `db:"changed_files"   json:"changed_files"` // JSON array TEXT
	TestCommand     string   `db:"test_command"    json:"test_command"`
	ProfileRef      string   `db:"profile_ref"     json:"profile_ref"`
	PolicyVersion   string   `db:"policy_version"  json:"policy_version"`
	PolicyDigest    string   `db:"policy_digest"   json:"policy_digest"`
	EvidenceDigest  string   `db:"evidence_digest" json:"evidence_digest"`
	WorkspaceDigest string   `db:"workspace_digest" json:"workspace_digest"`
	Authority       string   `db:"authority"        json:"authority"`
	Producer        string   `db:"producer"         json:"producer"`
	PipelineID      *string  `db:"pipeline_id"      json:"pipeline_id,omitempty"`
	JobID           *string  `db:"job_id"           json:"job_id,omitempty"`
	TestExitCode    *int     `db:"test_exit_code"  json:"test_exit_code,omitempty"`
	TestOutput      *string  `db:"test_output"      json:"test_output,omitempty"`
	OutputTruncated bool     `db:"output_truncated" json:"output_truncated"`
	Coverage        *float64 `db:"coverage"        json:"coverage,omitempty"`
	BoundaryOK      bool     `db:"boundary_ok"     json:"boundary_ok"`
	TestOK          bool     `db:"test_ok"         json:"test_ok"`
	CoverageOK      bool     `db:"coverage_ok"     json:"coverage_ok"`
	Summary         *string  `db:"summary"         json:"summary,omitempty"` // JSON TEXT
	Result          string   `db:"result"          json:"result"`
	ErrorCode       *string  `db:"error_code"      json:"error_code,omitempty"`
	DurationMs      int      `db:"duration_ms"     json:"duration_ms"`
	LogPath         *string  `db:"log_path"        json:"log_path,omitempty"`
	CreatedAt       string   `db:"created_at"      json:"created_at"`
}

// Worktree maps to the worktrees table.
type Worktree struct {
	ID           int64   `db:"id"            json:"id"`
	TaskID       string  `db:"task_id"       json:"task_id"`
	ProjectID    string  `db:"project_id"    json:"project_id"`
	SessionID    *string `db:"session_id"   json:"session_id,omitempty"`
	WorktreePath string  `db:"worktree_path" json:"worktree_path"`
	BranchName   string  `db:"branch_name"   json:"branch_name"`
	BaseCommit   string  `db:"base_commit"   json:"base_commit"`
	Status       string  `db:"status"        json:"status"`
	Generation   int64   `db:"generation"    json:"generation"`
	Version      int64   `db:"version"       json:"version"`
	CreatedAt    string  `db:"created_at"    json:"created_at"`
	UpdatedAt    string  `db:"updated_at"    json:"updated_at"`
}

// APIContract maps to the api_contracts table.
type APIContract struct {
	ID             int64   `db:"id"              json:"id"`
	ProjectID      string  `db:"project_id"      json:"project_id"`
	Method         string  `db:"method"          json:"method"`
	Path           string  `db:"path"            json:"path"`
	RequestSchema  *string `db:"request_schema"  json:"request_schema,omitempty"`
	ResponseSchema *string `db:"response_schema" json:"response_schema,omitempty"`
	Description    *string `db:"description"     json:"description,omitempty"`
	SourceFile     string  `db:"source_file"     json:"source_file"`
	ParsedAt       string  `db:"parsed_at"       json:"parsed_at"`
}

// AgentSession maps to the agent_sessions table.
type AgentSession struct {
	ID            string `db:"id"             json:"id"`
	ProjectID     string `db:"project_id"     json:"project_id"`
	Role          string `db:"role"           json:"role"`
	ClientType    string `db:"client_type"    json:"client_type"`
	Capacity      int    `db:"capacity"       json:"capacity"`
	Status        string `db:"status"         json:"status"`
	Version       int64  `db:"version"        json:"version"`
	LastHeartbeat string `db:"last_heartbeat" json:"last_heartbeat"`
	CreatedAt     string `db:"created_at"     json:"created_at"`
}

// AgentWorker maps to the agent_workers table.
type AgentWorker struct {
	ID             string  `db:"id"              json:"id"`
	SessionID      string  `db:"session_id"      json:"session_id"`
	ProjectID      string  `db:"project_id"      json:"project_id"`
	CurrentTaskID  *string `db:"current_task_id" json:"current_task_id,omitempty"`
	Status         string  `db:"status"          json:"status"`
	TasksCompleted int     `db:"tasks_completed" json:"tasks_completed"`
	Version        int64   `db:"version"         json:"version"`
	LastActive     string  `db:"last_active"     json:"last_active"`
}

// TaskLease is the append-preserving execution authority for a task attempt.
// Lease tokens are represented by the opaque ID at this prototype stage; the
// v3 Runner stores and transports a nonce hash in addition to these fields.
type TaskLease struct {
	ID        string `db:"id"          json:"id"`
	ProjectID string `db:"project_id"  json:"project_id"`
	TaskID    string `db:"task_id"     json:"task_id"`
	SessionID string `db:"session_id"  json:"session_id"`
	WorkerID  string `db:"worker_id"   json:"worker_id"`
	Epoch     int64  `db:"epoch"       json:"epoch"`
	Status    string `db:"status"      json:"status"`
	Version   int64  `db:"version"     json:"version"`
	ExpiresAt string `db:"expires_at"  json:"expires_at"`
	CreatedAt string `db:"created_at"  json:"created_at"`
	UpdatedAt string `db:"updated_at"  json:"updated_at"`
}

// ActivityLog maps to the activity_log table.
type ActivityLog struct {
	ID        int64   `db:"id"         json:"id"`
	ProjectID string  `db:"project_id" json:"project_id"`
	SessionID *string `db:"session_id" json:"session_id,omitempty"`
	TaskID    *string `db:"task_id"    json:"task_id,omitempty"`
	Action    string  `db:"action"     json:"action"`
	Detail    *string `db:"detail"     json:"detail,omitempty"` // JSON TEXT
	CreatedAt string  `db:"created_at" json:"created_at"`
}

// AuditLog maps to the audit_log table.
type AuditLog struct {
	ID            int64   `db:"id"             json:"id"`
	SessionID     *string `db:"session_id"     json:"session_id,omitempty"`
	BoundProject  string  `db:"bound_project"  json:"bound_project"`
	TargetProject *string `db:"target_project" json:"target_project,omitempty"`
	TargetTask    *string `db:"target_task"    json:"target_task,omitempty"`
	Action        string  `db:"action"         json:"action"`
	Path          *string `db:"path"           json:"path,omitempty"`
	Result        string  `db:"result"         json:"result"`
	Detail        *string `db:"detail"         json:"detail,omitempty"` // JSON TEXT
	CreatedAt     string  `db:"created_at"     json:"created_at"`
}

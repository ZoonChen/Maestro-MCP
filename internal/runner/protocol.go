// Package runner implements the member-side Runner (M1-RUN-001, ADR-001):
// an outbound-only HTTPS client for the frozen runner.yaml protocol. The
// Control Plane never dials the Runner; every message carries the device
// bearer token, the lease CAS version and the connection generation so
// stale or replayed authority fails closed on the server.
package runner

import "time"

// Wire types mirror docs/specs/openapi/runner.yaml field-for-field. The
// golden-shape tests fail on any drift from the frozen contract.

// EnrollmentRequest is the one-time, project-bound enrollment body.
type EnrollmentRequest struct {
	EnrollmentCode  string   `json:"enrollment_code"`
	DevicePublicKey string   `json:"device_public_key"`
	DisplayName     string   `json:"display_name"`
	Capabilities    []string `json:"capabilities"`
}

// Credential is the enrollment reply (state is always pending_approval
// until an admin approves).
type Credential struct {
	RunnerID    string    `json:"runner_id"`
	State       string    `json:"state"`
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ClaimRequest long-polls for the next eligible lease.
type ClaimRequest struct {
	ProtocolVersion      string   `json:"protocol_version"`
	ConnectionGeneration string   `json:"connection_generation"`
	Capabilities         []string `json:"capabilities"`
	WaitSeconds          int      `json:"wait_seconds"`
}

// Lease is a server-authorized work lease. Command references versioned
// profiles by digest only — never an arbitrary command string.
type Lease struct {
	ID                  string              `json:"id"`
	Version             int64               `json:"version"`
	Epoch               int64               `json:"epoch"`
	ExecutionID         string              `json:"execution_id"`
	WorkItemID          string              `json:"work_item_id"`
	ProjectID           string              `json:"project_id"`
	Repository          string              `json:"repository"`
	BaselineSHA         string              `json:"baseline_sha"`
	WorkspaceGeneration int64               `json:"workspace_generation"`
	PushIntent          PushIntent          `json:"push_intent"`
	ExpiresAt           time.Time           `json:"expires_at"`
	CommandProfiles     []CommandProfileRef `json:"command_profiles"`
}

// CommandProfileRef identifies an immutable, digest-pinned profile.
type CommandProfileRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// PushIntent is the broker input; it never enters the sandbox.
type PushIntent struct {
	GitLabInstanceID string  `json:"gitlab_instance_id"`
	GitLabProjectID  int64   `json:"gitlab_project_id"`
	ExpectedHost     string  `json:"expected_host"`
	SourceBranch     string  `json:"source_branch"`
	ExpectedOldSHA   *string `json:"expected_old_sha"`
}

// NoWork is the explicit empty claim reply.
type NoWork struct {
	Available         bool `json:"available"`
	RetryAfterSeconds int  `json:"retry_after_seconds"`
}

// HeartbeatRequest renews a lease and reports liveness.
type HeartbeatRequest struct {
	LeaseVersion         int64     `json:"lease_version"`
	ConnectionGeneration string    `json:"connection_generation"`
	ObservedAt           time.Time `json:"observed_at"`
}

// DiagnosticEvidence is bounded local evidence; authority is always
// diagnostic — it can never satisfy a merge gate.
type DiagnosticEvidence struct {
	LeaseID              string    `json:"lease_id"`
	LeaseVersion         int64     `json:"lease_version"`
	ConnectionGeneration string    `json:"connection_generation"`
	Type                 string    `json:"type"`
	Authority            string    `json:"authority"`
	Producer             string    `json:"producer"`
	Digest               string    `json:"digest"`
	CreatedAt            time.Time `json:"created_at"`
	Summary              string    `json:"summary,omitempty"`
}

// ExecutionCompletion is the terminal outcome report.
type ExecutionCompletion struct {
	LeaseID              string  `json:"lease_id"`
	LeaseVersion         int64   `json:"lease_version"`
	ConnectionGeneration string  `json:"connection_generation"`
	WorkspaceGeneration  int64   `json:"workspace_generation"`
	Outcome              string  `json:"outcome"`
	CommitSHA            *string `json:"commit_sha"`
	Summary              string  `json:"summary"`
}

// Frozen protocol vocabulary.
const (
	OutcomeCompleted = "completed"
	OutcomeBlocked   = "blocked"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"

	StatePendingApproval = "pending_approval"

	// ProtocolVersion is the frozen M1 wire version family.
	ProtocolVersion = "3.0"

	// DefaultCapabilityFloor is the minimum capability set the M1 runner
	// advertises; the server rejects claims below the schema minimum of
	// three entries.
	DefaultCapabilities = "rootless_oci,no_new_privileges,resource_limits"

	// MaxWaitSeconds is the schema bound for long-poll waits.
	MaxWaitSeconds = 30
)

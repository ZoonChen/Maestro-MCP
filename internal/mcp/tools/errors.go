package tools

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// MaestroError represents a structured error returned by MCP tools.
// Matches the format defined in docs/technical/api-spec.md Section 4.7.
type MaestroError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// Error implements the error interface.
func (e MaestroError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// maestroToolError creates a structured MCP tool error result from a MaestroError.
func maestroToolError(mae MaestroError) *mcp.CallToolResult {
	payload, _ := json.Marshal(mae) //nolint:errchkjson // MaestroError is a safe struct
	return mcp.NewToolResultError(string(payload))
}

// errorResult maps any error to a structured MCP tool error result.
// Uses errors.Is to correctly match wrapped sentinel errors.
func errorResult(err error) *mcp.CallToolResult {
	return maestroToolError(mapError(err))
}

// mapError maps a store/service error to a MaestroError with a machine-readable code.
func mapError(err error) MaestroError {
	type mapping struct {
		sentinel error
		code     string
		message  string
	}
	mappings := []mapping{
		{store.ErrTaskNotFound, "TASK_NOT_FOUND", "Task not found"},
		{store.ErrFeatureNotFound, "FEATURE_NOT_FOUND", "Feature not found"},
		{store.ErrProjectNotFound, "PROJECT_NOT_FOUND", "Project not found"},
		{store.ErrProjectNotBound, "PROJECT_NOT_BOUND", "No project bound to this connection"},
		{store.ErrProjectAmbiguous, "PROJECT_AMBIGUOUS", "Multiple projects match, specify project_id"},
		{store.ErrProjectArchived, "PROJECT_ARCHIVED", "Project archived"},
		{store.ErrProjectAlreadyExists, "PROJECT_ALREADY_EXISTS", "Project already exists"},
		{store.ErrSessionNotFound, "SESSION_NOT_FOUND", "Session not found"},
		{store.ErrWorktreeNotFound, "WORKTREE_NOT_FOUND", "Worktree not found"},
		{store.ErrWorktreeCreateFailed, "WORKTREE_CREATE_FAILED", "Worktree creation failed"},
		{store.ErrWorktreeCleanFailed, "WORKTREE_CLEAN_FAILED", "Worktree cleanup failed"},
		{store.ErrWorkerNotFound, "WORKER_NOT_FOUND", "Worker not found"},
		{store.ErrNoAvailableTask, "NO_AVAILABLE_TASK", "No available task"},
		{store.ErrTaskStateInvalid, "TASK_STATE_INVALID", "Task state invalid for this operation"},
		{store.ErrTaskNotOwned, "TASK_NOT_OWNED", "Task not owned by session"},
		{store.ErrTaskAlreadyCancelled, "TASK_ALREADY_CANCELLED", "Task already cancelled"},
		{store.ErrTaskDependencyUnmet, "TASK_DEPENDENCY_UNMET", "Task dependency not satisfied"},
		{store.ErrConcurrentConflict, "CONCURRENT_CONFLICT", "Concurrent conflict, please retry"},
		{store.ErrCircularDependency, "CIRCULAR_DEPENDENCY", "Circular dependency detected"},
		{store.ErrInvalidParameter, "INVALID_PARAMETER", "Invalid parameter"},
		{store.ErrSessionCapacityFull, "SESSION_CAPACITY_FULL", "Session capacity full"},
		{store.ErrBoundaryViolation, "BOUNDARY_VIOLATION", "File boundary violation"},
		{store.ErrCoverageBelowMin, "COVERAGE_BELOW_MIN", "Coverage below minimum threshold"},
		{store.ErrTestExecutionFailed, "TEST_EXECUTION_FAILED", "Test execution failed"},
		{store.ErrMergeConflict, "MERGE_CONFLICT", "Merge conflict detected"},
		{store.ErrValidationFailed, "VALIDATION_FAILED", "Validation failed"},
		{store.ErrDependencyNotReady, "DEPENDENCY_NOT_READY", "Dependency not ready"},
		{store.ErrFeatureStatusInvalid, "FEATURE_STATUS_INVALID", "Feature status invalid"},
		{store.ErrContractNotFound, "CONTRACT_NOT_FOUND", "Contract not found"},
	}

	for _, m := range mappings {
		if errors.Is(err, m.sentinel) {
			return MaestroError{
				Code:    m.code,
				Message: m.message,
				Detail:  err.Error(),
			}
		}
	}

	return MaestroError{
		Code:    "INTERNAL_ERROR",
		Message: "An unexpected error occurred",
		Detail:  err.Error(),
	}
}

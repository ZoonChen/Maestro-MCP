// Package publicerror is the single fail-closed translation boundary between
// internal failures and REST/MCP wire errors. Callers MUST NOT serialize an
// arbitrary error string directly.
package publicerror

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// Error is safe to expose on an untrusted transport. Message and Code are
// selected exclusively from this package's allowlist; CorrelationID contains
// no request or dependency data.
type Error struct {
	Code          string
	Message       string
	HTTPStatus    int
	CorrelationID string
}

type mapping struct {
	sentinel error
	code     string
	message  string
	status   int
}

var mappings = []mapping{ //nolint:gochecknoglobals // immutable classification table
	{store.ErrTaskNotFound, "TASK_NOT_FOUND", "Task not found", http.StatusNotFound},
	{store.ErrFeatureNotFound, "FEATURE_NOT_FOUND", "Feature not found", http.StatusNotFound},
	{store.ErrProjectNotFound, "PROJECT_NOT_FOUND", "Project not found", http.StatusNotFound},
	{store.ErrProjectScopeViolation, "PROJECT_NOT_FOUND", "Project not found", http.StatusNotFound},
	{store.ErrProjectNotBound, "PROJECT_NOT_BOUND", "No project is bound to this connection", http.StatusBadRequest},
	{store.ErrProjectAmbiguous, "PROJECT_AMBIGUOUS", "Multiple projects match the request", http.StatusBadRequest},
	{store.ErrProjectArchived, "PROJECT_ARCHIVED", "Project is archived", http.StatusForbidden},
	{store.ErrProjectAlreadyExists, "PROJECT_ALREADY_EXISTS", "Project already exists", http.StatusBadRequest},
	{store.ErrSessionNotFound, "SESSION_NOT_FOUND", "Session not found", http.StatusNotFound},
	{store.ErrWorktreeNotFound, "WORKTREE_NOT_FOUND", "Worktree not found", http.StatusNotFound},
	{store.ErrWorktreeCreateFailed, "WORKTREE_CREATE_FAILED", "Worktree creation failed", http.StatusInternalServerError},
	{store.ErrWorktreeCleanFailed, "WORKTREE_CLEAN_FAILED", "Worktree cleanup failed", http.StatusInternalServerError},
	{store.ErrWorkerNotFound, "WORKER_NOT_FOUND", "Worker not found", http.StatusNotFound},
	{store.ErrLeaseNotFound, "LEASE_NOT_FOUND", "Active lease not found", http.StatusNotFound},
	{store.ErrLeaseExpired, "LEASE_EXPIRED", "Lease expired", http.StatusGone},
	{store.ErrLeaseVersionMismatch, "LEASE_VERSION_MISMATCH", "Lease version mismatch", http.StatusConflict},
	{store.ErrIdempotencyConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key conflicts with an earlier request", http.StatusConflict},
	{store.ErrOperationDisabled, "OPERATION_DISABLED", "Operation disabled by platform policy", http.StatusForbidden},
	{store.ErrNoAvailableTask, "NO_AVAILABLE_TASK", "No available task", http.StatusNotFound},
	{store.ErrTaskStateInvalid, "TASK_STATE_INVALID", "Task state is invalid for this operation", http.StatusConflict},
	{store.ErrTaskNotOwned, "TASK_NOT_OWNED", "Task is not owned by this session", http.StatusForbidden},
	{store.ErrTaskAlreadyCancelled, "TASK_ALREADY_CANCELLED", "Task is already cancelled", http.StatusConflict}, //nolint:misspell // Canonical wire state.
	{store.ErrTaskDependencyUnmet, "TASK_DEPENDENCY_UNMET", "Task dependency is not satisfied", http.StatusPreconditionFailed},
	{store.ErrConcurrentConflict, "CONCURRENT_CONFLICT", "Concurrent conflict; retry with fresh state", http.StatusConflict},
	{store.ErrCircularDependency, "CIRCULAR_DEPENDENCY", "Circular dependency detected", http.StatusUnprocessableEntity},
	{store.ErrInvalidParameter, "INVALID_PARAMETER", "Invalid request parameter", http.StatusBadRequest},
	{store.ErrSessionCapacityFull, "SESSION_CAPACITY_FULL", "Session capacity is full", http.StatusTooManyRequests},
	{store.ErrBoundaryViolation, "BOUNDARY_VIOLATION", "File boundary validation failed", http.StatusUnprocessableEntity},
	{store.ErrCoverageBelowMin, "COVERAGE_BELOW_MIN", "Coverage is below the required threshold", http.StatusUnprocessableEntity},
	{store.ErrTestExecutionFailed, "TEST_EXECUTION_FAILED", "Test execution failed", http.StatusUnprocessableEntity},
	{store.ErrMergeConflict, "MERGE_CONFLICT", "Merge conflict detected", http.StatusConflict},
	{store.ErrValidationFailed, "VALIDATION_FAILED", "Validation failed", http.StatusUnprocessableEntity},
	{store.ErrDependencyNotReady, "DEPENDENCY_NOT_READY", "Dependency is not ready", http.StatusBadRequest},
	{store.ErrFeatureStatusInvalid, "FEATURE_STATUS_INVALID", "Feature status is invalid for this operation", http.StatusBadRequest},
	{store.ErrContractNotFound, "CONTRACT_NOT_FOUND", "Contract not found", http.StatusNotFound},
	{store.ErrRecoveryIntegrity, "INTERNAL_ERROR", "An unexpected error occurred", http.StatusInternalServerError},
}

var correlationFallback atomic.Uint64 //nolint:gochecknoglobals // process-local fallback only

// Classify maps an internal error to an allowlisted public representation.
func Classify(err error) Error {
	correlationID := newCorrelationID()

	var contextErr *service.ContextBuildError
	if errors.As(err, &contextErr) {
		code, message, status := classifyContextError(contextErr.Code)
		return Error{Code: code, Message: message, HTTPStatus: status, CorrelationID: correlationID}
	}

	var validationErr *service.ValidationError
	if errors.As(err, &validationErr) {
		code, status := classifyValidationError(validationErr.Code)
		return Error{Code: code, Message: "Validation failed", HTTPStatus: status, CorrelationID: correlationID}
	}

	for _, candidate := range mappings {
		if errors.Is(err, candidate.sentinel) {
			return Error{
				Code: candidate.code, Message: candidate.message,
				HTTPStatus: candidate.status, CorrelationID: correlationID,
			}
		}
	}

	return Error{
		Code: "INTERNAL_ERROR", Message: "An unexpected error occurred",
		HTTPStatus: http.StatusInternalServerError, CorrelationID: correlationID,
	}
}

// Log records only stable classification and Go type. The cause text is
// deliberately omitted because dependency errors routinely contain paths,
// queries, credentials, repository content, or other untrusted data.
func Log(err error, public Error) {
	attributes := []any{
		"error_code", public.Code,
		"correlation_id", public.CorrelationID,
		"cause_type", fmt.Sprintf("%T", err),
	}
	if public.HTTPStatus >= http.StatusInternalServerError {
		slog.Error("request failed", attributes...)
		return
	}
	slog.Warn("request rejected", attributes...)
}

func classifyContextError(code string) (string, string, int) {
	switch code {
	case service.ContextErrorRequiredSourceMissing:
		return code, "Required context source is missing", http.StatusUnprocessableEntity
	case service.ContextErrorSourceInvalid:
		return code, "Context source is invalid", http.StatusUnprocessableEntity
	case service.ContextErrorCompensationFailed:
		return code, "Context failure compensation did not complete", http.StatusInternalServerError
	case service.ContextErrorCleanupPending:
		return code, "Context cleanup is pending", http.StatusInternalServerError
	case service.ContextErrorBuildFailed:
		return code, "Context could not be built", http.StatusInternalServerError
	default:
		return service.ContextErrorBuildFailed, "Context could not be built", http.StatusInternalServerError
	}
}

func classifyValidationError(code string) (string, int) {
	// These are the only codes the M0 validation pipeline may place on a wire.
	// An unknown value can be influenced by a future dependency or programming
	// error and therefore collapses to the generic fail-closed code.
	switch code {
	case "BASELINE_UNAVAILABLE", "BOUNDARY_VIOLATION", "COVERAGE_BELOW", "COVERAGE_INVALID",
		"COVERAGE_MISSING", "DIFF_FAILED", "EVIDENCE_ENCODING_FAILED", "EVIDENCE_MISMATCH",
		"LEASE_INVALID", "LEASE_RENEWAL_FAILED", "OUTPUT_TRUNCATED", "POLICY_INVALID",
		"POLICY_STALE", "PROFILE_EXEC_ERROR", "PROFILE_NOT_APPROVED", "PROFILE_TIMEOUT",
		"TEST_FAILED", "VALIDATION_CANCELLED", "VALIDATION_INPUT_INVALID", "WORKTREE_ERROR",
		"WORKTREE_STALE":
		return code, http.StatusUnprocessableEntity
	case "EVIDENCE_PERSIST_FAILED", "EVIDENCE_SEAL_FAILED", "STATE_OR_EVIDENCE_PERSIST_FAILED":
		return code, http.StatusInternalServerError
	default:
		return "VALIDATION_FAILED", http.StatusUnprocessableEntity
	}
}

func newCorrelationID() string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	sequence := correlationFallback.Add(1)
	return fmt.Sprintf("fallback-%x-%x", time.Now().UnixNano(), sequence)
}

// NewCorrelationID returns an opaque identifier for an already allowlisted
// transport error that did not originate from an internal error value.
func NewCorrelationID() string {
	return newCorrelationID()
}

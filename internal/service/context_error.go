package service

import (
	"errors"
	"fmt"
)

// Stable context error codes returned by MCP. Context construction happens
// after a durable claim in the M0 compatibility flow, so callers must be able
// to distinguish a bad/missing source from an incomplete compensation.
const (
	ContextErrorRequiredSourceMissing = "CONTEXT_REQUIRED_SOURCE_MISSING"
	ContextErrorSourceInvalid         = "CONTEXT_SOURCE_INVALID"
	ContextErrorBuildFailed           = "CONTEXT_BUILD_FAILED"
	ContextErrorCompensationFailed    = "CONTEXT_COMPENSATION_FAILED"
	ContextErrorCleanupPending        = "CONTEXT_CLEANUP_PENDING"
)

// ContextBuildError is a stable, machine-readable failure. Cause is retained
// for errors.Is/errors.As and internal diagnostics; MCP exposes the stable Code
// and Message instead of ever returning a partially enriched Task.
type ContextBuildError struct {
	Code    string
	Message string
	Cause   error
}

func (e *ContextBuildError) Error() string {
	if e == nil {
		return "context build failed"
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

// Unwrap preserves source/store errors for retry and audit classification.
func (e *ContextBuildError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewContextBuildError wraps a failure with a stable context error code.
func NewContextBuildError(code, message string, cause error) *ContextBuildError {
	return &ContextBuildError{Code: code, Message: message, Cause: cause}
}

// ContextBuildErrorCode returns the stable context code or the conservative
// generic code for an unclassified failure.
func ContextBuildErrorCode(err error) string {
	var contextErr *ContextBuildError
	if errors.As(err, &contextErr) && contextErr.Code != "" {
		return contextErr.Code
	}
	return ContextErrorBuildFailed
}

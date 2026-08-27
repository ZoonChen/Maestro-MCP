package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

func TestMapErrorPreservesLeaseAndIdempotencyCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "lease not found", err: store.ErrLeaseNotFound, code: "LEASE_NOT_FOUND"},
		{name: "lease expired", err: store.ErrLeaseExpired, code: "LEASE_EXPIRED"},
		{name: "lease version", err: store.ErrLeaseVersionMismatch, code: "LEASE_VERSION_MISMATCH"},
		{name: "idempotency conflict", err: store.ErrIdempotencyConflict, code: "IDEMPOTENCY_CONFLICT"},
		{name: "operation disabled", err: store.ErrOperationDisabled, code: "OPERATION_DISABLED"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mapped := mapError(fmt.Errorf("wrapped: %w", test.err))
			if mapped.Code != test.code {
				t.Fatalf("mapError() code = %q, want %q", mapped.Code, test.code)
			}
		})
	}
}

func TestMCPErrorBoundaryNeverExposesInternalCause(t *testing.T) {
	t.Parallel()
	tests := []error{
		fmt.Errorf("m0-mcp-internal-canary: %w", store.ErrRecoveryIntegrity),
		fmt.Errorf("m0-mcp-client-canary: %w", store.ErrInvalidParameter),
		&service.ValidationError{
			Code: "VALIDATION_INPUT_INVALID", Message: "m0-mcp-validation-message-canary",
			Cause: errors.New("m0-mcp-validation-cause-canary"),
		},
		service.NewContextBuildError(
			service.ContextErrorSourceInvalid,
			"m0-mcp-context-message-canary",
			errors.New("m0-mcp-context-cause-canary"),
		),
	}
	for _, internal := range tests {
		mapped := mapError(internal)
		payload, err := json.Marshal(mapped)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "canary") {
			t.Fatalf("MCP payload exposed internal data: %s", payload)
		}
		if mapped.CorrelationID == "" {
			t.Fatal("MCP error is missing correlation_id")
		}
		if mapped.Detail != "" {
			t.Fatalf("MCP error detail must be empty, got %q", mapped.Detail)
		}
	}
}

func TestDirectMCPToolErrorsReceiveCorrelationID(t *testing.T) {
	t.Parallel()
	result := maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "Invalid parameter"})
	if result == nil || !result.IsError {
		t.Fatal("expected MCP tool error result")
	}
	content := mcpResultText(result)
	var payload MaestroError
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CorrelationID == "" {
		t.Fatal("direct MCP error is missing correlation_id")
	}
}

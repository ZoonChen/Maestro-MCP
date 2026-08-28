package tools

import (
	"context"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
)

// v3 wire-shape helpers shared by the lease-bound tools. Scope and identity
// always resolve from the TransportBinding; payloads never carry them.

// requireLeaseContext validates the idempotency key shape and resolves the
// server-side scope for a lease-bound call.
func requireLeaseContext(req mcp.CallToolRequest, services *Services) (idempotencyKey string, projectID, sessionID, workerID string, err error) {
	idempotencyKey, err = req.RequireString("idempotency_key")
	if err != nil {
		return "", "", "", "", err
	}
	if !claimIdempotencyKeyPattern.MatchString(idempotencyKey) {
		return "", "", "", "", fmt.Errorf("idempotency_key: %w", store.ErrInvalidParameter)
	}
	projectID, sessionID, workerID, err = services.Binding.scope()
	if err != nil {
		return "", "", "", "", err
	}
	return idempotencyKey, projectID, sessionID, workerID, nil
}

// requireLeaseVersion reads and validates the lease CAS version argument.
func requireLeaseVersion(req mcp.CallToolRequest, name string) (int64, error) {
	version, err := requireIntegerArg(req, name)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("%s: %w", name, store.ErrInvalidParameter)
	}
	return version, nil
}

// requireStringSlice reads a string-array argument with the schema bounds.
func requireStringSlice(req mcp.CallToolRequest, name string, min, max int) ([]string, error) {
	raw, ok := req.GetArguments()[name]
	if !ok {
		if min > 0 {
			return nil, fmt.Errorf("%s: %w", name, store.ErrInvalidParameter)
		}
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, store.ErrInvalidParameter)
	}
	if len(items) < min || len(items) > max {
		return nil, fmt.Errorf("%s: %w", name, store.ErrInvalidParameter)
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, fmt.Errorf("%s: %w", name, store.ErrInvalidParameter)
		}
		values = append(values, text)
	}
	return values, nil
}

// queueTokenCheck enforces the queue CAS token a conforming client must
// present before a claim-class call (execution or verification).
func queueTokenCheck(ctx context.Context, services *Services, projectID string, expected int64) error {
	current, err := services.Task.CurrentQueueVersion(ctx, projectID)
	if err != nil {
		return fmt.Errorf("queue state unavailable: %w", err)
	}
	if current != expected {
		return fmt.Errorf("CONCURRENCY_CONFLICT: queue_version %d is stale, current is %d: %w",
			expected, current, store.ErrConcurrentConflict)
	}
	return nil
}

// verifySessionRole loads the bound session and asserts its registered role.
func verifySessionRole(ctx context.Context, services *Services, projectID, sessionID, role string) error {
	session, err := services.Session.GetSession(ctx, projectID, sessionID)
	if err != nil {
		return fmt.Errorf("bound session is not registered: %w", err)
	}
	if session.Role != role {
		return fmt.Errorf("bound session role %q does not match required %q: %w", session.Role, role, store.ErrTaskNotOwned)
	}
	return nil
}

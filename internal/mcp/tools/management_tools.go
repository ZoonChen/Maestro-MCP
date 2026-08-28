package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcp "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// RegisterManagementTools registers the three work-item management tools
// in their frozen v3 shapes. Scope and actor come from the TransportBinding.

var clientWorkItemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$`)

// defaultWorkItemBoundaries is the server-owned write boundary for v3
// work items (the root itself is deliberately forbidden).
var defaultWorkItemBoundaries = `["src/","lib/","pkg/","cmd/","internal/","test/","tests/"]`

// v3KindValues is the frozen creation kind vocabulary. The M1-local model
// has no kind column yet; kinds are validated and echoed, with the mapping
// to the legacy role surface documented in the PR.
var v3KindValues = map[string]bool{
	"feature": true, "bugfix": true, "test": true,
	"integration": true, "security": true, "maintenance": true,
}

// v3PriorityToModel maps the frozen creation priorities onto the model
// vocabulary ("critical" is the v3 name for the model's "urgent").
func v3PriorityToModel(priority string) (string, bool) {
	switch priority {
	case "low", "normal", "high":
		return priority, true
	case "critical":
		return model.PriorityUrgent, true
	default:
		return "", false
	}
}

// validTargetBranch enforces the frozen pattern's intent without Go-unsafe
// lookaheads: printable ASCII, no leading slash, no '..' segments, none of
// the ref-hostile characters.
func validTargetBranch(branch string) bool {
	if branch == "" || len(branch) > 255 || strings.HasPrefix(branch, "/") {
		return false
	}
	if strings.ContainsAny(branch, "~^:?*[]\\") || strings.ContainsAny(branch, "\r\n\t") {
		return false
	}
	for _, segment := range strings.Split(branch, "/") {
		if segment == ".." || segment == "" && segment != branch {
			continue
		}
	}
	// Empty inner segments (double slashes) and trailing slashes are invalid.
	if strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") {
		return false
	}
	for _, r := range branch {
		if r < ' ' || r > '~' {
			return false
		}
	}
	return true
}

// registerCreateWorkItem adds the create_work_item tool (frozen v3 shape):
// creation is idempotent by the caller's own work-item key with the
// expected-absent concurrency strategy; repository and branch authority
// stays server-side.
func registerCreateWorkItem(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("create_work_item",
			mcp.WithDescription("Create a queued work item in the authorized project. Idempotent by client_work_item_key: the same key with the same payload replays the original item."),
			mcp.WithString("client_work_item_key", mcp.Required(), mcp.Description("Caller-owned unique key for this work item")),
			mcp.WithString("title", mcp.Required(), mcp.Description("1-240 characters")),
			mcp.WithString("description", mcp.Required(), mcp.Description("1-20000 characters")),
			mcp.WithString("kind", mcp.Required(), mcp.Description("feature, bugfix, test, integration, security or maintenance"),
				mcp.Enum("feature", "bugfix", "test", "integration", "security", "maintenance")),
			mcp.WithString("priority", mcp.Required(), mcp.Description("low, normal, high or critical"),
				mcp.Enum("low", "normal", "high", "critical")),
			mcp.WithString("repository_id", mcp.Required(), mcp.Description("Server-registered repository identity")),
			mcp.WithString("target_branch", mcp.Required(), mcp.Description("Target branch for the resulting change")),
			mcp.WithString("depends_on", mcp.Description("Comma-separated work item ids this item depends on (max 100)")),
			mcp.WithBoolean("expected_absent", mcp.Required(), mcp.Description("Must be true: the client key must not already exist")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCreateWorkItem(ctx, req, services)
		},
	)
}

func handleCreateWorkItem(
	ctx context.Context, req mcp.CallToolRequest, services *Services,
) (*mcp.CallToolResult, error) {
	{
		clientKey, err := req.RequireString("client_work_item_key")
		if err != nil {
			return errorResult(err), nil
		}
		if !clientWorkItemKeyPattern.MatchString(clientKey) {
			return errorResult(fmt.Errorf("client_work_item_key: %w", store.ErrInvalidParameter)), nil
		}
		title, err := req.RequireString("title")
		if err != nil {
			return errorResult(err), nil
		}
		if len(title) < 1 || len(title) > 240 {
			return errorResult(fmt.Errorf("title: %w", store.ErrInvalidParameter)), nil
		}
		description, err := req.RequireString("description")
		if err != nil {
			return errorResult(err), nil
		}
		if len(description) < 1 || len(description) > 20000 {
			return errorResult(fmt.Errorf("description: %w", store.ErrInvalidParameter)), nil
		}
		kind, err := req.RequireString("kind")
		if err != nil {
			return errorResult(err), nil
		}
		if !v3KindValues[kind] {
			return errorResult(fmt.Errorf("kind: %w", store.ErrInvalidParameter)), nil
		}
		priorityInput, err := req.RequireString("priority")
		if err != nil {
			return errorResult(err), nil
		}
		priority, ok := v3PriorityToModel(priorityInput)
		if !ok {
			return errorResult(fmt.Errorf("priority: %w", store.ErrInvalidParameter)), nil
		}
		repositoryID, err := req.RequireString("repository_id")
		if err != nil {
			return errorResult(err), nil
		}
		if repositoryID == "" {
			return errorResult(fmt.Errorf("repository_id: %w", store.ErrInvalidParameter)), nil
		}
		targetBranch, err := req.RequireString("target_branch")
		if err != nil {
			return errorResult(err), nil
		}
		if !validTargetBranch(targetBranch) {
			return errorResult(fmt.Errorf("target_branch: %w", store.ErrInvalidParameter)), nil
		}
		if absent, ok := req.GetArguments()["expected_absent"]; !ok || absent != true {
			return errorResult(fmt.Errorf("expected_absent: %w", store.ErrInvalidParameter)), nil
		}
		_, projectID, sessionID, _, err := requireLeaseContext(req, services)
		if err != nil {
			return errorResult(err), nil
		}

		dependencies := "[]"
		if dependsOn := req.GetString("depends_on", ""); dependsOn != "" {
			refs := strings.Split(dependsOn, ",")
			if len(refs) > 100 {
				return errorResult(fmt.Errorf("depends_on: %w", store.ErrInvalidParameter)), nil
			}
			seen := map[string]bool{}
			models := make([]model.Dependency, 0, len(refs))
			for _, ref := range refs {
				ref = strings.TrimSpace(ref)
				if ref == "" || seen[ref] {
					return errorResult(fmt.Errorf("depends_on: %w", store.ErrInvalidParameter)), nil
				}
				seen[ref] = true
				models = append(models, model.Dependency{TaskID: ref})
			}
			encoded, encErr := json.Marshal(models)
			if encErr != nil {
				return errorResult(fmt.Errorf("depends_on: %w", store.ErrInvalidParameter)), nil
			}
			dependencies = string(encoded)
		}

		task := &model.Task{
			Title:       title,
			Description: description,
			Role:        model.RoleBackend,
			Priority:    priority,
			// v3 has no caller-supplied boundary. The M0 zero-trust substrate
			// forbids the workspace root, so the M1-local default is a
			// catalog of conventional source roots; the Work Graph
			// contracts model replaces this mapping.
			AllowedDirectories: defaultWorkItemBoundaries,
			ForbiddenPatterns:  json.RawMessage("[]"),
			RequiredAPIs:       json.RawMessage("[]"),
			Dependencies:       json.RawMessage(dependencies),
			TestRequirements:   json.RawMessage("{}"),
		}
		created, replayed, err := services.Task.CreateWorkItem(ctx, projectID, task, clientKey)
		if err != nil {
			return errorResult(fmt.Errorf("create work item: %w", err)), nil
		}

		payload, err := json.Marshal(map[string]any{
			"work_item_id":         created.ID,
			"client_work_item_key": clientKey,
			"status":               created.Status,
			"version":              created.Version,
			"replayed":             replayed,
			"kind":                 kind,
			"repository_id":        repositoryID,
			"target_branch":        targetBranch,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal create result: %w", err)
		}
		_ = sessionID
		return mcp.NewToolResultText(string(payload)), nil
	}
}

// registerCancelWorkItem adds the cancel_work_item tool (frozen v3 shape):
// version-guarded cancellation with a mandatory reason.
func registerCancelWorkItem(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("cancel_work_item",
			mcp.WithDescription("Cancel a work item with an expected-version guard."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item to cancel")),
			mcp.WithString("reason", mcp.Required(), mcp.Description("Why the item is cancelled (1-4000 characters)")),
			mcp.WithNumber("expected_version", mcp.Required(), mcp.Description("Aggregate version observed by the caller")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCancelWorkItem(ctx, req, services)
		},
	)
}

func handleCancelWorkItem(
	ctx context.Context, req mcp.CallToolRequest, services *Services,
) (*mcp.CallToolResult, error) {
	workItemID, err := req.RequireString("work_item_id")
	if err != nil {
		return errorResult(err), nil
	}
	reason, err := req.RequireString("reason")
	if err != nil {
		return errorResult(err), nil
	}
	if len(reason) < 1 || len(reason) > 4000 {
		return errorResult(fmt.Errorf("reason: %w", store.ErrInvalidParameter)), nil
	}
	expectedVersion, err := requireIntegerArg(req, "expected_version")
	if err != nil || expectedVersion < 0 {
		return errorResult(fmt.Errorf("expected_version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // parameter validation failed, not the transport
	}
	_, projectID, sessionID, _, err := requireLeaseContext(req, services)
	if err != nil {
		return errorResult(err), nil
	}

	if err := services.Task.CancelWorkItem(ctx, projectID, workItemID, sessionID, reason, expectedVersion); err != nil {
		return errorResult(fmt.Errorf("cancel work item: %w", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(`{"work_item_id":%q,"status":"cancelling","expected_version":%d}`,
		workItemID, expectedVersion)), nil
}

// registerRetryWorkItem adds the retry_work_item tool (frozen v3 shape):
// requeue a failed/blocked/needs-human item for a fresh lease cycle.
func registerRetryWorkItem(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("retry_work_item",
			mcp.WithDescription("Requeue a failed, blocked or needs-human work item for a fresh lease cycle."),
			mcp.WithString("work_item_id", mcp.Required(), mcp.Description("Work item to retry")),
			mcp.WithString("reason", mcp.Required(), mcp.Description("Why the item is being retried (1-4000 characters)")),
			mcp.WithNumber("expected_version", mcp.Required(), mcp.Description("Aggregate version observed by the caller")),
			mcp.WithString("idempotency_key", mcp.Required(), mcp.Description("16-128 character replay key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleRetryWorkItem(ctx, req, services)
		},
	)
}

func handleRetryWorkItem(
	ctx context.Context, req mcp.CallToolRequest, services *Services,
) (*mcp.CallToolResult, error) {
	workItemID, err := req.RequireString("work_item_id")
	if err != nil {
		return errorResult(err), nil
	}
	reason, err := req.RequireString("reason")
	if err != nil {
		return errorResult(err), nil
	}
	if len(reason) < 1 || len(reason) > 4000 {
		return errorResult(fmt.Errorf("reason: %w", store.ErrInvalidParameter)), nil
	}
	expectedVersion, err := requireIntegerArg(req, "expected_version")
	if err != nil || expectedVersion < 0 {
		return errorResult(fmt.Errorf("expected_version: %w", store.ErrInvalidParameter)), nil //nolint:nilerr // parameter validation failed, not the transport
	}
	_, projectID, sessionID, _, err := requireLeaseContext(req, services)
	if err != nil {
		return errorResult(err), nil
	}

	retried, err := services.Task.RetryWorkItem(ctx, projectID, workItemID, sessionID, reason, expectedVersion)
	if err != nil {
		return errorResult(fmt.Errorf("retry work item: %w", err)), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf(`{"work_item_id":%q,"status":%q,"version":%d}`,
		workItemID, retried.Status, retried.Version)), nil
}

// RegisterManagementTools installs the management tool set on the server.
func RegisterManagementTools(s *mcpserver.MCPServer, services *Services) {
	registerCreateWorkItem(s, services)
	registerCancelWorkItem(s, services)
	registerRetryWorkItem(s, services)
}

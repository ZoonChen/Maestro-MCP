package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
)

// validTaskRoles lists the roles allowed for task assignment.
var validTaskRoles = map[string]bool{
	model.RoleBackend:  true,
	model.RoleFrontend: true,
	model.RoleDevops:   true,
	model.RoleVerifier: true,
}

// RegisterCoordinatorTools registers coordinator-level tools for feature and task management.
func RegisterCoordinatorTools(s *mcpserver.MCPServer, services *Services) {
	registerCreateFeature(s, services)
	registerSplitTask(s, services)
	registerUpdateTask(s, services)
	registerCancelTask(s, services)
	registerResolveBlocker(s, services)
	registerResolveMergeConflict(s, services)
}

// registerCreateFeature adds the create_feature tool.
func registerCreateFeature(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("create_feature",
			mcp.WithDescription("Create a new feature within a project. Features group related tasks together."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project this feature belongs to"),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Feature title"),
			),
			mcp.WithString("description",
				mcp.Required(),
				mcp.Description("Detailed feature description"),
			),
			mcp.WithString("reference_urls",
				mcp.Description("JSON array of reference URLs, e.g. [\"https://example.com/spec\"]"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			title, err := req.RequireString("title")
			if err != nil {
				return errorResult(err), nil
			}
			description, err := req.RequireString("description")
			if err != nil {
				return errorResult(err), nil
			}
			referenceURLs := req.GetString("reference_urls", "[]")

			// Validate reference_urls is valid JSON if provided.
			if referenceURLs != "[]" && !json.Valid([]byte(referenceURLs)) {
				return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "reference_urls must be a valid JSON array string"}), nil
			}

			feature := &model.Feature{
				ID:            "F-" + uuid.New().String()[:8],
				ProjectID:     projectID,
				Title:         title,
				Description:   description,
				ReferenceURLs: referenceURLs,
				Status:        model.FeatureStatusPlanning,
			}

			if err := services.Feature.CreateFeature(ctx, projectID, feature); err != nil {
				return errorResult(fmt.Errorf("failed to create feature: %w", err)), nil
			}

			result, _ := json.Marshal(map[string]string{
				"feature_id": feature.ID,
				"status":     feature.Status,
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

// registerSplitTask adds the split_task tool.
func registerSplitTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("split_task",
			mcp.WithDescription("Create (split) a new task within a feature. Tasks are the atomic unit of work assigned to agents."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("feature_id",
				mcp.Required(),
				mcp.Description("ID of the feature this task belongs to"),
			),
			mcp.WithString("role",
				mcp.Required(),
				mcp.Description("Agent role required for this task: backend, frontend, devops, or verifier"),
				mcp.Enum("backend", "frontend", "devops", "verifier"),
			),
			mcp.WithString("title",
				mcp.Required(),
				mcp.Description("Task title"),
			),
			mcp.WithString("description",
				mcp.Required(),
				mcp.Description("Detailed task description with acceptance criteria"),
			),
			mcp.WithString("allowed_directories",
				mcp.Required(),
				mcp.Description("JSON array of directory paths the agent may modify, e.g. [\"src/api/\"]"),
			),
			mcp.WithString("forbidden_patterns",
				mcp.Description("JSON array of glob patterns the agent must NOT modify, e.g. [\"*.env\",\"config/\"]"),
			),
			mcp.WithString("required_apis",
				mcp.Description("JSON array of API references the agent will need, e.g. [{\"method\":\"GET\",\"path\":\"/api/v1/users\"}]"),
			),
			mcp.WithString("dependencies",
				mcp.Description("JSON array of dependency objects, e.g. [{\"task_id\":\"T-abcde\",\"require_state\":\"done\"}]"),
			),
			mcp.WithString("test_requirements",
				mcp.Description("JSON object referencing an approved profile: {\"profile_id\":\"go-unit\",\"profile_version\":\"3.0.0\",\"profile_digest\":\"sha256:...\",\"coverage_format\":\"go-cover\",\"coverage_path\":\"coverage.out\",\"min_coverage\":80}"),
			),
			mcp.WithString("priority",
				mcp.Description("Task priority: low, normal, high, or urgent (default: normal)"),
				mcp.Enum("low", "normal", "high", "urgent"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			featureID, err := req.RequireString("feature_id")
			if err != nil {
				return errorResult(err), nil
			}
			role, err := req.RequireString("role")
			if err != nil {
				return errorResult(err), nil
			}
			title, err := req.RequireString("title")
			if err != nil {
				return errorResult(err), nil
			}
			description, err := req.RequireString("description")
			if err != nil {
				return errorResult(err), nil
			}
			allowedDirs, err := req.RequireString("allowed_directories")
			if err != nil {
				return errorResult(err), nil
			}

			// Validate role.
			if !validTaskRoles[role] {
				return maestroToolError(MaestroError{
					Code:    "INVALID_PARAMETER",
					Message: "role must be one of backend, frontend, devops, or verifier",
				}), nil
			}

			// Validate JSON fields.
			if !json.Valid([]byte(allowedDirs)) {
				return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "allowed_directories must be a valid JSON array string"}), nil
			}

			forbiddenPatterns := json.RawMessage("[]")
			if fp := req.GetString("forbidden_patterns", ""); fp != "" {
				if !json.Valid([]byte(fp)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "forbidden_patterns must be a valid JSON array string"}), nil
				}
				forbiddenPatterns = json.RawMessage(fp)
			}

			requiredAPIs := json.RawMessage("[]")
			if ra := req.GetString("required_apis", ""); ra != "" {
				if !json.Valid([]byte(ra)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "required_apis must be a valid JSON array string"}), nil
				}
				requiredAPIs = json.RawMessage(ra)
			}

			dependencies := json.RawMessage("[]")
			if dep := req.GetString("dependencies", ""); dep != "" {
				if !json.Valid([]byte(dep)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "dependencies must be a valid JSON array string"}), nil
				}
				dependencies = json.RawMessage(dep)
			}

			testReqs := json.RawMessage("{}")
			if tr := req.GetString("test_requirements", ""); tr != "" {
				if !json.Valid([]byte(tr)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "test_requirements must be a valid JSON object string"}), nil
				}
				testReqs = json.RawMessage(tr)
			}

			priority := req.GetString("priority", model.PriorityNormal)

			// Validate feature_id exists.
			if _, err := services.Feature.GetFeature(ctx, projectID, featureID); err != nil {
				return errorResult(fmt.Errorf("feature %q not found: %w", featureID, err)), nil
			}

			// Validate allowed_directories is a non-empty array.
			var allowedDirsArr []string
			if err := json.Unmarshal([]byte(allowedDirs), &allowedDirsArr); err != nil || len(allowedDirsArr) == 0 {
				return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "allowed_directories must be a non-empty JSON array of directory paths"}), nil //nolint:nilerr // MCP tool returns (result, nil)
			}

			// Validate allowed_directories: no ".." paths (security).
			for _, dir := range allowedDirsArr {
				if strings.Contains(dir, "..") {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "allowed_directories contains an unsafe path"}), nil
				}
			}

			// Reject arbitrary command material and require an immutable approved
			// profile reference whenever task-level validation policy is provided.
			if tr := req.GetString("test_requirements", ""); tr != "" && tr != "{}" {
				if err := service.ValidateTaskTestRequirements([]byte(tr)); err != nil {
					return errorResult(&service.ValidationError{Code: "VALIDATION_INPUT_INVALID", Cause: err}), nil //nolint:nilerr // MCP tool errors use a result payload with a nil transport error.
				}
			}

			// Validate required_apis format (if provided).
			if ra := req.GetString("required_apis", ""); ra != "" && ra != "[]" {
				var apiRefs []map[string]interface{}
				if err := json.Unmarshal([]byte(ra), &apiRefs); err == nil {
					validMethods := map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true}
					for _, ref := range apiRefs {
						method, _ := ref["method"].(string)
						path, _ := ref["path"].(string)
						if !validMethods[method] {
							return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "required_apis contains an unsupported method"}), nil
						}
						if !strings.HasPrefix(path, "/") {
							return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "required_apis contains an invalid path"}), nil
						}
					}
				}
			}

			// Validate dependency task_ids exist (if provided).
			if dep := req.GetString("dependencies", ""); dep != "" && dep != "[]" {
				var deps []model.Dependency
				if err := json.Unmarshal([]byte(dep), &deps); err == nil {
					for _, d := range deps {
						if _, err := services.Task.GetTask(ctx, projectID, d.TaskID); err != nil {
							return errorResult(fmt.Errorf("dependency task %q not found: %w", d.TaskID, err)), nil
						}
					}
				}
			}

			task := &model.Task{
				ID:                 "T-" + uuid.New().String()[:8],
				ProjectID:          projectID,
				FeatureID:          featureID,
				Title:              title,
				Description:        description,
				Role:               role,
				Status:             model.TaskStatusQueued,
				AllowedDirectories: allowedDirs,
				ForbiddenPatterns:  forbiddenPatterns,
				RequiredAPIs:       requiredAPIs,
				Dependencies:       dependencies,
				TestRequirements:   testReqs,
				Priority:           priority,
			}

			if err := services.Task.CreateTask(ctx, projectID, task); err != nil {
				return errorResult(fmt.Errorf("failed to create task: %w", err)), nil
			}

			result, _ := json.Marshal(map[string]string{
				"task_id": task.ID,
				"status":  task.Status,
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

// registerUpdateTask adds the update_task tool.
func registerUpdateTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("update_task",
			mcp.WithDescription("Update mutable fields of an existing task. Only provided fields are changed. Field restrictions by status: queued/blocked=all fields editable; executing=description and summary only; validating/ready_for_human_merge/done/needs_human/cancelled=read-only."), //nolint:misspell // "cancelled" is the canonical wire state.
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the task to update"),
			),
			mcp.WithString("title",
				mcp.Description("New task title"),
			),
			mcp.WithString("description",
				mcp.Description("New task description"),
			),
			mcp.WithString("summary",
				mcp.Description("Task execution summary"),
			),
			mcp.WithString("allowed_directories",
				mcp.Description("New allowed directories (JSON array string)"),
			),
			mcp.WithString("forbidden_patterns",
				mcp.Description("New forbidden patterns (JSON array string)"),
			),
			mcp.WithString("required_apis",
				mcp.Description("New required APIs (JSON array string)"),
			),
			mcp.WithString("test_requirements",
				mcp.Description("New test requirements (JSON object string)"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}

			task, err := services.Task.GetTask(ctx, projectID, taskID)
			if err != nil {
				return errorResult(fmt.Errorf("task not found: %w", err)), nil
			}

			// Status-based field restrictions (PRD task-management.md).
			switch task.Status {
			case model.TaskStatusCancelled:
				return maestroToolError(MaestroError{Code: "TASK_ALREADY_CANCELLED", Message: "cancelled tasks are read-only"}), nil //nolint:misspell // Stable error code and canonical wire state.
			case model.TaskStatusValidating, model.TaskStatusReadyForHumanMerge,
				model.TaskStatusDone, model.TaskStatusNeedsHuman:
				return maestroToolError(MaestroError{Code: "TASK_STATE_INVALID", Message: "Task state is invalid for this operation"}), nil
			case model.TaskStatusExecuting:
				// Only description and summary can be updated for executing tasks.
				title := req.GetString("title", "")
				allowedDirs := req.GetString("allowed_directories", "")
				forbiddenPats := req.GetString("forbidden_patterns", "")
				requiredAPIs := req.GetString("required_apis", "")
				testReqs := req.GetString("test_requirements", "")
				if title != "" || allowedDirs != "" || forbiddenPats != "" || requiredAPIs != "" || testReqs != "" {
					return maestroToolError(MaestroError{Code: "TASK_STATE_INVALID", Message: "executing tasks only allow updating description and summary"}), nil
				}
			}

			// Apply updates for provided fields.
			if v := req.GetString("title", ""); v != "" {
				task.Title = v
			}
			if v := req.GetString("description", ""); v != "" {
				task.Description = v
			}
			if v := req.GetString("summary", ""); v != "" {
				task.Summary = &v
			}
			if v := req.GetString("allowed_directories", ""); v != "" {
				if !json.Valid([]byte(v)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "allowed_directories must be valid JSON"}), nil
				}
				// Validate: no ".." path traversal (security).
				var dirs []string
				if err := json.Unmarshal([]byte(v), &dirs); err == nil {
					for _, dir := range dirs {
						if strings.Contains(dir, "..") {
							return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "allowed_directories contains an unsafe path"}), nil
						}
					}
				}
				task.AllowedDirectories = v
			}
			if v := req.GetString("forbidden_patterns", ""); v != "" {
				if !json.Valid([]byte(v)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "forbidden_patterns must be valid JSON"}), nil
				}
				task.ForbiddenPatterns = json.RawMessage(v)
			}
			if v := req.GetString("required_apis", ""); v != "" {
				if !json.Valid([]byte(v)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "required_apis must be valid JSON"}), nil
				}
				task.RequiredAPIs = json.RawMessage(v)
			}
			if v := req.GetString("test_requirements", ""); v != "" {
				if !json.Valid([]byte(v)) {
					return maestroToolError(MaestroError{Code: "INVALID_PARAMETER", Message: "test_requirements must be valid JSON"}), nil
				}
				if err := service.ValidateTaskTestRequirements([]byte(v)); err != nil {
					return errorResult(&service.ValidationError{Code: "VALIDATION_INPUT_INVALID", Cause: err}), nil //nolint:nilerr // MCP tool errors use a result payload with a nil transport error.
				}
				task.TestRequirements = json.RawMessage(v)
			}

			// Persist the updated task through the service layer.
			if err := services.Task.UpdateTask(ctx, projectID, task); err != nil {
				return errorResult(fmt.Errorf("failed to update task: %w", err)), nil
			}

			result, _ := json.Marshal(map[string]string{
				"task_id": task.ID,
				"status":  "updated",
			})
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

// registerCancelTask adds the cancel_task tool.
func registerCancelTask(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("cancel_task",
			mcp.WithDescription("Request cancellation of a queued, executing, blocked, or needs_human task. Executing work remains cancelling until recovery confirms the Lease stopped."), //nolint:misspell // "cancelling" is the canonical wire state.
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the task to cancel"),
			),
			mcp.WithString("reason",
				mcp.Required(),
				mcp.Description("Reason for cancellation"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}
			reason, err := req.RequireString("reason")
			if err != nil {
				return errorResult(err), nil
			}

			// sessionID — in production this comes from connection binding.
			sessionID := "coordinator"

			if err := services.Task.CancelTask(ctx, projectID, taskID, sessionID, reason); err != nil {
				return errorResult(fmt.Errorf("failed to cancel task: %w", err)), nil
			}
			updated, err := services.Task.GetTask(ctx, projectID, taskID)
			if err != nil {
				return errorResult(fmt.Errorf("read cancelled task: %w", err)), nil //nolint:misspell // "cancelled" is the canonical wire state.
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"%s"}`, taskID, updated.Status)), nil
		},
	)
}

// registerResolveBlocker adds the resolve_blocker tool.
func registerResolveBlocker(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("resolve_blocker",
			mcp.WithDescription("Resolve a blocked task back to queued for a fresh Lease. In M0, reassign=true is rejected because retaining a stopped Lease is unsafe."),
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the blocked task"),
			),
			mcp.WithString("resolution",
				mcp.Required(),
				mcp.Description("Description of how the blocker was resolved"),
			),
			mcp.WithBoolean("reassign",
				mcp.Description("Must be false in M0; true requires a future fresh-Lease reassignment workflow"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}
			resolution, err := req.RequireString("resolution")
			if err != nil {
				return errorResult(err), nil
			}
			reassign := req.GetBool("reassign", false)

			if err := services.Task.ResolveBlocker(ctx, projectID, taskID, reassign, resolution); err != nil {
				return errorResult(fmt.Errorf("failed to resolve blocker: %w", err)), nil
			}

			updated, err := services.Task.GetTask(ctx, projectID, taskID)
			if err != nil {
				return errorResult(fmt.Errorf("read resolved task: %w", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","status":"%s","resolution":%q}`, taskID, updated.Status, resolution)), nil
		},
	)
}

// registerResolveMergeConflict adds the resolve_merge_conflict tool.
func registerResolveMergeConflict(s *mcpserver.MCPServer, services *Services) {
	s.AddTool(
		mcp.NewTool("resolve_merge_conflict",
			mcp.WithDescription("Resolve a needs_human conflict by reopening it for a fresh lease or cancelling it. Follow-up creation is disabled until the v3 idempotent workflow is available."), //nolint:misspell // "cancelling" follows the canonical state vocabulary.
			mcp.WithString("project_id",
				mcp.Required(),
				mcp.Description("ID of the project"),
			),
			mcp.WithString("task_id",
				mcp.Required(),
				mcp.Description("ID of the merge-conflicted task"),
			),
			mcp.WithString("action",
				mcp.Required(),
				mcp.Description("Resolution action: reopen or cancel"),
				mcp.Enum("reopen", "cancel"),
			),
			mcp.WithString("reason",
				mcp.Description("Optional reason for the resolution choice"),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			projectID, err := req.RequireString("project_id")
			if err != nil {
				return errorResult(err), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return errorResult(err), nil
			}
			action, err := req.RequireString("action")
			if err != nil {
				return errorResult(err), nil
			}
			reason := req.GetString("reason", "")

			if err := services.Task.ResolveMergeConflict(ctx, projectID, taskID, action, reason); err != nil {
				return errorResult(fmt.Errorf("failed to resolve merge conflict: %w", err)), nil
			}
			updated, err := services.Task.GetTask(ctx, projectID, taskID)
			if err != nil {
				return errorResult(fmt.Errorf("read conflict resolution result: %w", err)), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf(`{"task_id":"%s","action":"%s","status":"%s","resolved":true}`, taskID, action, updated.Status)), nil
		},
	)
}

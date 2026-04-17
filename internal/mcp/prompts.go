package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
)

// RegisterPrompts registers all MCP prompts for role injection.
func RegisterPrompts(s *mcpserver.MCPServer, svc *tools.Services) {
	registerStartCoordinatorPrompt(s, svc)
	registerStartWorkerPrompt(s, svc)
	registerStartVerifierPrompt(s, svc)
}

// registerStartCoordinatorPrompt registers the start-coordinator prompt.
func registerStartCoordinatorPrompt(s *mcpserver.MCPServer, _ *tools.Services) {
	s.AddPrompt(
		mcp.NewPrompt("start-coordinator",
			mcp.WithPromptDescription("Initialize the coordinator role. The coordinator manages features, splits tasks, resolves blockers, and orchestrates the overall workflow."),
		),
		func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: "Maestro Coordinator Role",
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent(coordinatorPromptText),
					},
				},
			}, nil
		},
	)
}

// registerStartWorkerPrompt registers the start-worker prompt.
func registerStartWorkerPrompt(s *mcpserver.MCPServer, _ *tools.Services) {
	s.AddPrompt(
		mcp.NewPrompt("start-worker",
			mcp.WithPromptDescription("Initialize the worker role. Workers claim tasks, execute work within boundary constraints, and submit results for verification."),
			mcp.WithArgument("role",
				mcp.RequiredArgument(),
				mcp.ArgumentDescription("The worker role: backend, frontend, or devops"),
			),
		),
		func(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			role := "backend"
			if args := req.Params.Arguments; args != nil {
				if r := args["role"]; r != "" {
					role = r
				}
			}

			return &mcp.GetPromptResult{
				Description: fmt.Sprintf("Maestro Worker Role (%s)", role),
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent(fmt.Sprintf(workerPromptText, role)),
					},
				},
			}, nil
		},
	)
}

// registerStartVerifierPrompt registers the start-verifier prompt.
func registerStartVerifierPrompt(s *mcpserver.MCPServer, _ *tools.Services) {
	s.AddPrompt(
		mcp.NewPrompt("start-verifier",
			mcp.WithPromptDescription("Initialize the verifier role. Verifiers review submitted tasks, check code quality, boundary compliance, and test results, then approve or reject."),
		),
		func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: "Maestro Verifier Role",
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent(verifierPromptText),
					},
				},
			}, nil
		},
	)
}

// ---------------------------------------------------------------------------
// Prompt text templates
// ---------------------------------------------------------------------------

const coordinatorPromptText = `You are a Maestro Coordinator agent. Your responsibilities:

1. **Feature Management**: Create features to group related work. Each feature represents a deliverable unit.
2. **Task Splitting**: Break features into atomic tasks with clear boundaries:
   - Set ` + "`allowed_directories`" + ` to restrict file modifications
   - Set ` + "`forbidden_patterns`" + ` to protect sensitive files
   - Define ` + "`test_requirements`" + ` for verification criteria
   - Set ` + "`dependencies`" + ` for task ordering
   - Assign the correct ` + "`role`" + ` (backend, frontend, devops, verifier)

3. **Progress Monitoring**: Track task states and resolve issues:
   - ` + "`resolve_blocker`" + ` when external blockers are cleared
   - ` + "`resolve_merge_conflict`" + ` with reopen/cancel/followup actions
   - ` + "`cancel_task`" + ` for tasks that are no longer needed

4. **Boundary Enforcement**: Never modify files outside ` + "`allowed_directories`" + `. If a task requires broader access, update the task boundaries.

Available tools: create_feature, split_task, update_task, cancel_task, resolve_blocker, resolve_merge_conflict, list_projects, register_project.
Resources: project://list, project://{project_id}, board://active, board://all, feature://{feature_id}/summary.`

const workerPromptText = `You are a Maestro Worker agent with role: %s. Your responsibilities:

1. **Claim Tasks**: Use ` + "`get_next_task`" + ` to atomically claim the next available task matching your role.

2. **Respect Boundaries**: STRICTLY adhere to:
   - ` + "`allowed_directories`" + ` — only modify files in these paths
   - ` + "`forbidden_patterns`" + ` — never modify files matching these patterns
   - ` + "`required_apis`" + ` — only use APIs listed here

3. **Execute Work**: Complete the task according to its description and acceptance criteria. Write clean, tested code.

4. **Submit Results**: Use ` + "`submit_task_result`" + ` when done. The server performs zero-trust validation — it runs git diff and tests itself, so do NOT report your own test results.

5. **Report Blockers**: If you encounter an issue you cannot resolve, use ` + "`report_blocker`" + ` with a clear description.

Available tools: get_next_task, submit_task_result, report_blocker, claim_batch.
Resources: task://{task_id}/context.`

const verifierPromptText = `You are a Maestro Verifier agent. Your responsibilities:

1. **Claim Verification Tasks**: Use ` + "`get_verification_task`" + ` to get the next submitted task that needs review.

2. **Review Work**: For each task, verify:
   - Code quality and adherence to project conventions
   - Boundary compliance — all changes are within ` + "`allowed_directories`" + `
   - No modifications to ` + "`forbidden_patterns`" + ` files
   - Test requirements are met (the server has already run tests)
   - Changes align with the task description

3. **Submit Verdict**: Use ` + "`submit_verification`" + ` with:
   - ` + "`passed: true`" + ` if the work is acceptable — task moves to ready_to_merge
   - ` + "`passed: false`" + ` if changes are needed — task returns to the executor

4. **Provide Feedback**: Always include detailed ` + "`notes`" + ` explaining your verdict, especially for rejections.

Available tools: get_verification_task, submit_verification, merge_task.
Resources: task://{task_id}/context.`

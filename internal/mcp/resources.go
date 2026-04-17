package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// RegisterResources registers all MCP resources (static and templated).
func RegisterResources(s *mcpserver.MCPServer, svc *tools.Services) {
	registerProjectListResource(s, svc)
	registerProjectByIDResource(s, svc)
	registerBoardActiveResource(s, svc)
	registerBoardAllResource(s, svc)
	registerTaskContextResource(s, svc)
	registerFeatureSummaryResource(s, svc)
}

// registerProjectListResource registers the static resource project://list.
func registerProjectListResource(s *mcpserver.MCPServer, svc *tools.Services) {
	s.AddResource(
		mcp.NewResource(
			"project://list",
			"All Projects",
			mcp.WithResourceDescription("List all registered projects"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			projects, err := svc.Project.ListProjects(ctx, true)
			if err != nil {
				return nil, fmt.Errorf("list projects: %w", err)
			}
			data, err := json.Marshal(projects)
			if err != nil {
				return nil, fmt.Errorf("marshal projects: %w", err)
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		},
	)
}

// registerProjectByIDResource registers the templated resource project://{project_id}.
func registerProjectByIDResource(s *mcpserver.MCPServer, svc *tools.Services) {
	template := mcp.NewResourceTemplate(
		"project://{project_id}",
		"Project Details",
		mcp.WithTemplateDescription("Get details for a specific project by ID"),
		mcp.WithTemplateMIMEType("application/json"),
	)
	s.AddResourceTemplate(template, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		projectID := extractURIParam(req.Params.URI, "project://", "")
		if projectID == "" {
			return nil, fmt.Errorf("invalid project URI: %s", req.Params.URI)
		}
		project, err := svc.Project.GetProject(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("get project: %w", err)
		}
		data, err := json.Marshal(project)
		if err != nil {
			return nil, fmt.Errorf("marshal project: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(data),
			},
		}, nil
	})
}

// registerBoardActiveResource registers board://active.
func registerBoardActiveResource(s *mcpserver.MCPServer, svc *tools.Services) {
	s.AddResource(
		mcp.NewResource(
			"board://active",
			"Active Board",
			mcp.WithResourceDescription("Current project board summary with task counts by status"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			projects, err := svc.Project.ListProjects(ctx, false)
			if err != nil {
				return nil, fmt.Errorf("list active projects: %w", err)
			}

			type boardEntry struct {
				ProjectID   string         `json:"project_id"`
				ProjectName string         `json:"project_name"`
				TaskCounts  map[string]int `json:"task_counts"`
			}

			board := make([]boardEntry, 0, len(projects))
			for _, p := range projects {
				counts, countErr := svc.Task.ListTasks(ctx, p.ID, store.TaskFilter{})
				if countErr != nil {
					continue
				}
				statusCounts := make(map[string]int)
				for _, t := range counts {
					statusCounts[t.Status]++
				}
				board = append(board, boardEntry{
					ProjectID:   p.ID,
					ProjectName: p.Name,
					TaskCounts:  statusCounts,
				})
			}

			data, err := json.Marshal(board)
			if err != nil {
				return nil, fmt.Errorf("marshal board: %w", err)
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		},
	)
}

// registerBoardAllResource registers board://all.
func registerBoardAllResource(s *mcpserver.MCPServer, svc *tools.Services) {
	s.AddResource(
		mcp.NewResource(
			"board://all",
			"Cross-Project Board",
			mcp.WithResourceDescription("Cross-project board summary including archived projects"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			projects, err := svc.Project.ListProjects(ctx, true)
			if err != nil {
				return nil, fmt.Errorf("list all projects: %w", err)
			}

			type boardEntry struct {
				ProjectID   string         `json:"project_id"`
				ProjectName string         `json:"project_name"`
				Status      string         `json:"status"`
				TaskCounts  map[string]int `json:"task_counts"`
			}

			board := make([]boardEntry, 0, len(projects))
			for _, p := range projects {
				counts, countErr := svc.Task.ListTasks(ctx, p.ID, store.TaskFilter{})
				if countErr != nil {
					continue
				}
				statusCounts := make(map[string]int)
				for _, t := range counts {
					statusCounts[t.Status]++
				}
				board = append(board, boardEntry{
					ProjectID:   p.ID,
					ProjectName: p.Name,
					Status:      p.Status,
					TaskCounts:  statusCounts,
				})
			}

			data, err := json.Marshal(board)
			if err != nil {
				return nil, fmt.Errorf("marshal board: %w", err)
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		},
	)
}

// registerTaskContextResource registers task://{task_id}/context.
func registerTaskContextResource(s *mcpserver.MCPServer, svc *tools.Services) {
	template := mcp.NewResourceTemplate(
		"task://{task_id}/context",
		"Task Context",
		mcp.WithTemplateDescription("Full context for a task including dependency summaries and API contracts"),
		mcp.WithTemplateMIMEType("application/json"),
	)
	s.AddResourceTemplate(template, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		taskID := extractTaskIDFromContextURI(req.Params.URI)
		if taskID == "" {
			return nil, fmt.Errorf("invalid task context URI: %s", req.Params.URI)
		}

		// We need projectID to scope the query. Since we don't have it from the URI,
		// we scan all active projects to find the task. This is acceptable for an
		// MCP resource because it's read-only and called infrequently.
		projects, err := svc.Project.ListProjects(ctx, false)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}

		var taskCtx *service.TaskContextResult
		for _, p := range projects {
			result, resultErr := svc.Context.GetTaskContext(ctx, p.ID, taskID)
			if resultErr == nil {
				taskCtx = result
				break
			}
		}

		if taskCtx == nil {
			return nil, fmt.Errorf("task %s not found in any project", taskID)
		}

		data, err := json.Marshal(taskCtx)
		if err != nil {
			return nil, fmt.Errorf("marshal task context: %w", err)
		}
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(data),
			},
		}, nil
	})
}

// registerFeatureSummaryResource registers feature://{feature_id}/summary.
func registerFeatureSummaryResource(s *mcpserver.MCPServer, svc *tools.Services) {
	template := mcp.NewResourceTemplate(
		"feature://{feature_id}/summary",
		"Feature Progress",
		mcp.WithTemplateDescription("Progress summary for a feature including task counts by status"),
		mcp.WithTemplateMIMEType("application/json"),
	)
	s.AddResourceTemplate(template, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		featureID := extractFeatureIDFromSummaryURI(req.Params.URI)
		if featureID == "" {
			return nil, fmt.Errorf("invalid feature URI: %s", req.Params.URI)
		}

		// Scan projects to find the feature.
		projects, err := svc.Project.ListProjects(ctx, false)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}

		type featureSummary struct {
			Feature    *model.Feature `json:"feature"`
			TaskCounts map[string]int `json:"task_counts"`
			TotalTasks int            `json:"total_tasks"`
		}

		for _, p := range projects {
			feature, ferr := svc.Feature.GetFeature(ctx, p.ID, featureID)
			if ferr != nil {
				continue
			}

			tasks, terr := svc.Task.ListTasks(ctx, p.ID, store.TaskFilter{FeatureID: featureID})
			if terr != nil {
				tasks = nil
			}

			counts := make(map[string]int)
			for _, t := range tasks {
				counts[t.Status]++
			}

			summary := featureSummary{
				Feature:    feature,
				TaskCounts: counts,
				TotalTasks: len(tasks),
			}

			data, err := json.Marshal(summary)
			if err != nil {
				return nil, fmt.Errorf("marshal feature summary: %w", err)
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		}

		return nil, fmt.Errorf("feature %s not found in any project", featureID)
	})
}

// extractURIParam extracts the parameter between a prefix and optional suffix from a URI.
func extractURIParam(uri, prefix, suffix string) string {
	s := strings.TrimPrefix(uri, prefix)
	if suffix != "" {
		s = strings.TrimSuffix(s, suffix)
	}
	return strings.TrimSuffix(s, "/")
}

// extractTaskIDFromContextURI extracts task_id from "task://{task_id}/context".
var taskContextRe = regexp.MustCompile(`^task://([^/]+)/context$`)

func extractTaskIDFromContextURI(uri string) string {
	matches := taskContextRe.FindStringSubmatch(uri)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// extractFeatureIDFromSummaryURI extracts feature_id from "feature://{feature_id}/summary".
var featureSummaryRe = regexp.MustCompile(`^feature://([^/]+)/summary$`)

func extractFeatureIDFromSummaryURI(uri string) string {
	matches := featureSummaryRe.FindStringSubmatch(uri)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

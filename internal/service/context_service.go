package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// ContextService handles context filtering and de-noising for task context
// returned to agents. It resolves dependency summaries and required API contracts.
type ContextService struct {
	taskStore     store.TaskStore
	contractStore store.ContractStore
}

// NewContextService creates a new ContextService instance.
func NewContextService(taskStore store.TaskStore, contractStore store.ContractStore) *ContextService {
	return &ContextService{
		taskStore:     taskStore,
		contractStore: contractStore,
	}
}

// TaskContextResult holds the assembled context for a single task.
type TaskContextResult struct {
	Task                *model.Task
	DependencySummaries map[string]string    // taskID -> summary
	APIContracts        []*model.APIContract // resolved from required_apis
}

// apiRef is the JSON structure used in the tasks.required_apis column.
type apiRef struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// GetTaskContext assembles the full context for a task by resolving its
// dependency summaries and required API contracts.
func (s *ContextService) GetTaskContext(ctx context.Context, projectID, taskID string) (*TaskContextResult, error) {
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task context: get task %s: %w", taskID, err)
	}

	// Resolve dependency summaries.
	depSummaries, err := s.resolveDependencySummaries(ctx, projectID, task.Dependencies)
	if err != nil {
		return nil, fmt.Errorf("get task context: resolve dependencies: %w", err)
	}

	// Resolve required API contracts.
	apiContracts, err := s.resolveRequiredAPIs(ctx, projectID, task.RequiredAPIs)
	if err != nil {
		return nil, fmt.Errorf("get task context: resolve required apis: %w", err)
	}

	return &TaskContextResult{
		Task:                task,
		DependencySummaries: depSummaries,
		APIContracts:        apiContracts,
	}, nil
}

// GetDependencySummaries returns a map of taskID -> summary for the given dependency list.
// If a dependency task is not found, its summary will be an empty string.
// Other errors (database failures) are propagated.
// Summaries longer than maxDependencySummaryChars are truncated with [TRUNCATED] suffix.
func (s *ContextService) GetDependencySummaries(ctx context.Context, projectID string, deps []model.Dependency) (map[string]string, error) {
	summaries := make(map[string]string, len(deps))
	for _, dep := range deps {
		depTask, err := s.taskStore.GetByID(ctx, projectID, dep.TaskID)
		if err != nil {
			if errors.Is(err, store.ErrTaskNotFound) {
				summaries[dep.TaskID] = ""
				continue
			}
			return nil, fmt.Errorf("resolve dependency %s: %w", dep.TaskID, err)
		}
		if depTask.Summary != nil && *depTask.Summary != "" {
			summaries[dep.TaskID] = truncateSummary(*depTask.Summary, maxDependencySummaryChars)
		} else {
			// Fallback to title when no summary is available (PRD context-filtering.md).
			summaries[dep.TaskID] = depTask.Title
		}
	}
	return summaries, nil
}

const maxDependencySummaryChars = 2000

// truncateSummary truncates s to maxLen characters, appending "[TRUNCATED]" if needed.
func truncateSummary(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "[TRUNCATED]"
}

// resolveDependencySummaries parses the dependencies JSON field and resolves
// each dependency task's summary.
func (s *ContextService) resolveDependencySummaries(ctx context.Context, projectID string, depsJSON json.RawMessage) (map[string]string, error) {
	if len(depsJSON) == 0 {
		return make(map[string]string), nil
	}

	var deps []model.Dependency
	if err := json.Unmarshal(depsJSON, &deps); err != nil {
		return nil, fmt.Errorf("parse dependencies json: %w", err)
	}

	return s.GetDependencySummaries(ctx, projectID, deps)
}

// resolveRequiredAPIs parses the required_apis JSON field and queries the
// contract store for each referenced API. Missing contracts are silently skipped;
// other errors are propagated.
func (s *ContextService) resolveRequiredAPIs(ctx context.Context, projectID string, apisJSON json.RawMessage) ([]*model.APIContract, error) {
	if len(apisJSON) == 0 {
		return nil, nil
	}

	var refs []apiRef
	if err := json.Unmarshal(apisJSON, &refs); err != nil {
		return nil, fmt.Errorf("parse required_apis json: %w", err)
	}

	contracts := make([]*model.APIContract, 0, len(refs))
	for _, ref := range refs {
		contract, err := s.contractStore.GetByMethodPath(ctx, projectID, ref.Method, ref.Path)
		if err != nil {
			if errors.Is(err, store.ErrContractNotFound) {
				continue
			}
			return nil, fmt.Errorf("resolve api contract %s %s: %w", ref.Method, ref.Path, err)
		}
		contracts = append(contracts, contract)
	}
	return contracts, nil
}

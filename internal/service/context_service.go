package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	if s == nil || s.taskStore == nil || s.contractStore == nil {
		return nil, NewContextBuildError(
			ContextErrorBuildFailed,
			"context service dependencies are unavailable",
			nil,
		)
	}
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(taskID) == "" {
		return nil, NewContextBuildError(
			ContextErrorSourceInvalid,
			"project_id and task_id are required",
			store.ErrInvalidParameter,
		)
	}
	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, classifyContextSourceError("task "+taskID, err)
	}
	if task == nil || task.ProjectID != projectID || task.ID != taskID {
		return nil, NewContextBuildError(
			ContextErrorBuildFailed,
			"task source identity does not match the requested scope",
			store.ErrProjectScopeViolation,
		)
	}

	// Resolve dependency summaries.
	depSummaries, err := s.resolveDependencySummaries(ctx, projectID, task.Dependencies)
	if err != nil {
		return nil, err
	}

	// Resolve required API contracts.
	apiContracts, err := s.resolveRequiredAPIs(ctx, projectID, task.RequiredAPIs)
	if err != nil {
		return nil, err
	}

	return &TaskContextResult{
		Task:                task,
		DependencySummaries: depSummaries,
		APIContracts:        apiContracts,
	}, nil
}

// GetDependencySummaries returns a map of taskID -> summary for the given dependency list.
// Every listed dependency is a required source. Missing dependencies and
// storage errors fail closed; an empty placeholder is never synthesized.
// Summaries longer than maxDependencySummaryChars are truncated with [TRUNCATED] suffix.
func (s *ContextService) GetDependencySummaries(ctx context.Context, projectID string, deps []model.Dependency) (map[string]string, error) {
	if s == nil || s.taskStore == nil {
		return nil, NewContextBuildError(ContextErrorBuildFailed, "task source is unavailable", nil)
	}
	summaries := make(map[string]string, len(deps))
	seen := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		dep.TaskID = strings.TrimSpace(dep.TaskID)
		if dep.TaskID == "" {
			return nil, NewContextBuildError(
				ContextErrorSourceInvalid,
				"dependency task_id is required",
				store.ErrInvalidParameter,
			)
		}
		if dep.RequireState != "" && dep.RequireState != model.TaskStatusDone && dep.RequireState != model.TaskStatusValidating {
			return nil, NewContextBuildError(
				ContextErrorSourceInvalid,
				fmt.Sprintf("dependency %s has unsupported require_state", dep.TaskID),
				store.ErrInvalidParameter,
			)
		}
		if _, duplicate := seen[dep.TaskID]; duplicate {
			return nil, NewContextBuildError(
				ContextErrorSourceInvalid,
				fmt.Sprintf("dependency %s is duplicated", dep.TaskID),
				store.ErrInvalidParameter,
			)
		}
		seen[dep.TaskID] = struct{}{}
		depTask, err := s.taskStore.GetByID(ctx, projectID, dep.TaskID)
		if err != nil {
			return nil, classifyContextSourceError("dependency "+dep.TaskID, err)
		}
		if depTask == nil || depTask.ID != dep.TaskID || depTask.ProjectID != projectID {
			return nil, NewContextBuildError(
				ContextErrorBuildFailed,
				fmt.Sprintf("dependency %s returned a mismatched source", dep.TaskID),
				store.ErrProjectScopeViolation,
			)
		}
		if !contextDependencyStateSatisfied(dep.RequireState, depTask.Status) {
			return nil, NewContextBuildError(
				ContextErrorRequiredSourceMissing,
				fmt.Sprintf("dependency %s no longer satisfies required state", dep.TaskID),
				store.ErrDependencyNotReady,
			)
		}
		if depTask.Summary != nil && *depTask.Summary != "" {
			summaries[dep.TaskID] = truncateSummary(*depTask.Summary, maxDependencySummaryChars)
		} else if depTask.Title != "" {
			// Fallback to title when no summary is available (PRD context-filtering.md).
			summaries[dep.TaskID] = depTask.Title
		} else {
			return nil, NewContextBuildError(
				ContextErrorRequiredSourceMissing,
				fmt.Sprintf("dependency %s has no summary or title", dep.TaskID),
				store.ErrTaskNotFound,
			)
		}
	}
	return summaries, nil
}

const maxDependencySummaryChars = 2000

// truncateSummary truncates s to maxLen characters, appending "[TRUNCATED]" if needed.
func truncateSummary(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "[TRUNCATED]"
}

// resolveDependencySummaries parses the dependencies JSON field and resolves
// each dependency task's summary.
func (s *ContextService) resolveDependencySummaries(ctx context.Context, projectID string, depsJSON json.RawMessage) (map[string]string, error) {
	deps, err := decodeContextJSONArray[model.Dependency](depsJSON, "dependencies")
	if err != nil {
		return nil, err
	}

	return s.GetDependencySummaries(ctx, projectID, deps)
}

// resolveRequiredAPIs parses the required_apis JSON field and queries the
// contract store for each referenced API. Every listed contract is a required
// source; missing, malformed, cross-project, or mismatched sources fail closed.
func (s *ContextService) resolveRequiredAPIs(ctx context.Context, projectID string, apisJSON json.RawMessage) ([]*model.APIContract, error) {
	refs, err := decodeContextJSONArray[apiRef](apisJSON, "required_apis")
	if err != nil {
		return nil, err
	}

	contracts := make([]*model.APIContract, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if !validContextAPIMethod(ref.Method) || !validContextAPIPath(ref.Path) {
			return nil, NewContextBuildError(
				ContextErrorSourceInvalid,
				fmt.Sprintf("required API reference %q %q is invalid", ref.Method, ref.Path),
				store.ErrInvalidParameter,
			)
		}
		identity := ref.Method + " " + ref.Path
		if _, duplicate := seen[identity]; duplicate {
			return nil, NewContextBuildError(
				ContextErrorSourceInvalid,
				"required API reference "+identity+" is duplicated",
				store.ErrInvalidParameter,
			)
		}
		seen[identity] = struct{}{}
		contract, err := s.contractStore.GetByMethodPath(ctx, projectID, ref.Method, ref.Path)
		if err != nil {
			return nil, classifyContextSourceError("required API "+identity, err)
		}
		if contract == nil || contract.ProjectID != projectID || contract.Method != ref.Method || contract.Path != ref.Path {
			return nil, NewContextBuildError(
				ContextErrorBuildFailed,
				"required API "+identity+" returned a mismatched source",
				store.ErrProjectScopeViolation,
			)
		}
		contracts = append(contracts, contract)
	}
	return contracts, nil
}

func decodeContextJSONArray[T any](raw json.RawMessage, field string) ([]T, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return make([]T, 0), nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, NewContextBuildError(
			ContextErrorSourceInvalid,
			field+" must be a JSON array, not null",
			store.ErrInvalidParameter,
		)
	}
	var values []T
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, NewContextBuildError(
			ContextErrorSourceInvalid,
			field+" must be a strict JSON array",
			err,
		)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = store.ErrInvalidParameter
		}
		return nil, NewContextBuildError(
			ContextErrorSourceInvalid,
			field+" contains trailing data",
			err,
		)
	}
	if values == nil {
		values = make([]T, 0)
	}
	return values, nil
}

func classifyContextSourceError(source string, err error) error {
	if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, store.ErrContractNotFound) {
		return NewContextBuildError(
			ContextErrorRequiredSourceMissing,
			source+" is required but unavailable",
			err,
		)
	}
	return NewContextBuildError(
		ContextErrorBuildFailed,
		"failed to read "+source,
		err,
	)
}

func validContextAPIMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "DELETE", "PATCH":
		return true
	default:
		return false
	}
}

func validContextAPIPath(path string) bool {
	return strings.HasPrefix(path, "/") &&
		!strings.ContainsRune(path, '\x00') &&
		!strings.ContainsAny(path, "\r\n")
}

func contextDependencyStateSatisfied(requireState, currentState string) bool {
	if requireState == model.TaskStatusValidating {
		switch currentState {
		case model.TaskStatusValidating, model.TaskStatusReadyForHumanMerge,
			model.TaskStatusDone, model.TaskStatusCancelled:
			return true
		default:
			return false
		}
	}
	return currentState == model.TaskStatusDone || currentState == model.TaskStatusCancelled
}

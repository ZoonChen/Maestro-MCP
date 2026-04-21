package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// WorktreeService manages git worktree lifecycle: creation, status updates,
// and garbage collection of abandoned worktrees.
type WorktreeService struct {
	worktreeStore store.WorktreeStore
}

// NewWorktreeService creates a new WorktreeService with the required store dependency.
func NewWorktreeService(worktreeStore store.WorktreeStore) *WorktreeService {
	return &WorktreeService{
		worktreeStore: worktreeStore,
	}
}

// CreateWorktree creates a new worktree record and returns the auto-incremented ID.
func (s *WorktreeService) CreateWorktree(ctx context.Context, projectID string, w *model.Worktree) (int64, error) {
	id, err := s.worktreeStore.Create(ctx, projectID, w)
	if err != nil {
		return 0, fmt.Errorf("create worktree: %w", err)
	}
	return id, nil
}

// GetWorktreeByTask retrieves the worktree associated with a specific task.
func (s *WorktreeService) GetWorktreeByTask(ctx context.Context, projectID, taskID string) (*model.Worktree, error) {
	w, err := s.worktreeStore.GetByTaskID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("get worktree by task %s: %w", taskID, err)
	}
	return w, nil
}

// UpdateWorktreeStatus changes the status of a worktree (e.g., allocated -> active -> stale).
func (s *WorktreeService) UpdateWorktreeStatus(ctx context.Context, projectID string, id int64, status string) error {
	if err := s.worktreeStore.UpdateStatus(ctx, projectID, id, status); err != nil {
		return fmt.Errorf("update worktree status: %w", err)
	}
	return nil
}

// ListWorktreesByStatus returns all worktrees matching the given status within a project.
func (s *WorktreeService) ListWorktreesByStatus(ctx context.Context, projectID, status string) ([]*model.Worktree, error) {
	worktrees, err := s.worktreeStore.ListByStatus(ctx, projectID, status)
	if err != nil {
		return nil, fmt.Errorf("list worktrees by status %s: %w", status, err)
	}
	return worktrees, nil
}

// DeleteWorktree removes a worktree record by ID within a project.
func (s *WorktreeService) DeleteWorktree(ctx context.Context, projectID string, id int64) error {
	if err := s.worktreeStore.Delete(ctx, projectID, id); err != nil {
		return fmt.Errorf("delete worktree %d: %w", id, err)
	}
	return nil
}

// GCWorktrees performs garbage collection of abandoned and merged worktrees within a project.
// It finds all worktrees with status "abandoned" or "merged", physically removes their git
// worktrees, and deletes their database records.
func (s *WorktreeService) GCWorktrees(ctx context.Context, projectID string) error {
	for _, status := range []string{model.WorktreeStatusAbandoned, model.WorktreeStatusMerged} {
		worktrees, err := s.worktreeStore.ListByStatus(ctx, projectID, status)
		if err != nil {
			return fmt.Errorf("gc worktrees list %s: %w", status, err)
		}

		for _, wt := range worktrees {
			// Derive workspace path from worktree path convention: <workspace>/.maestro/worktrees/<taskID>
			workspacePath := wt.WorktreePath
			if idx := strings.LastIndex(wt.WorktreePath, "/.maestro/worktrees/"); idx > 0 {
				workspacePath = wt.WorktreePath[:idx]
			}

			// Attempt physical git worktree removal (best-effort).
			if wt.WorktreePath != "" {
				if err := removeWorktree(ctx, workspacePath, wt.WorktreePath); err != nil {
					slog.Error("GCWorktrees: failed to remove physical worktree", "worktree_path", wt.WorktreePath, "error", err)
				}
			}

			// Delete the associated git branch (best-effort).
			if wt.BranchName != "" && workspacePath != "" {
				if err := deleteBranch(ctx, workspacePath, wt.BranchName); err != nil {
					slog.Error("GCWorktrees: failed to delete branch", "branch", wt.BranchName, "error", err)
				}
			}

			if err := s.worktreeStore.Delete(ctx, projectID, wt.ID); err != nil {
				return fmt.Errorf("gc worktrees delete %d: %w", wt.ID, err)
			}
		}
	}
	return nil
}

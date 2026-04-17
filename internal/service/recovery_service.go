package service

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// RecoveryService handles startup recovery to reset stale state from previous
// unclean shutdowns. It must be called once after database initialization.
type RecoveryService struct {
	db           *sql.DB
	projectStore store.ProjectStore
}

// NewRecoveryService creates a new RecoveryService.
func NewRecoveryService(db *sql.DB, projectStore store.ProjectStore) *RecoveryService {
	return &RecoveryService{
		db:           db,
		projectStore: projectStore,
	}
}

// Run performs all startup recovery operations. Errors are logged but not fatal —
// the server should still start even if recovery partially fails.
func (s *RecoveryService) Run(ctx context.Context) {
	slog.Info("Running startup recovery...")

	// 1. Reset all online sessions to offline.
	if result, err := s.db.ExecContext(ctx,
		`UPDATE agent_sessions SET status = 'offline' WHERE status = 'online'`); err != nil {
		slog.Error("Recovery: failed to reset sessions", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("Recovery: reset online sessions to offline", "count", n)
	}

	// 2. Reset in_progress tasks back to pending (they were being worked on
	//    when the server crashed, and no worktree state is reliable).
	if result, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'pending',
		    assigned_session_id = NULL, assigned_worker_id = NULL, assigned_at = NULL,
		    updated_at = datetime('now')
		 WHERE status = 'in_progress'`); err != nil {
		slog.Error("Recovery: failed to reset in_progress tasks", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("Recovery: reset in_progress tasks to pending", "count", n)
	}

	// 3. Reset verifying tasks back to submitted (verification was interrupted).
	if result, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'submitted',
		    updated_at = datetime('now')
		 WHERE status = 'verifying'`); err != nil {
		slog.Error("Recovery: failed to reset verifying tasks", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("Recovery: reset verifying tasks to submitted", "count", n)
	}

	// 4. Mark active worktrees as stale.
	projects, err := s.projectStore.List(ctx, true)
	if err != nil {
		slog.Error("Recovery: failed to list projects for worktree cleanup", "error", err)
	} else {
		totalStale := 0
		for _, p := range projects {
			if result, err := s.db.ExecContext(ctx,
				`UPDATE worktrees SET status = 'stale', updated_at = datetime('now')
				 WHERE project_id = ? AND status = 'active'`, p.ID); err != nil {
				slog.Error("Recovery: failed to stale worktrees for project", "project_id", p.ID, "error", err)
			} else if n, _ := result.RowsAffected(); n > 0 {
				totalStale += int(n)
			}
		}
		if totalStale > 0 {
			slog.Info("Recovery: marked active worktrees as stale", "count", totalStale)
		}
	}

	// 5. Clear all workers' current_task_id (orphaned assignments).
	if result, err := s.db.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = NULL WHERE current_task_id IS NOT NULL`); err != nil {
		slog.Error("Recovery: failed to clear worker task assignments", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("Recovery: cleared worker task assignments", "count", n)
	}

	slog.Info("Startup recovery complete.")
}

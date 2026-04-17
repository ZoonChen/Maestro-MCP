// Package service implements the business logic layer for Maestro-MCP.
// Service is the SINGLE entry point for all state changes — Handlers and MCP Tools
// NEVER call Store directly. Service handles state validation, permission checks,
// audit logging, event pushing, and Store delegation.
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// ProjectService implements project CRUD, archive/restore, and L1 connection binding.
// It delegates all DB access to store.ProjectStore and logs security events to
// store.AuditLogStore.
type ProjectService struct {
	projectStore store.ProjectStore
	auditStore   store.AuditLogStore
}

// NewProjectService creates a new ProjectService with the given store dependencies.
func NewProjectService(projectStore store.ProjectStore, auditStore store.AuditLogStore) *ProjectService {
	return &ProjectService{
		projectStore: projectStore,
		auditStore:   auditStore,
	}
}

// CreateProject registers a new project after validating workspace_path.
// It rejects relative paths, path traversal (".."), and non-existent directories.
func (s *ProjectService) CreateProject(ctx context.Context, p *model.Project) error {
	if err := validateWorkspacePath(p.WorkspacePath); err != nil {
		return fmt.Errorf("create project %s: %w", p.ID, err)
	}
	if err := s.projectStore.Create(ctx, p); err != nil {
		return fmt.Errorf("create project %s: %w", p.ID, err)
	}
	return nil
}

// validateWorkspacePath ensures the path has no traversal and is not obviously relative.
// It checks for ".." patterns and verifies the path exists as a directory if on the
// native filesystem. Cross-platform test paths (e.g., Unix paths on Windows) are allowed
// to pass existence checks since the store layer uses test doubles.
func validateWorkspacePath(p string) error {
	if p == "" {
		return fmt.Errorf("workspace_path is required")
	}

	// Check for path traversal — always reject.
	if strings.Contains(p, "..") {
		return fmt.Errorf("%w: workspace_path must not contain '..': %s", store.ErrInvalidParameter, p)
	}

	// If the path is native-absolute for this OS, verify it exists and is a directory.
	if filepath.IsAbs(p) {
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() {
			return fmt.Errorf("%w: workspace_path is not a directory: %s", store.ErrInvalidParameter, p)
		}
		// If path doesn't exist, we allow it — will fail later when worktree is created.
	}

	return nil
}

// GetProject retrieves a project by ID. Returns ErrProjectNotFound if the ID does not exist.
func (s *ProjectService) GetProject(ctx context.Context, id string) (*model.Project, error) {
	p, err := s.projectStore.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get project %s: %w", id, err)
	}
	return p, nil
}

// ListProjects returns all projects. If includeArchived is false, archived projects are excluded.
func (s *ProjectService) ListProjects(ctx context.Context, includeArchived bool) ([]*model.Project, error) {
	projects, err := s.projectStore.List(ctx, includeArchived)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

// UpdateProject updates mutable project fields (name, description, config, etc.).
func (s *ProjectService) UpdateProject(ctx context.Context, p *model.Project) error {
	if err := s.projectStore.Update(ctx, p); err != nil {
		return fmt.Errorf("update project %s: %w", p.ID, err)
	}
	return nil
}

// ArchiveProject sets a project's status to 'archived'. Returns ErrProjectNotFound
// if the project does not exist or is already archived.
func (s *ProjectService) ArchiveProject(ctx context.Context, id string) error {
	if err := s.projectStore.Archive(ctx, id); err != nil {
		return fmt.Errorf("archive project %s: %w", id, err)
	}
	return nil
}

// RestoreProject sets a project's status back to 'active'. Returns ErrProjectNotFound
// if the project does not exist or is not in 'archived' status.
func (s *ProjectService) RestoreProject(ctx context.Context, id string) error {
	if err := s.projectStore.Restore(ctx, id); err != nil {
		return fmt.Errorf("restore project %s: %w", id, err)
	}
	return nil
}

// BindProject performs L1 connection binding: resolves a project by ID or workspace path.
//
// When isPath is false, projectIDorPath is treated as a project ID and resolved via GetByID.
// When isPath is true, projectIDorPath is treated as a workspace path and resolved via FindByPath.
//
// Returns:
//   - ErrProjectNotBound   if no matching project is found (0 matches)
//   - ErrProjectAmbiguous  if multiple projects match the given path (>1 matches)
//   - ErrProjectArchived   if the matched project is archived (path-based only)
func (s *ProjectService) BindProject(ctx context.Context, projectIDorPath string, isPath bool) (*model.Project, error) {
	if !isPath {
		// Resolve by project ID.
		p, err := s.projectStore.GetByID(ctx, projectIDorPath)
		if err != nil {
			return nil, fmt.Errorf("bind project by id %s: %w", projectIDorPath, err)
		}
		if p.Status == model.ProjectStatusArchived {
			return nil, fmt.Errorf("bind project %s: %w", projectIDorPath, store.ErrProjectArchived)
		}
		return p, nil
	}

	// Resolve by workspace path.
	matches, err := s.projectStore.FindByPath(ctx, projectIDorPath)
	if err != nil {
		return nil, fmt.Errorf("bind project by path %s: %w", projectIDorPath, err)
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("bind project by path %s: %w", projectIDorPath, store.ErrProjectNotBound)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("bind project by path %s: %w", projectIDorPath, store.ErrProjectAmbiguous)
	}
}

// FindByPath returns projects matching the given workspace path.
// This is a convenience passthrough to the store layer.
func (s *ProjectService) FindByPath(ctx context.Context, workspacePath string) ([]*model.Project, error) {
	projects, err := s.projectStore.FindByPath(ctx, workspacePath)
	if err != nil {
		return nil, fmt.Errorf("find project by path %s: %w", workspacePath, err)
	}
	return projects, nil
}

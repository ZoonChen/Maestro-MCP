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
	if p == nil {
		return fmt.Errorf("create project: %w: project is required", store.ErrInvalidParameter)
	}
	canonicalWorkspace, err := validateWorkspacePath(p.WorkspacePath)
	if err != nil {
		return fmt.Errorf("create project %s: %w", p.ID, err)
	}
	p.WorkspacePath = canonicalWorkspace
	if err := ValidateProjectConfig(p.Config); err != nil {
		return fmt.Errorf("create project %s: %w: %v", p.ID, store.ErrInvalidParameter, err)
	}
	if err := s.projectStore.Create(ctx, p); err != nil {
		return fmt.Errorf("create project %s: %w", p.ID, err)
	}
	return nil
}

// validateWorkspacePath returns one stable, existing canonical directory.
// Registration is the trust-boundary: storing a relative, missing, or unresolved
// symlink path would let the process resolve a different repository later.
func validateWorkspacePath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("%w: workspace_path is required", store.ErrInvalidParameter)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%w: workspace_path contains NUL", store.ErrInvalidParameter)
	}
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return "", fmt.Errorf("%w: workspace_path must not contain '..': %s", store.ErrInvalidParameter, raw)
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: workspace_path must be absolute: %s", store.ErrInvalidParameter, raw)
	}

	canonical, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%w: resolve workspace_path %s: %v", store.ErrInvalidParameter, raw, err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: canonicalize workspace_path %s: %v", store.ErrInvalidParameter, raw, err)
	}
	volumeRoot := filepath.Clean(filepath.VolumeName(canonical) + string(filepath.Separator))
	if canonical == volumeRoot {
		return "", fmt.Errorf("%w: filesystem root cannot be a project workspace", store.ErrInvalidParameter)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if resolvedHome, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil && canonical == filepath.Clean(resolvedHome) {
			return "", fmt.Errorf("%w: user home cannot be a project workspace", store.ErrInvalidParameter)
		}
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: stat workspace_path %s: %v", store.ErrInvalidParameter, canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: workspace_path is not a directory: %s", store.ErrInvalidParameter, canonical)
	}
	return canonical, nil
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
	if err := ValidateProjectConfig(p.Config); err != nil {
		return fmt.Errorf("update project %s: %w: %v", p.ID, store.ErrInvalidParameter, err)
	}
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

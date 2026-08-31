package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// PostgreSQL implementation of the GitLab instance registry and project
// mapping (M2-GL-001). Secrets enter only as references; the sanitized
// projections never read them back.

// ErrInstanceExists reports a duplicate base_url registration.
var ErrInstanceExists = errors.New("gitlab instance with this base_url already exists")

// ErrMappingConflict reports a mapping row-version mismatch.
var ErrMappingConflict = errors.New("gitlab mapping row version mismatch")

// InstanceView is the sanitized wire projection of one instance.
type InstanceView struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"`
	State   string `json:"state"`
	Version int64  `json:"version"`
}

// MappingView is the wire projection of one project mapping.
type MappingView struct {
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	GitLabInstanceID string `json:"gitlab_instance_id"`
	GitLabProjectID  int64  `json:"gitlab_project_numeric_id"`
	TargetBranch     string `json:"target_branch"`
	State            string `json:"state"`
	Version          int64  `json:"version"`
}

type pgInstanceStore struct{ db *sql.DB }

// Instances returns the instance registry store.
func (s *PostgresStore) Instances() pgInstanceStore { return pgInstanceStore{db: s.DB()} }

// ListInstances returns every instance, sanitized (no secret refs).
func (s pgInstanceStore) ListInstances(ctx context.Context) ([]InstanceView, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, base_url, status, version FROM gitlab_instances ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("instances: list: %w", err)
	}
	defer rows.Close()
	instances := []InstanceView{}
	for rows.Next() {
		var view InstanceView
		if err := rows.Scan(&view.ID, &view.BaseURL, &view.State, &view.Version); err != nil {
			return nil, fmt.Errorf("instances: scan: %w", err)
		}
		instances = append(instances, view)
	}
	return instances, rows.Err()
}

// CreateInstance registers an approved HTTPS host. The wire state enum
// is active/degraded/revoked; new rows enter active and the table's
// operational status (suspended) maps to degraded on read.
func (s pgInstanceStore) CreateInstance(ctx context.Context, baseURL, displayName, botCredentialRef, webhookSecretRef string) (InstanceView, error) {
	if err := validateInstanceURL(baseURL); err != nil {
		return InstanceView{}, err
	}
	if botCredentialRef == "" || webhookSecretRef == "" {
		return InstanceView{}, errors.New("instances: bot and webhook secret refs are required")
	}
	var inserted string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO gitlab_instances (id, base_url, display_name, bot_credential_ref, webhook_secret_ref, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
		ON CONFLICT (base_url) DO NOTHING
		RETURNING id::text`,
		pgNewUUID(), strings.TrimRight(baseURL, "/"), displayName, botCredentialRef, webhookSecretRef).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return InstanceView{}, ErrInstanceExists
	}
	if err != nil {
		return InstanceView{}, fmt.Errorf("instances: create: %w", err)
	}
	return s.byBaseURL(ctx, baseURL)
}

func (s pgInstanceStore) byBaseURL(ctx context.Context, baseURL string) (InstanceView, error) {
	var view InstanceView
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, base_url, status, version FROM gitlab_instances WHERE base_url = $1`,
		strings.TrimRight(baseURL, "/")).Scan(&view.ID, &view.BaseURL, &view.State, &view.Version)
	if err != nil {
		return InstanceView{}, fmt.Errorf("instances: read back: %w", err)
	}
	return view, nil
}

// GetMapping reads the project's current mapping (nil when absent).
func (s pgInstanceStore) GetMapping(ctx context.Context, projectID string) (*MappingView, error) {
	var view MappingView
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, gitlab_instance_id::text, gitlab_project_id,
			COALESCE(default_branch, ''), 'verified', version
		FROM gitlab_project_mappings WHERE project_id = $1`, projectID).
		Scan(&view.ID, &view.ProjectID, &view.GitLabInstanceID, &view.GitLabProjectID,
			&view.TargetBranch, &view.State, &view.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mapping: get: %w", err)
	}
	return &view, nil
}

// PutMapping creates or replaces the project mapping with compare-and-
// -swap semantics (0 demands absence, matching the PUT's If-None-Match).
func (s pgInstanceStore) PutMapping(ctx context.Context, projectID, instanceID string, gitlabProjectID int64, targetBranch string, expectedVersion int64) (*MappingView, error) {
	if gitlabProjectID < 1 {
		return nil, errors.New("mapping: numeric project id must be >= 1")
	}
	if targetBranch == "" || len(targetBranch) > 255 || strings.ContainsAny(targetBranch, " ~^:?*[\\") {
		return nil, errors.New("mapping: target branch is malformed")
	}
	var instanceExists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM gitlab_instances WHERE id = $1 AND status = 'active'`, instanceID).
		Scan(&instanceExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("mapping: instance %s is not an active approved host", instanceID)
		}
		return nil, fmt.Errorf("mapping: instance check: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mapping: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	var version int64
	if expectedVersion == 0 {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO gitlab_project_mappings (id, gitlab_instance_id, gitlab_project_id, project_id, default_branch)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (project_id) DO NOTHING
			RETURNING id::text, version`,
			pgNewUUID(), instanceID, gitlabProjectID, projectID, targetBranch).Scan(&id, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMappingConflict
		}
		if err != nil {
			return nil, fmt.Errorf("mapping: create: %w", err)
		}
	} else {
		result, execErr := tx.ExecContext(ctx, `
			UPDATE gitlab_project_mappings
			SET gitlab_instance_id = $2, gitlab_project_id = $3, default_branch = $4,
			    version = version + 1, updated_at = now()
			WHERE project_id = $1 AND version = $5`,
			projectID, instanceID, gitlabProjectID, targetBranch, expectedVersion)
		if execErr != nil {
			return nil, fmt.Errorf("mapping: replace: %w", execErr)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return nil, ErrMappingConflict
		}
		id, version = "", expectedVersion+1
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mapping: commit: %w", err)
	}
	view, err := s.GetMapping(ctx, projectID)
	if err != nil || view == nil {
		return nil, fmt.Errorf("mapping: read back: %w", err)
	}
	return view, nil
}

// MergeRequestProjection reads one cached MR projection with its
// pipeline states (the frozen read model).
func (s pgInstanceStore) MergeRequestProjection(ctx context.Context, projectID string, mrIID int64) (map[string]any, bool, error) {
	var id, state, source, target string
	var sourceSHA, targetSHA, mergeCommit sql.NullString
	var workItem sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id::text, state, source_branch, target_branch, source_sha, target_sha, merge_commit_sha, work_item_id::text
		FROM merge_requests WHERE project_id = $1 AND mr_iid = $2`, projectID, mrIID).
		Scan(&id, &state, &source, &target, &sourceSHA, &targetSHA, &mergeCommit, &workItem)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("mr projection: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT p.gitlab_pipeline_id, p.status, p.sha FROM pipelines p
		WHERE p.project_id = $1 AND p.ref = $2 ORDER BY p.observed_at DESC LIMIT 10`, projectID, source)
	if err != nil {
		return nil, false, fmt.Errorf("mr projection pipelines: %w", err)
	}
	defer rows.Close()
	pipelines := []map[string]any{}
	for rows.Next() {
		var pipelineID int64
		var status, sha string
		if err := rows.Scan(&pipelineID, &status, &sha); err != nil {
			return nil, false, fmt.Errorf("mr projection pipeline scan: %w", err)
		}
		pipelines = append(pipelines, map[string]any{
			"pipeline_id": pipelineID, "status": status, "sha": sha,
		})
	}

	projection := map[string]any{
		"id": id, "merge_request_iid": mrIID, "state": state,
		"source_branch": source, "target_branch": target,
		"pipelines": pipelines,
	}
	if sourceSHA.Valid {
		projection["source_sha"] = sourceSHA.String
	}
	if targetSHA.Valid {
		projection["target_sha"] = targetSHA.String
	}
	if mergeCommit.Valid {
		projection["merge_commit_sha"] = mergeCommit.String
	}
	if workItem.Valid {
		projection["work_item_id"] = workItem.String
	}
	return projection, true, nil
}

// ProjectExists scopes project-addressed control-plane reads.
func (s pgInstanceStore) ProjectExists(ctx context.Context, projectID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE id = $1`, projectID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("instances: project exists: %w", err)
	}
	return true, nil
}

// validateInstanceURL enforces the GLINT host rules: HTTPS only, no
// userinfo, no IP literals, a plain hostname (DNS names resolve through
// the connector's TLS stack; IP literals and ports with userinfo are
// the SSRF vectors the spec calls out).
func validateInstanceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("instances: base_url is unparseable: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("instances: base_url must be https")
	}
	if parsed.User != nil {
		return errors.New("instances: base_url must not carry userinfo")
	}
	host := parsed.Hostname()
	if host == "" {
		return errors.New("instances: base_url must carry a host")
	}
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		return errors.New("instances: IP-literal hosts are not allowed")
	}
	if strings.ContainsAny(host, " /\\") {
		return errors.New("instances: host is malformed")
	}
	return nil
}

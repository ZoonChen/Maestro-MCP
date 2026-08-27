package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// ---------------------------------------------------------------------------
// ActivityLogStore -- business activity log (append-only, project-scoped)
// ---------------------------------------------------------------------------

// SQLiteActivityLogStore implements ActivityLogStore backed by SQLite.
type SQLiteActivityLogStore struct {
	db *sql.DB
}

// NewActivityLogStore creates a new ActivityLogStore instance.
func NewActivityLogStore(db *sql.DB) *SQLiteActivityLogStore {
	return &SQLiteActivityLogStore{db: db}
}

// activityLogColumns is the ordered column list for SELECT queries on activity_log.
// Order matches DDL: id, project_id, session_id, task_id, action, detail, created_at.
const activityLogColumns = `id, project_id, session_id, task_id, action, detail, created_at`

// scanActivityLog scans a single row into an ActivityLog struct.
func scanActivityLog(scanner interface {
	Scan(dest ...any) error
}) (*model.ActivityLog, error) {
	var l model.ActivityLog
	var sessionID, taskID, detail sql.NullString

	err := scanner.Scan(
		&l.ID,
		&l.ProjectID,
		&sessionID,
		&taskID,
		&l.Action,
		&detail,
		&l.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	if sessionID.Valid {
		l.SessionID = &sessionID.String
	}
	if taskID.Valid {
		l.TaskID = &taskID.String
	}
	if detail.Valid {
		l.Detail = &detail.String
	}

	return &l, nil
}

// Create appends an activity log entry. projectID is taken from the parameter,
// not from the struct, to enforce L4 isolation at the store layer.
func (s *SQLiteActivityLogStore) Create(ctx context.Context, projectID string, log *model.ActivityLog) error {
	if log.CreatedAt == "" {
		log.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	const query = `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		projectID,
		log.SessionID,
		log.TaskID,
		log.Action,
		log.Detail,
		log.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("activity log create: %w", err)
	}
	return nil
}

// List returns activity logs ordered by created_at descending.
// limit controls the maximum number of rows returned.
// since is an optional ISO8601 timestamp; if non-empty, only logs after this time are returned.
func (s *SQLiteActivityLogStore) List(ctx context.Context, projectID string, limit int, since string) ([]*model.ActivityLog, error) {
	var query strings.Builder
	args := []interface{}{projectID}

	fmt.Fprintf(&query, `SELECT %s FROM activity_log WHERE project_id = ?`, activityLogColumns)

	if since != "" {
		query.WriteString(` AND created_at >= ?`)
		args = append(args, since)
	}

	query.WriteString(` ORDER BY created_at DESC`)

	if limit > 0 {
		query.WriteString(` LIMIT ?`)
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("activity log list: %w", err)
	}
	defer rows.Close()

	var logs []*model.ActivityLog
	for rows.Next() {
		l, err := scanActivityLog(rows)
		if err != nil {
			return nil, fmt.Errorf("activity log list scan: %w", err)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("activity log list rows: %w", err)
	}
	return logs, nil
}

// Verify interface compliance at compile time.
var _ ActivityLogStore = (*SQLiteActivityLogStore)(nil)

// ---------------------------------------------------------------------------
// AuditLogStore -- security audit log (append-only, cross-project)
// Create does not take projectID -- bound_project is in the AuditLog struct.
// List uses AuditFilter for dynamic WHERE clause.
// CountDenied counts where result='DENIED' for cross-project session abuse detection.
// ---------------------------------------------------------------------------

// SQLiteAuditLogStore implements AuditLogStore backed by SQLite.
type SQLiteAuditLogStore struct {
	db *sql.DB
}

// NewAuditLogStore creates a new AuditLogStore instance.
func NewAuditLogStore(db *sql.DB) *SQLiteAuditLogStore {
	return &SQLiteAuditLogStore{db: db}
}

// auditLogColumns is the ordered column list for SELECT queries on audit_log.
// Order matches DDL: id, session_id, bound_project, target_project, target_task,
// action, path, result, detail, created_at.
const auditLogColumns = `id, session_id, bound_project, target_project, target_task,
	action, path, result, detail, created_at`

// scanAuditLog scans a single row into an AuditLog struct.
func scanAuditLog(scanner interface {
	Scan(dest ...any) error
}) (*model.AuditLog, error) {
	var l model.AuditLog
	var sessionID, targetProject, targetTask, path, detail sql.NullString

	err := scanner.Scan(
		&l.ID,
		&sessionID,
		&l.BoundProject,
		&targetProject,
		&targetTask,
		&l.Action,
		&path,
		&l.Result,
		&detail,
		&l.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	if sessionID.Valid {
		l.SessionID = &sessionID.String
	}
	if targetProject.Valid {
		l.TargetProject = &targetProject.String
	}
	if targetTask.Valid {
		l.TargetTask = &targetTask.String
	}
	if path.Valid {
		l.Path = &path.String
	}
	if detail.Valid {
		l.Detail = &detail.String
	}

	return &l, nil
}

// Create appends a security audit log entry.
// The project context is in the AuditLog struct (bound_project), so no
// external projectID parameter is needed.
func (s *SQLiteAuditLogStore) Create(ctx context.Context, log *model.AuditLog) error {
	if log.CreatedAt == "" {
		log.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	const query = `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, path, result, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		log.SessionID,
		log.BoundProject,
		log.TargetProject,
		log.TargetTask,
		log.Action,
		log.Path,
		log.Result,
		log.Detail,
		log.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("audit log create: %w", err)
	}
	return nil
}

// List queries audit logs with dynamic filtering based on AuditFilter.
// Empty/zero fields in the filter are ignored (no filter applied for that field).
// filter.BoundProject -> bound_project, filter.SessionID -> session_id,
// filter.Result -> result.
func (s *SQLiteAuditLogStore) List(ctx context.Context, filter AuditFilter) ([]*model.AuditLog, error) {
	var query strings.Builder
	var args []interface{}

	fmt.Fprintf(&query, `SELECT %s FROM audit_log WHERE 1=1`, auditLogColumns)

	if filter.BoundProject != "" {
		query.WriteString(` AND bound_project = ?`)
		args = append(args, filter.BoundProject)
	}
	if filter.SessionID != "" {
		query.WriteString(` AND session_id = ?`)
		args = append(args, filter.SessionID)
	}
	if filter.Result != "" {
		query.WriteString(` AND result = ?`)
		args = append(args, filter.Result)
	}
	if filter.Since != "" {
		query.WriteString(` AND created_at >= ?`)
		args = append(args, filter.Since)
	}

	query.WriteString(` ORDER BY created_at DESC`)

	if filter.Limit > 0 {
		query.WriteString(` LIMIT ?`)
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("audit log list: %w", err)
	}
	defer rows.Close()

	var logs []*model.AuditLog
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return nil, fmt.Errorf("audit log list scan: %w", err)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit log list rows: %w", err)
	}
	return logs, nil
}

// CountDenied returns the number of DENIED audit records for a given session
// since the specified timestamp. This is used for cross-project session abuse
// detection and therefore does not take projectID.
func (s *SQLiteAuditLogStore) CountDenied(ctx context.Context, sessionID, since string) (int, error) {
	const query = `SELECT COUNT(*) FROM audit_log
		WHERE result = 'DENIED' AND session_id = ? AND created_at >= ?`

	var count int
	err := s.db.QueryRowContext(ctx, query, sessionID, since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("audit log count denied: %w", err)
	}
	return count, nil
}

// Verify interface compliance at compile time.
var _ AuditLogStore = (*SQLiteAuditLogStore)(nil)

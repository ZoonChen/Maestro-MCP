package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteFeatureStore implements FeatureStore backed by SQLite.
// All methods require projectID as the first parameter for L4 isolation.
type SQLiteFeatureStore struct {
	db *sql.DB
}

// NewSQLiteFeatureStore creates a new FeatureStore backed by the given db.
func NewSQLiteFeatureStore(db *sql.DB) *SQLiteFeatureStore {
	return &SQLiteFeatureStore{db: db}
}

// Create inserts a new feature within the given project.
func (s *SQLiteFeatureStore) Create(ctx context.Context, projectID string, f *model.Feature) error {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO features (id, project_id, title, description, reference_urls, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, projectID, f.Title, f.Description, f.ReferenceURLs, f.Status, ts, ts,
	)
	if err != nil {
		return fmt.Errorf("insert feature %s in project %s: %w", f.ID, projectID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("insert feature %s in project %s: rows affected: %w", f.ID, projectID, err)
	}
	if rows == 0 {
		return fmt.Errorf("insert feature %s in project %s: no rows affected", f.ID, projectID)
	}
	return nil
}

// GetByID retrieves a feature by ID within the given project.
func (s *SQLiteFeatureStore) GetByID(ctx context.Context, projectID, id string) (*model.Feature, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, title, description, reference_urls, status, created_at, updated_at
		 FROM features
		 WHERE project_id = ? AND id = ?`,
		projectID, id,
	)
	f, err := scanFeature(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrFeatureNotFound
		}
		return nil, fmt.Errorf("get feature %s in project %s: %w", id, projectID, err)
	}
	return f, nil
}

// List returns all features within the given project, ordered by created_at.
func (s *SQLiteFeatureStore) List(ctx context.Context, projectID string) ([]*model.Feature, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, title, description, reference_urls, status, created_at, updated_at
		 FROM features
		 WHERE project_id = ?
		 ORDER BY created_at`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list features in project %s: %w", projectID, err)
	}
	defer rows.Close()

	var features []*model.Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, fmt.Errorf("scan feature: %w", err)
		}
		features = append(features, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list features in project %s: %w", projectID, err)
	}
	return features, nil
}

// Update updates mutable feature fields within the given project.
func (s *SQLiteFeatureStore) Update(ctx context.Context, projectID string, f *model.Feature) error {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE features
		 SET title = ?, description = ?, reference_urls = ?, status = ?, updated_at = ?
		 WHERE project_id = ? AND id = ?`,
		f.Title, f.Description, f.ReferenceURLs, f.Status, ts,
		projectID, f.ID,
	)
	if err != nil {
		return fmt.Errorf("update feature %s in project %s: %w", f.ID, projectID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update feature %s in project %s: rows affected: %w", f.ID, projectID, err)
	}
	if rows == 0 {
		return ErrFeatureNotFound
	}
	return nil
}

// scanFeature scans a feature row from a *sql.Row or *sql.Rows.
// Column order: id, project_id, title, description, reference_urls, status, created_at, updated_at.
func scanFeature(sc scan) (*model.Feature, error) {
	var f model.Feature
	err := sc.Scan(
		&f.ID,
		&f.ProjectID,
		&f.Title,
		&f.Description,
		&f.ReferenceURLs,
		&f.Status,
		&f.CreatedAt,
		&f.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// CountByProject returns the number of features in a project.
func (s *SQLiteFeatureStore) CountByProject(ctx context.Context, projectID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM features WHERE project_id = ?`, projectID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count features for project %s: %w", projectID, err)
	}
	return count, nil
}

// Compile-time interface assertion.
var _ FeatureStore = (*SQLiteFeatureStore)(nil)

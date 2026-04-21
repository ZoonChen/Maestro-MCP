package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// SQLiteContractStore implements ContractStore backed by SQLite.
type SQLiteContractStore struct {
	db *sql.DB
}

// NewContractStore creates a new ContractStore instance.
func NewContractStore(db *sql.DB) *SQLiteContractStore {
	return &SQLiteContractStore{db: db}
}

// contractColumns is the ordered column list for SELECT queries on api_contracts.
// Order matches DDL: id, project_id, method, path, request_schema, response_schema,
// description, source_file, parsed_at.
const contractColumns = `id, project_id, method, path, request_schema, response_schema,
	description, source_file, parsed_at`

// scanContract scans a single row into an APIContract struct.
func scanContract(scanner interface {
	Scan(dest ...any) error
}) (*model.APIContract, error) {
	var c model.APIContract
	var requestSchema, responseSchema, description sql.NullString

	err := scanner.Scan(
		&c.ID,
		&c.ProjectID,
		&c.Method,
		&c.Path,
		&requestSchema,
		&responseSchema,
		&description,
		&c.SourceFile,
		&c.ParsedAt,
	)
	if err != nil {
		return nil, err
	}

	if requestSchema.Valid {
		c.RequestSchema = &requestSchema.String
	}
	if responseSchema.Valid {
		c.ResponseSchema = &responseSchema.String
	}
	if description.Valid {
		c.Description = &description.String
	}

	return &c, nil
}

// Upsert inserts or replaces an API contract. The unique key is (project_id, method, path).
func (s *SQLiteContractStore) Upsert(ctx context.Context, projectID string, c *model.APIContract) error {
	const query = `INSERT OR REPLACE INTO api_contracts
		(project_id, method, path, request_schema, response_schema, description, source_file, parsed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, query,
		projectID,
		c.Method,
		c.Path,
		c.RequestSchema,
		c.ResponseSchema,
		c.Description,
		c.SourceFile,
		c.ParsedAt,
	)
	if err != nil {
		return fmt.Errorf("contract upsert: %w", err)
	}
	return nil
}

// GetByMethodPath retrieves a contract by HTTP method and path within a project.
func (s *SQLiteContractStore) GetByMethodPath(ctx context.Context, projectID, method, path string) (*model.APIContract, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM api_contracts
		WHERE project_id = ? AND method = ? AND path = ?`, contractColumns)

	row := s.db.QueryRowContext(ctx, query, projectID, method, path)
	c, err := scanContract(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrContractNotFound
		}
		return nil, fmt.Errorf("contract get by method path: %w", err)
	}
	return c, nil
}

// List returns all API contracts for a project ordered by method and path.
func (s *SQLiteContractStore) List(ctx context.Context, projectID string) ([]*model.APIContract, error) {
	query := fmt.Sprintf(`
		SELECT %s FROM api_contracts
		WHERE project_id = ?
		ORDER BY method, path`, contractColumns)

	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("contract list: %w", err)
	}
	defer rows.Close()

	var contracts []*model.APIContract
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, fmt.Errorf("contract list scan: %w", err)
		}
		contracts = append(contracts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("contract list rows: %w", err)
	}
	return contracts, nil
}

// DeleteByProject removes all contracts for a project (used before re-parsing).
func (s *SQLiteContractStore) DeleteByProject(ctx context.Context, projectID string) error {
	const query = `DELETE FROM api_contracts WHERE project_id = ?`
	_, err := s.db.ExecContext(ctx, query, projectID)
	if err != nil {
		return fmt.Errorf("contract delete by project: %w", err)
	}
	return nil
}

// Verify interface compliance at compile time.
var _ ContractStore = (*SQLiteContractStore)(nil)

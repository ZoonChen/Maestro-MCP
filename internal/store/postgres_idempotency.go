package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PostgreSQL implementation of APIIdempotencyStore (TECH-DATA-001 section
// 8): the same key with the same request hash replays the stored outcome;
// the same key with a different body is a conflict, never a silent rerun.

type pgIdempotencyStore struct{ q pgExecer }

func (s pgIdempotencyStore) LookupOrCreate(ctx context.Context, record *IdempotencyRecord) (bool, *IdempotencyRecord, error) {
	if record.PrincipalID == "" || record.Operation == "" || record.Key == "" {
		return false, nil, errors.New("idempotency: principal, operation and key are required")
	}
	if record.ProjectID == "" {
		// Platform-scoped operations share the "-" bucket so the unique key
		// stays four-part everywhere.
		record.ProjectID = "-"
	}
	inserted, err := s.q.ExecContext(ctx, `
		INSERT INTO api_idempotency (principal_id, project_id, operation, key, request_hash, response_status, response_summary)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (principal_id, project_id, operation, key) DO NOTHING`,
		record.PrincipalID, record.ProjectID, record.Operation, record.Key,
		record.RequestHash, nullableStatus(record.ResponseStatus), record.ResponseSummary)
	if err != nil {
		return false, nil, fmt.Errorf("idempotency: insert: %w", err)
	}
	if affected, _ := inserted.RowsAffected(); affected == 1 {
		return false, nil, nil
	}

	var (
		existing IdempotencyRecord
		status   sql.NullInt64
	)
	err = s.q.QueryRowContext(ctx, `
		SELECT principal_id, project_id, operation, key, request_hash, response_status, response_summary
		FROM api_idempotency
		WHERE principal_id = $1 AND project_id = $2 AND operation = $3 AND key = $4`,
		record.PrincipalID, record.ProjectID, record.Operation, record.Key).
		Scan(&existing.PrincipalID, &existing.ProjectID, &existing.Operation, &existing.Key,
			&existing.RequestHash, &status, &existing.ResponseSummary)
	if err != nil {
		return false, nil, fmt.Errorf("idempotency: lookup: %w", err)
	}
	if status.Valid {
		existing.ResponseStatus = int(status.Int64)
	}
	if existing.RequestHash != record.RequestHash {
		return false, nil, ErrIdempotencyConflict
	}
	return true, &existing, nil
}

func nullableStatus(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

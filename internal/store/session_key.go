package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// scopedSessionKey is the physical v2 SQLite primary key. external_id remains
// the caller-visible Session ID, allowing two projects to use the same logical
// ID without sharing identity or foreign-key authority.
func scopedSessionKey(projectID, externalID string) string {
	digest := sha256.Sum256([]byte(projectID + "\x00" + externalID))
	return "ss_" + hex.EncodeToString(digest[:])
}

func resolveSessionKey(ctx context.Context, q queryRower, projectID, externalID string) (string, error) {
	var key string
	err := q.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, externalID,
	).Scan(&key)
	if err == sql.ErrNoRows {
		return "", ErrSessionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve scoped session key: %w", err)
	}
	return key, nil
}

func resolveNullableSessionKey(ctx context.Context, q queryRower, projectID string, externalID *string) (*string, error) {
	if externalID == nil || *externalID == "" {
		return nil, nil
	}
	key, err := resolveSessionKey(ctx, q, projectID, *externalID)
	if err != nil {
		return nil, err
	}
	return &key, nil
}

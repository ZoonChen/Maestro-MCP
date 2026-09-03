package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/contract"
)

// PostgreSQL persistence for the M3 contract registry (M3-CTR-001):
// versioned canonical hashes bound to the exact source SHA. The same
// (project, service, version) can never carry two different hashes —
// the unique constraint rejects the second write.

// ErrContractHashConflict reports a version whose hash differs from the
// registered one (silent contract swap attempt).
var ErrContractHashConflict = errors.New("contract version already registered with a different hash")

type pgContractStore struct{ db *sql.DB }

// Contracts returns the contract registry store.
func (s *PostgresStore) Contracts() pgContractStore { return pgContractStore{db: s.DB()} }

// RegisterContract versions a parsed contract. Re-registering the SAME
// hash is an idempotent replay; a different hash under the same
// (project, service, version) is the conflict the constraint exists for.
func (s pgContractStore) RegisterContract(ctx context.Context, projectID string, doc *contract.Document, format contract.SourceFormat, service, version, specDigest, sourceSHA string) error {
	var existing sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT canonical_hash FROM api_contracts
		WHERE project_id = $1 AND service = $2 AND version = $3`,
		projectID, service, version).Scan(&existing)
	switch {
	case err == nil:
		if existing.String == doc.CanonicalHash {
			return nil // replay of the identical contract
		}
		return ErrContractHashConflict
	case errors.Is(err, sql.ErrNoRows):
		// fall through to insert
	default:
		return fmt.Errorf("contracts: lookup: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_contracts (id, project_id, service, format, version, canonical_hash, spec_digest, source_sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (project_id, service, version) DO NOTHING`,
		pgNewUUID(), projectID, service, string(format), version,
		doc.CanonicalHash, specDigest, sourceSHA)
	if err != nil {
		return fmt.Errorf("contracts: register: %w", err)
	}
	return nil
}

// ContractVersion resolves the registered canonical hash for one
// (project, service, version); found=false when unversioned.
func (s pgContractStore) ContractVersion(ctx context.Context, projectID, service, version string) (hash string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT canonical_hash FROM api_contracts
		WHERE project_id = $1 AND service = $2 AND version = $3`,
		projectID, service, version).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("contracts: version: %w", err)
	}
	return hash, true, nil
}

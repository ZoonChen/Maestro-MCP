package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/evidence"
)

// PostgreSQL persistence for the quality engine (M2-QG-001): the
// versioned project policy overlay, append-only evidence rows, gate
// snapshot upserts with drift-driven staling, and the waiver lifecycle.
// The engine itself (internal/evidence) is pure and deterministic; every
// write here is single-transaction and state-guarded.

// Quality store sentinel conditions.
var (
	ErrQualityPolicyConflict = errors.New("quality policy row version mismatch")
	ErrQualityPolicyAbsent   = errors.New("quality policy row does not exist")
	ErrWaiverConflict        = errors.New("waiver state changed concurrently")
	ErrWaiverAbsent          = errors.New("waiver does not exist")
	ErrWaiverSelfApprove     = errors.New("waiver approver must differ from the requester")
)

type pgQualityStore struct{ db *sql.DB }

// Quality returns the quality-engine store.
func (s *PostgresStore) Quality() pgQualityStore { return pgQualityStore{db: s.DB()} }

// PutProjectPolicy inserts or replaces the project overlay with
// compare-and-swap semantics: expectedRowVersion 0 demands absence
// (If-None-Match), anything else demands the exact current version.
// The stored digest is recomputed server-side from the canonical policy
// document, never trusted from the caller.
func (s pgQualityStore) PutProjectPolicy(ctx context.Context, projectID string, policy *evidence.Policy, expectedRowVersion int64) (int64, error) {
	if err := policy.Validate(); err != nil {
		return 0, err
	}
	if policy.Scope != "project" {
		return 0, fmt.Errorf("quality policy: stored overlays must be project-scoped")
	}
	digest, err := policy.Digest()
	if err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return 0, fmt.Errorf("quality policy: encode: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("quality policy: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var rowVersion int64
	switch {
	case expectedRowVersion == 0:
		err = tx.QueryRowContext(ctx, `
			INSERT INTO quality_policies (id, project_id, policy_id, semver, policy, policy_digest, row_version)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, 1)
			ON CONFLICT (project_id) DO NOTHING
			RETURNING row_version`,
			pgNewUUID(), projectID, policy.ID, policy.Version, string(encoded), digest).Scan(&rowVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrQualityPolicyConflict
		}
	case expectedRowVersion > 0:
		result, execErr := tx.ExecContext(ctx, `
			UPDATE quality_policies
			SET policy_id = $3, semver = $4, policy = $5::jsonb, policy_digest = $6,
			    row_version = row_version + 1, updated_at = now()
			WHERE project_id = $1 AND row_version = $2`,
			projectID, expectedRowVersion, policy.ID, policy.Version, string(encoded), digest)
		if execErr != nil {
			return 0, fmt.Errorf("quality policy: replace: %w", execErr)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			// Absence and version drift must answer differently only when
			// the row is gone; both surface as the CAS conflict here.
			return 0, ErrQualityPolicyConflict
		}
		rowVersion = expectedRowVersion + 1
	default:
		return 0, fmt.Errorf("quality policy: expected row version must not be negative")
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("quality policy: commit: %w", err)
	}
	return rowVersion, nil
}

// GetProjectPolicy returns the current overlay (nil when none exists).
func (s pgQualityStore) GetProjectPolicy(ctx context.Context, projectID string) (*evidence.Policy, int64, error) {
	var raw []byte
	var rowVersion int64
	err := s.db.QueryRowContext(ctx, `
		SELECT policy, row_version FROM quality_policies WHERE project_id = $1`,
		projectID).Scan(&raw, &rowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("quality policy: get: %w", err)
	}
	policy, err := evidence.ParsePolicy(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("quality policy: stored document is invalid: %w", err)
	}
	return policy, rowVersion, nil
}

// AppendEvidence stores one immutable record. Duplicates by identity
// collapse (at-least-once ingestion), any other failure rejects.
func (s pgQualityStore) AppendEvidence(ctx context.Context, record *evidence.Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	digest := record.Digest
	if digest == "" {
		encoded, encodeErr := json.Marshal(record)
		if encodeErr != nil {
			return fmt.Errorf("evidence append: encode: %w", encodeErr)
		}
		digest = contentDigest(encoded)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO evidence (id, project_id, work_item_id, authority, producer, evidence_kind,
			source_sha, target_sha, pipeline_id, job_id, payload_digest, policy_version, supersedes_id, attempt, status, sensitivity,
			gitlab_pipeline_id, gitlab_job_id)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid, $11, $12, NULLIF($13, '')::uuid, $14, $15, $16, $17, $18)
		ON CONFLICT (id) DO NOTHING`,
		record.EvidenceID, record.ProjectID, record.WorkItemID, record.Authority,
		producerJSON(record), record.Kind, record.SourceSHA, record.TargetSHA,
		pipelineFK(record), jobFK(record), digest, record.PolicyVersion, record.Supersedes,
		record.Attempt, record.Status, sensitivityOrDefault(record),
		nullableInt64(record.PipelineID), nullableInt64(record.JobID))
	if err != nil {
		return fmt.Errorf("evidence append: %w", err)
	}
	return nil
}

// ListEvidenceForWorkItem returns every immutable record bound to the
// work item; the engine filters by exact SHA tuple.
func (s pgQualityStore) ListEvidenceForWorkItem(ctx context.Context, projectID, workItemID string) ([]evidence.Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, work_item_id, evidence_kind, authority, source_sha, target_sha,
			policy_version, producer, attempt, supersedes_id, status, payload_digest, sensitivity,
			to_char(observed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			gitlab_pipeline_id, gitlab_job_id
		FROM evidence WHERE project_id = $1 AND work_item_id = $2
		ORDER BY observed_at, id`, projectID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("evidence list: %w", err)
	}
	defer rows.Close()

	records := []evidence.Record{}
	for rows.Next() {
		var record evidence.Record
		var producer, supersedes sql.NullString
		var pipelineID, jobID sql.NullInt64
		if err := rows.Scan(&record.EvidenceID, &record.ProjectID, &record.WorkItemID,
			&record.Kind, &record.Authority, &record.SourceSHA, &record.TargetSHA,
			&record.PolicyVersion, &producer, &record.Attempt, &supersedes, &record.Status,
			&record.Digest, &record.Sensitivity,
			&record.ObservedAt, &record.CreatedAt, &pipelineID, &jobID); err != nil {
			return nil, fmt.Errorf("evidence list: scan: %w", err)
		}
		if pipelineID.Valid {
			value := pipelineID.Int64
			record.PipelineID = &value
		}
		if jobID.Valid {
			value := jobID.Int64
			record.JobID = &value
		}
		if err := json.Unmarshal([]byte(producer.String), &record.Producer); err != nil {
			return nil, fmt.Errorf("evidence list: producer decode: %w", err)
		}
		if supersedes.Valid {
			record.Supersedes = supersedes.String
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// PersistVerdict writes the evaluated snapshots and marks every drifted
// snapshot stale in one transaction (QG-RULE-003).
func (s pgQualityStore) PersistVerdict(ctx context.Context, verdict *evidence.Verdict) error {
	if verdict == nil {
		return errors.New("verdict: nil")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("verdict: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Drift-driven staling: any stored snapshot for this work item whose
	// SHA tuple or policy version no longer matches goes stale first.
	if _, err := tx.ExecContext(ctx, `
		UPDATE gate_snapshots SET status = 'stale'
		WHERE project_id = $1 AND work_item_id = $2
		  AND status <> 'stale'
		  AND (source_sha, target_sha, policy_version) <> ($3, $4, $5)`,
		verdict.Tuple.ProjectID, verdict.Tuple.WorkItemID,
		verdict.Tuple.SourceSHA, verdict.Tuple.TargetSHA, verdict.Tuple.PolicyVersion); err != nil {
		return fmt.Errorf("verdict: stale: %w", err)
	}

	for _, gate := range verdict.Gates {
		ids, err := json.Marshal(gate.EvidenceIDs)
		if err != nil {
			return fmt.Errorf("verdict: evidence ids: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gate_snapshots (id, project_id, work_item_id, gate_id, status, source_sha, target_sha, policy_version, evidence_ids)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
			ON CONFLICT (project_id, work_item_id, gate_id, source_sha, target_sha, policy_version)
			DO UPDATE SET status = EXCLUDED.status, evidence_ids = EXCLUDED.evidence_ids,
				evaluated_at = now(), version = gate_snapshots.version + 1`,
			gate.GateID, verdict.Tuple.ProjectID, verdict.Tuple.WorkItemID, gate.Check,
			gate.State, verdict.Tuple.SourceSHA, verdict.Tuple.TargetSHA,
			verdict.Tuple.PolicyVersion, string(ids)); err != nil {
			return fmt.Errorf("verdict: upsert gate %s: %w", gate.Check, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("verdict: commit: %w", err)
	}
	return nil
}

// ListGateSnapshots returns the stored snapshots for a work item.
func (s pgQualityStore) ListGateSnapshots(ctx context.Context, projectID, workItemID string) ([]evidence.StoredSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, work_item_id, source_sha, target_sha, policy_version, status, version, gate_id FROM gate_snapshots
		WHERE project_id = $1 AND work_item_id = $2`, projectID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("gate snapshots: list: %w", err)
	}
	defer rows.Close()
	snapshots := []evidence.StoredSnapshot{}
	for rows.Next() {
		var snapshot evidence.StoredSnapshot
		if err := rows.Scan(&snapshot.GateID, &snapshot.WorkItemID, &snapshot.SourceSHA, &snapshot.TargetSHA,
			&snapshot.PolicyVersion, &snapshot.Status, &snapshot.Version, &snapshot.Check); err != nil {
			return nil, fmt.Errorf("gate snapshots: scan: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, rows.Err()
}

// CreateWaiver persists a validated waiver request row and returns the
// minted waiver identity.
func (s pgQualityStore) CreateWaiver(ctx context.Context, waiver *evidence.Waiver, projectID, workItemID string) (string, error) {
	id := pgNewUUID()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO waivers (id, project_id, work_item_id, gate_id, source_sha, state,
			requester_principal, reason, expires_at, merge_request_iid)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (project_id, work_item_id, gate_id, source_sha) DO NOTHING`,
		id, projectID, workItemID, waiver.GateID, waiver.SourceSHA, waiver.State,
		waiver.Requester, waiver.Reason, waiver.ExpiresAt, waiver.MergeRequestIID)
	if err != nil {
		return "", fmt.Errorf("waiver: create: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return "", ErrWaiverConflict
	}
	waiver.ID = id
	return id, nil
}

// ApproveWaiver applies the distinct-approver transition under a state
// guard; the engine-level requester split is re-checked in SQL.
func (s pgQualityStore) ApproveWaiver(ctx context.Context, waiverID, approverID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE waivers SET state = 'approved', approver_principal = $2, approved_at = now(),
			updated_at = now(), version = version + 1
		WHERE id = $1 AND state = 'requested' AND requester_principal <> $2`,
		waiverID, approverID)
	if err != nil {
		return fmt.Errorf("waiver: approve: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var requester string
		lookupErr := s.db.QueryRowContext(ctx,
			`SELECT requester_principal FROM waivers WHERE id = $1`, waiverID).Scan(&requester)
		if lookupErr == nil && requester == approverID {
			return ErrWaiverSelfApprove
		}
		return s.waiverMismatch(ctx, waiverID)
	}
	return nil
}

// RevokeWaiver cancels a not-yet-terminal waiver.
func (s pgQualityStore) RevokeWaiver(ctx context.Context, waiverID string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE waivers SET state = 'revoked', revoked_at = now(), updated_at = now(), version = version + 1
		WHERE id = $1 AND state IN ('requested', 'approved', 'active')`, waiverID)
	if err != nil {
		return fmt.Errorf("waiver: revoke: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return s.waiverMismatch(ctx, waiverID)
	}
	return nil
}

// ListWaiversForWorkItem returns waiver rows with their lifecycle state.
func (s pgQualityStore) ListWaiversForWorkItem(ctx context.Context, projectID, workItemID string) ([]evidence.Waiver, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, gate_id, source_sha, state,
			COALESCE(requester_principal, ''), COALESCE(approver_principal, ''), reason, expires_at,
			merge_request_iid, version
		FROM waivers WHERE project_id = $1 AND work_item_id = $2`, projectID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("waiver: list: %w", err)
	}
	defer rows.Close()
	waivers := []evidence.Waiver{}
	for rows.Next() {
		var waiver evidence.Waiver
		if err := rows.Scan(&waiver.ID, &waiver.GateID, &waiver.SourceSHA, &waiver.State,
			&waiver.Requester, &waiver.Approver, &waiver.Reason, &waiver.ExpiresAt,
			&waiver.MergeRequestIID, &waiver.Version); err != nil {
			return nil, fmt.Errorf("waiver: scan: %w", err)
		}
		waivers = append(waivers, waiver)
	}
	return waivers, rows.Err()
}

// GateSnapshot resolves one snapshot row by its deterministic identity;
// unknown gates answer absent so the transport hides them (404).
func (s pgQualityStore) GateSnapshotByID(ctx context.Context, projectID, gateID string) (*evidence.StoredSnapshot, bool, error) {
	var snapshot evidence.StoredSnapshot
	err := s.db.QueryRowContext(ctx, `
		SELECT id, work_item_id, source_sha, target_sha, policy_version, status, version, gate_id
		FROM gate_snapshots WHERE project_id = $1 AND id = $2`, projectID, gateID).
		Scan(&snapshot.GateID, &snapshot.WorkItemID, &snapshot.SourceSHA, &snapshot.TargetSHA,
			&snapshot.PolicyVersion, &snapshot.Status, &snapshot.Version, &snapshot.Check)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("gate snapshot: by id: %w", err)
	}
	return &snapshot, true, nil
}

// WaiverByID resolves one waiver row within a project scope.
func (s pgQualityStore) WaiverByID(ctx context.Context, projectID, waiverID string) (*evidence.Waiver, bool, error) {
	var waiver evidence.Waiver
	err := s.db.QueryRowContext(ctx, `
		SELECT id, gate_id, source_sha, state,
			COALESCE(requester_principal, ''), COALESCE(approver_principal, ''), reason, expires_at,
			merge_request_iid, version
		FROM waivers WHERE project_id = $1 AND id = $2`, projectID, waiverID).
		Scan(&waiver.ID, &waiver.GateID, &waiver.SourceSHA, &waiver.State,
			&waiver.Requester, &waiver.Approver, &waiver.Reason, &waiver.ExpiresAt,
			&waiver.MergeRequestIID, &waiver.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("waiver: by id: %w", err)
	}
	return &waiver, true, nil
}

// WorkItemExists scopes work-item-addressed reads to the project.
func (s pgQualityStore) WorkItemExists(ctx context.Context, projectID, workItemID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM work_items WHERE project_id = $1 AND id = $2`, projectID, workItemID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("work item exists: %w", err)
	}
	return true, nil
}

func (s pgQualityStore) waiverMismatch(ctx context.Context, waiverID string) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM waivers WHERE id = $1`, waiverID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWaiverAbsent
	}
	if err != nil {
		return fmt.Errorf("waiver: verify: %w", err)
	}
	return ErrWaiverConflict
}

// producerJSON renders the producer object for the jsonb column.
func producerJSON(record *evidence.Record) string {
	raw, err := json.Marshal(record.Producer)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// contentDigest digests the canonical record encoding; used when the
// producer did not carry a content digest of its own.
func contentDigest(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// nullableInt64 maps an absent optional integer to SQL NULL.
func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// sensitivityOrDefault keeps the wire-required sensitivity field honest:
// producers that classify their reports set it; unclassified rows are
// confidential by default, never public.
func sensitivityOrDefault(record *evidence.Record) string {
	if record.Sensitivity == "" {
		return "confidential"
	}
	return record.Sensitivity
}

// pipelineFK/jobFK resolve the optional pipeline/job foreign keys from
// the numeric GitLab identifiers the record carries.
func pipelineFK(record *evidence.Record) string {
	// The evidence table links pipelines by uuid; the projection rows
	// arrive with the S4a connector. Until then merge_gate evidence
	// records the numeric ids inside the producer payload and the FKs
	// stay NULL (README deviation note: pipeline FK wiring lands with
	// the connector slice).
	_ = record.PipelineID
	return ""
}

func jobFK(record *evidence.Record) string {
	_ = record.JobID
	return ""
}

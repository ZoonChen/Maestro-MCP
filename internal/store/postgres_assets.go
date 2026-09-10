package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL persistence for the migration 0017 asset ledger (J2a;
// ADR-009 review record). Lifecycle writes commit the state change, the
// asset.* audit row and the outbox event in ONE transaction; approving a
// superseding version cascades the supersede + downstream stale marking
// in that same transaction (WGM-INV-009/013/014/015).

var ErrAssetAlreadyRegistered = errors.New("asset version already registered")

type pgAssetsStore struct{ db *sql.DB }

// Assets returns the asset ledger store bound to the pool.
func (s *PostgresStore) Assets() pgAssetsStore { return pgAssetsStore{db: s.db} }

const assetColumns = `asset_id, version, project_id::text, asset_type, title, status, owner_principal,
	reviewers, sensitivity, supersedes_ref, source_digest, locked_gate, content_ref, summary,
	created_at, reviewed_at, approved_at, superseded_at`

func scanAsset(scan func(dest ...any) error) (*Asset, error) {
	asset := &Asset{}
	var reviewers, summary []byte
	var supersedesRef, lockedGate, contentRef sql.NullString
	var createdAt time.Time
	var reviewedAt, approvedAt, supersededAt sql.NullTime
	if err := scan(&asset.AssetID, &asset.Version, &asset.ProjectID, &asset.AssetType,
		&asset.Title, &asset.Status, &asset.OwnerPrincipal, &reviewers, &asset.Sensitivity,
		&supersedesRef, &asset.SourceDigest, &lockedGate, &contentRef, &summary,
		&createdAt, &reviewedAt, &approvedAt, &supersededAt); err != nil {
		return nil, err
	}
	asset.Reviewers = decodeStringList(reviewers)
	asset.Summary = summary
	if supersedesRef.Valid {
		asset.SupersedesRef = supersedesRef.String
	}
	if lockedGate.Valid {
		asset.LockedGate = lockedGate.String
	}
	if contentRef.Valid {
		asset.ContentRef = contentRef.String
	}
	asset.CreatedAt = pgTimeString(createdAt)
	asset.ReviewedAt = optionalTimeString(reviewedAt)
	asset.ApprovedAt = optionalTimeString(approvedAt)
	asset.SupersededAt = optionalTimeString(supersededAt)
	return asset, nil
}

func decodeStringList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	_ = json.Unmarshal(raw, &list)
	return list
}

func optionalTimeString(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return pgTimeString(value.Time)
}

// RegisterAsset validates the machine rules and inserts one draft
// version row with the asset.registered audit + outbox pair. The
// supersedes target is verified to exist and be un-replaced.
func (s pgAssetsStore) RegisterAsset(ctx context.Context, asset Asset, actor string) (*Asset, error) {
	if actor == "" {
		return nil, fmt.Errorf("%w: actor principal is required", ErrInvalidParameter)
	}
	if err := ValidateAssetRegistration(asset); err != nil {
		return nil, err
	}
	reviewers, err := json.Marshal(asset.Reviewers)
	if err != nil {
		return nil, fmt.Errorf("assets: reviewers encode: %w", err)
	}
	summary, err := CanonicalJSON(asset.Summary)
	if err != nil {
		return nil, fmt.Errorf("assets: summary encode: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if asset.SupersedesRef != "" {
		targetID, targetVersion, parseErr := ParseSupersedesRef(asset.SupersedesRef)
		if parseErr != nil {
			return nil, parseErr
		}
		var targetStatus string
		err := tx.QueryRowContext(ctx, `
			SELECT status FROM assets WHERE asset_id = $1 AND version = $2 FOR UPDATE`,
			targetID, targetVersion).Scan(&targetStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: supersedes target %s not found",
				ErrAssetSupersedesInvalid, asset.SupersedesRef)
		}
		if err != nil {
			return nil, fmt.Errorf("assets: lock supersedes target: %w", err)
		}
		if targetStatus != AssetStatusApproved {
			return nil, fmt.Errorf("%w: supersedes target %s is %s, only approved versions can be replaced",
				ErrAssetSupersedesInvalid, asset.SupersedesRef, targetStatus)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assets (asset_id, version, project_id, asset_type, title, status,
			owner_principal, reviewers, sensitivity, supersedes_ref, source_digest,
			locked_gate, content_ref, summary)
		VALUES ($1, $2, $3, $4, $5, 'draft', $6, $7::jsonb, $8, $9, $10, $11, $12, $13::jsonb)`,
		asset.AssetID, asset.Version, asset.ProjectID, asset.AssetType, asset.Title,
		asset.OwnerPrincipal, string(reviewers), asset.Sensitivity, optionalText(asset.SupersedesRef),
		asset.SourceDigest, optionalText(asset.LockedGate), optionalText(asset.ContentRef),
		string(summary)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, fmt.Errorf("%w: %s", ErrAssetAlreadyRegistered, FormatAssetRef(asset.AssetID, asset.Version))
		}
		return nil, fmt.Errorf("assets: register: %w", err)
	}

	payload := map[string]any{"asset": FormatAssetRef(asset.AssetID, asset.Version),
		"type": asset.AssetType, "sensitivity": asset.Sensitivity}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: asset.ProjectID, Action: "asset.registered", ResourceType: "asset",
		ResourceID: FormatAssetRef(asset.AssetID, asset.Version), Actor: actor,
		Reason: "ledger registration", OutboxType: "asset.registered", Payload: mustMarshal(payload),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}
	return s.GetAsset(ctx, asset.AssetID, asset.Version)
}

// ReviewAsset moves draft -> reviewed (idempotent replay returns the
// stored row without a second audit).
func (s pgAssetsStore) ReviewAsset(ctx context.Context, assetID string, version int, actor string) (*Asset, error) {
	return s.transitionAsset(ctx, assetID, version, AssetStatusReviewed, actor, "asset.reviewed")
}

// ApproveAsset moves reviewed -> approved. When this version supersedes
// another one, the target flips to superseded and every gate binding
// pinned to the target version goes stale in the SAME transaction —
// downstream work items then fail the locked-gate consumption check
// (WGM-INV-009/015).
func (s pgAssetsStore) ApproveAsset(ctx context.Context, assetID string, version int, actor string) (*Asset, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	approved, err := transitionAssetTx(ctx, tx, assetID, version, AssetStatusApproved, actor, "asset.approved")
	if err != nil {
		return nil, err
	}

	if approved.SupersedesRef != "" {
		targetID, targetVersion, parseErr := ParseSupersedesRef(approved.SupersedesRef)
		if parseErr != nil {
			return nil, parseErr
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE assets SET status = 'superseded', superseded_at = now()
			WHERE asset_id = $1 AND version = $2 AND status = 'approved'`,
			targetID, targetVersion)
		if err != nil {
			return nil, fmt.Errorf("assets: supersede target: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			staleResult, err := tx.ExecContext(ctx, `
				UPDATE asset_gate_bindings SET status = 'stale', staled_at = now()
				WHERE asset_id = $1 AND bound_version = $2 AND status = 'bound'`,
				targetID, targetVersion)
			if err != nil {
				return nil, fmt.Errorf("assets: stale bindings: %w", err)
			}
			staled, _ := staleResult.RowsAffected()
			payload := map[string]any{"asset": approved.SupersedesRef,
				"successor": FormatAssetRef(assetID, version), "stale_bindings": staled}
			if err := recordGovernanceEvent(ctx, tx, governanceEvent{
				ProjectID: approved.ProjectID, Action: "asset.superseded", ResourceType: "asset",
				ResourceID: approved.SupersedesRef, Actor: actor,
				Reason:     "superseded by " + FormatAssetRef(assetID, version),
				OutboxType: "asset.superseded", Payload: mustMarshal(payload),
			}); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}
	return s.GetAsset(ctx, assetID, version)
}

// transitionAsset performs one guarded lifecycle step outside a wider
// transaction (used by ReviewAsset).
func (s pgAssetsStore) transitionAsset(ctx context.Context, assetID string, version int, toStatus string, actor string, action string) (*Asset, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	asset, err := transitionAssetTx(ctx, tx, assetID, version, toStatus, actor, action)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}
	return asset, nil
}

// assetTransitionSQL carries one complete statement per lifecycle step
// (kept whole so no SQL is ever assembled by concatenation).
var assetTransitionSQL = map[string]string{
	AssetStatusReviewed:   `UPDATE assets SET status = $3, reviewed_at = now() WHERE asset_id = $1 AND version = $2`,
	AssetStatusApproved:   `UPDATE assets SET status = $3, approved_at = now() WHERE asset_id = $1 AND version = $2`,
	AssetStatusSuperseded: `UPDATE assets SET status = $3, superseded_at = now() WHERE asset_id = $1 AND version = $2`,
}

// transitionAssetTx is the guarded draft->reviewed / reviewed->approved
// UPDATE plus its audit + outbox pair on the caller's transaction. A
// replayed transition is an idempotent no-op.
func transitionAssetTx(ctx context.Context, tx *sql.Tx, assetID string, version int, toStatus string, actor string, action string) (*Asset, error) {
	var projectID, fromStatus string
	err := tx.QueryRowContext(ctx, `
		SELECT project_id::text, status FROM assets WHERE asset_id = $1 AND version = $2 FOR UPDATE`,
		assetID, version).Scan(&projectID, &fromStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, FormatAssetRef(assetID, version))
	}
	if err != nil {
		return nil, fmt.Errorf("assets: lock: %w", err)
	}
	if fromStatus == toStatus {
		// Idempotent replay: return the stored row, no second audit.
		return readAssetTx(ctx, tx, assetID, version)
	}
	if !AssetTransitionAllowed(fromStatus, toStatus) {
		return nil, fmt.Errorf("%w: %s -> %s on %s", ErrAssetTransitionInvalid,
			fromStatus, toStatus, FormatAssetRef(assetID, version))
	}

	transitionSQL, ok := assetTransitionSQL[toStatus]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAssetTransitionInvalid, toStatus)
	}
	if _, err := tx.ExecContext(ctx, transitionSQL, assetID, version, toStatus); err != nil {
		return nil, fmt.Errorf("assets: transition to %s: %w", toStatus, err)
	}

	payload := map[string]any{"asset": FormatAssetRef(assetID, version), "from": fromStatus, "to": toStatus}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: action, ResourceType: "asset",
		ResourceID: FormatAssetRef(assetID, version), Actor: actor,
		Reason: fromStatus + " -> " + toStatus, OutboxType: action, Payload: mustMarshal(payload),
	}); err != nil {
		return nil, err
	}
	return readAssetTx(ctx, tx, assetID, version)
}

func readAssetTx(ctx context.Context, tx *sql.Tx, assetID string, version int) (*Asset, error) {
	asset, err := scanAsset(func(dest ...any) error {
		return tx.QueryRowContext(ctx,
			`SELECT `+assetColumns+` FROM assets WHERE asset_id = $1 AND version = $2`,
			assetID, version).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, FormatAssetRef(assetID, version))
	}
	if err != nil {
		return nil, fmt.Errorf("assets: read back: %w", err)
	}
	return asset, nil
}

func optionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// GetAsset returns one exact version row.
func (s pgAssetsStore) GetAsset(ctx context.Context, assetID string, version int) (*Asset, error) {
	asset, err := scanAsset(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+assetColumns+` FROM assets WHERE asset_id = $1 AND version = $2`,
			assetID, version).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, FormatAssetRef(assetID, version))
	}
	if err != nil {
		return nil, fmt.Errorf("assets: get: %w", err)
	}
	return asset, nil
}

// LatestAsset returns the newest registered version of an asset.
func (s pgAssetsStore) LatestAsset(ctx context.Context, assetID string) (*Asset, error) {
	asset, err := scanAsset(func(dest ...any) error {
		return s.db.QueryRowContext(ctx,
			`SELECT `+assetColumns+` FROM assets WHERE asset_id = $1 ORDER BY version DESC LIMIT 1`,
			assetID).Scan(dest...)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, assetID)
	}
	if err != nil {
		return nil, fmt.Errorf("assets: latest: %w", err)
	}
	return asset, nil
}

// ListAssets returns every version of every asset in a project,
// newest first per asset.
func (s pgAssetsStore) ListAssets(ctx context.Context, projectID string) ([]*Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE project_id = $1 ORDER BY asset_id, version DESC`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("assets: list: %w", err)
	}
	defer rows.Close()
	assets := []*Asset{}
	for rows.Next() {
		asset, scanErr := scanAsset(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("assets: scan: %w", scanErr)
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

// BindAssetGate pins a work item's gate to an approved ledger version;
// the digest recorded here is what the claim-time check compares
// against, so later drift fails closed (WGM-INV-015).
func (s pgAssetsStore) BindAssetGate(ctx context.Context, projectID, workItemID, assetID string, assetVersion int, gateID string, actor string) (*AssetGateBinding, error) {
	if gateID == "" || len([]rune(gateID)) > 128 {
		return nil, fmt.Errorf("%w: gate id must be 1-128 characters", ErrInvalidParameter)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("assets: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var digest, status string
	err = tx.QueryRowContext(ctx, `
		SELECT source_digest, status FROM assets WHERE asset_id = $1 AND version = $2`,
		assetID, assetVersion).Scan(&digest, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAssetNotFound, FormatAssetRef(assetID, assetVersion))
	}
	if err != nil {
		return nil, fmt.Errorf("assets: bind lookup: %w", err)
	}
	if status != AssetStatusApproved {
		return nil, fmt.Errorf("%w: %s is %s, gates may only bind approved versions",
			ErrAssetGateNotSatisfied, FormatAssetRef(assetID, assetVersion), status)
	}

	bindingID := uuid.Must(uuid.NewV7()).String()
	// A stale binding is the recovery path, not a dead end: re-binding
	// the same (item, asset, gate) to the approved successor version
	// upgrades the row in place (WGM-INV-015 recovery semantics). A live
	// duplicate binding is refused.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO asset_gate_bindings (id, project_id, work_item_id, asset_id, bound_version, bound_digest, gate_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (work_item_id, asset_id, gate_id) DO UPDATE
			SET bound_version = EXCLUDED.bound_version,
			    bound_digest = EXCLUDED.bound_digest,
			    status = 'bound',
			    staled_at = NULL,
			    bound_at = now()
			WHERE asset_gate_bindings.status = 'stale'`,
		bindingID, projectID, workItemID, assetID, assetVersion, digest, gateID)
	if err != nil {
		return nil, fmt.Errorf("assets: bind: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil, fmt.Errorf("%w: %s already bound to gate %s (live binding)", ErrInvalidParameter,
			FormatAssetRef(assetID, assetVersion), gateID)
	}
	payload := map[string]any{"asset": FormatAssetRef(assetID, assetVersion),
		"work_item": workItemID, "gate": gateID, "digest": digest}
	if err := recordGovernanceEvent(ctx, tx, governanceEvent{
		ProjectID: projectID, Action: "asset.gate.bound", ResourceType: "asset_gate_binding",
		ResourceID: bindingID, Actor: actor, Reason: "gate locked to " + FormatAssetRef(assetID, assetVersion),
		OutboxType: "asset.gate.bound", Payload: mustMarshal(payload),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("assets: commit: %w", err)
	}
	binding, err := s.GetAssetGateBinding(ctx, workItemID, assetID, gateID)
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// GetAssetGateBinding returns one binding row.
func (s pgAssetsStore) GetAssetGateBinding(ctx context.Context, workItemID, assetID, gateID string) (*AssetGateBinding, error) {
	binding := &AssetGateBinding{}
	var staledAt sql.NullTime
	var boundAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id::text, work_item_id, asset_id, bound_version, bound_digest, gate_id, status, bound_at, staled_at
		FROM asset_gate_bindings WHERE work_item_id = $1 AND asset_id = $2 AND gate_id = $3`,
		workItemID, assetID, gateID).
		Scan(&binding.ID, &binding.ProjectID, &binding.WorkItemID, &binding.AssetID,
			&binding.BoundVersion, &binding.BoundDigest, &binding.GateID, &binding.Status,
			&boundAt, &staledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAssetBindingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("assets: get binding: %w", err)
	}
	binding.BoundAt = pgTimeString(boundAt)
	binding.StaledAt = optionalTimeString(staledAt)
	return binding, nil
}

// CheckAssetGateConsumption is the claim-time fail-closed gate: every
// bound asset must still be approved with an unchanged digest and a
// live (non-stale) binding. The empty-binding case is a pass — items
// without gates are unconstrained.
func (s pgAssetsStore) CheckAssetGateConsumption(ctx context.Context, projectID, workItemID string) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.asset_id, b.bound_version, b.status, b.bound_digest, a.status, a.source_digest
		FROM asset_gate_bindings b
		LEFT JOIN assets a ON a.asset_id = b.asset_id AND a.version = b.bound_version
		WHERE b.work_item_id = $1 AND b.project_id = $2`,
		workItemID, projectID)
	if err != nil {
		return fmt.Errorf("assets: gate check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var assetID, bindingStatus, boundDigest string
		var version int
		var assetStatus, assetDigest *string
		if err := rows.Scan(&assetID, &version, &bindingStatus, &boundDigest, &assetStatus, &assetDigest); err != nil {
			return fmt.Errorf("assets: gate check scan: %w", err)
		}
		switch {
		case bindingStatus != "bound":
			return fmt.Errorf("%w: binding for %s@%d is %s", ErrAssetGateNotSatisfied, assetID, version, bindingStatus)
		case assetStatus == nil:
			return fmt.Errorf("%w: %s@%d missing from ledger", ErrAssetGateNotSatisfied, assetID, version)
		case *assetStatus != AssetStatusApproved:
			return fmt.Errorf("%w: %s@%d is %s", ErrAssetGateNotSatisfied, assetID, version, *assetStatus)
		case *assetDigest != boundDigest:
			return fmt.Errorf("%w: %s@%d digest drift", ErrAssetGateNotSatisfied, assetID, version)
		}
	}
	return rows.Err()
}

// ListStaleBindings returns the project's stale gate bindings (the
// operational surface for J2b alerting).
func (s pgAssetsStore) ListStaleBindings(ctx context.Context, projectID string) ([]*AssetGateBinding, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id::text, work_item_id, asset_id, bound_version, bound_digest, gate_id, status, bound_at, staled_at
		FROM asset_gate_bindings WHERE project_id = $1 AND status = 'stale' ORDER BY staled_at`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("assets: list stale: %w", err)
	}
	defer rows.Close()
	bindings := []*AssetGateBinding{}
	for rows.Next() {
		binding := &AssetGateBinding{}
		var staledAt sql.NullTime
		var boundAt time.Time
		if err := rows.Scan(&binding.ID, &binding.ProjectID, &binding.WorkItemID, &binding.AssetID,
			&binding.BoundVersion, &binding.BoundDigest, &binding.GateID, &binding.Status,
			&boundAt, &staledAt); err != nil {
			return nil, fmt.Errorf("assets: scan stale: %w", err)
		}
		binding.BoundAt = pgTimeString(boundAt)
		binding.StaledAt = optionalTimeString(staledAt)
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

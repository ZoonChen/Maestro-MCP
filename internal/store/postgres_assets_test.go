package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated asset ledger (J2a): machine-check rejections, the guarded
// lifecycle with atomic asset.* audits, single-successor supersede
// chains, trigger-level immutability and the claim-time gate
// integration (WGM-INV-013/014/015).

func TestAssetLedgerLifecycle(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	assets := tNewStore(t, db).Assets()
	projectID := tSeedProject(t, db, "astl", 0x40)
	digest := "sha256:" + repeatHex("ab", 32)

	// Machine-check rejection family (WGM-GATE-003).
	_, err := assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 1,
		ProjectID: projectID, AssetType: "outside-catalog", Title: "x",
		OwnerPrincipal: "p", Sensitivity: SensitivityInternal,
		SourceDigest: digest, ContentRef: "a"}, "actor")
	assert.ErrorIs(t, err, ErrAssetTypeInvalid)

	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 1,
		ProjectID: projectID, AssetType: "research", Title: "x",
		OwnerPrincipal: "p", Sensitivity: SensitivityInternal,
		SourceDigest: "md5:zz", ContentRef: "a"}, "actor")
	assert.ErrorIs(t, err, ErrAssetDigestInvalid)

	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "not-an-asset-id", Version: 1,
		ProjectID: projectID, AssetType: "research", Title: "x",
		OwnerPrincipal: "p", Sensitivity: SensitivityInternal,
		SourceDigest: digest, ContentRef: "a"}, "actor")
	assert.ErrorIs(t, err, ErrInvalidParameter)

	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 2,
		ProjectID: projectID, AssetType: "research", Title: "x",
		OwnerPrincipal: "p", Sensitivity: SensitivityInternal,
		SourceDigest: digest, SupersedesRef: "ART-blueprint-001@1", ContentRef: "a"}, "actor")
	assert.ErrorIs(t, err, ErrAssetSupersedesInvalid)

	// Happy path: register -> review -> approve with atomic audits.
	registered, err := assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 1,
		ProjectID: projectID, AssetType: "research", Title: "开源选型调研",
		OwnerPrincipal: "arch", Sensitivity: SensitivityInternal,
		SourceDigest: digest, ContentRef: "assets/ART-research-001/research.md",
		Summary: []byte(`[{"section":"选型结论"}]`)}, "arch")
	require.NoError(t, err)
	assert.Equal(t, AssetStatusDraft, registered.Status)

	var auditCount, outboxCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'asset.registered' AND resource_id = $1`,
		"ART-research-001@1").Scan(&auditCount))
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = 'asset.registered' AND subject = $1`,
		"ART-research-001@1").Scan(&outboxCount))
	assert.Equal(t, 1, auditCount)
	assert.Equal(t, 1, outboxCount)

	// Jumping the lifecycle is rejected (application guard; the trigger
	// is the backstop asserted below).
	_, err = assets.ApproveAsset(ctx, "ART-research-001", 1, "product", nil)
	assert.ErrorIs(t, err, ErrAssetTransitionInvalid)

	reviewed, err := assets.ReviewAsset(ctx, "ART-research-001", 1, "tech-lead")
	require.NoError(t, err)
	assert.Equal(t, AssetStatusReviewed, reviewed.Status)

	approved, err := assets.ApproveAsset(ctx, "ART-research-001", 1, "product", nil)
	require.NoError(t, err)
	assert.Equal(t, AssetStatusApproved, approved.Status)
	assert.NotEmpty(t, approved.ApprovedAt)

	// Supersede: v2 replaces v1 on approval; the chain allows exactly
	// one successor.
	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 2,
		ProjectID: projectID, AssetType: "research", Title: "开源选型调研（修订）",
		OwnerPrincipal: "arch", Sensitivity: SensitivityInternal,
		SourceDigest:  "sha256:" + repeatHex("cd", 32),
		SupersedesRef: "ART-research-001@1",
		ContentRef:    "assets/ART-research-001/research-v2.md"}, "arch")
	require.NoError(t, err)
	_, err = assets.ReviewAsset(ctx, "ART-research-001", 2, "tech-lead")
	require.NoError(t, err)
	_, err = assets.ApproveAsset(ctx, "ART-research-001", 2, "product", nil)
	require.NoError(t, err)

	v1After, err := assets.GetAsset(ctx, "ART-research-001", 1)
	require.NoError(t, err)
	assert.Equal(t, AssetStatusSuperseded, v1After.Status)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'asset.superseded' AND resource_id = $1`,
		"ART-research-001@1").Scan(&auditCount))
	assert.Equal(t, 1, auditCount)

	// A third version pointing at the already-replaced v1 is refused:
	// single-successor chains (WGM-INV-014).
	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 3,
		ProjectID: projectID, AssetType: "research", Title: "再次修订",
		OwnerPrincipal: "arch", Sensitivity: SensitivityInternal,
		SourceDigest:  "sha256:" + repeatHex("ef", 32),
		SupersedesRef: "ART-research-001@1",
		ContentRef:    "assets/ART-research-001/research-v3.md"}, "arch")
	assert.ErrorIs(t, err, ErrAssetSupersedesInvalid)

	// Duplicate registration of an existing (asset, version) is a clear
	// error, never an update.
	_, err = assets.RegisterAsset(ctx, Asset{AssetID: "ART-research-001", Version: 1,
		ProjectID: projectID, AssetType: "research", Title: "重复注册",
		OwnerPrincipal: "arch", Sensitivity: SensitivityInternal,
		SourceDigest: digest, ContentRef: "a"}, "arch")
	assert.ErrorIs(t, err, ErrAssetAlreadyRegistered)

	// Trigger backstop: per-version content is immutable, lifecycle may
	// only walk forward, rows are never deleted.
	_, err = db.ExecContext(ctx, `UPDATE assets SET title = 'tampered' WHERE asset_id = 'ART-research-001' AND version = 2`)
	require.Error(t, err, "content columns must be immutable per version")
	_, err = db.ExecContext(ctx, `INSERT INTO assets (asset_id, version, project_id, asset_type, title, status, owner_principal, sensitivity, source_digest)
		VALUES ('ART-research-009', 1, $1, 'research', 'jump', 'approved', 'p', 'internal', $2)`, projectID, digest)
	require.Error(t, err, "approved without reviewed_at must violate the lifecycle CHECKs")
	_, err = db.ExecContext(ctx, `DELETE FROM assets WHERE asset_id = 'ART-research-001' AND version = 2`)
	require.Error(t, err, "ledger rows are append-only")

	// Idempotent replay: re-approving the approved version audits
	// nothing new.
	_, err = assets.ApproveAsset(ctx, "ART-research-001", 2, "product", nil)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'asset.approved' AND resource_id = $1`,
		"ART-research-001@2").Scan(&auditCount))
	assert.Equal(t, 1, auditCount)
}

func TestAssetGateConsumptionAndStalePropagation(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	ctx := context.Background()
	db := testPostgresDB(t)
	resetWorkItemSchema(t, db)
	store := tNewStore(t, db)
	assets := store.Assets()

	seedWorkItemFixture(t, db, 7)
	seedWorkItemFixture(t, db, 8)
	approveRunner(t, db, 7)
	approveRunner(t, db, 8)
	project7 := fmtProjectUUID(7)
	project8 := fmtProjectUUID(8)

	digest := "sha256:" + repeatHex("77", 32)
	register := func(version int, supersedes string, dig string) {
		t.Helper()
		_, err := assets.RegisterAsset(ctx, Asset{AssetID: "ART-detailed-design-001", Version: version,
			ProjectID: project7, AssetType: "detailed-design", Title: "详设",
			OwnerPrincipal: "dev-lead", Sensitivity: SensitivityInternal,
			SourceDigest: dig, SupersedesRef: supersedes,
			ContentRef: "assets/ART-detailed-design-001/design.md"}, "dev-lead")
		require.NoError(t, err)
		_, err = assets.ReviewAsset(ctx, "ART-detailed-design-001", version, "tech-lead")
		require.NoError(t, err)
		_, err = assets.ApproveAsset(ctx, "ART-detailed-design-001", version, "product", nil)
		require.NoError(t, err)
	}
	register(1, "", digest)

	// Gates may only bind approved versions.
	binding, err := assets.BindAssetGate(ctx, project7, workItemUUID(7),
		"ART-detailed-design-001", 1, "design.approved", "scheduler")
	require.NoError(t, err)
	assert.Equal(t, "bound", binding.Status)
	assert.Equal(t, digest, binding.BoundDigest)

	// Claim-time check passes and the gated item dispatches.
	require.NoError(t, assets.CheckAssetGateConsumption(ctx, project7, workItemUUID(7)))
	claim, err := store.ClaimNextWorkItem(ctx, runnerUUID(7), "gen-7", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(7), claim.WorkItemID)

	// A second item binds the same version; superseding the asset makes
	// that binding stale and the item undispatchable (fail-closed skip).
	_, err = assets.BindAssetGate(ctx, project8, workItemUUID(8),
		"ART-detailed-design-001", 1, "design.approved", "scheduler")
	require.NoError(t, err)
	register(2, "ART-detailed-design-001@1", "sha256:"+repeatHex("88", 32))

	stale, err := assets.ListStaleBindings(ctx, project8)
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Equal(t, workItemUUID(8), stale[0].WorkItemID)

	err = assets.CheckAssetGateConsumption(ctx, project8, workItemUUID(8))
	assert.ErrorIs(t, err, ErrAssetGateNotSatisfied)

	_, err = store.ClaimNextWorkItem(ctx, runnerUUID(8), "gen-8", 0, 90*time.Second)
	assert.ErrorIs(t, err, ErrNoAvailableTask, "stale-gated items must not dispatch")

	// Recovery: re-binding the same gate to the approved successor
	// version upgrades the stale row and the item dispatches again.
	rebound, err := assets.BindAssetGate(ctx, project8, workItemUUID(8),
		"ART-detailed-design-001", 2, "design.approved", "scheduler")
	require.NoError(t, err)
	assert.Equal(t, "bound", rebound.Status)
	assert.Equal(t, 2, rebound.BoundVersion)
	require.NoError(t, assets.CheckAssetGateConsumption(ctx, project8, workItemUUID(8)))
	claim8, err := store.ClaimNextWorkItem(ctx, runnerUUID(8), "gen-8", 0, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workItemUUID(8), claim8.WorkItemID)

	// Live duplicate bindings are refused.
	_, err = assets.BindAssetGate(ctx, project8, workItemUUID(8),
		"ART-detailed-design-001", 2, "design.approved", "scheduler")
	assert.ErrorIs(t, err, ErrInvalidParameter)

	// Bindings may only pin approved versions.
	register(3, "ART-detailed-design-001@2", "sha256:"+repeatHex("99", 32))
	_, err = assets.BindAssetGate(ctx, project7, workItemUUID(7),
		"ART-detailed-design-001", 1, "design.approved", "scheduler")
	assert.ErrorIs(t, err, ErrAssetGateNotSatisfied, "superseded versions are not bindable")
}

func fmtProjectUUID(index int) string {
	return fmt.Sprintf("018f4100-0000-7000-8000-%012d", index)
}

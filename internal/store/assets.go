package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Asset ledger types and pure machine checks (J2a; authority: ADR-009
// review record promoting the ARTIFACT-STANDARDS §1–§5 ledger semantics).
// The database layer carries the same rules as CHECKs, a partial unique
// index and triggers; these helpers give callers stable errors before
// PostgreSQL rejects the write.

var (
	ErrAssetNotFound          = errors.New("asset version not found")
	ErrAssetTypeInvalid       = errors.New("asset type outside the catalog")
	ErrAssetDigestInvalid     = errors.New("asset source digest must be sha256:<64hex>")
	ErrAssetSupersedesInvalid = errors.New("supersedes reference invalid")
	ErrAssetTransitionInvalid = errors.New("asset status transition invalid")
	ErrAssetGateNotSatisfied  = errors.New("locked gate asset not consumable (not approved, digest drift or stale binding)")
	ErrAssetBindingNotFound   = errors.New("asset gate binding not found")
	ErrAssetContentForbidden  = errors.New("confidential assets are pointer-only: content may not be registered")
)

// AssetTypeCatalog is the frozen 15-entry pilot catalog (0017 assets
// CHECK mirrors this list; growing it requires a new migration by
// design).
var AssetTypeCatalog = []string{
	"blueprint", "prd", "research", "bom", "hld", "detailed-design",
	"test-plan", "test-report", "sec-review", "ops-runbook", "deploy-plan",
	"release-note", "incident", "retrospective", "legacy-intake",
}

// Asset lifecycle statuses (0017 assets.status).
const (
	AssetStatusDraft      = "draft"
	AssetStatusReviewed   = "reviewed"
	AssetStatusApproved   = "approved"
	AssetStatusSuperseded = "superseded"
)

// Asset sensitivity levels (0017 assets.sensitivity).
const (
	SensitivityPublic       = "public"
	SensitivityInternal     = "internal"
	SensitivityConfidential = "confidential"
)

// Asset is one stored assets row (one version of one asset).
type Asset struct {
	AssetID        string
	Version        int
	ProjectID      string
	AssetType      string
	Title          string
	Status         string
	OwnerPrincipal string
	Reviewers      []string
	Sensitivity    string
	SupersedesRef  string
	SourceDigest   string
	LockedGate     string
	ContentRef     string
	Summary        []byte
	CreatedAt      string
	ReviewedAt     string
	ApprovedAt     string
	SupersededAt   string
}

// AssetGateBinding is one stored asset_gate_bindings row: the pilot
// bridge from a flat work item to the ledger version its gate locked.
type AssetGateBinding struct {
	ID           string
	ProjectID    string
	WorkItemID   string
	AssetID      string
	BoundVersion int
	BoundDigest  string
	GateID       string
	Status       string
	BoundAt      string
	StaledAt     string
}

// ContentDigest streams raw file bytes through sha256 and returns the
// ledger wire digest plus the byte count. Unlike SpecDigest this pins
// the exact content, not a canonical form — the intake path computes it
// so the ledger never trusts a self-reported digest.
func ContentDigest(reader io.Reader) (string, int64, error) {
	hasher := sha256.New()
	size, err := io.Copy(hasher, reader)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), size, nil
}

// AssetTypeKnown reports whether the type is inside the frozen catalog.
func AssetTypeKnown(assetType string) bool {
	for _, known := range AssetTypeCatalog {
		if known == assetType {
			return true
		}
	}
	return false
}

// ParseSupersedesRef splits "<asset-id>@<version>".
func ParseSupersedesRef(ref string) (assetID string, version int, err error) {
	match := supersedesPattern.FindStringSubmatch(ref)
	if match == nil {
		return "", 0, fmt.Errorf("%w: %q", ErrAssetSupersedesInvalid, ref)
	}
	version, parseErr := strconv.Atoi(match[2])
	if parseErr != nil || version < 1 {
		return "", 0, fmt.Errorf("%w: %q", ErrAssetSupersedesInvalid, ref)
	}
	return match[1], version, nil
}

// ValidateAssetRegistration applies the §1 machine rules the store can
// check without touching the database (formats, catalog, title, version,
// same-asset supersede chain shape).
func ValidateAssetRegistration(asset Asset) error {
	if !assetIDPattern.MatchString(asset.AssetID) {
		return fmt.Errorf("%w: asset_id must match ^ART-[a-z-]+-[0-9]{3,}$: %q",
			ErrInvalidParameter, asset.AssetID)
	}
	if asset.Version < 1 {
		return fmt.Errorf("%w: version must be >= 1: %d", ErrInvalidParameter, asset.Version)
	}
	if !AssetTypeKnown(asset.AssetType) {
		return fmt.Errorf("%w: %q", ErrAssetTypeInvalid, asset.AssetType)
	}
	if l := len([]rune(asset.Title)); l < 1 || l > 200 {
		return fmt.Errorf("%w: title must be 1-200 characters", ErrInvalidParameter)
	}
	switch asset.Sensitivity {
	case SensitivityPublic, SensitivityInternal, SensitivityConfidential:
	default:
		return fmt.Errorf("%w: sensitivity must be public/internal/confidential: %q",
			ErrInvalidParameter, asset.Sensitivity)
	}
	if err := ValidateSpecDigestFormat(asset.SourceDigest); err != nil {
		return fmt.Errorf("%w: %w", ErrAssetDigestInvalid, err)
	}
	if asset.SupersedesRef != "" {
		targetID, targetVersion, err := ParseSupersedesRef(asset.SupersedesRef)
		if err != nil {
			return err
		}
		if targetID != asset.AssetID {
			return fmt.Errorf("%w: supersedes must reference the same asset_id: %q vs %q",
				ErrAssetSupersedesInvalid, targetID, asset.AssetID)
		}
		if targetVersion >= asset.Version {
			return fmt.Errorf("%w: supersedes must reference a lower version: @%d -> v%d",
				ErrAssetSupersedesInvalid, targetVersion, asset.Version)
		}
	}
	// Confidential registrations are pointer-only by construction (ADR-009
	// review record, realistic decision 1): no content copy may exist.
	if asset.Sensitivity == SensitivityConfidential && asset.ContentRef == "" {
		return fmt.Errorf("%w: confidential intake needs a file pointer", ErrAssetContentForbidden)
	}
	return nil
}

// AssetTransitionAllowed enforces draft→reviewed→approved→superseded
// with idempotent self-loops (WGM-INV-013; the 0017 trigger is the
// backstop).
func AssetTransitionAllowed(from, to string) bool {
	switch from {
	case AssetStatusDraft:
		return to == AssetStatusDraft || to == AssetStatusReviewed
	case AssetStatusReviewed:
		return to == AssetStatusReviewed || to == AssetStatusApproved
	case AssetStatusApproved:
		return to == AssetStatusApproved || to == AssetStatusSuperseded
	case AssetStatusSuperseded:
		return to == AssetStatusSuperseded
	default:
		return false
	}
}

// FormatAssetRef renders the "<asset_id>@<version>" ledger reference.
func FormatAssetRef(assetID string, version int) string {
	return assetID + "@" + strconv.Itoa(version)
}

// SplitAssetRef parses "<asset_id>@<version>" without the same-asset
// constraint (used for display and lookups).
func SplitAssetRef(ref string) (string, int, error) {
	assetID, version, err := ParseSupersedesRef(ref)
	if err != nil {
		return "", 0, err
	}
	return assetID, version, nil
}

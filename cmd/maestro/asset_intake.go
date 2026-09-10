package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// runAssetIntake registers legacy artifacts into the governed asset
// ledger (J2a / ARTIFACT-STANDARDS §5). Three intake modes:
//
//	file    — copy the content file into the pilot repo assets dir and
//	          register it (md originals; public/internal only);
//	digest  — register the original by digest + pointer, content is NOT
//	          copied (BOM xlsx: 原件 digest 入账, 不改原件);
//	pointer — digest + file pointer only, for confidential session
//	          records whose bodies must never enter the ledger or the
//	          assets dir.
//
// The digest is computed here (by the intake tooling, never accepted
// from the operator as a claim), the store re-validates every machine
// rule, and registration commits with its asset.registered audit row.
func runAssetIntake(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("asset-intake", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var projectID, assetID, assetType, title, owner, reviewersCSV, sensitivity string
	var mode, path, assetsDir, gate, summaryFile, note, supersedes string
	var version int
	var asJSON bool
	fs.StringVar(&projectID, "project", "", "owning project uuid")
	fs.StringVar(&assetID, "asset-id", "", "asset id ART-<type>-<seq> (global, immutable once published)")
	fs.StringVar(&assetType, "type", "", "asset type from the 15-entry catalog (e.g. research, bom, legacy-intake)")
	fs.StringVar(&title, "title", "", "asset title (1-200 characters)")
	fs.StringVar(&owner, "owner", "", "owning principal recorded in the ledger")
	fs.StringVar(&reviewersCSV, "reviewers", "", "comma-separated reviewer principals")
	fs.StringVar(&sensitivity, "sensitivity", "internal", "public | internal | confidential")
	fs.StringVar(&mode, "mode", "file", "file | digest | pointer")
	fs.StringVar(&path, "path", "", "path to the artifact file (hashed in every mode)")
	fs.StringVar(&assetsDir, "assets-dir", "assets", "pilot-repo assets directory for mode=file copies")
	fs.StringVar(&gate, "gate", "", "locked gate id this artifact is a precondition of (optional)")
	fs.StringVar(&summaryFile, "summary-file", "", "JSON file with summary rows (e.g. per-sheet BOM summaries)")
	fs.StringVar(&note, "note", "", "one-line summary row (alternative to --summary-file)")
	fs.StringVar(&supersedes, "supersedes", "", "asset@version this registration replaces (optional)")
	fs.IntVar(&version, "version", 0, "version number (default: latest+1, or supersedes version+1)")
	fs.BoolVar(&asJSON, "json", false, "write a machine-readable intake summary")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected asset-intake arguments: %s", strings.Join(fs.Args(), " ")))
	}

	switch mode {
	case "file", "digest", "pointer":
	default:
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("--mode must be file/digest/pointer: %q", mode))
	}
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(assetID) == "" ||
		strings.TrimSpace(assetType) == "" || strings.TrimSpace(title) == "" ||
		strings.TrimSpace(owner) == "" || strings.TrimSpace(path) == "" {
		return fail(exitUsage, "USAGE_ERROR", errors.New("--project/--asset-id/--type/--title/--owner/--path are required"))
	}
	if sensitivity != store.SensitivityPublic && sensitivity != store.SensitivityInternal &&
		sensitivity != store.SensitivityConfidential {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("--sensitivity must be public/internal/confidential: %q", sensitivity))
	}
	// Confidential artifacts are pointer-only by construction (ADR-009
	// review record, realistic decision 1): their bodies may not be
	// copied anywhere by the intake path.
	if sensitivity == store.SensitivityConfidential && mode == "file" {
		return fail(exitUsage, "USAGE_ERROR",
			errors.New("confidential artifacts are pointer-only: use --mode pointer (or digest)"))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	if !cfg.PostgresEnabled() {
		return fail(exitUsage, "CONFIG_INVALID", errors.New("asset-intake requires the postgres database configuration"))
	}

	digest, fileSize, err := hashFile(path)
	if err != nil {
		return fail(exitUsage, "USAGE_ERROR", err)
	}

	var contentRef string
	if mode == "file" {
		copyPath, copyErr := copyIntoAssetsDir(assetsDir, assetID, path)
		if copyErr != nil {
			return fail(exitInternal, "INTAKE_FAILED", copyErr)
		}
		contentRef = copyPath
	} else {
		// digest/pointer: the ledger points at the original's location;
		// nothing is copied.
		contentRef = path
	}

	summary, err := loadSummary(summaryFile, note)
	if err != nil {
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	reviewers := []string{}
	for _, reviewer := range strings.Split(reviewersCSV, ",") {
		if trimmed := strings.TrimSpace(reviewer); trimmed != "" {
			reviewers = append(reviewers, trimmed)
		}
	}

	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	if err := store.ValidatePostgresSchema(ctx, database); err != nil {
		return postgresMigrationFailure(err)
	}
	pgStore, err := store.NewPostgresStore(database)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	assets := pgStore.Assets()

	if version == 0 {
		version, err = nextAssetVersion(ctx, assets, assetID, supersedes)
		if err != nil {
			return fail(exitUsage, "USAGE_ERROR", err)
		}
	}

	registered, err := assets.RegisterAsset(ctx, store.Asset{
		AssetID: assetID, Version: version, ProjectID: projectID,
		AssetType: assetType, Title: title, OwnerPrincipal: owner,
		Reviewers: reviewers, Sensitivity: sensitivity, SupersedesRef: supersedes,
		SourceDigest: digest, LockedGate: gate, ContentRef: contentRef, Summary: summary,
	}, owner)
	if err != nil {
		return fail(exitInternal, "INTAKE_REJECTED", err)
	}

	summaryOut := map[string]any{
		"asset":       store.FormatAssetRef(registered.AssetID, registered.Version),
		"status":      registered.Status,
		"type":        registered.AssetType,
		"sensitivity": registered.Sensitivity,
		"mode":        mode,
		"digest":      registered.SourceDigest,
		"bytes":       fileSize,
		"content_ref": registered.ContentRef,
		"supersedes":  registered.SupersedesRef,
	}
	if asJSON {
		return json.NewEncoder(ioStreams.out).Encode(summaryOut)
	}
	fmt.Fprintf(ioStreams.out, "registered %s status=%s type=%s sensitivity=%s mode=%s\n",
		store.FormatAssetRef(registered.AssetID, registered.Version), registered.Status,
		registered.AssetType, registered.Sensitivity, mode)
	fmt.Fprintf(ioStreams.out, "  digest=%s (%d bytes)\n", registered.SourceDigest, fileSize)
	fmt.Fprintf(ioStreams.out, "  content_ref=%s\n", registered.ContentRef)
	if registered.SupersedesRef != "" {
		fmt.Fprintf(ioStreams.out, "  supersedes=%s\n", registered.SupersedesRef)
	}
	fmt.Fprintln(ioStreams.out, "  audit=asset.registered (same transaction)")
	return nil
}

// latestAssetLookup is the one ledger capability the version derivation
// needs; satisfied by the store's asset ledger.
type latestAssetLookup interface {
	LatestAsset(ctx context.Context, assetID string) (*store.Asset, error)
}

// nextAssetVersion derives the registration version: supersedes chain
// first, else latest+1, else 1.
func nextAssetVersion(ctx context.Context, assets latestAssetLookup, assetID, supersedes string) (int, error) {
	if supersedes != "" {
		_, targetVersion, err := store.ParseSupersedesRef(supersedes)
		if err != nil {
			return 0, err
		}
		return targetVersion + 1, nil
	}
	latest, err := assets.LatestAsset(ctx, assetID)
	if err != nil {
		if errors.Is(err, store.ErrAssetNotFound) {
			return 1, nil
		}
		return 0, err
	}
	return latest.Version + 1, nil
}

// hashFile streams the artifact and returns its ledger digest: sha256
// over the raw bytes (NOT canonical JSON) so the digest pins the exact
// file content.
func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open artifact: %w", err)
	}
	defer file.Close()
	digest, size, err := store.ContentDigest(file)
	if err != nil {
		return "", 0, fmt.Errorf("hash artifact: %w", err)
	}
	return digest, size, nil
}

// copyIntoAssetsDir copies the artifact into <assetsDir>/<assetID>/ and
// returns the stored relative reference.
func copyIntoAssetsDir(assetsDir, assetID, source string) (string, error) {
	targetDir := filepath.Join(assetsDir, assetID)
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("create assets dir: %w", err)
	}
	filename := filepath.Base(source)
	target := filepath.Join(targetDir, filename)
	sourceFile, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer sourceFile.Close()
	targetFile, err := os.Create(target)
	if err != nil {
		return "", fmt.Errorf("create copy: %w", err)
	}
	defer targetFile.Close()
	if _, err := io.Copy(targetFile, sourceFile); err != nil {
		return "", fmt.Errorf("copy content: %w", err)
	}
	return filepath.Join(assetsDir, assetID, filename), nil
}

// loadSummary resolves the summary rows: an explicit JSON file wins,
// then --note, then the empty array.
func loadSummary(summaryFile, note string) ([]byte, error) {
	if summaryFile != "" {
		raw, err := os.ReadFile(summaryFile)
		if err != nil {
			return nil, fmt.Errorf("read summary file: %w", err)
		}
		var rows []any
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("summary file must be a JSON array: %w", err)
		}
		if len(rows) == 0 {
			return nil, errors.New("summary file must contain at least one row")
		}
		return raw, nil
	}
	if note != "" {
		return []byte(fmt.Sprintf(`[{"note":%q}]`, note)), nil
	}
	return []byte(`[]`), nil
}

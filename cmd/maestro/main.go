package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/app"
	"github.com/ZoonChen/Maestro-MCP/internal/config"
	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/identity"
	maestrotools "github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const (
	exitOK             = 0
	exitUsage          = 2
	exitDependency     = 3
	exitAuth           = 4
	exitMigration      = 5
	exitIncompatible   = 6
	exitInternal       = 10
	minProtocolVersion = "2024-11-05"
	maxProtocolVersion = "2025-11-25"
)

type commandError struct {
	exitCode int
	code     string
	err      error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func fail(exitCode int, code string, err error) error {
	return &commandError{exitCode: exitCode, code: code, err: err}
}

type streams struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	var signalExit atomic.Int32
	go func() {
		sig := <-signals
		switch sig {
		case syscall.SIGINT:
			signalExit.Store(130)
		case syscall.SIGTERM:
			signalExit.Store(143)
		}
		cancel()
	}()

	code := execute(ctx, os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr})
	if code == exitOK && signalExit.Load() != 0 {
		code = int(signalExit.Load())
	}
	os.Exit(code)
}

func execute(ctx context.Context, args []string, ioStreams streams) int {
	if ioStreams.in == nil {
		ioStreams.in = strings.NewReader("")
	}
	if ioStreams.out == nil {
		ioStreams.out = io.Discard
	}
	if ioStreams.err == nil {
		ioStreams.err = io.Discard
	}

	// MCP protocol bytes are written only to the explicit stdout stream. All
	// process logs are routed to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(ioStreams.err, &slog.HandlerOptions{Level: slog.LevelInfo})))

	err := dispatch(ctx, args, ioStreams)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return exitOK
	}

	var cmdErr *commandError
	if !errors.As(err, &cmdErr) {
		cmdErr = &commandError{exitCode: exitInternal, code: "INTERNAL_ERROR", err: err}
	}
	fmt.Fprintf(ioStreams.err, "code=%s correlation_id=%s error=%q\n", cmdErr.code, correlationID(), cmdErr.err.Error())
	return cmdErr.exitCode
}

func dispatch(ctx context.Context, args []string, ioStreams streams) error {
	if len(args) == 0 {
		printRootUsage(ioStreams.err)
		return fail(exitUsage, "USAGE_ERROR", errors.New("a subcommand is required"))
	}

	switch args[0] {
	case "server":
		return runServer(ctx, args[1:], ioStreams)
	case "runner":
		return runRunner(ctx, args[1:], ioStreams)
	case "migrate":
		return runMigrate(ctx, args[1:], ioStreams)
	case "pg-import":
		return runPGImport(ctx, args[1:], ioStreams)
	case "doctor":
		return runDoctor(ctx, args[1:], ioStreams)
	case "version":
		return runVersion(args[1:], ioStreams)
	case "help", "-h", "--help":
		printRootUsage(ioStreams.out)
		return nil
	default:
		printRootUsage(ioStreams.err)
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unknown subcommand %q", args[0]))
	}
}

type commonFlags struct {
	configPath string
	dbPath     string
}

func bindCommonFlags(fs *flag.FlagSet, flags *commonFlags) {
	fs.StringVar(&flags.configPath, "config", "", "configuration file path")
	fs.StringVar(&flags.dbPath, "db", "", "SQLite database path (M0 local baseline)")
}

func loadRuntimeConfig(flags commonFlags) (*config.Config, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil, fail(exitUsage, "CONFIG_INVALID", err)
	}
	if err := config.ApplyEnvOverrides(cfg); err != nil {
		return nil, fail(exitUsage, "CONFIG_INVALID", err)
	}
	if flags.dbPath != "" {
		cfg.DBPath = flags.dbPath
	}
	if err := cfg.Validate(); err != nil {
		return nil, fail(exitUsage, "CONFIG_INVALID", err)
	}
	return cfg, nil
}

func applicationOptions(cfg *config.Config) (app.Options, error) {
	testExecution, err := cfg.TestExecutionConfig()
	if err != nil {
		return app.Options{}, err
	}
	return app.Options{
		DBPath:               cfg.DBPath,
		AuthToken:            cfg.AuthToken,
		AllowedOrigins:       append([]string(nil), cfg.AllowedOrigins...),
		RemoteWrite:          cfg.RemoteWrite,
		MaxConnections:       cfg.MaxConnections,
		StaleTimeout:         time.Duration(cfg.StaleTimeout) * time.Second,
		StaleScannerInterval: 30 * time.Second,
		WorktreeGCInterval:   time.Duration(cfg.WorktreeGCInterval) * time.Second,
		DataGCInterval:       time.Duration(cfg.DataGCInterval) * time.Hour,
		ActivityLogRetention: cfg.ActivityLogRetention,
		TestExecution:        testExecution,
	}, nil
}

func runServer(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var httpAddr string
	var shutdownTimeout time.Duration
	fs.StringVar(&httpAddr, "http", "", "HTTP listen address (port 0 is supported)")
	fs.DurationVar(&shutdownTimeout, "shutdown-timeout", 30*time.Second, "graceful HTTP drain timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected server arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	if httpAddr != "" {
		cfg.HTTPAddr = httpAddr
	}
	if err := cfg.Validate(); err != nil {
		return fail(exitUsage, "CONFIG_INVALID", err)
	}
	if shutdownTimeout <= 0 {
		return fail(exitUsage, "CONFIG_INVALID", errors.New("shutdown timeout must be positive"))
	}
	configureLogger(cfg, ioStreams.err)

	options, err := applicationOptions(cfg)
	if err != nil {
		return fail(exitUsage, "CONFIG_INVALID", err)
	}
	// PostgreSQL control-plane composition (M1 interim): the M0 task
	// substrate stays on SQLite until the work-item cutover while
	// identity, the Runner registry and the v3 Runner API bind to the
	// PostgreSQL source of truth.
	if cfg.PostgresEnabled() {
		composed, composeErr := composePostgresSurfaces(ctx, cfg, &options)
		if composeErr != nil {
			return composeErr
		}
		options = composed
	}
	options.MaintenanceOwner = true
	options.HTTPLogWriter = ioStreams.err
	runtimeApp, err := app.New(ctx, options)
	if err != nil {
		return runtimeStartupFailure(err)
	}
	defer func() {
		if closeErr := runtimeApp.Close(); closeErr != nil {
			slog.Error("close runtime", "error", closeErr)
		}
	}()

	if err := runtimeApp.ServeHTTP(ctx, cfg.HTTPAddr, shutdownTimeout); err != nil {
		return fail(exitDependency, "RUNTIME_UNAVAILABLE", err)
	}
	return nil
}

func runRunner(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var runnerID string
	fs.StringVar(&runnerID, "runner-id", "local", "local Runner device identifier")
	var boundProject string
	fs.StringVar(&boundProject, "project", "", "server-side project binding for MCP claim tools (required for claims; identity is derived, never client-supplied)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected runner arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if strings.TrimSpace(runnerID) == "" {
		return fail(exitUsage, "CONFIG_INVALID", errors.New("runner-id must not be empty"))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	options, err := applicationOptions(cfg)
	if err != nil {
		return fail(exitUsage, "CONFIG_INVALID", err)
	}
	options.HTTPLogWriter = ioStreams.err
	if boundProject != "" {
		// The stdio transport's delegated context (single user, single
		// project): session and worker identity are server-derived from the
		// runner id and cannot be overridden by any tool argument.
		options.MCPBinding = &maestrotools.TransportBinding{
			ProjectID: boundProject,
			SessionID: runnerID + "-session",
			WorkerID:  runnerID + "-worker",
		}
	}
	runtimeApp, err := app.New(ctx, options)
	if err != nil {
		return runtimeStartupFailure(err)
	}
	defer func() {
		if closeErr := runtimeApp.Close(); closeErr != nil {
			slog.Error("close runtime", "runner_id", runnerID, "error", closeErr)
		}
	}()

	stdio := mcpserver.NewStdioServer(runtimeApp.MCPServer())
	stdio.SetErrorLogger(log.New(ioStreams.err, "mcp stdio: ", log.LstdFlags))
	slog.Info("maestro local runner started", "runner_id", runnerID, "transport", "stdio")
	if err := stdio.Listen(ctx, ioStreams.in, ioStreams.out); err != nil && !errors.Is(err, context.Canceled) {
		return fail(exitInternal, "MCP_TRANSPORT_ERROR", err)
	}
	return nil
}

func runMigrate(ctx context.Context, args []string, ioStreams streams) error {
	if len(args) == 0 {
		fmt.Fprintln(ioStreams.err, "usage: maestro migrate up|revert [--config FILE] [--db PATH] [--steps N]")
		return fail(exitUsage, "USAGE_ERROR", errors.New("a migrate action is required"))
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(ioStreams.out, "usage: maestro migrate up|revert [--config FILE] [--db PATH] [--steps N]")
		return nil
	}
	if args[0] == "revert" {
		return runMigrateRevert(ctx, args[1:], ioStreams)
	}
	if args[0] != "up" {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unsupported migrate action %q", args[0]))
	}

	fs := flag.NewFlagSet("migrate up", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected migrate arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	if cfg.PostgresEnabled() {
		return runPostgresMigrateUp(ctx, cfg, ioStreams)
	}
	if cfg.DBPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
			return fail(exitMigration, "MIGRATION_FAILED", fmt.Errorf("create database directory: %w", err))
		}
	}
	runtimeLock, err := app.AcquireRuntimeLock(cfg.DBPath)
	if err != nil {
		return fail(exitMigration, "MIGRATION_FAILED", fmt.Errorf("acquire runtime database lock: %w", err))
	}
	if runtimeLock != nil {
		defer runtimeLock.Close()
	}
	database, err := store.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		return fail(exitMigration, "MIGRATION_FAILED", err)
	}
	defer database.Close()
	var currentSchemaVersion int
	if err := database.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentSchemaVersion); err != nil {
		return fail(exitMigration, "MIGRATION_FAILED", fmt.Errorf("read migration plan schema version: %w", err))
	}
	fmt.Fprintf(
		ioStreams.out,
		"migration plan current_schema=%d target_schema=%d\n",
		currentSchemaVersion,
		store.CurrentSchemaVersion(),
	)
	if err := database.Init(ctx); err != nil {
		return migrationFailure(err)
	}
	fmt.Fprintf(ioStreams.out, "migration complete schema_version=%d\n", store.CurrentSchemaVersion())
	return nil
}

func runPostgresMigrateUp(ctx context.Context, cfg *config.Config, ioStreams streams) error {
	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	target, err := store.CurrentPostgresSchemaVersion()
	if err != nil {
		return fail(exitMigration, "MIGRATION_FAILED", err)
	}
	applied, err := store.ApplyPostgresMigrations(ctx, database)
	if err != nil {
		return postgresMigrationFailure(err)
	}
	fmt.Fprintf(ioStreams.out, "postgres migration complete applied=%d target_schema=%d\n", applied, target)
	return nil
}

func runMigrateRevert(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("migrate revert", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var steps int
	fs.IntVar(&steps, "steps", 1, "number of migrations to roll back (pre-cutover drill only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected migrate arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	if !cfg.PostgresEnabled() {
		return fail(exitUsage, "USAGE_ERROR", errors.New("migrate revert requires the postgres driver (pre-cutover rollback drill)"))
	}
	if steps < 1 {
		return fail(exitUsage, "USAGE_ERROR", errors.New("steps must be positive"))
	}
	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	reverted, err := store.RevertPostgresMigrations(ctx, database, steps)
	if err != nil {
		return postgresMigrationFailure(err)
	}
	fmt.Fprintf(ioStreams.out, "postgres migration reverted=%d\n", reverted)
	return nil
}

func postgresMigrationFailure(err error) error {
	if errors.Is(err, store.ErrPostgresMigrationIntegrity) {
		return fail(exitIncompatible, "SCHEMA_INTEGRITY_FAILED", fmt.Errorf(
			"%w; restore the database from a verified backup before retrying", err))
	}
	return fail(exitMigration, "MIGRATION_FAILED", err)
}

// runPGImport executes the four-stage SQLite -> PostgreSQL migration flow
// (M1-DATA-001): --dry-run plans without writing, the default applies the
// plan in one transaction, --reconcile verifies coverage and invariants.
func runPGImport(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("pg-import", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var sqlitePath string
	var dryRun, reconcile bool
	var reportPath string
	fs.StringVar(&sqlitePath, "sqlite", "", "frozen SQLite database to import (read-only)")
	fs.BoolVar(&dryRun, "dry-run", false, "plan and validate without writing to PostgreSQL")
	fs.BoolVar(&reconcile, "reconcile", false, "compare source and target instead of importing")
	fs.StringVar(&reportPath, "report", "", "optional path to write the JSON report")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected pg-import arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if strings.TrimSpace(sqlitePath) == "" {
		return fail(exitUsage, "USAGE_ERROR", errors.New("--sqlite PATH is required"))
	}
	if dryRun && reconcile {
		return fail(exitUsage, "USAGE_ERROR", errors.New("--dry-run and --reconcile are mutually exclusive"))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	if !cfg.PostgresEnabled() {
		return fail(exitUsage, "CONFIG_INVALID", errors.New("pg-import requires the postgres database configuration"))
	}

	sqliteDB, err := store.OpenSQLiteReadOnly(sqlitePath)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer sqliteDB.Close()
	pgDB, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer pgDB.Close()
	if err := store.ValidatePostgresSchema(ctx, pgDB); err != nil {
		return postgresMigrationFailure(err)
	}

	importer, err := store.NewSQLiteImporter(sqliteDB, pgDB)
	if err != nil {
		return fail(exitUsage, "CONFIG_INVALID", err)
	}
	var report *store.ImportReport
	switch {
	case dryRun:
		report, err = importer.DryRun(ctx)
	case reconcile:
		report, err = importer.Reconcile(ctx)
	default:
		report, err = importer.Import(ctx)
	}
	if err != nil {
		return fail(exitInternal, "IMPORT_FAILED", err)
	}

	encoder := json.NewEncoder(ioStreams.out)
	encoder.SetEscapeHTML(false)
	if writeErr := encoder.Encode(report); writeErr != nil {
		return fail(exitInternal, "IMPORT_FAILED", writeErr)
	}
	if reportPath != "" {
		if writeErr := os.WriteFile(reportPath, mustMarshalReport(report), 0o600); writeErr != nil {
			return fail(exitInternal, "IMPORT_FAILED", fmt.Errorf("write report: %w", writeErr))
		}
	}
	return nil
}

func mustMarshalReport(report *store.ImportReport) []byte {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		// ImportReport is a plain data struct; encoding cannot fail.
		return []byte("{}")
	}
	return append(encoded, '\n')
}

// composePostgresSurfaces binds the PostgreSQL-backed identity and Runner
// surfaces into the application options. Every failure is fatal: a
// half-configured identity layer must never start.
func composePostgresSurfaces(ctx context.Context, cfg *config.Config, options *app.Options) (app.Options, error) {
	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return *options, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	if err := store.ValidatePostgresSchema(ctx, database); err != nil {
		_ = database.Close()
		return *options, postgresMigrationFailure(err)
	}
	pgStore, err := store.NewPostgresStore(database)
	if err != nil {
		_ = database.Close()
		return *options, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}

	policy, err := identity.EmbeddedPolicy()
	if err != nil {
		_ = database.Close()
		return *options, fail(exitUsage, "CONFIG_INVALID", err)
	}

	// OIDC identity: issuer/audience from trusted configuration; the
	// principal resolver derives memberships from the registry.
	if cfg.OIDC != nil {
		verifier, verifierErr := identity.NewTokenVerifier(cfg.OIDC.Issuer, cfg.OIDC.Audience, nil)
		if verifierErr != nil {
			_ = database.Close()
			return *options, fail(exitUsage, "CONFIG_INVALID", verifierErr)
		}
		middleware := handler.NewOIDCMiddleware(policy, verifier, identity.NewStoreResolver(pgStore.Identities()))
		options.Identity = middleware.IdentityMount()
	}

	// v3 Runner API: requires the device-token secret from the
	// environment (secrets never live in the config file). Without it the
	// Runner API stays unexposed — honest degradation, never a fake.
	if tokenSecret := os.Getenv("MAESTRO_RUNNER_TOKEN_SECRET"); tokenSecret != "" {
		tokens, tokenErr := identity.NewDeviceTokenMinter(tokenSecret, nil)
		if tokenErr != nil {
			_ = database.Close()
			return *options, fail(exitUsage, "CONFIG_INVALID", tokenErr)
		}
		admin := options.Identity != nil
		var adminMiddleware *handler.OIDCMiddleware
		if admin {
			// Reuse the mounted identity middleware for admin routes: the
			// same verifier and resolver instance govern both surfaces.
			verifier, _ := identity.NewTokenVerifier(cfg.OIDC.Issuer, cfg.OIDC.Audience, nil)
			adminMiddleware = handler.NewOIDCMiddleware(policy, verifier, identity.NewStoreResolver(pgStore.Identities()))
		}
		options.RunnerV3 = &handler.RunnerV3Options{
			Registry: pgStore,
			Tokens:   tokens,
			Policy:   policy,
			Admin:    adminMiddleware,
		}
	} else {
		slog.Warn("MAESTRO_RUNNER_TOKEN_SECRET not set; the v3 Runner API stays unexposed")
	}

	options.Dependencies = append(options.Dependencies, pgDependency{store: pgStore})
	// The pool lives for the process lifetime; closing happens on exit.
	return *options, nil
}

// pgDependency reports PostgreSQL readiness into the aggregate health gate.
type pgDependency struct{ store *store.PostgresStore }

func (d pgDependency) Name() string { return "postgresql" }

func (d pgDependency) Check(ctx context.Context) error {
	if _, err := d.store.DB().ExecContext(ctx, `SELECT 1`); err != nil {
		return fmt.Errorf("postgres readiness: %w", err)
	}
	return nil
}

func runtimeStartupFailure(err error) error {
	if errors.Is(err, store.ErrSchemaVersionIncompatible) {
		return fail(exitIncompatible, "VERSION_INCOMPATIBLE", fmt.Errorf("%w; run maestro migrate up", err))
	}
	if errors.Is(err, store.ErrSchemaIntegrity) {
		return fail(exitIncompatible, "SCHEMA_INTEGRITY_FAILED", fmt.Errorf(
			"%w; restore the database from a verified backup before retrying",
			err,
		))
	}
	return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
}

func migrationFailure(err error) error {
	if errors.Is(err, store.ErrSchemaIntegrity) {
		return fail(exitMigration, "MIGRATION_FAILED", fmt.Errorf(
			"%w; refusing automatic repair: restore the database from a verified backup",
			err,
		))
	}
	return fail(exitMigration, "MIGRATION_FAILED", err)
}

func doctorSchemaFailure(err error) error {
	if errors.Is(err, store.ErrSchemaVersionIncompatible) {
		return fail(exitIncompatible, "VERSION_INCOMPATIBLE", fmt.Errorf("%w; run maestro migrate up", err))
	}
	if errors.Is(err, store.ErrSchemaIntegrity) {
		return fail(exitIncompatible, "SCHEMA_INTEGRITY_FAILED", fmt.Errorf(
			"%w; restore the database from a verified backup before retrying",
			err,
		))
	}
	return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
}

type doctorReport struct {
	Status             string                           `json:"status"`
	Driver             string                           `json:"driver"`
	Database           string                           `json:"database"`
	Health             string                           `json:"health,omitempty"`
	Schema             int                              `json:"schema_version"`
	Runtime            string                           `json:"runtime"`
	RemoteWrite        bool                             `json:"remote_write"`
	HostExecution      bool                             `json:"host_execution"`
	PolicyVersion      string                           `json:"policy_version,omitempty"`
	PolicyDigest       string                           `json:"policy_digest,omitempty"`
	ValidationProfiles []config.ValidationProfileReport `json:"validation_profiles"`
}

func runDoctor(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var jsonOutput bool
	var healthURL string
	fs.BoolVar(&jsonOutput, "json", false, "write a machine-readable report")
	fs.StringVar(&healthURL, "health-url", "", "optional readyz URL to probe")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected doctor arguments: %s", strings.Join(fs.Args(), " ")))
	}

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	profileReports, err := cfg.ValidationProfileReports()
	if err != nil {
		return fail(exitUsage, "CONFIG_INVALID", err)
	}

	var report doctorReport
	if cfg.PostgresEnabled() {
		report, err = runDoctorPostgres(ctx, cfg, profileReports)
	} else {
		report, err = runDoctorSQLite(ctx, cfg, profileReports)
	}
	if err != nil {
		return err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if healthURL != "" {
		request, requestErr := http.NewRequestWithContext(pingCtx, http.MethodGet, healthURL, nil)
		if requestErr != nil {
			return fail(exitUsage, "CONFIG_INVALID", fmt.Errorf("health URL: %w", requestErr))
		}
		response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if requestErr != nil {
			return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", fmt.Errorf("health probe: %w", requestErr))
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", fmt.Errorf("health probe returned %s", response.Status))
		}
		report.Health = "ok"
	}

	if jsonOutput {
		encoder := json.NewEncoder(ioStreams.out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(report)
	}
	fmt.Fprintf(ioStreams.out, "status=%s driver=%s database=%s schema_version=%d runtime=%s remote_write=%t host_execution=%t profiles=%d", report.Status, report.Driver, report.Database, report.Schema, report.Runtime, report.RemoteWrite, report.HostExecution, len(report.ValidationProfiles))
	if report.Health != "" {
		fmt.Fprintf(ioStreams.out, " health=%s", report.Health)
	}
	fmt.Fprintln(ioStreams.out)
	return nil
}

func runDoctorSQLite(ctx context.Context, cfg *config.Config, profileReports []config.ValidationProfileReport) (doctorReport, error) {
	if cfg.DBPath != ":memory:" {
		parent := filepath.Dir(cfg.DBPath)
		if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
			if statErr == nil {
				statErr = errors.New("database parent is not a directory")
			}
			return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", fmt.Errorf("database parent %s: %w", parent, statErr))
		}
	}

	database, err := store.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := database.DB().PingContext(pingCtx); err != nil {
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", fmt.Errorf("database ping: %w", err))
	}
	if err := database.ValidateSchema(pingCtx); err != nil {
		return doctorReport{}, doctorSchemaFailure(err)
	}
	var actualSchemaVersion int
	if err := database.DB().QueryRowContext(pingCtx, "PRAGMA user_version").Scan(&actualSchemaVersion); err != nil {
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", fmt.Errorf("database schema version: %w", err))
	}
	return doctorReport{
		Status:             "ok",
		Driver:             config.DatabaseDriverSQLite,
		Database:           "ok",
		Schema:             actualSchemaVersion,
		Runtime:            runtime.Version(),
		RemoteWrite:        cfg.RemoteWrite,
		HostExecution:      cfg.Validation.AllowHostExecution,
		PolicyVersion:      cfg.Validation.PolicyVersion,
		PolicyDigest:       cfg.Validation.PolicyDigest,
		ValidationProfiles: profileReports,
	}, nil
}

func runDoctorPostgres(ctx context.Context, cfg *config.Config, profileReports []config.ValidationProfileReport) (doctorReport, error) {
	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	if err := store.ValidatePostgresSchema(ctx, database); err != nil {
		if errors.Is(err, store.ErrPostgresMigrationIntegrity) {
			return doctorReport{}, fail(exitIncompatible, "SCHEMA_INTEGRITY_FAILED", fmt.Errorf(
				"%w; restore the database from a verified backup before retrying", err))
		}
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	target, err := store.CurrentPostgresSchemaVersion()
	if err != nil {
		return doctorReport{}, fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	return doctorReport{
		Status:             "ok",
		Driver:             config.DatabaseDriverPostgres,
		Database:           "ok",
		Schema:             int(target),
		Runtime:            runtime.Version(),
		RemoteWrite:        cfg.RemoteWrite,
		HostExecution:      cfg.Validation.AllowHostExecution,
		PolicyVersion:      cfg.Validation.PolicyVersion,
		PolicyDigest:       cfg.Validation.PolicyDigest,
		ValidationProfiles: profileReports,
	}, nil
}

type versionInfo struct {
	Version            string `json:"version"`
	Commit             string `json:"commit"`
	BuildTime          string `json:"build_time"`
	GoVersion          string `json:"go_version"`
	SchemaVersion      int    `json:"schema_version"`
	MinProtocolVersion string `json:"min_protocol_version"`
	MaxProtocolVersion string `json:"max_protocol_version"`
}

func runVersion(args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var jsonOutput bool
	fs.BoolVar(&jsonOutput, "json", false, "write machine-readable version information")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected version arguments: %s", strings.Join(fs.Args(), " ")))
	}

	info := versionInfo{
		Version:            Version,
		Commit:             Commit,
		BuildTime:          BuildTime,
		GoVersion:          runtime.Version(),
		SchemaVersion:      store.CurrentSchemaVersion(),
		MinProtocolVersion: minProtocolVersion,
		MaxProtocolVersion: maxProtocolVersion,
	}
	if jsonOutput {
		encoder := json.NewEncoder(ioStreams.out)
		encoder.SetEscapeHTML(false)
		return encoder.Encode(info)
	}
	fmt.Fprintf(ioStreams.out, "maestro %s commit=%s build_time=%s go=%s schema=%d protocol=%s..%s\n",
		info.Version,
		info.Commit,
		info.BuildTime,
		info.GoVersion,
		info.SchemaVersion,
		info.MinProtocolVersion,
		info.MaxProtocolVersion,
	)
	return nil
}

func printRootUsage(w io.Writer) {
	fmt.Fprintln(w, `Maestro MCP

Usage:
  maestro server  [--config FILE] [--db PATH] [--http ADDR]
  maestro runner  [--config FILE] [--db PATH] [--runner-id ID]
  maestro migrate up     [--config FILE] [--db PATH]
  maestro migrate revert [--config FILE] [--steps N]   (postgres, pre-cutover drill)
  maestro pg-import --sqlite PATH [--dry-run|--reconcile] [--report FILE] [--config FILE]
  maestro doctor  [--config FILE] [--db PATH] [--health-url URL] [--json]
  maestro version [--json]`)
}

func configureLogger(cfg *config.Config, writer io.Writer) {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(writer, options)))
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(writer, options)))
}

func correlationID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(random[:])
}

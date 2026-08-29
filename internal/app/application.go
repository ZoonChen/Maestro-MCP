// Package app is the single M0 composition root. It owns infrastructure
// lifetimes and wires every transport to the same service instances.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/handler"
	"github.com/ZoonChen/Maestro-MCP/internal/health"
	maestromcp "github.com/ZoonChen/Maestro-MCP/internal/mcp"
	maestrotools "github.com/ZoonChen/Maestro-MCP/internal/mcp/tools"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/ws"
	"github.com/gin-gonic/gin"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"golang.org/x/net/netutil"
)

const (
	defaultShutdownTimeout = 30 * time.Second
	defaultScannerInterval = 30 * time.Second
	defaultDataGCInterval  = 24 * time.Hour
	// Keep the listener available briefly after readiness and write authority
	// are revoked so already-accepted/keep-alive clients receive a stable 503
	// instead of racing an opaque connection reset.
	drainPropagationDelay = 100 * time.Millisecond
)

// Options contains only the M0 local-runtime settings needed by the
// composition root. Higher-stage Control Plane/Runner settings deliberately do
// not leak into the SQLite baseline.
type Options struct {
	DBPath string
	// MaintenanceOwner marks the single M0 process allowed to run startup
	// recovery and destructive/background reconciliation for this SQLite DB.
	// The HTTP server sets it; the stdio Runner deliberately does not.
	MaintenanceOwner     bool
	AuthToken            string
	AllowedOrigins       []string
	RemoteWrite          bool
	HTTPLogWriter        io.Writer
	MaxConnections       int
	StaleTimeout         time.Duration
	StaleScannerInterval time.Duration
	WorktreeGCInterval   time.Duration
	DataGCInterval       time.Duration
	ActivityLogRetention int
	// TestExecution is trusted process-owned validation configuration. Its zero
	// value intentionally has no approved profiles and disables host execution,
	// so submission fails closed until the operator injects versioned policy.
	TestExecution service.TestExecutionConfig
	// Identity mounts the OIDC authorization middleware (M1-AUTH-001);
	// nil keeps the M0 fail-closed shared-token baseline.
	Identity *handler.IdentityMount
	// RunnerV3 mounts the Control Plane side of the frozen runner.yaml
	// (M1-RUN-001); nil leaves the v3 Runner API unexposed.
	RunnerV3 *handler.RunnerV3Options
	// MCPBinding is the server-side transport scope for MCP claim tools
	// (M1-MCP-001): project, session and worker identity are assigned here,
	// never accepted from tool arguments. Nil leaves claim tools fail-closed.
	MCPBinding *maestrotools.TransportBinding
	// Dependencies are the M1 dependency-health probes (M1-ARCH-001). M0
	// registers none; readiness keeps its local-baseline semantics until a
	// stream wires PostgreSQL/OIDC/runner-pool probes.
	Dependencies []health.Dependency
}

// Application owns the SQLite adapter, the shared application services, all
// transports, and cancellable background work.
type Application struct {
	database     *store.SQLiteDB
	sqlDB        *sql.DB
	httpHandler  *gin.Engine
	mcpServer    *mcpserver.MCPServer
	mcpHTTP      *mcpserver.StreamableHTTPServer
	dependencies *health.Registry

	backgroundCtx    context.Context
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup

	ready       atomic.Bool
	draining    *atomic.Bool
	closed      atomic.Bool
	drainOnce   sync.Once
	closeOnce   sync.Once
	runtimeLock *runtimeLock

	options Options
}

// New creates the real M0 runtime graph. Dependency construction and schema
// initialization are fail-fast: a partially wired process is never marked
// ready. Only the maintenance owner may bootstrap an empty SQLite database;
// a local stdio Runner validates but never creates or alters schema.
func New(ctx context.Context, opts Options) (*Application, error) {
	if opts.StaleTimeout <= 0 {
		opts.StaleTimeout = 120 * time.Second
	}
	if opts.StaleScannerInterval <= 0 {
		opts.StaleScannerInterval = defaultScannerInterval
	}
	if opts.WorktreeGCInterval <= 0 {
		opts.WorktreeGCInterval = 10 * time.Minute
	}
	if opts.DataGCInterval <= 0 {
		opts.DataGCInterval = defaultDataGCInterval
	}
	if opts.ActivityLogRetention <= 0 {
		opts.ActivityLogRetention = 90
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = 50
	}
	if opts.TestExecution.DefaultTimeout <= 0 {
		opts.TestExecution.DefaultTimeout = 120 * time.Second
	}
	if opts.TestExecution.MaxOutputBytes <= 0 {
		opts.TestExecution.MaxOutputBytes = 64 << 10
	}
	if opts.TestExecution.AllowHostExecution {
		slog.Warn("SECURITY WARNING: validation profiles execute on the host",
			"security_warning", "HOST_EXECUTION_ENABLED",
			"required_action", "use only on an isolated loopback development host")
	}
	if opts.DBPath != "" && opts.DBPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	var runtimeOwnerLock *runtimeLock
	if opts.MaintenanceOwner {
		var err error
		runtimeOwnerLock, err = acquireRuntimeLock(opts.DBPath)
		if err != nil {
			return nil, fmt.Errorf("acquire M0 maintenance ownership: %w", err)
		}
	}

	database, err := store.NewSQLiteDB(opts.DBPath)
	if err != nil {
		if runtimeOwnerLock != nil {
			_ = runtimeOwnerLock.Close()
		}
		return nil, fmt.Errorf("open database: %w", err)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = database.Close()
			if runtimeOwnerLock != nil {
				_ = runtimeOwnerLock.Close()
			}
		}
	}()

	if opts.MaintenanceOwner {
		if err := database.EnsureRuntimeSchema(ctx); err != nil {
			return nil, fmt.Errorf("ensure runtime database schema: %w", err)
		}
	} else if err := database.ValidateSchema(ctx); err != nil {
		return nil, fmt.Errorf("validate runner database schema: %w", err)
	}

	sqlDB := database.DB()
	hub := ws.NewHub()

	projectStore := store.NewSQLiteProjectStore(sqlDB)
	featureStore := store.NewSQLiteFeatureStore(sqlDB)
	taskStore := store.NewSQLiteTaskStore(sqlDB)
	resultStore := store.NewSQLiteTaskResultStore(sqlDB)
	validationStore := store.NewSQLiteValidationRunStore(sqlDB)
	sessionStore := store.NewSessionStore(sqlDB)
	workerStore := store.NewWorkerStore(sqlDB)
	worktreeStore := store.NewWorktreeStore(sqlDB)
	activityStore := store.NewActivityLogStore(sqlDB)
	auditStore := store.NewAuditLogStore(sqlDB)
	contractStore := store.NewContractStore(sqlDB)

	projectService := service.NewProjectService(projectStore, auditStore)
	featureService := service.NewFeatureService(featureStore, taskStore, hub)
	worktreeService := service.NewWorktreeService(worktreeStore, projectStore, sqlDB)
	sessionService := service.NewSessionService(
		sessionStore,
		workerStore,
		taskStore,
		worktreeStore,
		auditStore,
		hub,
	)
	taskService := service.NewTaskService(
		taskStore,
		resultStore,
		validationStore,
		sessionStore,
		workerStore,
		worktreeStore,
		activityStore,
		auditStore,
		projectStore,
		featureStore,
		sqlDB,
		hub,
	)
	taskService.OnFeatureStatusChange = func(callbackCtx context.Context, projectID, featureID string) {
		if err := featureService.AutoTransitionStatus(callbackCtx, projectID, featureID); err != nil {
			slog.Error("feature status transition failed", "project_id", projectID, "feature_id", featureID, "error", err)
		}
	}
	validationService := service.NewValidationService(
		taskStore,
		resultStore,
		validationStore,
		worktreeStore,
		activityStore,
		projectStore,
		sqlDB,
		hub,
		opts.TestExecution,
	)
	contractService := service.NewContractService(contractStore)
	contextService := service.NewContextService(taskStore, contractStore)

	projectHandler := handler.NewProjectHandler(projectService)
	featureHandler := handler.NewFeatureHandler(featureService)
	taskHandler := handler.NewTaskHandler(taskService, validationService, sessionService)
	sessionHandler := handler.NewSessionHandler(sessionService)
	boardHandler := handler.NewBoardHandler(
		taskService,
		featureService,
		activityStore,
		projectService,
		worktreeService,
		sessionService,
	)

	mcpServices := &maestrotools.Services{
		Binding:    opts.MCPBinding,
		Project:    projectService,
		Feature:    featureService,
		Task:       taskService,
		Session:    sessionService,
		Worktree:   worktreeService,
		Validation: validationService,
		Contract:   contractService,
		Context:    contextService,
	}
	mcpServer := maestromcp.NewMaestroMCPServer(mcpServices)

	draining := &atomic.Bool{}
	router := handler.SetupRouter(
		projectHandler,
		featureHandler,
		taskHandler,
		sessionHandler,
		boardHandler,
		hub,
		projectService,
		sessionService,
		worktreeService,
		opts.AuthToken,
		handler.RouterOptions{
			AllowedOrigins: append([]string(nil), opts.AllowedOrigins...),
			RemoteWrite:    opts.RemoteWrite,
			LogWriter:      opts.HTTPLogWriter,
			IsDraining:     draining.Load,
			Identity:       opts.Identity,
		},
	)
	if opts.RunnerV3 != nil {
		handler.RegisterRunnerV3(router, *opts.RunnerV3)
	}

	dependencies := &health.Registry{}
	for _, dependency := range opts.Dependencies {
		if err := dependencies.Register(dependency); err != nil {
			return nil, fmt.Errorf("dependency health: %w", err)
		}
	}
	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	a := &Application{
		database:         database,
		sqlDB:            sqlDB,
		httpHandler:      router,
		mcpServer:        mcpServer,
		dependencies:     dependencies,
		backgroundCtx:    backgroundCtx,
		backgroundCancel: backgroundCancel,
		draining:         draining,
		runtimeLock:      runtimeOwnerLock,
		options:          opts,
	}

	a.mountRuntimeRoutes(router)

	// Only the exclusive HTTP maintenance owner may infer that persisted work
	// belongs to a previous process. A concurrently launched stdio Runner shares
	// the M0 database but must never recover live server state.
	if opts.MaintenanceOwner {
		recoveryService := service.NewRecoveryService(sqlDB, projectStore)
		if err := recoveryService.Run(ctx); err != nil {
			return nil, fmt.Errorf("startup recovery: %w", err)
		}
	}

	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		hub.RunContext(backgroundCtx)
	}()
	if opts.MaintenanceOwner {
		a.backgroundWG.Add(1)
		go func() {
			defer a.backgroundWG.Done()
			sessionService.RunStaleSessionScanner(
				backgroundCtx,
				opts.StaleScannerInterval,
				int(opts.StaleTimeout/time.Second),
			)
		}()
		a.startDataGC(service.NewGCService(sqlDB))
		a.startWorktreeGC(projectService, worktreeService)
	}

	cleanupOnError = false
	return a, nil
}

func (a *Application) mountRuntimeRoutes(router *gin.Engine) {
	streamable := mcpserver.NewStreamableHTTPServer(a.mcpServer)
	a.mcpHTTP = streamable

	// These routes are registered after SetupRouter so that they inherit the
	// same body limit, Origin policy, rate limit, and authentication middleware.
	router.GET("/livez", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "alive"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		if err := a.Ready(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status": "not_ready",
				"code":   "DEPENDENCY_UNAVAILABLE",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	router.Any("/mcp", gin.WrapH(streamable))
}

func (a *Application) startDataGC(gc *service.GCService) {
	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		ticker := time.NewTicker(a.options.DataGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-a.backgroundCtx.Done():
				return
			case <-ticker.C:
				if err := gc.RunActivityLogGC(a.backgroundCtx, a.options.ActivityLogRetention); err != nil {
					slog.Error("activity log GC failed", "error", err)
				}
				if err := gc.RunTestLogGC(a.backgroundCtx, 30); err != nil {
					slog.Error("validation log GC failed", "error", err)
				}
			}
		}
	}()
}

func (a *Application) startWorktreeGC(projects *service.ProjectService, worktrees *service.WorktreeService) {
	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		ticker := time.NewTicker(a.options.WorktreeGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-a.backgroundCtx.Done():
				return
			case <-ticker.C:
				projectList, err := projects.ListProjects(a.backgroundCtx, true)
				if err != nil {
					slog.Error("worktree GC project scan failed", "error", err)
					continue
				}
				for _, project := range projectList {
					if err := worktrees.GCWorktrees(a.backgroundCtx, project.ID); err != nil {
						slog.Error("worktree GC failed", "project_id", project.ID, "error", err)
					}
				}
			}
		}
	}()
}

// Handler returns the fully assembled HTTP transport graph.
func (a *Application) Handler() http.Handler {
	return a.httpHandler
}

// MCPServer returns the shared MCP protocol server for the local stdio Runner.
func (a *Application) MCPServer() *mcpserver.MCPServer {
	return a.mcpServer
}

// Ready checks runtime state and the local source-of-truth dependency. M1
// dependency-health probes aggregate into the same decision: any unavailable
// dependency makes the whole Control Plane not ready (fail-closed).
func (a *Application) Ready(ctx context.Context) error {
	if !a.ready.Load() {
		return errors.New("runtime is not accepting traffic")
	}
	if a.draining == nil || a.draining.Load() || a.closed.Load() {
		return errors.New("runtime is draining")
	}
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.sqlDB.PingContext(pingCtx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	if _, ready := a.dependencies.Check(pingCtx); !ready {
		return errors.New("dependency health: not ready")
	}
	return nil
}

// BeginDrain immediately makes readiness fail before the HTTP listener starts
// draining in-flight requests.
func (a *Application) BeginDrain() {
	a.drainOnce.Do(func() {
		if a.draining != nil {
			a.draining.Store(true)
		}
		a.ready.Store(false)
		a.backgroundCancel()
		if a.options.MaintenanceOwner {
			slog.Info("maestro server drain started", "lifecycle", "DRAINING")
		}
	})
}

// ServeHTTP owns the listener and graceful shutdown lifecycle. An address with
// port zero is supported for hermetic tests; the selected address is logged.
func (a *Application) ServeHTTP(ctx context.Context, addr string, shutdownTimeout time.Duration) error {
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	listener = netutil.LimitListener(listener, a.options.MaxConnections)

	httpServer := &http.Server{
		Handler:           a.httpHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streamable HTTP MCP may hold an authorized event stream open. Handler
		// and shutdown contexts bound work; a server-wide WriteTimeout would
		// corrupt otherwise healthy long-lived protocol responses.
		WriteTimeout:   0,
		IdleTimeout:    90 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	if a.draining == nil || a.draining.Load() || a.closed.Load() {
		_ = listener.Close()
		return errors.New("runtime is already draining")
	}
	a.ready.Store(true)
	slog.Info("maestro server ready", "address", listener.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.Serve(listener)
	}()

	select {
	case err := <-errCh:
		a.BeginDrain()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
		a.BeginDrain()
		drainTimer := time.NewTimer(drainPropagationDelay)
		<-drainTimer.C
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			return fmt.Errorf("shutdown HTTP: %w", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP during shutdown: %w", err)
		}
		return nil
	}
}

// Close stops cancellable workers and releases the database. It is idempotent.
func (a *Application) Close() error {
	var closeErr error
	a.closeOnce.Do(func() {
		a.BeginDrain()
		a.closed.Store(true)
		a.backgroundCancel()
		a.backgroundWG.Wait()
		if a.mcpHTTP != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.mcpHTTP.Shutdown(shutdownCtx); err != nil {
				closeErr = fmt.Errorf("shutdown MCP HTTP: %w", err)
			}
			cancel()
		}
		if err := a.database.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		if a.runtimeLock != nil {
			if err := a.runtimeLock.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

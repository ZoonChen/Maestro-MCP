package handler

import (
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/publicerror"
	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/ws"
	mcpweb "github.com/ZoonChen/Maestro-MCP/web"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// RouterOptions contains security controls that must be supplied by trusted
// server configuration, never by an HTTP request.
type RouterOptions struct {
	AllowedOrigins []string
	RemoteWrite    bool
	LogWriter      io.Writer
	IsDraining     func() bool

	// ControlPlane mounts the frozen control-plane.yaml human tree under
	// its declared /api/v3 base path (quality, GitLab registry) with the
	// same bearer authentication and frozen policy as the v1 tree; nil
	// leaves those surfaces unexposed.
	ControlPlane *ControlPlaneOptions

	// Identity is the M1 identity-layer mount point (M1-AUTH-001). While
	// nil, the M0 fail-closed shared-token AuthMiddleware keeps guarding the
	// tree. Once the identity layer is wired (S2), Authenticate replaces the
	// shared-token middleware, Authorize enforces the unified
	// authorize(principal, action, resource) decision on /api/v1, and
	// RegisterRoutes attaches the OIDC Authorization Code + PKCE endpoints
	// under /auth.
	Identity *IdentityMount

	// Quality mounts the frozen control-plane.yaml Quality endpoints
	// (M2-QG-001) behind the same authorize decision as every /api/v1
	// route; nil leaves them unexposed (non-PostgreSQL deployments).
	Quality *QualityHandler
}

// IdentityMount is the frozen mounting contract between the router and the
// identity layer. All three members are optional individually so the layer
// can land incrementally, but a mounted Authenticate MUST construct the
// server-side PrincipalContext (health paths stay anonymous) and never
// trust self-reported role/project/session fields from the payload.
type IdentityMount struct {
	// Authenticate resolves the request credential (session cookie or
	// bearer token) into a PrincipalContext on the gin context. When set it
	// replaces the M0 shared-token AuthMiddleware for the whole tree except
	// the /auth protocol group, whose handlers enforce their own credential
	// rules (anonymous login/callback, cookie-bound logout).
	Authenticate gin.HandlerFunc

	// Authorize is the policy decision point applied to every /api/v1
	// route after authentication. Deny must answer 401/403/404 per
	// SEC-IDENTITY-RBAC section 5 and be audited without business or
	// outbox side effects.
	Authorize gin.HandlerFunc

	// RegisterRoutes attaches protocol routes (for example OIDC login,
	// callback, logout) to the /auth group. The group inherits the same
	// body limit, CORS and rate-limit chain as /api/v1.
	RegisterRoutes func(group *gin.RouterGroup)
}

func init() {
	// Gin's package-level debug writer defaults to stdout. The same process also
	// serves the local stdio MCP transport, whose stdout is protocol-only, so
	// route-registration diagnostics must never use that global default. Runtime
	// access/recovery logs are installed explicitly on each Engine below.
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard
}

// SetupRouter creates and configures the Gin engine with all REST API routes
// and the WebSocket upgrade endpoint. Returns the configured *gin.Engine.
func SetupRouter(
	ph *ProjectHandler,
	fh *FeatureHandler,
	th *TaskHandler,
	sh *SessionHandler,
	bh *BoardHandler,
	hub *ws.Hub,
	projectSvc *service.ProjectService,
	_ *service.SessionService,
	_ *service.WorktreeService,
	authToken string,
	options ...RouterOptions,
) *gin.Engine {
	opts := RouterOptions{}
	if len(options) > 0 {
		opts = options[0]
	}

	logWriter := opts.LogWriter
	if logWriter == nil {
		logWriter = io.Discard
	}
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		panic("trusted proxy configuration rejected")
	}
	r.Use(safeAccessLogger(logWriter), safeRecoveryWithWriter(logWriter))

	// Limit request body size to 1MB across all endpoints.
	r.Use(MaxBodySize(1 << 20))

	// CORS headers for cross-origin API consumers.
	r.Use(CORS(opts.AllowedOrigins...))

	// Rate limiting: 100 requests per minute per IP.
	r.Use(RateLimit(100, time.Minute))

	// Identity protocol endpoints (OIDC Authorization Code + PKCE login,
	// callback, logout). The group only exists when the identity layer
	// provides its routes; without it no /auth route is exposed. Login and
	// callback are anonymous protocol entries, so the group deliberately
	// mounts before the authentication, drain and remote-write guards (it
	// still carries logging, recovery, body limits, CORS and rate limits).
	// Route handlers enforce their own credential rules (logout requires the
	// session cookie, callback validates state/nonce).
	if opts.Identity != nil && opts.Identity.RegisterRoutes != nil {
		auth := r.Group("/auth")
		opts.Identity.RegisterRoutes(auth)
	}

	// Authentication fails closed when authToken is empty. A mounted
	// identity layer (M1) takes over the authentication decision and the
	// shared-token middleware is retired for that tree.
	if opts.Identity != nil && opts.Identity.Authenticate != nil {
		r.Use(opts.Identity.Authenticate)
	} else {
		r.Use(AuthMiddleware(authToken))
	}

	// Once shutdown begins, all new state-changing REST and MCP calls stop at
	// the transport boundary. Health and explicitly read-only protocol calls
	// remain available for drain observation.
	r.Use(DrainGuard(opts.IsDraining))

	// Mutating remote requests require an explicit server-side feature flag.
	r.Use(RemoteWriteGuard(opts.RemoteWrite))

	// WebSocket upgrader with same-origin check.
	originAllowlist := buildOriginAllowlist(opts.AllowedOrigins)
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			normalized, ok := normalizeOrigin(origin)
			return ok && requestOriginAllowed(r, normalized, originAllowlist)
		},
	}

	api := r.Group("/api/v1")
	{
		// Unified authorize(principal, action, resource) decision point for
		// every /api/v1 route once the M1 identity layer is mounted.
		if opts.Identity != nil && opts.Identity.Authorize != nil {
			api.Use(opts.Identity.Authorize)
		}
		// Global (non-project-scoped) endpoints.
		api.GET("/projects", ph.ListProjects)
		api.POST("/projects", ph.CreateProject)
		api.GET("/overview", bh.GetOverview)
		api.GET("/metrics", bh.GetMetrics)

		// Project endpoints — use :id consistently.
		// Nested resources (features, tasks, sessions, board) use the same :id param.
		project := api.Group("/projects/:id")
		project.Use(ProjectGuard(projectSvc))
		{
			// Single project CRUD
			project.GET("", ph.GetProject)
			project.PATCH("", ph.UpdateProject)
			project.POST("/archive", ph.ArchiveProject)
			project.POST("/restore", ph.RestoreProject)

			// Features
			project.POST("/features", fh.CreateFeature)
			project.GET("/features", fh.ListFeatures)
			project.GET("/features/:fid", fh.GetFeature)
			project.PATCH("/features/:fid", fh.UpdateFeature)

			// Tasks
			project.POST("/tasks", th.CreateTask)
			project.GET("/tasks", th.ListTasks)
			// Claiming changes Task, Lease, Session/Worker and workspace state. It
			// is intentionally a POST so caches, crawlers and the remote-write
			// feature gate can never treat it as a read.
			project.POST("/tasks/next", th.GetNextTask)
			project.POST("/tasks/next-verification", th.GetNextVerificationTask)
			project.GET("/tasks/:tid", th.GetTask)
			project.PATCH("/tasks/:tid", th.UpdateTask)
			project.POST("/tasks/:tid/claim", th.ClaimTask)
			project.POST("/tasks/:tid/heartbeat", th.HeartbeatTask)
			project.POST("/tasks/:tid/submit", th.SubmitTask)
			project.POST("/tasks/:tid/block", th.BlockTask)
			project.POST("/tasks/:tid/resolve", th.ResolveBlocker)
			project.POST("/tasks/:tid/verify", th.VerifyTask)
			project.POST("/tasks/:tid/resolve-merge-conflict", th.ResolveMergeConflict)
			project.POST("/tasks/:tid/cancel", th.CancelTask)
			project.GET("/tasks/:tid/validation", th.GetValidationHistory)
			project.GET("/tasks/:tid/result", th.GetTaskResult)
			project.GET("/tasks/:tid/diff", th.GetTaskDiff)
			project.POST("/tasks/:tid/force-rollback", th.ForceRollbackTask)

			// Sessions
			project.POST("/sessions", sh.RegisterSession)
			project.GET("/sessions", sh.ListSessions)
			project.GET("/sessions/:sid", sh.GetSession)
			project.PUT("/sessions/:sid/heartbeat", sh.Heartbeat)
			project.DELETE("/sessions/:sid", sh.DisconnectSession)
			project.POST("/sessions/:sid/force-release", sh.ForceReleaseSession)

			// Workers (nested under sessions)
			project.POST("/sessions/:sid/workers", sh.RegisterWorker)
			project.GET("/sessions/:sid/workers", sh.ListWorkers)
			project.DELETE("/sessions/:sid/workers/:wid", sh.RemoveWorker)

			// Board
			project.GET("/board", bh.GetBoard)
			project.GET("/board/activity", bh.GetActivity)
			project.POST("/worktrees/gc", bh.TriggerWorktreeGC)

			// WebSocket endpoint for real-time event streaming.
			project.GET("/ws", func(c *gin.Context) {
				serveWS(hub, upgrader, c)
			})
		}
	}

	// Health endpoint â lightweight liveness probe.
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Serve embedded web UI.
	// Root "/" redirects to /dashboard; "/dashboard" serves the SPA index;
	// "/dashboard/assets/*" serves Vite build assets.
	uiFS := mcpweb.DistFS()
	uiHandler := http.FileServer(uiFS)
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/dashboard")
	})
	r.GET("/dashboard", func(c *gin.Context) {
		f, err := uiFS.Open("index.html")
		if err != nil {
			c.String(http.StatusNotFound, "dashboard not found")
			return
		}
		defer f.Close()
		stat, _ := f.Stat()
		c.DataFromReader(http.StatusOK, stat.Size(), "text/html; charset=utf-8", f, nil)
	})
	r.GET("/dashboard/assets/*filepath", func(c *gin.Context) {
		c.Request.URL.Path = "/assets" + c.Param("filepath")
		uiHandler.ServeHTTP(c.Writer, c.Request)
	})
	r.NoRoute(func(c *gin.Context) {
		staticErrorReply(c, http.StatusNotFound, "ROUTE_NOT_FOUND", "Route not found")
	})

	return r
}

// safeRecoveryWithWriter preserves a useful panic signal and stack without
// dumping the request headers, query string, body, or recovered value. Gin's
// default Recovery request dump only masks Authorization and can otherwise
// persist Cookies or API keys in diagnostic logs.
func safeRecoveryWithWriter(writer io.Writer) gin.HandlerFunc {
	logger := log.New(writer, "", log.LstdFlags)
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			public := publicerror.Classify(store.ErrRecoveryIntegrity)
			route := c.FullPath()
			if route == "" {
				route = "<unmatched>"
			}
			logger.Printf("http panic recovered method=%q route=%q correlation_id=%q\n%s",
				c.Request.Method, route, public.CorrelationID, debug.Stack())
			c.AbortWithStatusJSON(public.HTTPStatus, gin.H{
				"error": public.Message, "error_code": public.Code,
				"correlation_id": public.CorrelationID,
			})
		}()
		c.Next()
	}
}

// safeAccessLogger records only trusted route templates, never a raw path,
// query string, headers, body, or Gin error text.
func safeAccessLogger(writer io.Writer) gin.HandlerFunc {
	logger := log.New(writer, "", log.LstdFlags)
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "<unmatched>"
		}
		logger.Printf("http request client_ip=%q method=%q route=%q status=%d duration_ms=%d",
			c.ClientIP(), c.Request.Method, route, c.Writer.Status(), time.Since(started).Milliseconds())
	}
}

// serveWS handles the WebSocket upgrade handshake and registers the client with the hub.
func serveWS(hub *ws.Hub, upgrader websocket.Upgrader, c *gin.Context) {
	pid := c.Param("id")

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	client := &ws.Client{
		Hub:     hub,
		Conn:    conn,
		Send:    make(chan []byte, 256),
		Filters: map[string]bool{pid: true},
	}

	hub.RegisterClient(client)

	// Run read and write pumps in goroutines.
	go client.WritePump()
	go client.ReadPump()
}

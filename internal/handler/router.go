package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"github.com/ZoonChen/Maestro-MCP/internal/ws"
	mcpweb "github.com/ZoonChen/Maestro-MCP/web"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

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
) *gin.Engine {
	r := gin.Default()

	// Limit request body size to 1MB across all endpoints.
	r.Use(MaxBodySize(1 << 20))

	// CORS headers for cross-origin API consumers.
	r.Use(CORS())

	// Rate limiting: 100 requests per minute per IP.
	r.Use(RateLimit(100, time.Minute))

	// Apply Bearer token auth middleware (no-op if authToken is empty).
	r.Use(AuthMiddleware(authToken))

	// WebSocket upgrader with same-origin check.
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true
			}
			host := r.Host
			return strings.HasPrefix(origin, "http://"+host) || strings.HasPrefix(origin, "https://"+host)
		},
	}

	api := r.Group("/api/v1")
	{
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
			project.GET("/tasks/next", th.GetNextTask)
			project.GET("/tasks/next-verification", th.GetNextVerificationTask)
			project.GET("/tasks/:tid", th.GetTask)
			project.PATCH("/tasks/:tid", th.UpdateTask)
			project.POST("/tasks/:tid/claim", th.ClaimTask)
			project.POST("/tasks/:tid/submit", th.SubmitTask)
			project.POST("/tasks/:tid/block", th.BlockTask)
			project.POST("/tasks/:tid/resolve", th.ResolveBlocker)
			project.POST("/tasks/:tid/verify", th.VerifyTask)
			project.POST("/tasks/:tid/merge", th.MergeTask)
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
		c.JSON(http.StatusNotFound, gin.H{"error": "route not found", "path": c.Request.URL.Path})
	})

	return r
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

package api

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

//go:embed openapi.yml
var openapiSpec []byte

//go:embed all:resources
var frontendFS embed.FS

// Server represents the REST API server.
type Server struct {
	store              *store.Store
	encryptionKey      []byte
	logger             *slog.Logger
	httpServer         *http.Server
	rateLimiter        *RateLimiter
	authFailureTracker *authFailureTracker
	authCache          *cache.AuthCache
	config             *config.Config
}

// NewServer creates a new API server.
func NewServer(dataStore *store.Store, encryptionKey []byte, logger *slog.Logger, cfg *config.Config) *Server {
	var rateLimiter *RateLimiter
	var authCache *cache.AuthCache

	if cfg != nil {
		rateLimiter = NewRateLimiter(cfg.RateLimit)
		authCache = cache.NewAuthCache(cache.AuthCacheConfig{
			Enabled:    cfg.AuthCache.Enabled,
			TTLSeconds: cfg.AuthCache.TTLSeconds,
			MaxSize:    cfg.AuthCache.MaxSize,
		})
	}

	return &Server{
		store:              dataStore,
		encryptionKey:      encryptionKey,
		logger:             logger,
		rateLimiter:        rateLimiter,
		authFailureTracker: newAuthFailureTracker(),
		authCache:          authCache,
		config:             cfg,
	}
}

const (
	httpReadTimeout  = 15 * time.Second
	httpWriteTimeout = 15 * time.Second
	httpIdleTimeout  = 60 * time.Second
)

// Start starts the API server.
func (s *Server) Start(addr string) error {
	router := s.setupRouter()

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
		IdleTimeout:  httpIdleTimeout,
	}

	s.logger.InfoContext(context.Background(), "Starting API server", slog.String("addr", addr))

	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}

	return nil
}

// setupRouter configures the Gin router.
func (s *Server) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Disable automatic trailing slash redirect to avoid loops with SPA routing
	router.RedirectTrailingSlash = false

	// Middleware
	router.Use(gin.Recovery())
	router.Use(s.loggingMiddleware())

	// Documentation endpoints (not versioned)
	api := router.Group("/api")
	{
		api.GET("/openapi.yml", s.handleOpenAPISpec)
		api.GET("/docs", s.handleSwaggerUI)
		api.GET("/docs/*any", s.handleSwaggerUI)
	}

	// Versioned API endpoints
	v1 := router.Group("/api/v1")
	{
		// Health check and version info (unauthenticated)
		v1.GET("/health", s.handleHealth)
		v1.GET("/version", s.handleVersion)

		// Auth endpoints (login and pre-login password change are unauthenticated)
		auth := v1.Group("/auth")
		auth.POST("/login", s.handleLogin)
		auth.PUT("/password", s.handlePreLoginPasswordChange)

		// Password change endpoint uses credential auth from body (not Bearer token)
		v1.PUT("/users/:uid/password", s.handleChangePassword)

		// All other routes require authentication
		authenticated := v1.Group("")
		authenticated.Use(s.authMiddleware())
		// Add rate limiting after authentication (uses user ID for rate limiting)
		if s.rateLimiter != nil {
			authenticated.Use(s.rateLimiter.PostAuthMiddleware())
		}
		// Note: requirePasswordChanged middleware removed - users cannot login without
		// changing their password first (enforced at login time, not here)
		{
			// Authenticated auth endpoints
			authenticatedAuth := authenticated.Group("/auth")
			authenticatedAuth.POST("/logout", s.handleLogout)
			authenticatedAuth.GET("/me", s.handleMe)

			// User endpoints
			users := authenticated.Group("/users")
			users.POST("", s.requireAdmin(), s.handleCreateUser)
			users.GET("", s.handleListUsers) // Non-admins see only themselves
			users.GET("/:uid", s.handleGetUser)
			users.PUT("/:uid", s.handleUpdateUser)
			// Note: PUT /:uid/password is registered separately (uses credential auth, not Bearer)
			users.DELETE("/:uid", s.requireAdmin(), s.handleDeleteUser)

			// Database endpoints
			databases := authenticated.Group("/databases")
			databases.POST("", s.requireAdmin(), s.handleCreateDatabase)
			databases.GET("", s.handleListDatabases)
			databases.GET("/:uid", s.handleGetDatabase)
			databases.PUT("/:uid", s.requireAdmin(), s.handleUpdateDatabase)
			databases.DELETE("/:uid", s.requireAdmin(), s.handleDeleteDatabase)

			// Grant endpoints
			grants := authenticated.Group("/grants")
			grants.POST("", s.requireAdmin(), s.handleCreateGrant)
			grants.GET("", s.handleListGrants)
			grants.GET("/:uid", s.handleGetGrant)
			grants.DELETE("/:uid", s.requireAdmin(), s.handleRevokeGrant)

			// API Key endpoints
			keys := authenticated.Group("/keys")
			// Create and revoke require Web Session or Basic Auth (API keys cannot manage API keys)
			keys.POST("", s.requireWebSessionOrBasicAuth(), s.handleCreateAPIKey)
			keys.GET("", s.handleListAPIKeys)
			keys.GET("/:id", s.handleGetAPIKey)
			keys.DELETE("/:id", s.requireWebSessionOrBasicAuth(), s.handleRevokeAPIKey)

			// Observability endpoints
			// Connections: admin/viewer see all, connector sees own only (filtered in handler)
			authenticated.GET("/connections", s.handleListConnections)
			// Queries: admin/viewer only
			authenticated.GET("/queries", s.requireAdminOrViewer(), s.handleListQueries)
			authenticated.GET("/queries/:uid", s.requireAdminOrViewer(), s.handleGetQuery)
			authenticated.GET("/queries/:uid/rows", s.requireAdminOrViewer(), s.handleGetQueryRows)
			// Audit: admin/viewer only
			authenticated.GET("/audit", s.requireAdminOrViewer(), s.handleListAudit)
		}
	}

	// Frontend routes - serve the SPA (must be registered last when using NoRoute)
	s.setupFrontendRoutes(router)

	return router
}

// handleHealth returns the health status.
func (s *Server) handleHealth(c *gin.Context) {
	if err := s.store.Health(c.Request.Context()); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database unhealthy"})

		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "healthy"})
}

// handleVersion returns API and build version information.
func (s *Server) handleVersion(c *gin.Context) {
	runMode := ""
	if s.config != nil {
		runMode = string(s.config.RunMode)
	}

	c.JSON(http.StatusOK, gin.H{
		"api_version":   "v1",
		"build_version": version.Version,
		"build_commit":  version.Commit,
		"build_time":    version.GitTime,
		"run_mode":      runMode,
	})
}

// handleOpenAPISpec serves the OpenAPI specification.
func (s *Server) handleOpenAPISpec(c *gin.Context) {
	c.Data(http.StatusOK, "application/x-yaml", openapiSpec)
}

// handleSwaggerUI serves the Swagger UI HTML page.
func (s *Server) handleSwaggerUI(c *gin.Context) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>DBBat API Documentation</title>
    <link rel="stylesheet" type="text/css" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
    <script>
        window.onload = function() {
            SwaggerUIBundle({
                url: "/api/openapi.yml",
                dom_id: '#swagger-ui',
                presets: [
                    SwaggerUIBundle.presets.apis,
                    SwaggerUIBundle.SwaggerUIStandalonePreset
                ],
                layout: "BaseLayout"
            });
        };
    </script>
</body>
</html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// loggingMiddleware logs HTTP requests.
func (s *Server) loggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		statusCode := c.Writer.Status()

		s.logger.InfoContext(c.Request.Context(), "API request",
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.String("query", query),
			slog.Int("status", statusCode),
			slog.Duration("latency", latency),
			slog.String("client_ip", c.ClientIP()),
		)
	}
}

// errorResponse sends an error response.
func errorResponse(c *gin.Context, code int, message string) {
	c.JSON(code, gin.H{"error": message})
}

// successResponse sends a success response.
func successResponse(c *gin.Context, data any) {
	c.JSON(http.StatusOK, data)
}

// Helper to get current user from context.
func getCurrentUser(c *gin.Context) *store.User {
	user, exists := c.Get("current_user")
	if !exists {
		return nil
	}

	u, ok := user.(*store.User)
	if !ok {
		return nil
	}

	return u
}

// parseUIDParam parses a UID parameter from the URL.
func parseUIDParam(c *gin.Context) (uuid.UUID, error) {
	uid, err := uuid.Parse(c.Param("uid"))
	if err != nil {
		return uuid.Nil, ErrInvalidUID
	}

	return uid, nil
}

// setupFrontendRoutes configures routes to serve the frontend SPA.
func (s *Server) setupFrontendRoutes(router *gin.Engine) {
	// Strip the "resources" prefix from the embedded FS
	frontendContent, err := fs.Sub(frontendFS, "resources")
	if err != nil {
		s.logger.ErrorContext(context.Background(), "Failed to create sub-filesystem for frontend", slog.Any("error", err))
		return
	}

	// Pre-read index.html for SPA fallback
	indexHTML, err := fs.ReadFile(frontendContent, "index.html")
	if err != nil {
		s.logger.ErrorContext(context.Background(), "Failed to read index.html", slog.Any("error", err))
		return
	}

	// Determine base URL (defaults to "/app")
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	// Build redirect lookup map for quick matching
	redirects := s.buildRedirectMap()

	// Redirect root to base URL
	router.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, baseURL+"/")
	})

	// Serve all routes under base URL
	router.GET(baseURL+"/*filepath", func(c *gin.Context) {
		requestPath := c.Request.URL.Path

		// Check for dev redirect
		if rule := s.matchRedirect(redirects, requestPath); rule != nil {
			s.proxyToDevServer(c, rule, requestPath)
			return
		}

		// Serve static file or fall back to index.html
		s.serveStaticOrIndex(c, frontendContent, indexHTML, baseURL, requestPath)
	})
}

// buildRedirectMap builds a map of path prefixes to redirect rules.
func (s *Server) buildRedirectMap() map[string]*config.RedirectRule {
	if s.config == nil || len(s.config.Redirects) == 0 {
		return nil
	}

	redirects := make(map[string]*config.RedirectRule, len(s.config.Redirects))
	for i := range s.config.Redirects {
		r := &s.config.Redirects[i]
		redirects[r.PathPrefix] = r
	}

	return redirects
}

// matchRedirect finds a matching redirect rule for the given path.
func (s *Server) matchRedirect(redirects map[string]*config.RedirectRule, path string) *config.RedirectRule {
	if redirects == nil {
		return nil
	}

	// Check for exact prefix match or path under the prefix
	for prefix, rule := range redirects {
		if path == prefix || strings.HasPrefix(path, prefix+"/") || strings.HasPrefix(path, prefix+"?") {
			return rule
		}
	}

	return nil
}

// proxyToDevServer proxies the request to a dev server.
func (s *Server) proxyToDevServer(c *gin.Context, rule *config.RedirectRule, originalPath string) {
	// Build the new path by replacing the matched prefix with the target path
	suffix := strings.TrimPrefix(originalPath, rule.PathPrefix)
	newPath := rule.TargetPath + suffix
	// Clean up double slashes (e.g., "//" -> "/")
	if strings.HasPrefix(newPath, "//") {
		newPath = newPath[1:]
	}

	s.logger.DebugContext(c.Request.Context(), "Proxying request to dev server",
		slog.String("originalPath", originalPath),
		slog.String("targetHost", rule.TargetHost),
		slog.String("newPath", newPath),
	)

	targetURL := &url.URL{
		Scheme: "http",
		Host:   rule.TargetHost,
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Modify the request to use the new path
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.URL.Path = newPath
		r.URL.RawPath = newPath
	}

	// Handle WebSocket upgrades
	proxy.ModifyResponse = func(_ *http.Response) error {
		return nil
	}

	proxy.ServeHTTP(c.Writer, c.Request)
}

// serveStaticOrIndex serves a static file or falls back to index.html for SPA routing.
func (s *Server) serveStaticOrIndex(c *gin.Context, frontendContent fs.FS, indexHTML []byte, baseURL, requestPath string) {
	// Calculate the file path relative to the base URL
	filePath := requestPath
	if baseURL != "" {
		filePath = strings.TrimPrefix(requestPath, baseURL)
	}

	// Remove leading slash for file lookup
	filePath = strings.TrimPrefix(filePath, "/")

	// Try to serve static file first
	if filePath != "" {
		if data, err := fs.ReadFile(frontendContent, filePath); err == nil {
			contentType := getContentType(filePath)
			c.Data(http.StatusOK, contentType, data)
			return
		}
	}

	// Fall back to index.html for SPA routing
	c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
}

// getContentType returns the content type based on file extension.
func getContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(path, ".woff"):
		return "font/woff"
	case strings.HasSuffix(path, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

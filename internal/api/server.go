package api

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/auth"
	"github.com/fclairamb/dbbat/internal/auth/oidc"
	"github.com/fclairamb/dbbat/internal/auth/slack"
	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/mcp"
	"github.com/fclairamb/dbbat/internal/notify"
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
	oauthProviders     map[string]auth.OAuthProvider
	// notifier is the outbound Slack client; nil when notifications are
	// disabled (no bot token configured).
	notifier *notify.SlackNotifier
	// socketCancel stops the Slack Socket Mode connection on shutdown; nil
	// when Socket Mode is not running.
	socketCancel context.CancelFunc

	// broker fans live events to /api/v1/stream subscribers.
	broker *events.Broker
	// approvals wakes proxy sessions parked on an approval decision that
	// happen to live on this replica. Decisions for sessions on other
	// replicas travel over LISTEN/NOTIFY.
	approvals *approval.Registry
	// approvalNotifier updates a posted Slack escalation in place when a hold
	// resolves. nil when Slack is not configured.
	approvalNotifier approvalEscalator
	// listenCancel stops the cross-replica LISTEN loop on shutdown.
	listenCancel context.CancelFunc

	// dumpStorage reads back session captures that have been uploaded to blob
	// storage. nil when DBB_DUMP_UPLOAD_URL is unset, in which case captures
	// only ever exist in the local spool.
	dumpStorage *dump.Uploader

	// mcp serves the Model Context Protocol endpoint. Built in setupRouter,
	// nil when DBB_MCP_ENABLED is false — in which case the routes are not
	// registered either.
	mcp *mcp.Server

	// chainVerify bounds GET /audit/verify: a chain walk is O(rows), so its
	// outcome is cached and only one walk runs at a time.
	chainVerify *chainVerifyCache
}

// SetDumpStorage installs the blob store holding uploaded session captures, so
// downloads can fall back to it when the capture is no longer (or never was) in
// this replica's local spool. Called by the process wiring; nil is fine and
// means local-only captures.
func (s *Server) SetDumpStorage(uploader *dump.Uploader) {
	s.dumpStorage = uploader
}

// SetEventPlumbing installs the shared broker and approval registry. Called by
// the process wiring so the API and the proxies publish into — and resolve
// against — the same instances.
func (s *Server) SetEventPlumbing(broker *events.Broker, registry *approval.Registry, notifier approvalEscalator) {
	s.broker = broker
	s.approvals = registry
	s.approvalNotifier = notifier
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

		// Share auth cache with store for API key verification
		dataStore.SetAuthCache(authCache)
	}

	// Initialize OAuth providers
	oauthProviders := make(map[string]auth.OAuthProvider)
	if cfg != nil && cfg.SlackAuth.Enabled() {
		oauthProviders["slack"] = slack.NewProvider(
			cfg.SlackAuth.ClientID,
			cfg.SlackAuth.ClientSecret,
			cfg.SlackAuth.TeamID,
		)
		logger.InfoContext(context.Background(), "Slack OAuth provider enabled")
	}

	if cfg != nil && cfg.OIDCAuth.Enabled() {
		// Construction does no network I/O — discovery is lazy — so an
		// unreachable IdP delays the first login, it does not block startup.
		provider, err := oidc.NewProvider(oidc.Config{
			Issuer:       cfg.OIDCAuth.Issuer,
			ClientID:     cfg.OIDCAuth.ClientID,
			ClientSecret: cfg.OIDCAuth.ClientSecret,
			Scopes:       cfg.OIDCAuth.ScopeList(),
			Label:        cfg.OIDCAuth.DisplayName,
			EmailDomains: cfg.OIDCAuth.EmailDomainList(),
			GroupsClaim:  cfg.OIDCAuth.GroupsClaimName(),
		})
		if err != nil {
			logger.ErrorContext(context.Background(), "OIDC provider misconfigured", slog.Any("error", err))
		} else {
			oauthProviders[oidc.ProviderName] = provider
			logger.InfoContext(context.Background(), "OIDC provider enabled",
				slog.String("issuer", cfg.OIDCAuth.Issuer),
				slog.String("display_name", provider.DisplayName()))
		}
	}

	// Initialize Slack notifier (outbound only — distinct from OAuth above)
	var notifier *notify.SlackNotifier
	if cfg != nil {
		var err error
		// Prefer the operator-editable public.web_ui_url global parameter
		// over the raw env var, so an admin can point Slack deep-links at
		// dbbat.company.com from the Settings page without redeploying.
		// Resolved once at startup, not live-updated without a restart.
		webUIURL := dataStore.ResolveWebUIURL(context.Background(), cfg)
		notifier, err = notify.NewSlackNotifier(cfg.SlackNotify, webUIURL, dataStore, logger)
		if err != nil {
			// Misconfiguration — log loudly but don't crash the server.
			// The deployer can fix the env vars without losing the service.
			logger.ErrorContext(context.Background(), "slack notifications misconfigured", slog.Any("error", err))
		}
	}

	return &Server{
		// A broker and registry always exist so handlers never nil-check; the
		// process wiring replaces them with the shared instances.
		broker:             events.New(),
		approvals:          approval.NewRegistry(),
		store:              dataStore,
		encryptionKey:      encryptionKey,
		logger:             logger,
		rateLimiter:        rateLimiter,
		authFailureTracker: newAuthFailureTracker(),
		authCache:          authCache,
		config:             cfg,
		oauthProviders:     oauthProviders,
		notifier:           notifier,
		chainVerify:        newChainVerifyCache(),
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

	// Subscribe to cross-replica events so a hold on another pod reaches the
	// admins connected here.
	s.StartEventListener(context.Background())

	// Start the outbound Slack Socket Mode connection (no-op unless an
	// app-level token is configured). Runs for the server's lifetime.
	s.startSocketMode()

	// When interactivity rides on the inbound HTTP endpoint alone, surface
	// the Slack-side reachability requirement in the startup logs.
	s.logSlackInteractivityTransport()

	s.logger.InfoContext(context.Background(), "Starting API server", slog.String("addr", addr))

	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.socketCancel != nil {
		s.socketCancel()
	}

	if s.listenCancel != nil {
		s.listenCancel()
	}

	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}

	return nil
}

// setupRouter configures the Gin router.
//
//nolint:funlen // route registration is intentionally sequential and grouped by resource
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
		auth.GET("/providers", s.handleAuthProviders)

		// OAuth provider routes (unauthenticated)
		for name := range s.oauthProviders {
			auth.GET("/"+name, s.handleOAuthAuthorize(name))
			auth.GET("/"+name+"/callback", s.handleOAuthCallback(name))
		}

		// One-time code -> web session token exchange. The OAuth callback
		// hands the browser a short-lived single-use code instead of the
		// session token, so no session credential ever appears in a URL; the
		// SPA redeems it here. Unauthenticated (the code is the credential),
		// IP rate limited like the other pre-auth endpoints. Registered
		// unconditionally so the route surface does not depend on which
		// providers are configured.
		if s.rateLimiter != nil {
			auth.POST("/oauth/exchange", s.rateLimiter.PreAuthMiddleware(), s.handleOAuthExchange)
		} else {
			auth.POST("/oauth/exchange", s.handleOAuthExchange)
		}

		// OAuth 2.0 Device Authorization Grant (RFC 8628): lets external
		// tools (CLI, desktop apps) obtain an API key through a browser
		// approval instead of manual copy/paste. The authorization request
		// endpoint is IP rate limited like other unauthenticated auth
		// endpoints; the token (poll) endpoint deliberately is not — see
		// handleDeviceToken for why.
		if s.rateLimiter != nil {
			auth.POST("/device", s.rateLimiter.PreAuthMiddleware(), s.handleDeviceAuthorization)
		} else {
			auth.POST("/device", s.handleDeviceAuthorization)
		}
		auth.POST("/device/token", s.handleDeviceToken)

		// Slack interactivity webhook (unauthenticated at the middleware
		// layer — the Slack request signature is the authentication). Only
		// registered when interactivity is configured (signing secret or
		// Socket Mode app token set), so notification-only deployments don't
		// expose an inbound endpoint. Under Socket Mode without a signing
		// secret the handler rejects every request with 401.
		if s.notifier.Interactive() {
			v1.POST("/slack/interactions", s.handleSlackInteraction)
		}

		// Password change endpoint uses credential auth from body (not Bearer token)
		v1.PUT("/users/:uid/password", s.handleChangePassword)

		// All other routes require authentication
		authenticated := v1.Group("")
		// Browsers can't set an Authorization header on a WebSocket handshake,
		// so /stream carries its bearer token in Sec-WebSocket-Protocol; this
		// promotes it before the normal auth middleware runs.
		authenticated.Use(s.streamAuthMiddleware())
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
			// Device authorization consent page + decision. GET is any
			// authenticated user (the page is behind login); POST mints a
			// real API key, so it requires Web Session or Basic Auth, like
			// direct key creation.
			authenticatedAuth.GET("/device/consent", s.handleGetDeviceConsent)
			authenticatedAuth.POST("/device/consent", s.requireWebSessionOrBasicAuth(), s.handleDeviceConsent)

			// User endpoints
			users := authenticated.Group("/users")
			users.POST("", s.requireAdmin(), s.handleCreateUser)
			users.GET("", s.handleListUsers) // Non-admins see only themselves
			users.GET("/:uid", s.handleGetUser)
			users.PUT("/:uid", s.handleUpdateUser)
			// Note: PUT /:uid/password is registered separately (uses credential auth, not Bearer)
			users.DELETE("/:uid", s.requireAdmin(), s.handleDeleteUser)
			// Admin password reset (requires web session, not API key)
			users.POST("/:uid/reset-password", s.requireAdmin(), s.handleResetPassword)

			// User group endpoints — organizational groupings used to scope
			// grant definitions. Admin-only: membership is access-relevant.
			userGroups := authenticated.Group("/user-groups")
			userGroups.POST("", s.requireAdmin(), s.handleCreateUserGroup)
			userGroups.GET("", s.requireAdmin(), s.handleListUserGroups)
			userGroups.GET("/:uid", s.requireAdmin(), s.handleGetUserGroup)
			userGroups.PATCH("/:uid", s.requireAdmin(), s.handleUpdateUserGroup)
			userGroups.DELETE("/:uid", s.requireAdmin(), s.handleDeleteUserGroup)
			userGroups.GET("/:uid/members", s.requireAdmin(), s.handleListUserGroupMembers)
			userGroups.PUT("/:uid/members/:user_uid", s.requireAdmin(), s.handleAddUserGroupMember)
			userGroups.DELETE("/:uid/members/:user_uid", s.requireAdmin(), s.handleRemoveUserGroupMember)

			// Server group endpoints — named sets of database servers, the unit
			// rights are scoped on. Admin-only for the same reason user groups
			// are: membership is access-relevant, and here it is *live* — see
			// handleUpdateServerGroup.
			serverGroups := authenticated.Group("/server-groups")
			serverGroups.POST("", s.requireAdmin(), s.handleCreateServerGroup)
			serverGroups.GET("", s.requireAdmin(), s.handleListServerGroups)
			serverGroups.GET("/:uid", s.requireAdmin(), s.handleGetServerGroup)
			serverGroups.PATCH("/:uid", s.requireAdmin(), s.handleUpdateServerGroup)
			serverGroups.DELETE("/:uid", s.requireAdmin(), s.handleDeleteServerGroup)
			serverGroups.GET("/:uid/members", s.requireAdmin(), s.handleListServerGroupMembers)
			serverGroups.PUT("/:uid/members/:server_uid", s.requireAdmin(), s.handleAddServerGroupMember)
			serverGroups.DELETE("/:uid/members/:server_uid", s.requireAdmin(), s.handleRemoveServerGroupMember)

			// Server endpoints. The table now holds SSH bastions too (protocol
			// 'ssh'); the route is /servers. Database *targets* are listed here
			// (SSH rows are excluded by the store's targets-only scope).
			databases := authenticated.Group("/servers")
			databases.POST("", s.requireAdmin(), s.handleCreateDatabase)
			databases.GET("", s.handleListDatabases)
			databases.GET("/:uid", s.handleGetDatabase)
			databases.PUT("/:uid", s.requireAdmin(), s.handleUpdateDatabase)
			databases.DELETE("/:uid", s.requireAdmin(), s.handleDeleteDatabase)
			databases.GET("/:uid/connection", s.handleGetDatabaseConnection)
			// Provisioning-time connectivity validation (admin): dial the row for
			// real rather than trusting that it was typed correctly.
			databases.POST("/:uid/test", s.requireAdmin(), s.handleTestServerConnection)

			// SSH bastion management (admin). Kept on a separate path because a
			// static /servers/ssh segment would conflict with /servers/:uid.
			sshServers := authenticated.Group("/ssh-servers")
			sshServers.GET("", s.requireAdmin(), s.handleListSSHServers)

			// Every dial-path row (ssh bastions + kubernetes clusters), for the
			// target form's "via" selector.
			tunnelServers := authenticated.Group("/tunnel-servers")
			tunnelServers.GET("", s.requireAdmin(), s.handleListTunnelServers)

			// Grant endpoints
			grants := authenticated.Group("/grants")
			grants.POST("", s.requireAdmin(), s.handleAssignGrant)
			grants.GET("", s.handleListGrants)
			grants.GET("/:uid", s.handleGetGrant)
			grants.DELETE("/:uid", s.requireAdmin(), s.handleRevokeGrant)

			// Grant definition endpoints — admin-managed templates that
			// bound the shapes a user is allowed to request via the grant
			// request workflow. Read access is open to any authenticated
			// user so the request UI can populate its definition picker.
			grantDefs := authenticated.Group("/grant-definitions")
			grantDefs.POST("", s.requireAdmin(), s.handleCreateGrantDefinition)
			grantDefs.GET("", s.handleListGrantDefinitions)
			// validate-patterns is a pure compute endpoint (no store access,
			// no :uid to resolve) for previewing approval patterns against
			// sample queries before a definition is saved. Registered ahead
			// of GET/PATCH/DELETE /:uid on a different HTTP method (POST),
			// so it never competes with the :uid param route.
			grantDefs.POST("/validate-patterns", s.requireAdmin(), s.handleValidateGrantDefinitionPatterns)
			grantDefs.GET("/:uid", s.handleGetGrantDefinition)
			grantDefs.PATCH("/:uid", s.requireAdmin(), s.handleUpdateGrantDefinition)
			grantDefs.DELETE("/:uid", s.requireAdmin(), s.handleDeactivateGrantDefinition)

			// Grant request endpoints — user self-service workflow.
			// Approve/deny require admin; cancel is open to the requester
			// (handler enforces ownership).
			grantReqs := authenticated.Group("/grant-requests")
			grantReqs.POST("", s.handleCreateGrantRequest)
			grantReqs.GET("", s.handleListGrantRequests)
			grantReqs.GET("/:uid", s.handleGetGrantRequest)
			grantReqs.POST("/:uid/approve", s.requireAdmin(), s.handleApproveGrantRequest)
			grantReqs.POST("/:uid/deny", s.requireAdmin(), s.handleDenyGrantRequest)
			grantReqs.POST("/:uid/cancel", s.handleCancelGrantRequest)

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
			authenticated.GET("/connections/:uid", s.handleGetConnection)
			// Capture download is admin-only (narrowed from admin-or-viewer):
			// the raw pcapng is byte-fidelity beyond what a viewer sees on the
			// queries pages, including the auth handshake and every result row.
			authenticated.GET("/connections/:uid/dump", s.requireAdmin(), s.handleGetConnectionDump)
			authenticated.DELETE("/connections/:uid/dump", s.requireAdmin(), s.handleDeleteConnectionDump)
			// Live event stream (WebSocket). Per-topic authorization happens
			// inside the handler and is re-checked on every send, so no role
			// middleware here — a connector may watch their own connection.
			authenticated.GET("/stream", s.handleStream)

			// Approval holds. Deliberately *not* behind requireAdmin: an
			// approver-group member who is neither admin nor viewer must be
			// able to resolve the holds they are responsible for. Each handler
			// does the real check (and always rejects self-approval).
			authenticated.GET("/queries/pending", s.handleListPendingApprovals)
			authenticated.POST("/queries/pending/deny-all", s.requireAdmin(), s.handleDenyAllPending)
			authenticated.POST("/queries/:uid/approve", s.handleApproveQuery)
			authenticated.POST("/queries/:uid/deny", s.handleDenyQuery)

			// Queries: admin/viewer only
			authenticated.GET("/queries", s.requireAdminOrViewer(), s.handleListQueries)
			authenticated.GET("/queries/:uid", s.requireAdminOrViewer(), s.handleGetQuery)
			authenticated.GET("/queries/:uid/rows", s.requireAdminOrViewer(), s.handleGetQueryRows)
			// Audit: admin/viewer only
			authenticated.GET("/audit", s.requireAdminOrViewer(), s.handleListAudit)
			// Tamper-evidence verification. Admin only — narrower than the
			// audit list a viewer may read, because this is the control
			// evidence itself and running it costs a full chain walk. See
			// internal/api/audit_verify.go for the trust caveat: this answer
			// is only as good as the process serving it, which is why
			// `dbbat audit verify` stays the authoritative check.
			authenticated.GET("/audit/verify", s.requireAdmin(), s.handleVerifyAuditChain)
			authenticated.GET("/audit/verify/queries", s.requireAdmin(), s.handleVerifyQueryChains)
			authenticated.GET("/audit/verify/rows", s.requireAdmin(), s.handleVerifyRowChains)

			// Global parameters (admin-only CRUD; GET /instance open to any authenticated user)
			params := authenticated.Group("/parameters")
			params.GET("", s.requireAdmin(), s.handleListParameters)
			params.GET("/:group/:key", s.requireAdmin(), s.handleGetParameter)
			params.PUT("/:group/:key", s.requireAdmin(), s.handleSetParameter)
			params.DELETE("/:group/:key", s.requireAdmin(), s.handleDeleteParameter)

			// Instance info
			authenticated.GET("/instance", s.handleGetInstance)
			authenticated.PUT("/instance/public", s.requireAdmin(), s.handleUpdateInstancePublic)

			// Model Context Protocol endpoint (Streamable HTTP), for AI
			// agents. Registered only when enabled: a disabled feature should
			// not answer at all. GET and DELETE exist so an MCP client's
			// session-management probes get the protocol's own 405 rather than
			// a 404 it has to guess at — the server runs stateless.
			if s.mcpEnabled() {
				s.mcp = s.newMCPServer()
				authenticated.POST("/mcp", s.handleMCP)
				authenticated.GET("/mcp", s.handleMCP)
				authenticated.DELETE("/mcp", s.handleMCP)
			}
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

// llmsTxtTemplate is the body of the instance-local GET /llms.txt endpoint
// (see the llms.txt convention at https://llmstxt.org/). It intentionally
// omits listen addresses, public endpoints, and enabled proxies — this route
// is unauthenticated and must not become a side channel for instance
// topology (that's what the authenticated GET /api/v1/instance is for).
const llmsTxtTemplate = `# DBBat (this instance)

> Transparent database proxy for PostgreSQL, Oracle, MySQL/MariaDB and MongoDB.
> Every query logged, every connection tracked, access granted for a limited time.

This is a running DBBat instance, version %s.

## This instance
- [Web UI](%s/)
- [OpenAPI spec](/api/openapi.yml)
- [API reference (Swagger UI)](/api/docs)
- [Version info](/api/v1/version)

## Documentation
- [Full documentation index](https://dbbat.com/llms.txt)
- [Full documentation text](https://dbbat.com/llms-full.txt)
- [Website](https://dbbat.com)
`

// handleLLMsTxt serves a small instance-local llms.txt so an agent (or a
// human) pointed at a running DBBat instance gets a clean doc map instead of
// a 404 or having to crawl the SPA. Deliberately not a redirect to
// dbbat.com: instances are typically VPN-gated with restricted outbound
// internet, so a redirect can resolve to a timeout that looks like it
// worked. See specs/todos/2026-08-02-02-llms-txt.md for the rationale.
func (s *Server) handleLLMsTxt(c *gin.Context) {
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	body := fmt.Sprintf(llmsTxtTemplate, version.Version, baseURL)
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(body))
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
		query := redactQuery(c.Request.URL.RawQuery)

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

// redactedValue replaces the value of a query parameter that is not on the
// access log's allowlist. The parameter name is kept so the log still shows
// the shape of the request — the names are not the secret, the values are.
const redactedValue = "REDACTED"

// loggableQueryParams is an ALLOWLIST: the only query parameter names whose
// *values* may reach the access log. Everything else is redacted by default.
//
// This is deliberately an allowlist and not a denylist. A denylist enumerating
// known-sensitive names (`token`, `code`, `state`, …) fails silently and late:
// whoever later adds a `?reset_token=`, `?otp=`, `?invite=` or `?signature=`
// gets it written to the access log verbatim, and neither review nor CI points
// at the omission. Inverted, a new parameter is redacted until someone with the
// context consciously declares it safe. **Do not turn this back into a
// denylist** — TestRedactQueryDefaultsToRedacted pins the direction.
//
// Adding an entry here is a one-line change; the bar is that the value is a
// bounded, non-secret, non-caller-controlled-free-text token: an enum, a
// boolean, a number, a timestamp, or an opaque entity UID. It is NOT enough
// that a value "looks harmless today" — `redirect` is excluded precisely
// because it carries an arbitrary caller-supplied path that the device flow
// populates with `/device?user_code=...`, i.e. a secret nested inside an
// otherwise innocuous parameter.
//
// Matched case-insensitively, after percent-decoding the name.
var loggableQueryParams = map[string]bool{
	// Pagination and cursoring.
	"before":   true,
	"cursor":   true,
	"limit":    true,
	"offset":   true,
	"order":    true,
	"page":     true,
	"per_page": true,
	"sort":     true,

	// Boolean and enum filters.
	"active_only": true,
	"all_users":   true,
	"event_type":  true,
	"hard":        true,
	"include_all": true,
	"protocol":    true,
	"status":      true,

	// Opaque entity identifiers (UUIDs / config group keys), not credentials.
	"connection_id": true,
	"database_id":   true,
	"group_key":     true,
	"performed_by":  true,
	"user_id":       true,

	// Time-range filters.
	"end_time":   true,
	"start_time": true,

	// OAuth failure code — a short enumerated value ("access_denied") that is
	// the whole reason to look at these logs. Its free-text sibling
	// `error_description` is intentionally absent.
	"error": true,
}

// redactQuery rewrites a raw query string, keeping the value of allowlisted
// parameters and replacing every other value with redactedValue. Parameter
// names are always preserved so a request stays diagnosable.
//
// It works on the raw string rather than url.Values so an unparseable query
// (which url.ParseQuery would partially drop) is still redacted rather than
// logged verbatim, and so parameter order is preserved.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}

	pairs := strings.Split(raw, "&")
	for i, pair := range pairs {
		name, _, hasValue := strings.Cut(pair, "=")

		if loggableQueryName(name) {
			continue
		}

		if !hasValue {
			// A bare segment with no "=" is all name and no value, so there is
			// no key worth preserving; it could itself be a pasted secret.
			pairs[i] = redactedValue
			continue
		}

		pairs[i] = name + "=" + redactedValue
	}

	return strings.Join(pairs, "&")
}

// loggableQueryName reports whether a raw (still percent-encoded) parameter
// name is on the allowlist. A name that will not decode is never allowlisted:
// an attacker must not be able to smuggle a value past the filter by encoding
// the name it rides on.
func loggableQueryName(rawName string) bool {
	decoded, err := url.QueryUnescape(rawName)
	if err != nil {
		return false
	}

	return loggableQueryParams[strings.ToLower(decoded)]
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
	// Determine base URL (defaults to "/app")
	baseURL := "/app"
	if s.config != nil && s.config.BaseURL != "" {
		baseURL = s.config.BaseURL
	}

	// Build redirect lookup map for quick matching
	redirects := s.buildRedirectMap()

	// Try to load embedded frontend assets (optional — dev mode uses redirects instead)
	var frontendContent fs.FS
	var indexHTML []byte

	if sub, err := fs.Sub(frontendFS, "resources"); err != nil {
		s.logger.ErrorContext(context.Background(), "Failed to create sub-filesystem for frontend", slog.Any("error", err))
	} else if html, err := fs.ReadFile(sub, "index.html"); err != nil {
		s.logger.ErrorContext(context.Background(), "Failed to read index.html", slog.Any("error", err))
	} else {
		frontendContent = sub
		indexHTML = html
	}

	// Redirect root to base URL
	router.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, baseURL+"/")
	})

	// llms.txt (https://llmstxt.org/) — unauthenticated, always a local 200,
	// never a redirect to dbbat.com (see handleLLMsTxt).
	router.GET("/llms.txt", s.handleLLMsTxt)

	// Serve all routes under base URL
	router.GET(baseURL+"/*filepath", func(c *gin.Context) {
		requestPath := c.Request.URL.Path

		// Check for dev redirect (takes priority over embedded assets)
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

// Notifier exposes the server's Slack client so the process wiring can build
// the approval escalator on top of it instead of opening a second connection.
// nil when Slack notifications are disabled.
func (s *Server) Notifier() *notify.SlackNotifier {
	return s.notifier
}

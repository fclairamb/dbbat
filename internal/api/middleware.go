package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// Auth context keys
const (
	contextKeyUser       = "current_user"
	contextKeyAPIKey     = "api_key"
	contextKeyAuthMethod = "auth_method"
	authMethodBasic      = "basic"
	authMethodAPIKey     = "api_key"
	authMethodWebSession = "web_session"
)

// authMiddleware validates Basic Auth or Bearer token credentials
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")

		// Try Bearer token first
		if strings.HasPrefix(authHeader, "Bearer ") {
			s.handleBearerAuth(c, strings.TrimPrefix(authHeader, "Bearer "))
			return
		}

		// Fall back to Basic Auth
		s.handleBasicAuth(c)
	}
}

// handleBearerAuth handles API key authentication
func (s *Server) handleBearerAuth(c *gin.Context, token string) {
	ctx := c.Request.Context()

	// Verify the API key
	apiKey, err := s.store.VerifyAPIKey(ctx, token)
	if err != nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "invalid API key")
		c.Abort()
		return
	}

	// Get the user associated with the API key
	user, err := s.store.GetUserByUID(ctx, apiKey.UserID)
	if err != nil {
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "user not found")
		c.Abort()
		return
	}

	// Update API key usage (async to not block the request). The helper detaches
	// the write from the request context, which is canceled the moment this
	// handler returns — the bump used to race that and usually lose.
	shared.BumpAPIKeyUsage(ctx, s.logger, s.store, apiKey.ID)

	// Determine auth method based on key type
	authMethod := authMethodAPIKey
	if apiKey.IsWebSession() {
		authMethod = authMethodWebSession
	}

	// Store user and auth method in context
	c.Set(contextKeyUser, user)
	c.Set(contextKeyAPIKey, apiKey)
	c.Set(contextKeyAuthMethod, authMethod)
	c.Next()
}

// handleBasicAuth handles Basic Auth authentication
func (s *Server) handleBasicAuth(c *gin.Context) {
	username, password, ok := c.Request.BasicAuth()
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="DBBat"`)
		writeError(c, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required")
		c.Abort()
		return
	}

	// Check rate limit BEFORE verifying credentials (fail fast)
	if !s.checkAuthThrottle(c, username) {
		c.Abort()
		return
	}

	// Look up user
	user, err := s.store.GetUserByUsername(c.Request.Context(), username)
	if err != nil {
		// Record the failure AFTER credential verification fails; past the
		// username ceiling this is a 429, as for POST /auth/login.
		c.Header("WWW-Authenticate", `Basic realm="DBBat"`)
		s.failAuth(c, username, "invalid credentials")
		c.Abort()
		return
	}

	// Verify password (using cache if available)
	var valid bool
	if s.authCache != nil {
		valid, err = s.authCache.VerifyPassword(c.Request.Context(), user.UID.String(), password, user.PasswordHash)
	} else {
		// Fallback to direct verification if cache not initialized
		valid, err = verifyPasswordDirect(user.PasswordHash, password)
	}
	if err != nil || !valid {
		// Record the failure AFTER credential verification fails; past the
		// username ceiling this is a 429, as for POST /auth/login.
		c.Header("WWW-Authenticate", `Basic realm="DBBat"`)
		s.failAuth(c, username, "invalid credentials")
		c.Abort()
		return
	}

	// Reset failure count on successful login; a success during a guessing
	// burst is audited and lifts the username ceiling, as for POST /auth/login.
	s.noteAuthSuccess(c, username)

	// Same rule as POST /auth/login: a password that was never changed — the
	// seeded admin/admin, or one an admin handed out — opens nothing until its
	// owner changes it through PUT /auth/password. Basic Auth is a login on
	// every request, so it cannot be the way around that.
	if !user.HasChangedPassword() {
		writeError(c, http.StatusForbidden, ErrCodePasswordChangeRequired, "You must change your password before logging in")
		c.Abort()
		return
	}

	// Store user in context
	c.Set(contextKeyUser, user)
	c.Set(contextKeyAuthMethod, authMethodBasic)
	c.Next()
}

// requireRole returns a middleware that ensures the user has the specified role
func (s *Server) requireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := getCurrentUser(c)
		if user == nil || !user.HasRole(role) {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, role+" access required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireAdmin middleware ensures the user has the admin role
func (s *Server) requireAdmin() gin.HandlerFunc {
	return s.requireRole("admin")
}

// requireAdminOrViewer middleware ensures the user has either admin or viewer role
func (s *Server) requireAdminOrViewer() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := getCurrentUser(c)
		if user == nil || (!user.IsAdmin() && !user.IsViewer()) {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "admin or viewer access required")
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireBasicAuth middleware ensures the request uses Basic Auth (not API key or web session)
// This is used for sensitive operations that should not be performed with any token.
//
//nolint:unused // Reserved for future use in sensitive endpoints
func (s *Server) requireBasicAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authMethod := getAuthMethod(c)
		if authMethod != authMethodBasic {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "this operation requires password authentication")
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireWebSessionOrBasicAuth middleware ensures the request uses Basic Auth or Web Session
// API keys cannot perform these operations (e.g., creating/revoking API keys)
func (s *Server) requireWebSessionOrBasicAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authMethod := getAuthMethod(c)
		if authMethod != authMethodBasic && authMethod != authMethodWebSession {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "API keys cannot perform this operation")
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireWebSession middleware ensures the request carries a web session: not
// an API key, and not Basic Auth either.
func (s *Server) requireWebSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if getAuthMethod(c) != authMethodWebSession {
			writeError(c, http.StatusForbidden, ErrCodeForbidden, "this operation requires a web session")
			c.Abort()
			return
		}
		c.Next()
	}
}

// getAuthMethod returns the authentication method used for the current request
func getAuthMethod(c *gin.Context) string {
	method, exists := c.Get(contextKeyAuthMethod)
	if !exists {
		return ""
	}
	m, ok := method.(string)
	if !ok {
		return ""
	}
	return m
}

// isAPIKeyAuth returns true if the current request is authenticated via API key
func isAPIKeyAuth(c *gin.Context) bool {
	return getAuthMethod(c) == authMethodAPIKey
}

// authListenerAPI is the listener field of the login-throttle audit entries
// the REST API writes. The proxies write the same entries, sharing the same
// counters, with their protocol name there (see shared.LoginThrottle).
const authListenerAPI = "api"

// checkAuthThrottle applies the per-source backoff before a password check.
// It writes the 429 and returns false when the attempt is throttled. Test
// mode skips the check, as it always has. The username-wide ceiling is
// applied after the check, by failAuth, and only to wrong passwords.
func (s *Server) checkAuthThrottle(c *gin.Context, username string) bool {
	if s.isTestMode() {
		return true
	}

	allowed, retryAfter := s.authFailureTracker.CheckRateLimit(username, c.ClientIP())
	if !allowed {
		writeRateLimited(c, retryAfter)
	}

	return allowed
}

// failAuth answers a failed credential check: it records the failure, then
// writes a 401 INVALID_CREDENTIALS with message, or a 429 when the username is in a
// guessing burst (past its ceiling before this attempt). The burst status is
// read before the failure is recorded, so the attempt that reaches the
// ceiling still gets its 401. Test mode skips the ceiling, as it skips the
// backoff.
func (s *Server) failAuth(c *gin.Context, username, message string) {
	retryAfter := 0
	if !s.isTestMode() {
		retryAfter = s.authFailureTracker.CeilingRetryAfter(username)
	}

	s.noteAuthFailure(c, username)

	if retryAfter > 0 {
		writeRateLimited(c, retryAfter)
		return
	}

	writeError(c, http.StatusUnauthorized, ErrCodeInvalidCredentials, message)
}

// noteAuthFailure records a failed password check and, when it brings the
// username to the per-username ceiling, logs and audits the guessing burst.
func (s *Server) noteAuthFailure(c *gin.Context, username string) {
	ip := c.ClientIP()
	if !s.authFailureTracker.RecordFailure(username, ip) {
		return
	}

	ctx := c.Request.Context()
	s.logger.WarnContext(ctx, "username reached the login failure ceiling; wrong passwords are refused from every source",
		slog.String("username", username),
		slog.String("client_ip", ip),
		slog.Int("failures", cache.AuthUsernameCeiling),
		slog.Duration("window", cache.AuthUsernameCeilingWindow))

	if s.store == nil {
		return
	}

	details, err := json.Marshal(map[string]any{
		"username":       username,
		"failures":       cache.AuthUsernameCeiling,
		"window_seconds": int(cache.AuthUsernameCeilingWindow.Seconds()),
		"last_client_ip": ip,
		"listener":       authListenerAPI,
	})
	if err != nil {
		return
	}

	event := &store.AuditEvent{
		EventType: store.AuditEventAuthFailureCeiling,
		Details:   details,
	}
	if user := s.throttleKeyUser(ctx, username); user != nil {
		event.UserID = &user.UID
	}

	if err := s.store.LogAuditEvent(ctx, event); err != nil {
		s.logger.ErrorContext(ctx, "failed to audit the login failure ceiling", slog.Any("error", err))
	}
}

// noteAuthSuccess clears the caller's backoff and the username's ceiling
// after a correct password. A success that ends an active guessing burst is
// logged and audited, so the signal survives the burst being lifted.
func (s *Server) noteAuthSuccess(c *gin.Context, username string) {
	ip := c.ClientIP()
	failures := s.authFailureTracker.ResetFailures(username, ip)
	if failures == 0 {
		return
	}

	ctx := c.Request.Context()
	s.logger.WarnContext(ctx, "correct password during an active guessing burst; login failure ceiling lifted",
		slog.String("username", username),
		slog.String("client_ip", ip),
		slog.Int("failures", failures))

	if s.store == nil {
		return
	}

	details, err := json.Marshal(map[string]any{
		"username":  username,
		"failures":  failures,
		"client_ip": ip,
		"listener":  authListenerAPI,
	})
	if err != nil {
		return
	}

	event := &store.AuditEvent{
		EventType: store.AuditEventAuthSuccessDuringCeiling,
		Details:   details,
	}
	if user := s.throttleKeyUser(ctx, username); user != nil {
		event.UserID = &user.UID
	}

	if err := s.store.LogAuditEvent(ctx, event); err != nil {
		s.logger.ErrorContext(ctx, "failed to audit a login during a guessing burst", slog.Any("error", err))
	}
}

// throttleKeyUser resolves a throttle key to its user, for the audit trail.
// The key is a username, except for PUT /users/:uid/password without one,
// which throttles on the target UID. It returns nil when no user matches.
func (s *Server) throttleKeyUser(ctx context.Context, key string) *store.User {
	if user, err := s.store.GetUserByUsername(ctx, key); err == nil {
		return user
	}

	if uid, err := uuid.Parse(key); err == nil {
		if user, err := s.store.GetUserByUID(ctx, uid); err == nil {
			return user
		}
	}

	return nil
}

// isTestMode returns true if the server is running in test mode
func (s *Server) isTestMode() bool {
	return s.config != nil && s.config.RunMode == "test"
}

// verifyPasswordDirect verifies a password directly without caching.
// Used as fallback when auth cache is not initialized.
func verifyPasswordDirect(storedHash, password string) (bool, error) {
	return crypto.VerifyPassword(storedHash, password)
}

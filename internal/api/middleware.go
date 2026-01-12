package api

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/fclairamb/dbbat/internal/crypto"
)

// Error codes for API responses
const (
	ErrCodePasswordChangeRequired = "password_change_required"
	ErrCodeAuthRateLimited        = "auth_rate_limited"
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
		errorResponse(c, http.StatusUnauthorized, "invalid API key")
		c.Abort()
		return
	}

	// Get the user associated with the API key
	user, err := s.store.GetUserByUID(ctx, apiKey.UserID)
	if err != nil {
		errorResponse(c, http.StatusUnauthorized, "user not found")
		c.Abort()
		return
	}

	// Update API key usage (async to not block the request)
	go func() {
		_ = s.store.IncrementAPIKeyUsage(ctx, apiKey.ID)
	}()

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
		errorResponse(c, http.StatusUnauthorized, "authentication required")
		c.Abort()
		return
	}

	// Check rate limit BEFORE verifying credentials (fail fast)
	// Skip rate limiting in test mode
	if !s.isTestMode() {
		if allowed, retryAfter := s.authFailureTracker.checkRateLimit(username); !allowed {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       ErrCodeAuthRateLimited,
				"message":     "Too many failed login attempts. Try again in " + strconv.Itoa(retryAfter) + " seconds.",
				"retry_after": retryAfter,
			})
			c.Abort()
			return
		}
	}

	// Look up user
	user, err := s.store.GetUserByUsername(c.Request.Context(), username)
	if err != nil {
		// Record failure AFTER credential verification fails
		s.authFailureTracker.recordFailure(username)
		c.Header("WWW-Authenticate", `Basic realm="DBBat"`)
		errorResponse(c, http.StatusUnauthorized, "invalid credentials")
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
		// Record failure AFTER credential verification fails
		s.authFailureTracker.recordFailure(username)
		c.Header("WWW-Authenticate", `Basic realm="DBBat"`)
		errorResponse(c, http.StatusUnauthorized, "invalid credentials")
		c.Abort()
		return
	}

	// Reset failure count on successful login
	s.authFailureTracker.resetFailures(username)

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
			errorResponse(c, http.StatusForbidden, role+" access required")
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
			errorResponse(c, http.StatusForbidden, "admin or viewer access required")
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
			errorResponse(c, http.StatusForbidden, "this operation requires password authentication")
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
			errorResponse(c, http.StatusForbidden, "API keys cannot perform this operation")
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

// authFailureTracker tracks failed login attempts per username
type authFailureTracker struct {
	mu       sync.RWMutex
	failures map[string]*failureRecord
}

type failureRecord struct {
	count       int
	lastFailure time.Time
}

// Auth failure rate limiting constants
const (
	authFailureResetDuration   = 15 * time.Minute
	authFailureCleanupInterval = 5 * time.Minute
)

// Backoff delays based on failure count
var authBackoffDelays = []struct {
	minFailures int
	delay       time.Duration
}{
	{10, 5 * time.Minute},
	{7, 2 * time.Minute},
	{5, 30 * time.Second},
	{3, 5 * time.Second},
}

// newAuthFailureTracker creates a new auth failure tracker
func newAuthFailureTracker() *authFailureTracker {
	tracker := &authFailureTracker{
		failures: make(map[string]*failureRecord),
	}
	go tracker.cleanup()
	return tracker
}

// cleanup periodically removes stale entries
func (t *authFailureTracker) cleanup() {
	ticker := time.NewTicker(authFailureCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		t.mu.Lock()
		now := time.Now()
		for username, record := range t.failures {
			if now.Sub(record.lastFailure) > authFailureResetDuration {
				delete(t.failures, username)
			}
		}
		t.mu.Unlock()
	}
}

// checkRateLimit checks if the username is rate limited
// Returns (allowed, retryAfter in seconds)
func (t *authFailureTracker) checkRateLimit(username string) (bool, int) {
	t.mu.RLock()
	record, exists := t.failures[username]
	t.mu.RUnlock()

	if !exists {
		return true, 0
	}

	// Check if enough time has passed since last failure
	now := time.Now()

	// Auto-reset after reset duration
	if now.Sub(record.lastFailure) > authFailureResetDuration {
		return true, 0
	}

	// Find applicable backoff delay
	for _, backoff := range authBackoffDelays {
		if record.count >= backoff.minFailures {
			blockedUntil := record.lastFailure.Add(backoff.delay)
			if now.Before(blockedUntil) {
				retryAfter := int(time.Until(blockedUntil).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				return false, retryAfter
			}
			break
		}
	}

	return true, 0
}

// recordFailure records a failed authentication attempt
func (t *authFailureTracker) recordFailure(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	record, exists := t.failures[username]
	if !exists {
		t.failures[username] = &failureRecord{
			count:       1,
			lastFailure: time.Now(),
		}
		return
	}

	// Reset if too old
	if time.Since(record.lastFailure) > authFailureResetDuration {
		record.count = 1
	} else {
		record.count++
	}
	record.lastFailure = time.Now()
}

// resetFailures resets the failure count for a username (on successful login)
func (t *authFailureTracker) resetFailures(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, username)
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

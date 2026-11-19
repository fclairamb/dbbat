package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/safe"
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

// authFailureTracker throttles password guessing on the credential-checking
// endpoints. It keeps two counters:
//
//   - a backoff per (username, client IP): the escalating delays of
//     authBackoffDelays. Keying on the source as well as the account is what
//     stops a stranger who knows a username (admin always exists) from keeping
//     that account locked out with one wrong password every five minutes: their
//     failures lock out their own address, not the account.
//   - a coarse ceiling per username alone (authUsernameCeiling failures per
//     authUsernameCeilingWindow), the backstop against guessing spread across
//     many addresses. Crossing it blocks the username from every source until
//     the window ends, and is logged and audited by the caller (recordFailure
//     reports the crossing) rather than happening silently.
type authFailureTracker struct {
	mu       sync.RWMutex
	failures map[authFailureKey]*failureRecord
	ceilings map[string]*ceilingRecord
}

// authFailureKey is the backoff key: one account as seen from one address.
type authFailureKey struct {
	username string
	ip       string
}

type failureRecord struct {
	count       int
	lastFailure time.Time
}

// ceilingRecord counts a username's failures, from any source, in a fixed
// window starting at its first failure.
type ceilingRecord struct {
	count       int
	windowStart time.Time
}

// Auth failure rate limiting constants
const (
	authFailureResetDuration   = 15 * time.Minute
	authFailureCleanupInterval = 5 * time.Minute

	// authUsernameCeiling failures against one username within
	// authUsernameCeilingWindow, summed over every source address, put that
	// username under a guessing burst until the window ends: every wrong
	// password, from any source, is answered 429 rather than 401, so the
	// failure feedback stops being useful at scale. The password is still
	// checked, and a correct one is let through and lifts the ceiling, so the
	// burst is a backstop against distributed guessing and not a lockout
	// lever.
	authUsernameCeiling       = 100
	authUsernameCeilingWindow = time.Hour
)

// goroutineNameAuthFailureSweep is what a panic in the stale-entry sweep is
// logged under. The tracker predates the server's logger and has no access to
// it, so the sweep reports through slog.Default().
const goroutineNameAuthFailureSweep = "auth failure tracker sweep"

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
		failures: make(map[authFailureKey]*failureRecord),
		ceilings: make(map[string]*ceilingRecord),
	}
	go tracker.cleanup()
	return tracker
}

// cleanup periodically removes stale entries
func (t *authFailureTracker) cleanup() {
	ticker := time.NewTicker(authFailureCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		// Per turn, so a panic neither ends the process nor retires the sweep:
		// a tracker that stopped expiring entries would lock users out on stale
		// failure counts. See safe.RunMaintenance.
		safe.RunMaintenance(context.Background(), slog.Default(), goroutineNameAuthFailureSweep, func() {
			t.sweep(time.Now())
		})
	}
}

// sweep drops the entries that no longer throttle anything at now.
func (t *authFailureTracker) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for key, record := range t.failures {
		if now.Sub(record.lastFailure) > authFailureResetDuration {
			delete(t.failures, key)
		}
	}

	for username, record := range t.ceilings {
		if now.Sub(record.windowStart) > authUsernameCeilingWindow {
			delete(t.ceilings, username)
		}
	}
}

// checkRateLimit reports whether username may attempt a password check from
// ip, under the per-source backoff. Returns (allowed, retryAfter in seconds).
// The username-wide ceiling is not consulted here: it never stops the
// password from being checked (see ceilingRetryAfter).
func (t *authFailureTracker) checkRateLimit(username, ip string) (bool, int) {
	now := time.Now()

	t.mu.RLock()
	record, exists := t.failures[authFailureKey{username: username, ip: ip}]
	var recordCopy failureRecord
	if exists {
		recordCopy = *record
	}
	t.mu.RUnlock()

	if !exists {
		return true, 0
	}

	// Auto-reset after reset duration
	if now.Sub(recordCopy.lastFailure) > authFailureResetDuration {
		return true, 0
	}

	// Find applicable backoff delay
	for _, backoff := range authBackoffDelays {
		if recordCopy.count >= backoff.minFailures {
			blockedUntil := recordCopy.lastFailure.Add(backoff.delay)
			if now.Before(blockedUntil) {
				return false, retryAfterSeconds(blockedUntil.Sub(now))
			}
			break
		}
	}

	return true, 0
}

// ceilingRetryAfter returns the seconds left in username's guessing burst, or
// 0 when the username is under its ceiling. During a burst a wrong password
// is answered 429 with this Retry-After, from every source.
func (t *authFailureTracker) ceilingRetryAfter(username string) int {
	now := time.Now()

	t.mu.RLock()
	ceiling, exists := t.ceilings[username]
	var ceilingCopy ceilingRecord
	if exists {
		ceilingCopy = *ceiling
	}
	t.mu.RUnlock()

	if !exists || ceilingCopy.count < authUsernameCeiling {
		return 0
	}

	blockedUntil := ceilingCopy.windowStart.Add(authUsernameCeilingWindow)
	if !now.Before(blockedUntil) {
		return 0
	}

	return retryAfterSeconds(blockedUntil.Sub(now))
}

// retryAfterSeconds rounds a remaining block to whole seconds, at least one.
func retryAfterSeconds(d time.Duration) int {
	retryAfter := int(d.Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}

	return retryAfter
}

// recordFailure records a failed authentication attempt for username from ip.
// It returns true exactly once per window: on the failure that brings the
// username to authUsernameCeiling, so the caller can log and audit it.
func (t *authFailureTracker) recordFailure(username, ip string) bool {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	key := authFailureKey{username: username, ip: ip}
	record, exists := t.failures[key]
	switch {
	case !exists:
		t.failures[key] = &failureRecord{count: 1, lastFailure: now}
	case now.Sub(record.lastFailure) > authFailureResetDuration:
		record.count = 1
		record.lastFailure = now
	default:
		record.count++
		record.lastFailure = now
	}

	ceiling, exists := t.ceilings[username]
	if !exists || now.Sub(ceiling.windowStart) > authUsernameCeilingWindow {
		ceiling = &ceilingRecord{windowStart: now}
		t.ceilings[username] = ceiling
	}
	ceiling.count++

	return ceiling.count == authUsernameCeiling
}

// resetFailures clears, on a successful login, the backoff of username from
// ip and the username-wide ceiling: whoever holds the password has nothing
// left to guess, so a burst against the account ends with its owner's next
// correct login. The backoff of the other sources is left alone. It returns
// the number of failures the ceiling had counted when it was reached, or 0
// when no guessing burst was active, so the caller can audit the success.
func (t *authFailureTracker) resetFailures(username, ip string) int {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, authFailureKey{username: username, ip: ip})

	ceiling, exists := t.ceilings[username]
	if !exists {
		return 0
	}
	delete(t.ceilings, username)

	if ceiling.count >= authUsernameCeiling && now.Before(ceiling.windowStart.Add(authUsernameCeilingWindow)) {
		return ceiling.count
	}

	return 0
}

// checkAuthThrottle applies the per-source backoff before a password check.
// It writes the 429 and returns false when the attempt is throttled. Test
// mode skips the check, as it always has. The username-wide ceiling is
// applied after the check, by failAuth, and only to wrong passwords.
func (s *Server) checkAuthThrottle(c *gin.Context, username string) bool {
	if s.isTestMode() {
		return true
	}

	allowed, retryAfter := s.authFailureTracker.checkRateLimit(username, c.ClientIP())
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
		retryAfter = s.authFailureTracker.ceilingRetryAfter(username)
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
	if !s.authFailureTracker.recordFailure(username, ip) {
		return
	}

	ctx := c.Request.Context()
	s.logger.WarnContext(ctx, "username reached the login failure ceiling; wrong passwords are refused from every source",
		slog.String("username", username),
		slog.String("client_ip", ip),
		slog.Int("failures", authUsernameCeiling),
		slog.Duration("window", authUsernameCeilingWindow))

	if s.store == nil {
		return
	}

	details, err := json.Marshal(map[string]any{
		"username":       username,
		"failures":       authUsernameCeiling,
		"window_seconds": int(authUsernameCeilingWindow.Seconds()),
		"last_client_ip": ip,
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
	failures := s.authFailureTracker.resetFailures(username, ip)
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

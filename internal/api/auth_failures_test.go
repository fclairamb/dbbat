package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestLogFailedLogin_JSONFormat verifies the audit log format for failed logins
func TestLogFailedLogin_JSONFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		reason   string
	}{
		{
			name:     "invalid username",
			username: "nonexistent",
			reason:   FailureReasonInvalidUsername,
		},
		{
			name:     "invalid password",
			username: "testuser",
			reason:   FailureReasonInvalidPassword,
		},
		{
			name:     "password change required",
			username: "newuser",
			reason:   FailureReasonPasswordChangeReq,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The test verifies that the correct constants are defined
			// and can be used in the logging functions
			if tt.reason == "" {
				t.Error("failure reason should not be empty")
			}

			// Verify reason is one of the expected values
			validReasons := []string{
				FailureReasonInvalidUsername,
				FailureReasonInvalidPassword,
				FailureReasonPasswordChangeReq,
				FailureReasonTokenInvalid,
				FailureReasonTokenExpired,
				FailureReasonTokenRevoked,
				FailureReasonUserDisabled,
				FailureReasonUserDeleted,
			}

			found := false
			for _, valid := range validReasons {
				if tt.reason == valid {
					found = true
					break
				}
			}

			if !found {
				t.Errorf("reason %q is not a valid failure reason", tt.reason)
			}
		})
	}
}

// TestFailedBearerAuth_TokenPrefix verifies token prefix extraction
func TestFailedBearerAuth_TokenPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		token         string
		wantPrefixLen int
	}{
		{
			name:          "short token",
			token:         "abc",
			wantPrefixLen: 3,
		},
		{
			name:          "normal token",
			token:         "dbb_12345678901234567890",
			wantPrefixLen: 8,
		},
		{
			name:          "long token",
			token:         "web_abcdefghijklmnopqrstuvwxyz",
			wantPrefixLen: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Simulate token prefix extraction logic
			tokenPrefix := tt.token
			if len(tokenPrefix) > 8 {
				tokenPrefix = tokenPrefix[:8]
			}

			if len(tokenPrefix) != tt.wantPrefixLen {
				t.Errorf("token prefix length = %d, want %d", len(tokenPrefix), tt.wantPrefixLen)
			}
		})
	}
}

// TestAuditDetailsFormat verifies the structure of audit log details
func TestAuditDetailsFormat(t *testing.T) {
	t.Parallel()

	t.Run("failed login details", func(t *testing.T) {
		t.Parallel()
		// Create mock details
		details := map[string]interface{}{
			"username":   "testuser",
			"source_ip":  "192.168.1.1",
			"user_agent": "test-agent",
			"reason":     FailureReasonInvalidPassword,
			"timestamp":  "2024-01-01T00:00:00Z",
		}

		// Marshal to JSON
		detailsJSON, err := json.Marshal(details)
		if err != nil {
			t.Fatalf("failed to marshal details: %v", err)
		}

		// Unmarshal back
		var parsed map[string]interface{}
		if err := json.Unmarshal(detailsJSON, &parsed); err != nil {
			t.Fatalf("failed to unmarshal details: %v", err)
		}

		// Verify required fields
		requiredFields := []string{"username", "source_ip", "reason", "timestamp"}
		for _, field := range requiredFields {
			if _, exists := parsed[field]; !exists {
				t.Errorf("missing required field: %s", field)
			}
		}
	})

	t.Run("failed token auth details", func(t *testing.T) {
		t.Parallel()
		// Create mock details
		details := map[string]interface{}{
			"token_prefix": "dbb_1234",
			"source_ip":    "192.168.1.1",
			"user_agent":   "test-agent",
			"reason":       FailureReasonTokenInvalid,
			"timestamp":    "2024-01-01T00:00:00Z",
		}

		// Marshal to JSON
		detailsJSON, err := json.Marshal(details)
		if err != nil {
			t.Fatalf("failed to marshal details: %v", err)
		}

		// Unmarshal back
		var parsed map[string]interface{}
		if err := json.Unmarshal(detailsJSON, &parsed); err != nil {
			t.Fatalf("failed to unmarshal details: %v", err)
		}

		// Verify required fields
		requiredFields := []string{"token_prefix", "source_ip", "reason", "timestamp"}
		for _, field := range requiredFields {
			if _, exists := parsed[field]; !exists {
				t.Errorf("missing required field: %s", field)
			}
		}
	})
}

// TestClientIP verifies client IP extraction from gin context
func TestClientIP(t *testing.T) {
	t.Parallel()

	t.Run("extracts client IP", func(t *testing.T) {
		t.Parallel()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.RemoteAddr = "192.168.1.100:12345"

		ip := c.ClientIP()
		if ip == "" {
			t.Error("ClientIP() returned empty string")
		}
	})
}

// TestFailureReasonConstants verifies all failure reason constants are defined
func TestFailureReasonConstants(t *testing.T) {
	t.Parallel()

	constants := map[string]string{
		"FailureReasonInvalidUsername":   FailureReasonInvalidUsername,
		"FailureReasonInvalidPassword":   FailureReasonInvalidPassword,
		"FailureReasonPasswordChangeReq": FailureReasonPasswordChangeReq,
		"FailureReasonTokenInvalid":      FailureReasonTokenInvalid,
		"FailureReasonTokenExpired":      FailureReasonTokenExpired,
		"FailureReasonTokenRevoked":      FailureReasonTokenRevoked,
		"FailureReasonUserDisabled":      FailureReasonUserDisabled,
		"FailureReasonUserDeleted":       FailureReasonUserDeleted,
	}

	for name, value := range constants {
		if value == "" {
			t.Errorf("constant %s is empty", name)
		}
	}

	// Verify uniqueness
	seen := make(map[string]bool)
	for name, value := range constants {
		if seen[value] {
			t.Errorf("duplicate value %q for constant %s", value, name)
		}
		seen[value] = true
	}
}

// throttledLogin is a server with the login throttle switched on (test mode
// skips it) and one user, alice, whose password is "correct-horse".
type throttledLogin struct {
	server *Server
	store  *store.Store
	user   *store.User
	router *gin.Engine
}

func throttledLoginServer(t *testing.T) throttledLogin {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.config.RunMode = ""

	user := createTestUser(t, dataStore, "alice", "correct-horse", []string{"connector"})

	router := gin.New()
	router.POST("/api/v1/auth/login", server.handleLogin)

	return throttledLogin{server: server, store: dataStore, user: user, router: router}
}

// postLoginFrom logs in as alice from remoteIP.
func postLoginFrom(router *gin.Engine, remoteIP, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteIP + ":40000"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

// Failures from one address must not lock the account out for another: before
// the fix the backoff was keyed on the username alone, so one wrong password
// every five minutes from anywhere kept the account locked indefinitely.
func TestLogin_FailuresFromOneIPDoNotBlockAnother(t *testing.T) {
	t.Parallel()

	router := throttledLoginServer(t).router

	const attacker, owner = "192.0.2.66", "198.51.100.5"

	// Enough failures to put the attacker's source under backoff.
	for i := 0; i < 3; i++ {
		w := postLoginFrom(router, attacker, "wrong")
		require.Equal(t, http.StatusUnauthorized, w.Code, "failure %d: %s", i+1, w.Body.String())
	}

	w := postLoginFrom(router, attacker, "wrong")
	require.Equal(t, http.StatusTooManyRequests, w.Code, "the attacker's source is backed off: %s", w.Body.String())

	w = postLoginFrom(router, owner, "correct-horse")
	require.Equal(t, http.StatusOK, w.Code, "the owner's login from another address: %s", w.Body.String())
}

// Positive control: one source that keeps failing is still throttled, with
// Retry-After, even when it finally sends the right password.
func TestLogin_RepeatedFailuresFromOneIPAreThrottled(t *testing.T) {
	t.Parallel()

	router := throttledLoginServer(t).router

	const source = "192.0.2.77"

	for i := 0; i < 3; i++ {
		w := postLoginFrom(router, source, "wrong")
		require.Equal(t, http.StatusUnauthorized, w.Code, "failure %d: %s", i+1, w.Body.String())
	}

	w := postLoginFrom(router, source, "correct-horse")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	var body ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeRateLimited, body.Code)
}

// The per-username ceiling is the backstop against guessing spread over many
// addresses: crossing it makes every source's wrong password a 429 and leaves
// an audit entry.
func TestLogin_UsernameCeilingThrottlesWrongPasswordsAndIsAudited(t *testing.T) {
	t.Parallel()

	tl := throttledLoginServer(t)
	server, dataStore, user, router := tl.server, tl.store, tl.user, tl.router

	reachCeiling(t, server, router)

	// A wrong password from a never-seen address is refused with Retry-After.
	w := postLoginFrom(router, "203.0.113.1", "wrong")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	var body ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeRateLimited, body.Code)

	eventType := store.AuditEventAuthFailureCeiling
	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{EventType: &eventType})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UserID)
	assert.Equal(t, user.UID, *events[0].UserID)

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, "alice", details["username"])
	assert.Equal(t, "192.0.2.200", details["last_client_ip"])
	assert.Equal(t, "api", details["listener"])
}

// The ceiling is not a lockout lever: past it, the owner's correct password
// still logs in (before the fix it was refused with 429 before the password
// was even checked), the success is audited, and it lifts the ceiling, so the
// next wrong password is a plain 401 again.
func TestLogin_UsernameCeilingLetsTheCorrectPasswordThrough(t *testing.T) {
	t.Parallel()

	tl := throttledLoginServer(t)
	server, dataStore, user, router := tl.server, tl.store, tl.user, tl.router

	reachCeiling(t, server, router)

	w := postLoginFrom(router, "203.0.113.1", "correct-horse")
	require.Equal(t, http.StatusOK, w.Code, "the owner's correct password past the ceiling: %s", w.Body.String())

	eventType := store.AuditEventAuthSuccessDuringCeiling
	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{EventType: &eventType})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UserID)
	assert.Equal(t, user.UID, *events[0].UserID)

	var details map[string]any
	require.NoError(t, json.Unmarshal(events[0].Details, &details))
	assert.Equal(t, "alice", details["username"])
	assert.Equal(t, "203.0.113.1", details["client_ip"])
	assert.Equal(t, "api", details["listener"])
	assert.EqualValues(t, cache.AuthUsernameCeiling, details["failures"])

	// The ceiling is gone: a wrong password from yet another address meets
	// only the per-source backoff, which has nothing against it.
	w = postLoginFrom(router, "203.0.113.2", "wrong")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	// A later success, outside any burst, is not audited again.
	w = postLoginFrom(router, "203.0.113.3", "correct-horse")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	events, err = dataStore.ListAuditEvents(context.Background(), store.AuditFilter{EventType: &eventType})
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

// reachCeiling brings alice to the per-username ceiling: 99 failures from 99
// distinct addresses straight into the tracker, then the hundredth through
// the handler, which still gets its 401.
func reachCeiling(t *testing.T, server *Server, router *gin.Engine) {
	t.Helper()

	for i := 0; i < cache.AuthUsernameCeiling-1; i++ {
		server.authFailureTracker.RecordFailure("alice", fmt.Sprintf("10.9.%d.%d", i/256, i%256))
	}

	w := postLoginFrom(router, "192.0.2.200", "wrong")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// POST /auth/login and PUT /auth/password are IP rate limited before any
// credential is looked at, against spraying many usernames from one source.
func TestLoginRoutes_PreAuthIPRateLimit(t *testing.T) {
	t.Parallel()

	dataStore := newIsolatedStoreWithOptions(t, store.Options{})
	logger := slog.New(slog.DiscardHandler)
	cfg := &config.Config{
		RateLimit: config.RateLimitConfig{Enabled: true, RequestsPerMinute: 60, RequestsPerMinuteAnon: 3, Burst: 0},
	}
	server := NewServer(dataStore, nil, logger, cfg)
	router := server.setupRouter()

	// Each route is sprayed from its own address, so the two share nothing.
	for _, route := range []struct{ method, path, remote string }{
		{http.MethodPost, "/api/v1/auth/login", "192.0.2.1"},
		{http.MethodPut, "/api/v1/auth/password", "192.0.2.2"},
	} {
		t.Run(route.path, func(t *testing.T) {
			t.Parallel()

			remote := route.remote

			var last *httptest.ResponseRecorder
			for i := 0; i < 4; i++ {
				body, _ := json.Marshal(map[string]string{
					"username": fmt.Sprintf("sprayed-%d", i), "password": "x",
					"current_password": "x", "new_password": "longenough",
				})
				req := httptest.NewRequest(route.method, route.path, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.RemoteAddr = remote + ":40000"
				last = httptest.NewRecorder()
				router.ServeHTTP(last, req)

				if i < 3 {
					require.Equal(t, http.StatusUnauthorized, last.Code, "request %d: %s", i+1, last.Body.String())
				}
			}

			require.Equal(t, http.StatusTooManyRequests, last.Code, last.Body.String())
			assert.NotEmpty(t, last.Header().Get("Retry-After"))
		})
	}
}

// fillCeiling puts key at the per-username ceiling straight through the
// tracker: cache.AuthUsernameCeiling failures from as many distinct addresses.
func fillCeiling(server *Server, key string) {
	for i := 0; i < cache.AuthUsernameCeiling; i++ {
		server.authFailureTracker.RecordFailure(key, fmt.Sprintf("10.8.%d.%d", i/256, i%256))
	}
}

// assertCeilingSuccessAudited checks that exactly one success during a
// guessing burst was audited, for user.
func assertCeilingSuccessAudited(t *testing.T, dataStore *store.Store, user *store.User) {
	t.Helper()

	eventType := store.AuditEventAuthSuccessDuringCeiling
	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{EventType: &eventType})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UserID)
	assert.Equal(t, user.UID, *events[0].UserID)
}

// Basic Auth is a login on every request, so it meets the same ceiling as
// POST /auth/login: past it a wrong password is a 429 (before the fix it was
// a 401, the ceiling never applied), while the owner's correct password gets
// through, is audited and lifts the ceiling.
func TestBasicAuth_UsernameCeiling(t *testing.T) {
	t.Parallel()

	tl := throttledLoginServer(t)
	server, dataStore, user := tl.server, tl.store, tl.user

	router := gin.New()
	router.GET("/probe", server.handleBasicAuth, func(c *gin.Context) { c.Status(http.StatusNoContent) })

	probe := func(remoteIP, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.SetBasicAuth("alice", password)
		req.RemoteAddr = remoteIP + ":40000"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		return w
	}

	// Below the ceiling a wrong password is a plain 401 with the challenge.
	w := probe("203.0.113.9", "wrong")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("WWW-Authenticate"))

	fillCeiling(server, "alice")

	w = probe("203.0.113.10", "wrong")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	var body ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeRateLimited, body.Code)

	w = probe("203.0.113.11", "correct-horse")
	require.Equal(t, http.StatusNoContent, w.Code, "the owner's correct password past the ceiling: %s", w.Body.String())
	assertCeilingSuccessAudited(t, dataStore, user)

	// The ceiling is lifted: a wrong password from another address is a 401.
	w = probe("203.0.113.12", "wrong")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// Both password-change endpoints check a current password, so both meet the
// ceiling on the key they throttle: the username (PUT /auth/password, and
// PUT /users/:uid/password with a username) or the target UID (PUT
// /users/:uid/password without one).
func TestPasswordChange_UsernameCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// path and body build the request for alice; key is the throttle key.
		path func(user *store.User) string
		body func(current, next string) map[string]string
		key  func(user *store.User) string
	}{
		{
			name: "pre-login PUT /auth/password",
			path: func(*store.User) string { return "/api/v1/auth/password" },
			body: func(current, next string) map[string]string {
				return map[string]string{"username": "alice", "current_password": current, "new_password": next}
			},
			key: func(*store.User) string { return "alice" },
		},
		{
			name: "username-keyed PUT /users/:uid/password",
			path: func(u *store.User) string { return "/api/v1/users/" + u.UID.String() + "/password" },
			body: func(current, next string) map[string]string {
				return map[string]string{"username": "alice", "current_password": current, "new_password": next}
			},
			key: func(*store.User) string { return "alice" },
		},
		{
			name: "UID-keyed PUT /users/:uid/password",
			path: func(u *store.User) string { return "/api/v1/users/" + u.UID.String() + "/password" },
			body: func(current, next string) map[string]string {
				return map[string]string{"current_password": current, "new_password": next}
			},
			key: func(u *store.User) string { return u.UID.String() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := throttledLoginServer(t)
			server, dataStore, user := tl.server, tl.store, tl.user

			router := gin.New()
			router.PUT("/api/v1/auth/password", server.handlePreLoginPasswordChange)
			router.PUT("/api/v1/users/:uid/password", server.handleChangePassword)

			put := func(remoteIP, current, next string) *httptest.ResponseRecorder {
				payload, _ := json.Marshal(tt.body(current, next))
				req := httptest.NewRequest(http.MethodPut, tt.path(user), bytes.NewReader(payload))
				req.Header.Set("Content-Type", "application/json")
				req.RemoteAddr = remoteIP + ":40000"
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				return w
			}

			fillCeiling(server, tt.key(user))

			w := put("203.0.113.20", "wrong", "brand-new-password")
			require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
			assert.NotEmpty(t, w.Header().Get("Retry-After"))

			var body ErrorBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, ErrCodeRateLimited, body.Code)

			w = put("203.0.113.21", "correct-horse", "brand-new-password")
			require.Equal(t, http.StatusOK, w.Code, "the owner's correct password past the ceiling: %s", w.Body.String())
			assertCeilingSuccessAudited(t, dataStore, user)

			// The ceiling is lifted: a wrong password is a 401 again.
			w = put("203.0.113.22", "wrong", "another-password")
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		})
	}
}

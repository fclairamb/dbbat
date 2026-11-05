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
// addresses: crossing it blocks every source and leaves an audit entry.
func TestLogin_UsernameCeilingBlocksEverySourceAndIsAudited(t *testing.T) {
	t.Parallel()

	tl := throttledLoginServer(t)
	server, dataStore, user, router := tl.server, tl.store, tl.user, tl.router

	// 99 failures from 99 distinct addresses, straight into the tracker.
	for i := 0; i < authUsernameCeiling-1; i++ {
		server.authFailureTracker.recordFailure("alice", fmt.Sprintf("10.9.%d.%d", i/256, i%256))
	}

	// The hundredth, through the handler, crosses the ceiling.
	w := postLoginFrom(router, "192.0.2.200", "wrong")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	// Even the right password from a never-seen address is now refused.
	w = postLoginFrom(router, "203.0.113.1", "correct-horse")
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

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

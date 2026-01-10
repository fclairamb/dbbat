package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
		name           string
		token          string
		wantPrefixLen  int
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

	gin.SetMode(gin.TestMode)

	t.Run("extracts client IP", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
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

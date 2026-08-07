package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

func TestParseUIDParam(t *testing.T) {
	t.Parallel()

	validUUID := "550e8400-e29b-41d4-a716-446655440000"
	parsedUUID, _ := uuid.Parse(validUUID)

	tests := []struct {
		name      string
		paramVal  string
		wantUID   uuid.UUID
		wantError bool
	}{
		{name: "valid UUID", paramVal: validUUID, wantUID: parsedUUID, wantError: false},
		{name: "valid UUID v4", paramVal: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", wantUID: uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"), wantError: false},
		{name: "invalid string", paramVal: "abc", wantUID: uuid.Nil, wantError: true},
		{name: "empty string", paramVal: "", wantUID: uuid.Nil, wantError: true},
		{name: "integer", paramVal: "123", wantUID: uuid.Nil, wantError: true},
		{name: "partial UUID", paramVal: "550e8400-e29b", wantUID: uuid.Nil, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create a test context with the UID parameter
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "uid", Value: tt.paramVal}}

			uid, err := parseUIDParam(c)

			if tt.wantError {
				if err == nil {
					t.Errorf("parseUIDParam() expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("parseUIDParam() unexpected error: %v", err)
				}
				if uid != tt.wantUID {
					t.Errorf("parseUIDParam() = %s, want %s", uid, tt.wantUID)
				}
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		code     ErrorCode
		message  string
		wantCode int
		wantBody string
	}{
		{
			name:     "bad request",
			status:   http.StatusBadRequest,
			code:     ErrCodeValidationError,
			message:  "invalid input",
			wantCode: http.StatusBadRequest,
			wantBody: `{"code":"VALIDATION_ERROR","message":"invalid input"}`,
		},
		{
			name:     "not found",
			status:   http.StatusNotFound,
			code:     ErrCodeNotFound,
			message:  "resource not found",
			wantCode: http.StatusNotFound,
			wantBody: `{"code":"NOT_FOUND","message":"resource not found"}`,
		},
		{
			name:     "forbidden",
			status:   http.StatusForbidden,
			code:     ErrCodeForbidden,
			message:  "access denied",
			wantCode: http.StatusForbidden,
			wantBody: `{"code":"FORBIDDEN","message":"access denied"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			writeError(c, tt.status, tt.code, tt.message)

			if w.Code != tt.wantCode {
				t.Errorf("writeError() status code = %d, want %d", w.Code, tt.wantCode)
			}

			if w.Body.String() != tt.wantBody {
				t.Errorf("writeError() body = %s, want %s", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestSuccessResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     any
		wantBody string
	}{
		{
			name:     "simple map",
			data:     gin.H{"message": "success"},
			wantBody: `{"message":"success"}`,
		},
		{
			name:     "nested data",
			data:     gin.H{"user": gin.H{"id": 1, "name": "test"}},
			wantBody: `{"user":{"id":1,"name":"test"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			successResponse(c, tt.data)

			if w.Code != http.StatusOK {
				t.Errorf("successResponse() status code = %d, want %d", w.Code, http.StatusOK)
			}

			if w.Body.String() != tt.wantBody {
				t.Errorf("successResponse() body = %s, want %s", w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestGetCurrentUser(t *testing.T) {
	t.Parallel()

	t.Run("no user in context", func(t *testing.T) {
		t.Parallel()

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)

		user := getCurrentUser(c)
		if user != nil {
			t.Errorf("getCurrentUser() = %v, want nil", user)
		}
	})

	t.Run("wrong type in context", func(t *testing.T) {
		t.Parallel()

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("current_user", "not a user")

		user := getCurrentUser(c)
		if user != nil {
			t.Errorf("getCurrentUser() = %v, want nil", user)
		}
	})
}

// TestRedactQuery pins the access log's secret filter: a credential riding in
// a query string (the OAuth exchange code, a device code, a bearer token
// pasted as ?token=) must never reach the logs, while ordinary filtering
// params stay readable.
func TestRedactQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{
			name: "ordinary params survive",
			raw:  "limit=50&offset=100&status=active",
			want: "limit=50&offset=100&status=active",
		},
		{
			name: "oauth exchange code is redacted",
			raw:  "code=1f3a9c&redirect=%2Fgrants",
			want: "code=REDACTED&redirect=%2Fgrants",
		},
		{
			name: "legacy token handoff is redacted",
			raw:  "token=web_abcdef",
			want: "token=REDACTED",
		},
		{
			name: "csrf state and device code are redacted",
			raw:  "state=abc&device_code=def&user_code=WDJP4KXR",
			want: "state=REDACTED&device_code=REDACTED&user_code=REDACTED",
		},
		{
			name: "matching is case-insensitive",
			raw:  "Token=abc&ACCESS_TOKEN=def",
			want: "Token=REDACTED&ACCESS_TOKEN=REDACTED",
		},
		{
			name: "valueless flags are left alone",
			raw:  "verbose&token=abc",
			want: "verbose&token=REDACTED",
		},
		{
			name: "percent-encoded sensitive name is still caught",
			raw:  "%74oken=abc",
			want: "%74oken=REDACTED",
		},
		{
			name: "undecodable name is redacted rather than trusted",
			raw:  "%zz=abc",
			want: "%zz=REDACTED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redactQuery(tt.raw); got != tt.want {
				t.Errorf("redactQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

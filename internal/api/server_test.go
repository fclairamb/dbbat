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
// pasted as ?token=) must never reach the logs, while the allowlisted
// pagination and filtering params stay readable.
func TestRedactQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", raw: "", want: ""},
		{
			name: "allowlisted pagination and filter params survive",
			raw:  "limit=50&offset=100&status=active&cursor=abc&sort=created_at",
			want: "limit=50&offset=100&status=active&cursor=abc&sort=created_at",
		},
		{
			name: "allowlisted entity ids and time ranges survive",
			raw:  "user_id=u-1&database_id=d-2&start_time=2026-08-07T00%3A00%3A00Z",
			want: "user_id=u-1&database_id=d-2&start_time=2026-08-07T00%3A00%3A00Z",
		},
		{
			name: "oauth exchange code is redacted, allowlisted neighbor is not",
			raw:  "code=1f3a9c&status=ok",
			want: "code=REDACTED&status=ok",
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
			// redirect is caller-controlled and the device flow puts
			// "/device?user_code=..." in it, so it is not allowlisted even
			// though it usually holds nothing but an app path.
			name: "post-login redirect target is redacted",
			raw:  "redirect=%2Fdevice%3Fuser_code%3DWDJP4KXR",
			want: "redirect=REDACTED",
		},
		{
			name: "oauth error code survives but its free-text description does not",
			raw:  "error=access_denied&error_description=User%20said%20no",
			want: "error=access_denied&error_description=REDACTED",
		},
		{
			name: "matching is case-insensitive in both directions",
			raw:  "Token=abc&LIMIT=50",
			want: "Token=REDACTED&LIMIT=50",
		},
		{
			name: "valueless allowlisted flag is left alone",
			raw:  "hard&token=abc",
			want: "hard&token=REDACTED",
		},
		{
			name: "bare valueless segment could be a pasted secret",
			raw:  "ZXlKaGJHY2lPaUpJVXpJMU5pSjkK",
			want: "REDACTED",
		},
		{
			name: "percent-encoded sensitive name is still caught",
			raw:  "%74oken=abc",
			want: "%74oken=REDACTED",
		},
		{
			name: "percent-encoded allowlisted name is recognized",
			raw:  "%6cimit=50",
			want: "%6cimit=50",
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

// TestRedactQueryDefaultsToRedacted is the point of the allowlist, and the
// guard against anyone quietly inverting it back into a denylist.
//
// Every name below is one nobody has ever added to a list — a hypothetical
// future parameter, or a typo of a real one. None of them appear in
// loggableQueryParams and none of them ever appeared in the old denylist
// either, which is exactly how the denylist would have leaked them. The
// requirement is that they redact with NO code change: if this test starts
// failing, the filter has been turned back into a denylist and the next
// sensitive parameter someone adds will land in the access log verbatim.
//
// The parameter NAME must stay visible in every case — hiding which parameters
// were present makes the log far harder to debug, and the names are not the
// secret.
func TestRedactQueryDefaultsToRedacted(t *testing.T) {
	t.Parallel()

	// Deliberately not in any list: future credentials, and near-misses of
	// allowlisted names that must not be given the benefit of the doubt.
	neverSeenParams := []string{
		"reset_token",
		"otp",
		"invite",
		"signature",
		"magic_link",
		"impersonate",
		"limits",   // near-miss of the allowlisted "limit"
		"user_id2", // near-miss of the allowlisted "user_id"
		"x-custom",
	}

	for _, name := range neverSeenParams {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if loggableQueryParams[name] {
				t.Fatalf("test bug: %q is allowlisted, pick a name that is not", name)
			}

			got := redactQuery(name + "=s3cr3t-value")
			want := name + "=REDACTED"

			if got != want {
				t.Errorf("redactQuery redacts by default: %q = %q, want %q "+
					"(an unknown parameter must be redacted without anyone "+
					"adding it to a list)", name, got, want)
			}
		})
	}
}

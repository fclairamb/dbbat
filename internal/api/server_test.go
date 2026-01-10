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

func TestErrorResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		code     int
		message  string
		wantCode int
		wantBody string
	}{
		{
			name:     "bad request",
			code:     http.StatusBadRequest,
			message:  "invalid input",
			wantCode: http.StatusBadRequest,
			wantBody: `{"error":"invalid input"}`,
		},
		{
			name:     "not found",
			code:     http.StatusNotFound,
			message:  "resource not found",
			wantCode: http.StatusNotFound,
			wantBody: `{"error":"resource not found"}`,
		},
		{
			name:     "internal server error",
			code:     http.StatusInternalServerError,
			message:  "something went wrong",
			wantCode: http.StatusInternalServerError,
			wantBody: `{"error":"something went wrong"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			errorResponse(c, tt.code, tt.message)

			if w.Code != tt.wantCode {
				t.Errorf("errorResponse() status code = %d, want %d", w.Code, tt.wantCode)
			}

			if w.Body.String() != tt.wantBody {
				t.Errorf("errorResponse() body = %s, want %s", w.Body.String(), tt.wantBody)
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

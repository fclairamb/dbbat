package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

var (
	testContainer       *postgres.PostgresContainer
	testDSN             string
	containerOnce       sync.Once
	errContainerStartup error
)

// setupPostgresContainer starts a PostgreSQL container for testing.
func setupPostgresContainer(t *testing.T) string {
	t.Helper()

	containerOnce.Do(func() {
		ctx := context.Background()

		testContainer, errContainerStartup = postgres.Run(ctx,
			"postgres:15-alpine",
			postgres.WithDatabase("dbbat_test"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if errContainerStartup != nil {
			return
		}

		testDSN, errContainerStartup = testContainer.ConnectionString(ctx, "sslmode=disable")
	})

	if errContainerStartup != nil {
		t.Fatalf("failed to start postgres container: %v", errContainerStartup)
	}

	return testDSN
}

// setupTestServer creates an API server with a real database for testing.
func setupTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	dsn := setupPostgresContainer(t)
	ctx := context.Background()

	dataStore, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// Clean up tables
	cleanupTables := []string{
		"api_keys",
		"query_rows",
		"queries",
		"connections",
		"access_grants",
		"audit_log",
		"databases",
		"users",
	}

	for _, table := range cleanupTables {
		_, err := dataStore.DB().ExecContext(ctx, "DELETE FROM "+table)
		if err != nil {
			dataStore.Close()
			t.Fatalf("failed to clean up table %s: %v", table, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := &config.Config{
		RunMode: "test",
	}

	server := NewServer(dataStore, nil, logger, cfg)

	t.Cleanup(func() {
		dataStore.Close()
	})

	return server, dataStore
}

// createTestUser creates a user for testing and returns the user.
func createTestUser(t *testing.T, dataStore *store.Store, username, password string, roles []string) *store.User {
	t.Helper()

	hashedPassword, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	user, err := dataStore.CreateUser(context.Background(), username, hashedPassword, roles)
	if err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Mark password as changed so user can login
	if err := dataStore.UpdateUser(context.Background(), user.UID, store.UserUpdate{PasswordHash: &hashedPassword}); err != nil {
		t.Fatalf("failed to update user password: %v", err)
	}

	// Refetch user to get updated state
	user, err = dataStore.GetUserByUID(context.Background(), user.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}

	return user
}

// loginUser logs in a user and returns a web session token.
func loginUser(t *testing.T, server *Server, username, password string) string {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/auth/login", server.handleLogin)

	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login failed with status %d: %s", w.Code, w.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse login response: %v", err)
	}

	token, ok := response["token"].(string)
	if !ok {
		t.Fatalf("token not found in login response")
	}

	return token
}

func TestResetPassword_SelfResetForbidden(t *testing.T) {
	server, dataStore := setupTestServer(t)

	// Create an admin user
	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Add auth middleware that verifies the token
	router.Use(server.authMiddleware())
	router.POST("/api/v1/users/:uid/reset-password", server.requireAdmin(), server.handleResetPassword)

	// Try to reset own password
	body, _ := json.Marshal(map[string]string{
		"new_password": "newpassword123",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/"+adminUser.UID.String()+"/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should be forbidden
	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}

	// Check error message
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	expectedError := "cannot reset your own password; use the password change endpoint instead"
	if response["error"] != expectedError {
		t.Errorf("expected error message %q, got %q", expectedError, response["error"])
	}
}

func TestResetPassword_AdminCanResetOtherUser(t *testing.T) {
	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Create a regular user
	targetUser := createTestUser(t, dataStore, "regularuser", "regularpassword123", []string{"connector"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/users/:uid/reset-password", server.requireAdmin(), server.handleResetPassword)

	// Reset the regular user's password
	newPassword := "newpassword456"
	body, _ := json.Marshal(map[string]string{
		"new_password": newPassword,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/"+targetUser.UID.String()+"/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should succeed
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the password was changed by trying to login with new password
	// Refetch user and verify password
	updatedUser, err := dataStore.GetUserByUID(context.Background(), targetUser.UID)
	if err != nil {
		t.Fatalf("failed to get updated user: %v", err)
	}

	valid, err := crypto.VerifyPassword(updatedUser.PasswordHash, newPassword)
	if err != nil {
		t.Fatalf("failed to verify password: %v", err)
	}
	if !valid {
		t.Error("new password should be valid")
	}

	// Verify old password no longer works
	valid, _ = crypto.VerifyPassword(updatedUser.PasswordHash, "regularpassword123")
	if valid {
		t.Error("old password should no longer be valid")
	}
}

func TestResetPassword_NonAdminForbidden(t *testing.T) {
	server, dataStore := setupTestServer(t)

	// Create a regular user (non-admin)
	createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})

	// Create another user to be the target
	targetUser := createTestUser(t, dataStore, "target", "targetpassword123", []string{"connector"})

	// Login as viewer to get a token
	token := loginUser(t, server, "viewer", "viewerpassword123")

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/users/:uid/reset-password", server.requireAdmin(), server.handleResetPassword)

	// Try to reset target user's password
	body, _ := json.Marshal(map[string]string{
		"new_password": "newpassword123",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/"+targetUser.UID.String()+"/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should be forbidden (requireAdmin middleware)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}
}

func TestResetPassword_WeakPasswordRejected(t *testing.T) {
	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Create a target user
	targetUser := createTestUser(t, dataStore, "target", "targetpassword123", []string{"connector"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/users/:uid/reset-password", server.requireAdmin(), server.handleResetPassword)

	// Try to reset with a weak password
	body, _ := json.Marshal(map[string]string{
		"new_password": "short",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/"+targetUser.UID.String()+"/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should be bad request
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 Bad Request, got %d: %s", w.Code, w.Body.String())
	}

	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if response["error"] != "weak_password" {
		t.Errorf("expected error 'weak_password', got %q", response["error"])
	}
}

func TestResetPassword_UserNotFound(t *testing.T) {
	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/users/:uid/reset-password", server.requireAdmin(), server.handleResetPassword)

	// Try to reset a non-existent user's password
	body, _ := json.Marshal(map[string]string{
		"new_password": "newpassword123",
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/00000000-0000-0000-0000-000000000000/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	// Should be not found
	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404 Not Found, got %d: %s", w.Code, w.Body.String())
	}
}

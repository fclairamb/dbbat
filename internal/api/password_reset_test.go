package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// testTemplateDB is migrated once and then only ever used as the source of
// CREATE DATABASE ... TEMPLATE. Nothing may hold a connection to it: PostgreSQL
// refuses to copy a template another session is attached to.
const testTemplateDB = "dbbat_api_template"

// testMaxConns is the connection budget one test store gets.
//
// store.New sizes its pool for a server, not for a hundred of them: the whole
// package now runs in parallel, every test opens its own store, and 25
// connections each blows past PostgreSQL's max_connections long before the
// tests do anything interesting ("sorry, too many clients already"). A test
// store issues a handful of statements at a time, so a small pool costs it
// nothing. testContainerMaxConns raises the server-side ceiling to match.
const (
	testMaxConns          = 4
	testContainerMaxConns = "300"
)

var (
	testContainer       *postgres.PostgresContainer
	testDSN             string
	testAdminDB         *sql.DB
	testDBSeq           atomic.Uint64
	containerOnce       sync.Once
	errContainerStartup error

	errTemplateBusy = errors.New("template database still has open connections")
)

// setupPostgresContainer starts the PostgreSQL container shared by this package
// and prepares the migrated template every test is cloned from.
func setupPostgresContainer(t *testing.T) {
	t.Helper()

	containerOnce.Do(prepareTestContainer)

	if errContainerStartup != nil {
		t.Fatalf("failed to start postgres container: %v", errContainerStartup)
	}
}

// prepareTestContainer boots the container, opens the admin connection used to
// create and drop the per-test databases, and migrates the template.
func prepareTestContainer() {
	ctx := context.Background()

	testContainer, errContainerStartup = postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithCmdArgs("-c", "max_connections="+testContainerMaxConns),
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
	if errContainerStartup != nil {
		return
	}

	// Attached to the container's default database, never to the template.
	testAdminDB = sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(testDSN)))
	testAdminDB.SetMaxOpenConns(testMaxConns)
	testAdminDB.SetMaxIdleConns(testMaxConns)

	if _, err := testAdminDB.ExecContext(ctx, "CREATE DATABASE "+testTemplateDB); err != nil {
		errContainerStartup = fmt.Errorf("failed to create the template database: %w", err)

		return
	}

	// Migrate exactly once, here. Every test then clones the finished schema,
	// which PostgreSQL does as a file copy instead of replaying the migrations.
	migrateStore, err := store.New(ctx, testDatabaseDSN(testTemplateDB))
	if err != nil {
		errContainerStartup = fmt.Errorf("failed to migrate the template database: %w", err)

		return
	}

	migrateStore.Close()

	errContainerStartup = waitForTemplateIdle(ctx)
}

// testDatabaseDSN points the container DSN at another database on the same
// server.
func testDatabaseDSN(name string) string {
	parsed, err := url.Parse(testDSN)
	if err != nil {
		panic("test container DSN is not a URL: " + err.Error())
	}

	parsed.Path = "/" + name

	return parsed.String()
}

// waitForTemplateIdle blocks until no backend is attached to the template.
// Closing a *sql.DB hands its connections back for teardown; the server-side
// backends disappear a moment later, and CREATE DATABASE ... TEMPLATE fails
// outright if one is still there.
func waitForTemplateIdle(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)

	for {
		var backends int

		row := testAdminDB.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE datname = $1", testTemplateDB)
		if err := row.Scan(&backends); err != nil {
			return fmt.Errorf("failed to count template backends: %w", err)
		}

		if backends == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return errTemplateBusy
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// newIsolatedStore gives the calling test a private, already-migrated database.
//
// Most tests in this package run in parallel against the one shared container.
// They used to share a single database and wipe every table on setup, which
// meant one test's setup deleted a running test's rows — a user disappearing
// mid-test surfaced as foreign-key violations on user_identities and api_keys.
// A database per test removes the shared state instead of trying to time the
// wipes.
func newIsolatedStore(t *testing.T) *store.Store {
	t.Helper()

	setupPostgresContainer(t)

	ctx := context.Background()
	// Built from a literal prefix and a counter: a database name is an
	// identifier, so it cannot go through a placeholder.
	name := fmt.Sprintf("dbbat_api_%d", testDBSeq.Add(1))

	if _, err := testAdminDB.ExecContext(ctx,
		"CREATE DATABASE "+name+" TEMPLATE "+testTemplateDB); err != nil {
		t.Fatalf("failed to create test database %s: %v", name, err)
	}

	dataStore, err := store.New(ctx, testDatabaseDSN(name))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	dataStore.DB().SetMaxOpenConns(testMaxConns)
	dataStore.DB().SetMaxIdleConns(testMaxConns)

	t.Cleanup(func() {
		dataStore.Close()

		// FORCE: a test that failed mid-request can leave a connection behind,
		// and a pinned database would fail the drop and leak.
		if _, err := testAdminDB.ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("failed to drop test database %s: %v", name, err)
		}
	})

	return dataStore
}

// setupTestServer creates an API server with a real database for testing.
func setupTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()

	dataStore := newIsolatedStore(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := &config.Config{
		RunMode: "test",
	}

	server := NewServer(dataStore, nil, logger, cfg)

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
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// Create an admin user
	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
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

	if response["code"] != "FORBIDDEN" {
		t.Errorf("expected error code %q, got %q", "FORBIDDEN", response["code"])
	}
}

func TestResetPassword_AdminCanResetOtherUser(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Create a regular user
	targetUser := createTestUser(t, dataStore, "regularuser", "regularpassword123", []string{"connector"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
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
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// Create a regular user (non-admin)
	createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})

	// Create another user to be the target
	targetUser := createTestUser(t, dataStore, "target", "targetpassword123", []string{"connector"})

	// Login as viewer to get a token
	token := loginUser(t, server, "viewer", "viewerpassword123")

	// Setup router with auth middleware
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
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Create a target user
	targetUser := createTestUser(t, dataStore, "target", "targetpassword123", []string{"connector"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
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

	if response["code"] != "WEAK_PASSWORD" {
		t.Errorf("expected error code 'WEAK_PASSWORD', got %q", response["code"])
	}
}

func TestResetPassword_UserNotFound(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// Create an admin user
	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})

	// Login as admin to get a token
	token := loginUser(t, server, "admin", "adminpassword123")

	// Setup router with auth middleware
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

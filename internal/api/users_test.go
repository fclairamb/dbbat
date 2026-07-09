package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newUsersTestRouter mounts the user update/delete routes with the same
// middleware chain as production (see server.go).
func newUsersTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.PUT("/api/v1/users/:uid", server.handleUpdateUser)
	router.DELETE("/api/v1/users/:uid", server.requireAdmin(), server.handleDeleteUser)
	return router
}

// doUpdateUserRoles performs a PUT /users/:uid with a roles payload.
func doUpdateUserRoles(router *gin.Engine, token, uid string, roles []string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"roles": roles})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+uid, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestUpdateUser_LastAdminDemotionRejected(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserRoles(router, token, adminUser.UID.String(), []string{"connector"})

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d: %s", w.Code, w.Body.String())
	}

	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if response["code"] != "CONFLICT" {
		t.Errorf("expected error code %q, got %q", "CONFLICT", response["code"])
	}

	// Roles must be unchanged
	user, err := dataStore.GetUserByUID(context.Background(), adminUser.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	if !user.IsAdmin() {
		t.Error("admin role should not have been removed")
	}
}

func TestUpdateUser_LastAdminEmptyRolesRejected(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserRoles(router, token, adminUser.UID.String(), []string{})

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateUser_DemotionAllowedWithAnotherAdmin(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	secondAdmin := createTestUser(t, dataStore, "admin2", "adminpassword456", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserRoles(router, token, secondAdmin.UID.String(), []string{"viewer"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	user, err := dataStore.GetUserByUID(context.Background(), secondAdmin.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	if user.IsAdmin() {
		t.Error("admin role should have been removed")
	}
	if !user.IsViewer() {
		t.Error("viewer role should have been added")
	}
}

func TestUpdateUser_PromoteAddsAdminRole(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	target := createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserRoles(router, token, target.UID.String(), []string{"admin", "connector"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	user, err := dataStore.GetUserByUID(context.Background(), target.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	if !user.IsAdmin() {
		t.Error("admin role should have been added")
	}
	if !user.IsConnector() {
		t.Error("connector role should have been kept")
	}
}

func TestUpdateUser_NonAdminCannotClearOwnRoles(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	viewerUser := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	token := loginUser(t, server, "viewer", "viewerpassword123")
	router := newUsersTestRouter(server)

	// An explicit empty roles array is also a roles change and must be refused
	w := doUpdateUserRoles(router, token, viewerUser.UID.String(), []string{})

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}

	user, err := dataStore.GetUserByUID(context.Background(), viewerUser.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	if !user.IsViewer() {
		t.Error("viewer role should not have been removed")
	}
}

func TestDeleteUser_LastAdminRejected(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})
	token := loginUser(t, server, "connector", "connectorpassword123")

	// Mount the handler without requireAdmin to exercise its own last-admin
	// guard directly: through the production chain the scenario is unreachable
	// (the actor is always a second admin), the guard is defense in depth.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.DELETE("/api/v1/users/:uid", server.handleDeleteUser)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+adminUser.UID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := dataStore.GetUserByUID(context.Background(), adminUser.UID); err != nil {
		t.Errorf("admin user should not have been deleted: %v", err)
	}
}

func TestDeleteUser_AdminCanDeleteOtherAdmin(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	secondAdmin := createTestUser(t, dataStore, "admin2", "adminpassword456", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+secondAdmin.UID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	if _, err := dataStore.GetUserByUID(context.Background(), secondAdmin.UID); err == nil {
		t.Error("second admin should have been deleted")
	}
}

func TestDeleteUser_NotFound(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 Not Found, got %d: %s", w.Code, w.Body.String())
	}
}

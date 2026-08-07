package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
)

// newUsersTestRouter mounts the user read/update/delete routes with the same
// middleware chain as production (see server.go).
func newUsersTestRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/users", server.handleListUsers)
	router.GET("/api/v1/users/:uid", server.handleGetUser)
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

func TestUpdateUser_LastAdminDemotionRejected(t *testing.T) {
	t.Parallel()

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

func TestUpdateUser_LastAdminEmptyRolesRejected(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserRoles(router, token, adminUser.UID.String(), []string{})

	if w.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateUser_DemotionAllowedWithAnotherAdmin(t *testing.T) {
	t.Parallel()

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

func TestUpdateUser_PromoteAddsAdminRole(t *testing.T) {
	t.Parallel()

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

func TestUpdateUser_NonAdminCannotClearOwnRoles(t *testing.T) {
	t.Parallel()

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

func TestDeleteUser_LastAdminRejected(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})
	token := loginUser(t, server, "connector", "connectorpassword123")

	// Mount the handler without requireAdmin to exercise its own last-admin
	// guard directly: through the production chain the scenario is unreachable
	// (the actor is always a second admin), the guard is defense in depth.
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

func TestDeleteUser_AdminCanDeleteOtherAdmin(t *testing.T) {
	t.Parallel()

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

func TestDeleteUser_NotFound(t *testing.T) {
	t.Parallel()

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

// doUpdateUserPassword performs a PUT /users/:uid with a new-password payload.
func doUpdateUserPassword(router *gin.Engine, token, uid, newPassword string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"password": newPassword})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+uid, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestUpdateUser_DemoModeAdminPasswordChangeRejected(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.config.RunMode = config.RunModeDemo

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	token := loginUser(t, server, "admin", "adminpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserPassword(router, token, adminUser.UID.String(), "newpassword456")

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 Forbidden, got %d: %s", w.Code, w.Body.String())
	}

	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if response["code"] != "FORBIDDEN" {
		t.Errorf("expected error code %q, got %q", "FORBIDDEN", response["code"])
	}

	// Password must be unchanged
	user, err := dataStore.GetUserByUID(context.Background(), adminUser.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	ok, err := crypto.VerifyPassword(user.PasswordHash, "adminpassword123")
	if err != nil {
		t.Fatalf("failed to verify password: %v", err)
	}
	if !ok {
		t.Error("admin password should not have been changed")
	}
}

func TestUpdateUser_DemoModeNonAdminPasswordChangeAllowed(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.config.RunMode = config.RunModeDemo

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	viewerUser := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	token := loginUser(t, server, "viewer", "viewerpassword123")
	router := newUsersTestRouter(server)

	w := doUpdateUserPassword(router, token, viewerUser.UID.String(), "newviewerpassword456")

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	user, err := dataStore.GetUserByUID(context.Background(), viewerUser.UID)
	if err != nil {
		t.Fatalf("failed to refetch user: %v", err)
	}
	ok, err := crypto.VerifyPassword(user.PasswordHash, "newviewerpassword456")
	if err != nil {
		t.Fatalf("failed to verify password: %v", err)
	}
	if !ok {
		t.Error("viewer password should have been changed")
	}
}

// doGetUser performs a GET /users/:uid as the holder of token.
func doGetUser(router *gin.Engine, token, uid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+uid, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestGetUser_Authorization pins the detail endpoint's visibility to the same
// policy the list endpoint implements: admins and viewers see everyone, anyone
// else sees only themselves. Before this, any authenticated caller — a
// connector, or anything holding a dbb_ key — could read another user's row
// and group membership by UID.
func TestGetUser_Authorization(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	adminUser := createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	viewerUser := createTestUser(t, dataStore, "viewer", "viewerpassword123", []string{"viewer"})
	connectorUser := createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})
	otherConnector := createTestUser(t, dataStore, "connector2", "connectorpassword456", []string{"connector"})

	adminToken := loginUser(t, server, "admin", "adminpassword123")
	viewerToken := loginUser(t, server, "viewer", "viewerpassword123")
	connectorToken := loginUser(t, server, "connector", "connectorpassword123")

	router := newUsersTestRouter(server)

	tests := []struct {
		name     string
		token    string
		targetID string
		want     int
	}{
		{"admin reads another user", adminToken, connectorUser.UID.String(), http.StatusOK},
		{"admin reads themselves", adminToken, adminUser.UID.String(), http.StatusOK},
		// The list endpoint lets viewers see every user, so the detail
		// endpoint must too — a viewer is a read-only operator role.
		{"viewer reads another user", viewerToken, connectorUser.UID.String(), http.StatusOK},
		{"viewer reads themselves", viewerToken, viewerUser.UID.String(), http.StatusOK},
		{"connector reads themselves", connectorToken, connectorUser.UID.String(), http.StatusOK},
		// 404, not 403: a 403 would confirm the account exists.
		{"connector reads another user", connectorToken, otherConnector.UID.String(), http.StatusNotFound},
		{"connector reads an admin", connectorToken, adminUser.UID.String(), http.StatusNotFound},
		{"connector reads an unknown uid", connectorToken, uuid.NewString(), http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := doGetUser(router, tt.token, tt.targetID)
			if w.Code != tt.want {
				t.Fatalf("expected status %d, got %d: %s", tt.want, w.Code, w.Body.String())
			}
		})
	}
}

// TestGetUser_ForeignUIDLeaksNothing checks the refusal body as well as its
// status: a connector denied another user's row must not learn the username,
// the roles, or the group membership that would have been returned.
func TestGetUser_ForeignUIDLeaksNothing(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin", "adminpassword123", []string{"admin"})
	target := createTestUser(t, dataStore, "secret-target", "targetpassword123", []string{"viewer"})
	createTestUser(t, dataStore, "connector", "connectorpassword123", []string{"connector"})

	connectorToken := loginUser(t, server, "connector", "connectorpassword123")
	router := newUsersTestRouter(server)

	w := doGetUser(router, connectorToken, target.UID.String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "secret-target") || strings.Contains(body, "groups") {
		t.Errorf("the refusal must not describe the user, got %s", body)
	}

	// And the list endpoint agrees: a connector only ever sees themselves.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+connectorToken)
	listRec := httptest.NewRecorder()
	router.ServeHTTP(listRec, req)

	var listResp struct {
		Users []struct {
			Username string `json:"username"`
		} `json:"users"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if len(listResp.Users) != 1 || listResp.Users[0].Username != "connector" {
		t.Errorf("a connector must only list themselves, got %+v", listResp.Users)
	}
}

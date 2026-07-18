package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// newConnectionsTestRouter mounts the connection routes with the same
// middleware chain as production (see server.go: authenticated.GET
// "/connections/:uid" with no extra role gate — ownership is checked
// inside the handler itself, same as the list endpoint).
func newConnectionsTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/connections/:uid", server.handleGetConnection)
	return router
}

// doGetConnection performs a GET /connections/{uid} with the given token.
func doGetConnection(router *gin.Engine, token, uid string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+uid, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestGetConnection_OwnerSeesOwnConnection verifies a connector fetching
// their own connection gets it back in full.
func TestGetConnection_OwnerSeesOwnConnection(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "gcown"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	token := loginUser(t, server, "owner-"+suffix, "ownerpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.1.1.1")
	require.NoError(t, err)

	router := newConnectionsTestRouter(server)
	w := doGetConnection(router, token, conn.UID.String())

	require.Equal(t, http.StatusOK, w.Code, "response body: %s", w.Body.String())

	var got store.Connection
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, conn.UID, got.UID)
	require.Equal(t, owner.UID, got.UserID)
	require.Equal(t, db.UID, got.DatabaseID)
}

// TestGetConnection_NonOwnerConnectorGets404NotForbidden verifies that a
// non-admin/non-viewer user fetching another user's connection is reported
// as 404, not 403 — connectors must not be able to distinguish "doesn't
// exist" from "exists but isn't mine".
func TestGetConnection_NonOwnerConnectorGets404NotForbidden(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "gcother"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "other-"+suffix, "otherpass123", []string{store.RoleConnector})
	otherToken := loginUser(t, server, "other-"+suffix, "otherpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.1.1.2")
	require.NoError(t, err)

	router := newConnectionsTestRouter(server)
	w := doGetConnection(router, otherToken, conn.UID.String())

	require.Equal(t, http.StatusNotFound, w.Code, "response body: %s", w.Body.String())
}

// TestGetConnection_AdminAndViewerSeeAnyConnection verifies both admin and
// viewer roles can fetch a connection they don't own.
func TestGetConnection_AdminAndViewerSeeAnyConnection(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "gcprivileged"

	owner := createTestUser(t, dataStore, "owner-"+suffix, "ownerpass123", []string{store.RoleConnector})
	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	createTestUser(t, dataStore, "viewer-"+suffix, "viewerpass123", []string{store.RoleViewer})
	viewerToken := loginUser(t, server, "viewer-"+suffix, "viewerpass123")

	db := createTestDBEntry(t, dataStore, "db-"+suffix, true)
	conn, err := dataStore.CreateConnection(t.Context(), owner.UID, db.UID, "10.1.1.3")
	require.NoError(t, err)

	router := newConnectionsTestRouter(server)

	adminResp := doGetConnection(router, adminToken, conn.UID.String())
	require.Equal(t, http.StatusOK, adminResp.Code, "admin response body: %s", adminResp.Body.String())

	viewerResp := doGetConnection(router, viewerToken, conn.UID.String())
	require.Equal(t, http.StatusOK, viewerResp.Code, "viewer response body: %s", viewerResp.Body.String())
}

// TestGetConnection_NotFound verifies a UID with no matching connection is
// reported as 404, regardless of role.
func TestGetConnection_NotFound(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "gcnf"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newConnectionsTestRouter(server)
	w := doGetConnection(router, token, "00000000-0000-0000-0000-000000000000")

	require.Equal(t, http.StatusNotFound, w.Code, "response body: %s", w.Body.String())
}

// TestGetConnection_InvalidUID verifies a malformed UID is rejected as a
// 400, not a 404 or 500.
func TestGetConnection_InvalidUID(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	suffix := "gcbaduid"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	router := newConnectionsTestRouter(server)
	w := doGetConnection(router, token, "not-a-uuid")

	require.Equal(t, http.StatusBadRequest, w.Code, "response body: %s", w.Body.String())
}

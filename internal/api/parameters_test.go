package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// setupInstanceRouter wires the /instance GET and /instance/public PUT
// routes behind the real auth + admin-gating middleware, matching the
// production mounting in server.go.
func setupInstanceRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/instance", server.handleGetInstance)
	router.PUT("/api/v1/instance/public", server.requireAdmin(), server.handleUpdateInstancePublic)

	return router
}

// getInstance performs an authenticated GET /instance and returns the
// decoded JSON body as a generic map, so tests can assert on the presence
// (or absence) of the admin-only "public" key as well as field values.
func getInstance(t *testing.T, router *gin.Engine, token string) map[string]any {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/instance", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	return resp
}

// TestHandleGetInstance_WebUIURL_ResolvedForAllCallers_PublicAdminOnly
// covers spec item: GET /instance must return web_ui_url in "resolved" for
// every authenticated caller, but only expose the raw "public" block
// (including its web_ui_url) to admins.
func TestHandleGetInstance_WebUIURL_ResolvedForAllCallers_PublicAdminOnly(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	const wantWebUIURL = "https://instance-test.example.com"
	require.NoError(t, dataStore.SetPublicEndpoints(ctx, store.PublicEndpoints{WebUIURL: wantWebUIURL}))
	t.Cleanup(func() {
		_ = dataStore.DeleteParameter(ctx, store.GroupPublic, store.KeyPublicWebUIURL)
	})

	createTestUser(t, dataStore, "admin-inst", "adminpass123", []string{store.RoleAdmin, store.RoleConnector})
	createTestUser(t, dataStore, "conn-inst", "connpass123", []string{store.RoleConnector})

	adminToken := loginUser(t, server, "admin-inst", "adminpass123")
	connToken := loginUser(t, server, "conn-inst", "connpass123")

	router := setupInstanceRouter(server)

	t.Run("admin sees resolved and raw public web_ui_url", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		resp := getInstance(t, router, adminToken)

		resolved, ok := resp["resolved"].(map[string]any)
		require.True(t, ok, "resolved must be an object")
		assert.Equal(t, wantWebUIURL, resolved["web_ui_url"])

		public, ok := resp["public"].(map[string]any)
		require.True(t, ok, "admin response must include the public object")
		assert.Equal(t, wantWebUIURL, public["web_ui_url"])
	})

	t.Run("non-admin sees resolved web_ui_url but no public block", func(t *testing.T) { //nolint:paralleltest // shared server/router state
		resp := getInstance(t, router, connToken)

		resolved, ok := resp["resolved"].(map[string]any)
		require.True(t, ok, "resolved must be an object")
		assert.Equal(t, wantWebUIURL, resolved["web_ui_url"])

		_, hasPublic := resp["public"]
		assert.False(t, hasPublic, "non-admin response must not include the public block")
	})
}

// TestHandleUpdateInstancePublic_WebUIURLRoundTrip covers spec item: PUT
// /instance/public sets web_ui_url and a subsequent GET /instance reflects
// it in both the raw public block and the resolved value.
func TestHandleUpdateInstancePublic_WebUIURLRoundTrip(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	t.Cleanup(func() {
		_ = dataStore.DeleteParameter(ctx, store.GroupPublic, store.KeyPublicWebUIURL)
	})

	createTestUser(t, dataStore, "admin-put", "adminpass123", []string{store.RoleAdmin, store.RoleConnector})
	adminToken := loginUser(t, server, "admin-put", "adminpass123")

	router := setupInstanceRouter(server)

	const wantWebUIURL = "https://roundtrip.example.com"
	body, err := json.Marshal(map[string]string{"web_ui_url": wantWebUIURL})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/instance/public", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	resp := getInstance(t, router, adminToken)

	resolved, ok := resp["resolved"].(map[string]any)
	require.True(t, ok, "resolved must be an object")
	assert.Equal(t, wantWebUIURL, resolved["web_ui_url"])

	public, ok := resp["public"].(map[string]any)
	require.True(t, ok, "admin response must include the public object")
	assert.Equal(t, wantWebUIURL, public["web_ui_url"])
}

// TestPublicURLForMessage_LiveStoreOverridesConfig covers spec item: the
// live-store branch of publicURLForMessage (slack_interactions.go) — when
// the server has a store and public.web_ui_url is set, that value wins over
// the config.PublicURL env-var fallback. Existing tests only exercise the
// nil-store fallback via testServer(), which never sets s.store.
func TestPublicURLForMessage_LiveStoreOverridesConfig(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	t.Cleanup(func() {
		_ = dataStore.DeleteParameter(ctx, store.GroupPublic, store.KeyPublicWebUIURL)
	})

	// Give config a distinct, non-empty fallback so the assertion actually
	// proves the store value wins rather than passing by coincidence.
	server.config.PublicURL = "https://env-fallback.example.com"

	const wantWebUIURL = "https://livestore.example.com"
	require.NoError(t, dataStore.SetPublicEndpoints(ctx, store.PublicEndpoints{WebUIURL: wantWebUIURL}))

	got := server.publicURLForMessage(ctx)
	assert.Equal(t, wantWebUIURL, got)
}

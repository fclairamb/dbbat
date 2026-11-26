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

func newListSortRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/connections", server.handleListConnections)
	router.GET("/api/v1/queries", server.requireAdminOrViewer(), server.handleListQueries)

	return router
}

func doGetList(router *gin.Engine, token, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	return w
}

func TestListSort_RejectsBadParameters(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	createTestUser(t, dataStore, "sortadmin", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "sortadmin", "adminpass123")
	router := newListSortRouter(server)

	uid := "0194a1b2-0000-7000-8000-000000000000"

	for _, path := range []string{
		"/api/v1/connections?sort=nope",
		"/api/v1/connections?sort=duration&order=sideways",
		"/api/v1/connections?sort=duration&before=" + uid,
		"/api/v1/connections?order=sideways",
		"/api/v1/connections?sort=duration_ms", // a queries key
		"/api/v1/queries?sort=nope",
		"/api/v1/queries?sort=duration_ms&order=sideways",
		"/api/v1/queries?sort=rows_affected&before=" + uid,
		"/api/v1/queries?sort=queries", // a connections key
	} {
		w := doGetList(router, token, path)
		require.Equal(t, http.StatusBadRequest, w.Code, "%s: %s", path, w.Body.String())
	}

	// The default key keeps the cursor valid, and valid keys are accepted.
	for _, path := range []string{
		"/api/v1/connections?sort=connected_at&before=" + uid,
		"/api/v1/connections?sort=bytes_transferred&order=asc&offset=1",
		"/api/v1/queries?sort=executed_at&order=asc&before=" + uid,
		"/api/v1/queries?sort=duration_ms&order=desc&offset=1",
	} {
		w := doGetList(router, token, path)
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", path, w.Body.String())
	}
}

func TestListConnectionsSort_ConnectorStaysScoped(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	mine := createTestUser(t, dataStore, "sortmine", "minepass1234", []string{store.RoleConnector})
	other := createTestUser(t, dataStore, "sortother", "otherpass123", []string{store.RoleConnector})
	token := loginUser(t, server, "sortmine", "minepass1234")
	db := createTestDBEntry(t, dataStore, "db_sortscope", true)

	_, err := dataStore.CreateConnection(t.Context(), mine.UID, db.UID, "10.2.2.1")
	require.NoError(t, err)
	_, err = dataStore.CreateConnection(t.Context(), other.UID, db.UID, "10.2.2.2")
	require.NoError(t, err)

	router := newListSortRouter(server)
	w := doGetList(router, token, "/api/v1/connections?sort=duration&order=asc&user_id="+other.UID.String())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Connections []store.Connection `json:"connections"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Connections, 1)
	require.Equal(t, mine.UID, body.Connections[0].UserID)
}

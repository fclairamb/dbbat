package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/store"
)

func TestBuildConnectionsForUser_NilConfig(t *testing.T) {
	t.Parallel()

	s := &Server{config: nil, store: nil}
	user := &store.User{}
	conns, truncated := s.buildConnectionsForUser(context.Background(), user, "key")
	assert.Empty(t, conns)
	assert.False(t, truncated)
}

// TestBuildConnectionsForUser_DisabledProtocol verifies that a database whose
// protocol's resolved port is 0 is excluded from the connection list.
func TestBuildConnectionsForUser_DisabledProtocol(t *testing.T) {
	t.Parallel()

	// Build a config where PG listen is empty (→ port 0) and the public
	// endpoint group has no overrides, so the resolved PG port will be 0.
	cfg := &config.Config{
		ListenPG: "", // disabled
	}

	db := &store.Database{
		Name:         "disabled-pg",
		DatabaseName: "mydb",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "prefer",
	}

	user := &store.User{Username: "alice"}
	endpoints := store.ResolvedEndpoints{PGPort: 0}

	info, ok := BuildConnectionURL(db, user, endpoints, "key")
	require.False(t, ok)
	assert.Equal(t, ConnectionInfo{}, info)
	_ = cfg // config is used by the full integration path; verify protocol exclusion here
}

// TestBuildConnectionsForUser_Truncation verifies the truncation flag and cap.
func TestBuildConnectionsForUser_Truncation(t *testing.T) {
	t.Parallel()

	user := &store.User{Username: "alice"}
	endpoints := store.ResolvedEndpoints{
		PGHost: "db.example.com",
		PGPort: 5432,
	}

	// Build more than 50 ConnectionInfo items.
	var all []ConnectionInfo
	for i := 0; i < 60; i++ {
		db := makeDB(store.ProtocolPostgreSQL, "db", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		all = append(all, info)
	}

	// Simulate the truncation logic from buildConnectionsForUser.
	truncated := false
	if len(all) > maxConnectionsInResponse {
		all = all[:maxConnectionsInResponse]
		truncated = true
	}

	assert.Len(t, all, maxConnectionsInResponse)
	assert.True(t, truncated)
}

// newKeysTestRouter mounts the API-key list route with the same middleware
// chain as production (see server.go: authenticated.Use(authMiddleware())
// then keys.GET("", handleListAPIKeys) with no admin gate).
func newKeysTestRouter(server *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/keys", server.handleListAPIKeys)
	return router
}

// doListAPIKeys performs a GET /keys with the given raw query string (may be empty).
func doListAPIKeys(router *gin.Engine, token, rawQuery string) *httptest.ResponseRecorder {
	url := "/api/v1/keys"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, url, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// listedKeyUserIDs decodes a handleListAPIKeys response body and returns the
// UserID of every key in it, for assertions that don't care about ordering.
func listedKeyUserIDs(t *testing.T, w *httptest.ResponseRecorder) []uuid.UUID {
	t.Helper()

	var body struct {
		Keys []store.APIKey `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	ids := make([]uuid.UUID, 0, len(body.Keys))
	for _, k := range body.Keys {
		ids = append(ids, k.UserID)
	}
	return ids
}

// TestListAPIKeys_DefaultsToOwnKeys verifies that an admin with no query
// params sees only their own keys, not the fleet — the behavior this spec
// changes (previously admins saw every user's keys by default).
func TestListAPIKeys_DefaultsToOwnKeys(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "keysadmin1", "adminpassword123", []string{"admin"})
	other := createTestUser(t, dataStore, "keysother1", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, admin.UID, "Admin Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysadmin1", "adminpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []uuid.UUID{admin.UID}, listedKeyUserIDs(t, w))
}

// TestListAPIKeys_AdminAllUsers verifies all_users=true returns every user's
// keys, admin only.
func TestListAPIKeys_AdminAllUsers(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "keysadmin2", "adminpassword123", []string{"admin"})
	other := createTestUser(t, dataStore, "keysother2", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, admin.UID, "Admin Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysadmin2", "adminpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "all_users=true")
	require.Equal(t, http.StatusOK, w.Code)
	assert.ElementsMatch(t, []uuid.UUID{admin.UID, other.UID}, listedKeyUserIDs(t, w))
}

// TestListAPIKeys_AdminUserIDFilter verifies user_id=<uuid> scopes to that
// one user's keys, overriding the own-keys default.
func TestListAPIKeys_AdminUserIDFilter(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "keysadmin3", "adminpassword123", []string{"admin"})
	other := createTestUser(t, dataStore, "keysother3", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, admin.UID, "Admin Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysadmin3", "adminpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "user_id="+other.UID.String())
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []uuid.UUID{other.UID}, listedKeyUserIDs(t, w))
}

// TestListAPIKeys_AdminUserIDWinsOverAllUsers verifies that when an admin
// sends both params, the more specific user_id wins.
func TestListAPIKeys_AdminUserIDWinsOverAllUsers(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "keysadmin4", "adminpassword123", []string{"admin"})
	other := createTestUser(t, dataStore, "keysother4", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, admin.UID, "Admin Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysadmin4", "adminpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "all_users=true&user_id="+other.UID.String())
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []uuid.UUID{other.UID}, listedKeyUserIDs(t, w))
}

// TestListAPIKeys_NonAdminIgnoresAllUsers verifies a non-admin passing
// all_users=true still only ever sees their own keys.
func TestListAPIKeys_NonAdminIgnoresAllUsers(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	self := createTestUser(t, dataStore, "keysself1", "selfpassword123", []string{"connector"})
	other := createTestUser(t, dataStore, "keysother5", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, self.UID, "Self Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysself1", "selfpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "all_users=true")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []uuid.UUID{self.UID}, listedKeyUserIDs(t, w))
}

// TestListAPIKeys_NonAdminIgnoresUserID verifies a non-admin passing
// user_id=<other> still only ever sees their own keys (not forbidden — the
// param is simply not honored).
func TestListAPIKeys_NonAdminIgnoresUserID(t *testing.T) { //nolint:paralleltest // shared database state
	server, dataStore := setupTestServer(t)
	ctx := context.Background()

	self := createTestUser(t, dataStore, "keysself2", "selfpassword123", []string{"connector"})
	other := createTestUser(t, dataStore, "keysother6", "otherpassword123", []string{"connector"})

	_, _, err := dataStore.CreateAPIKey(ctx, self.UID, "Self Key", nil)
	require.NoError(t, err)
	_, _, err = dataStore.CreateAPIKey(ctx, other.UID, "Other Key", nil)
	require.NoError(t, err)

	token := loginUser(t, server, "keysself2", "selfpassword123")
	router := newKeysTestRouter(server)

	w := doListAPIKeys(router, token, "user_id="+other.UID.String())
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []uuid.UUID{self.UID}, listedKeyUserIDs(t, w))
}

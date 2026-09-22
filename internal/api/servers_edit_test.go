package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// serverReferencesBody is the shape GET /servers/{uid}/references answers with.
type serverReferencesBody struct {
	ActiveGrants int64 `json:"active_grants"`
	ServerGroups int64 `json:"server_groups"`
}

// serverEditRouter wires the two routes this file exercises behind the real
// admin guard, so the tests run the production authorization path.
func serverEditRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.PUT("/api/v1/servers/:uid", server.requireAdmin(), server.handleUpdateDatabase)
	router.GET("/api/v1/servers/:uid/references", server.requireAdmin(), server.handleGetServerReferences)

	return router
}

func newEditTestServerRow(t *testing.T, dataStore *store.Store, name string) *store.Server {
	t.Helper()

	created, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name: name, Host: "db.internal", Port: 5432,
		DatabaseName: "app", Username: "app", Password: "pw",
		SSLMode: "disable", Protocol: store.ProtocolPostgreSQL, Listable: true,
	}, dbTestEncryptionKey)
	require.NoError(t, err)

	return created
}

// TestServerReferencesReportsWhatAnEditMoves pins the payload the edit dialog
// warns from: the live grants that would reach the new target, and the groups
// carrying the row.
func TestServerReferencesReportsWhatAnEditMoves(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	ctx := context.Background()

	admin := createTestUser(t, dataStore, "admin-refs", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-refs", "adminpass123")

	row := newEditTestServerRow(t, dataStore, "refs_target")

	read := func() serverReferencesBody {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers/"+row.UID.String()+"/references", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		serverEditRouter(server).ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var body serverReferencesBody
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

		return body
	}

	body := read()
	assert.Equal(t, int64(0), body.ActiveGrants)
	assert.Equal(t, int64(0), body.ServerGroups)

	user := createTestUser(t, dataStore, "holder-refs", "userpass123", []string{store.RoleConnector})
	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name: "refs-def", Slug: "refs-def", DurationSeconds: 3600, CreatedBy: admin.UID,
	})
	require.NoError(t, err)

	grant := store.BuildGrantFromDefinition(def, user.UID, row.UID, admin.UID, time.Now().Add(-time.Minute))
	_, err = dataStore.CreateGrant(ctx, grant)
	require.NoError(t, err)

	group, err := dataStore.CreateServerGroup(ctx, &store.ServerGroup{Name: "refs-grp", CreatedBy: &admin.UID})
	require.NoError(t, err)
	require.NoError(t, dataStore.AddServerToGroup(ctx, group.UID, row.UID))

	body = read()
	assert.Equal(t, int64(1), body.ActiveGrants, "the live grant would reach the new target")
	assert.Equal(t, int64(1), body.ServerGroups)
}

// TestServerReferencesUnknownRowIs404 keeps the endpoint from inventing zeros
// for a row that is not there.
func TestServerReferencesUnknownRowIs404(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	createTestUser(t, dataStore, "admin-refs404", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-refs404", "adminpass123")

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/servers/00000000-0000-0000-0000-000000000009/references", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	serverEditRouter(server).ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func putServer(t *testing.T, server *Server, token, uid string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	encoded, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/servers/"+uid, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serverEditRouter(server).ServeHTTP(w, req)

	return w
}

// TestProtocolChangeRefusedOnceTheRowHasHistory is the invariant the UI's edit
// form expresses by omitting the field: a row that has been granted or
// connected to cannot become a different kind of server, because the grants,
// sessions and query chains under its uid would re-label themselves.
func TestProtocolChangeRefusedOnceTheRowHasHistory(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	ctx := context.Background()

	createTestUser(t, dataStore, "admin-proto", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-proto", "adminpass123")

	user := createTestUser(t, dataStore, "connector-proto", "userpass123", []string{store.RoleConnector})
	row := newEditTestServerRow(t, dataStore, "proto_used")

	conn, err := dataStore.CreateConnection(ctx, user.UID, row.UID, "10.0.0.1")
	require.NoError(t, err)
	require.NoError(t, dataStore.CloseConnection(ctx, conn.UID))

	w := putServer(t, server, token, row.UID.String(), map[string]any{"protocol": "mysql"})
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cannot change the protocol")

	after, err := dataStore.GetServerByUID(ctx, row.UID)
	require.NoError(t, err)
	assert.Equal(t, store.ProtocolPostgreSQL, after.Protocol, "the refusal must not have half-applied")

	// Every other field is still editable on the very same row: the guard is
	// about the protocol alone, not about freezing a used server.
	w = putServer(t, server, token, row.UID.String(), map[string]any{"host": "moved.internal"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after, err = dataStore.GetServerByUID(ctx, row.UID)
	require.NoError(t, err)
	assert.Equal(t, "moved.internal", after.Host)
}

// TestProtocolChangeAllowedOnAPristineRow keeps the create-dialog typo a
// one-click fix: nothing references the row, so nothing is re-labeled.
func TestProtocolChangeAllowedOnAPristineRow(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	ctx := context.Background()

	createTestUser(t, dataStore, "admin-proto2", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-proto2", "adminpass123")

	row := newEditTestServerRow(t, dataStore, "proto_pristine")

	w := putServer(t, server, token, row.UID.String(), map[string]any{"protocol": "mysql"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	after, err := dataStore.GetServerByUID(ctx, row.UID)
	require.NoError(t, err)
	assert.Equal(t, "mysql", after.Protocol)

	// Repeating the current protocol is never a change, so it is never
	// refused, even once the row is used.
	user := createTestUser(t, dataStore, "connector-proto2", "userpass123", []string{store.RoleConnector})
	conn, err := dataStore.CreateConnection(ctx, user.UID, row.UID, "10.0.0.2")
	require.NoError(t, err)
	require.NoError(t, dataStore.CloseConnection(ctx, conn.UID))

	w = putServer(t, server, token, row.UID.String(), map[string]any{"protocol": "mysql", "host": "still.here"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestUpdateAuditRecordsOnlyTheFieldsSent is what makes the edit form's
// changed-fields-only submit worth doing: the audit entry lists the real edit
// and says nothing about the name.
func TestUpdateAuditRecordsOnlyTheFieldsSent(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey
	ctx := context.Background()

	createTestUser(t, dataStore, "admin-auditfields", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-auditfields", "adminpass123")

	row := newEditTestServerRow(t, dataStore, "audit_fields")

	w := putServer(t, server, token, row.UID.String(), map[string]any{
		"host":     "new.internal",
		"password": "rotated",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	eventType := "database.updated"
	logs, err := dataStore.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, logs)

	var details struct {
		UpdatedFields map[string]any `json:"updated_fields"`
	}

	require.NoError(t, json.Unmarshal(logs[0].Details, &details))

	assert.Equal(t, "new.internal", details.UpdatedFields["host"])
	assert.Equal(t, true, details.UpdatedFields["password_changed"], "the secret is a marker, never a value")
	assert.NotContains(t, details.UpdatedFields, "name", "a rename-free edit says nothing about the name")
	assert.NotContains(t, details.UpdatedFields, "port")
	assert.NotContains(t, string(logs[0].Details), "rotated")
}

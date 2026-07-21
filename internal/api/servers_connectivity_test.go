package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// connCheckRouter wires the real route (including the admin guard) so the tests
// exercise the same authorization path as production.
func connCheckRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/servers/:uid/test", server.requireAdmin(), server.handleTestServerConnection)

	return router
}

// closedTCPPort returns a host/port nothing is listening on.
func closedTCPPort(t *testing.T) (string, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NoError(t, ln.Close())

	return addr.IP.String(), addr.Port
}

func TestTestServerConnection_UnreachableTarget(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "admin-tsc1", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-tsc1", "adminpass123")

	host, port := closedTCPPort(t)
	created, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name: "unreachable-tsc1", Host: host, Port: port,
		DatabaseName: "app", Username: "app", Password: "s3cr3t-pg-password",
		SSLMode: "disable", Protocol: store.ProtocolPostgreSQL, Listable: true,
	}, dbTestEncryptionKey)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+created.UID.String()+"/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	connCheckRouter(server).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "a failed check is still a 200: the staged result is the answer")

	var res ConnectionTestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))

	assert.False(t, res.OK)
	assert.Equal(t, "target_dial", res.Stage)
	assert.Equal(t, "unreachable", res.Code)
	assert.NotEmpty(t, res.Message)

	// The response must never echo the stored credentials.
	assert.NotContains(t, w.Body.String(), "s3cr3t-pg-password")
}

func TestTestServerConnection_UnreachableBastion(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "admin-tsc2", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-tsc2", "adminpass123")

	host, port := closedTCPPort(t)
	created, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name: "bastion-tsc2", Host: host, Port: port,
		Username: "www-data", Protocol: store.ProtocolSSH, Listable: false,
		ProtocolData: &store.ServerProtocolData{SSH: &store.SSHServerData{
			PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n",
		}},
	}, dbTestEncryptionKey)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+created.UID.String()+"/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	connCheckRouter(server).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var res ConnectionTestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))

	assert.False(t, res.OK)
	// An unparseable key is caught before any packet leaves, so `config` wins
	// over `bastion_dial` here — either way it is never reported as success.
	assert.Equal(t, "config", res.Stage)
	assert.Equal(t, "bad_private_key", res.Code)

	// The stored key material must not come back out.
	assert.NotContains(t, w.Body.String(), "PRIVATE KEY")
}

func TestTestServerConnection_NotFoundAndBadUID(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "admin-tsc3", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-tsc3", "adminpass123")

	router := connCheckRouter(server)

	for name, tc := range map[string]struct {
		uid  string
		want int
	}{
		"unknown uid": {uuid.NewString(), http.StatusNotFound},
		"bad uid":     {"not-a-uuid", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+tc.uid+"/test", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, tc.want, w.Code, name)
	}
}

func TestTestServerConnection_NonAdminForbidden(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "viewer-tsc4", "viewerpass123", []string{store.RoleViewer})
	token := loginUser(t, server, "viewer-tsc4", "viewerpass123")

	db := createTestDBEntry(t, dataStore, "target-tsc4", true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/servers/"+db.UID.String()+"/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	connCheckRouter(server).ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestRedactUpdateForAudit is the security gate on the update audit record: a
// rotated credential must be recorded as *having changed*, never as its value.
func TestRedactUpdateForAudit(t *testing.T) {
	t.Parallel()

	password := "rotated-db-password"
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n-----END OPENSSH PRIVATE KEY-----\n"
	passphrase := "key-passphrase"
	host := "db.internal"

	out := redactUpdateForAudit(UpdateDatabaseRequest{
		Host:          &host,
		Password:      &password,
		SSHPrivateKey: &key,
		SSHPassphrase: &passphrase,
	})

	blob, err := json.Marshal(out)
	require.NoError(t, err)

	rendered := string(blob)
	assert.NotContains(t, rendered, password)
	assert.NotContains(t, rendered, "PRIVATE KEY")
	assert.NotContains(t, rendered, passphrase)

	assert.Equal(t, true, out["password_changed"])
	assert.Equal(t, true, out["ssh_private_key_changed"])
	assert.Equal(t, true, out["ssh_passphrase_changed"])

	// Non-secret fields stay visible — the audit trail still says what changed.
	assert.True(t, strings.Contains(rendered, "db.internal"))
	assert.NotContains(t, out, "database_name")
}

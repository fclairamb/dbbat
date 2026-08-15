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

// createTestOracleEntry inserts an Oracle row claiming service at host:1521.
func createTestOracleEntry(t *testing.T, dataStore *store.Store, name, host, service string) *store.Server {
	t.Helper()

	svc := service
	created, err := dataStore.CreateServer(context.Background(), &store.Server{
		Name:              name,
		Host:              host,
		Port:              1521,
		DatabaseName:      name,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
		OracleServiceName: &svc,
		Listable:          true,
	}, dbTestEncryptionKey)
	require.NoError(t, err, "createTestOracleEntry %q", name)

	return created
}

// TestOracleServiceNameConflict_SurfacedToAdmin is the reachability half of the
// spec: the warning is useless if the frontend cannot see it. Both admin-facing
// read paths — the listing the servers page renders and the per-row GET — must
// carry it on the conflicting rows and omit it everywhere else.
func TestOracleServiceNameConflict_SurfacedToAdmin(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	createTestUser(t, dataStore, "admin-osnc", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-osnc", "adminpass123")

	const (
		conflicting = "MUTU02"
		agreeing    = "MUTU03"
	)

	// One machine, two spellings, one service name — the reported incident.
	conflictA := createTestOracleEntry(t, dataStore, "abyla_mutu02_ro", "oracle-abymutualise02.db.stonal.io", conflicting)
	createTestOracleEntry(t, dataStore, "abyla_mutu02_rw", "abymutualise02.rds.amazonaws.com", conflicting)
	// Two rows agreeing on the address: a legitimate mutualized instance.
	agreeA := createTestOracleEntry(t, dataStore, "abyla_mutu03_a", "oracle-abymutualise03.db.stonal.io", agreeing)
	createTestOracleEntry(t, dataStore, "abyla_mutu03_b", "oracle-abymutualise03.db.stonal.io", agreeing)
	// A PostgreSQL row, which carries no service name at all.
	pgDB := createTestDBEntry(t, dataStore, "pg-osnc", true)

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/databases", server.handleListDatabases)
	router.GET("/api/v1/databases/:uid", server.handleGetDatabase)

	get := func(t *testing.T, path string) map[string]any {
		t.Helper()

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "GET %s", path)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

		return body
	}

	t.Run("the listing flags both conflicting rows and nothing else", func(t *testing.T) {
		t.Parallel()

		body := get(t, "/api/v1/databases")

		databases, ok := body["databases"].([]any)
		require.True(t, ok, "response has no databases array: %v", body)

		flagged := map[string]map[string]any{}

		for _, entry := range databases {
			row, ok := entry.(map[string]any)
			require.True(t, ok)

			if conflict, ok := row["oracle_service_name_conflict"].(map[string]any); ok {
				name, _ := row["name"].(string)
				flagged[name] = conflict
			}
		}

		assert.Contains(t, flagged, "abyla_mutu02_ro")
		assert.Contains(t, flagged, "abyla_mutu02_rw")
		assert.NotContains(t, flagged, "abyla_mutu03_a")
		assert.NotContains(t, flagged, "abyla_mutu03_b")
		assert.NotContains(t, flagged, "pg-osnc")

		conflict := flagged["abyla_mutu02_ro"]
		require.NotNil(t, conflict)
		assert.Equal(t, conflicting, conflict["service_name"])
		assert.Len(t, conflict["upstreams"], 2)
		assert.Len(t, conflict["servers"], 2)
		assert.Contains(t, conflict["message"], "ORA-12514")
	})

	t.Run("the per-row GET carries it too", func(t *testing.T) {
		t.Parallel()

		body := get(t, "/api/v1/databases/"+conflictA.UID.String())
		conflict, ok := body["oracle_service_name_conflict"].(map[string]any)
		require.True(t, ok, "expected a conflict on the row, got %v", body)
		assert.Equal(t, conflicting, conflict["service_name"])
	})

	t.Run("a row whose neighbors agree carries nothing", func(t *testing.T) {
		t.Parallel()

		for _, uid := range []string{agreeA.UID.String(), pgDB.UID.String()} {
			body := get(t, "/api/v1/databases/"+uid)
			assert.NotContains(t, body, "oracle_service_name_conflict")
		}
	})
}

// TestOracleServiceNameConflict_OnCreate covers the provisioning moment: adding
// the second spelling is when the trap is laid, so the create response is where
// the admin should hear about it without a second round trip.
func TestOracleServiceNameConflict_OnCreate(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)

	// The create handler encrypts the target password with s.encryptionKey, and
	// the shared setup wires a nil one.
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "admin-osnc-create", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "admin-osnc-create", "adminpass123")

	const service = "MUTU04"

	createTestOracleEntry(t, dataStore, "abyla_mutu04_first", "oracle-abymutualise04.db.stonal.io", service)

	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/databases", server.handleCreateDatabase)

	payload, err := json.Marshal(map[string]any{
		"name":                "abyla_mutu04_second",
		"host":                "abymutualise04.rds.amazonaws.com",
		"port":                1521,
		"username":            "system",
		"password":            "oracle",
		"protocol":            store.ProtocolOracle,
		"oracle_service_name": service,
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/databases", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	conflict, ok := body["oracle_service_name_conflict"].(map[string]any)
	require.True(t, ok, "expected the create response to warn, got %v", body)
	assert.Equal(t, service, conflict["service_name"])
	assert.Len(t, conflict["upstreams"], 2)
}

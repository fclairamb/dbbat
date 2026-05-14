package api

import (
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

// dbTestEncryptionKey is a 32-byte AES key for API-level database tests.
var dbTestEncryptionKey = []byte("dbtest-key-012345678901234567890")

// createTestDBEntry inserts a database directly via the store for API tests.
func createTestDBEntry(t *testing.T, dataStore *store.Store, name string, listable bool) *store.Database {
	t.Helper()
	db := &store.Database{
		Name:         name,
		Host:         "db.example.com",
		Port:         5432,
		DatabaseName: "testdb",
		Username:     "dbuser",
		Password:     "dbpass",
		SSLMode:      "prefer",
		Protocol:     store.ProtocolPostgreSQL,
		Listable:     listable,
	}
	created, err := dataStore.CreateDatabase(context.Background(), db, dbTestEncryptionKey)
	require.NoError(t, err, "createTestDBEntry %q", name)
	return created
}

func TestListDatabases_AdminSeesAll(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "lda"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin, store.RoleConnector})
	token := loginUser(t, server, "admin-"+suffix, "adminpass123")

	createTestDBEntry(t, dataStore, "listable-db-"+suffix, true)
	createTestDBEntry(t, dataStore, "hidden-db-"+suffix, false)

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/databases", server.handleListDatabases)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	dbs := resp["databases"].([]any)
	// Admin must see both (at minimum these two).
	assert.GreaterOrEqual(t, len(dbs), 2)

	// Responses must include the listable field, including the hidden one.
	names := make([]string, 0, len(dbs))
	for _, entry := range dbs {
		db := entry.(map[string]any)
		_, hasListable := db["listable"]
		assert.True(t, hasListable, "admin response must include 'listable' field")
		if name, ok := db["name"].(string); ok {
			names = append(names, name)
		}
	}
	assert.Contains(t, names, "listable-db-"+suffix)
	assert.Contains(t, names, "hidden-db-"+suffix)
}

func TestListDatabases_ConnectorSeesOnlyListable(t *testing.T) { //nolint:paralleltest // shared migration lock
	server, dataStore := setupTestServer(t)
	suffix := "ldc"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin, store.RoleConnector})
	createTestDBEntry(t, dataStore, "visible-db-"+suffix, true)
	createTestDBEntry(t, dataStore, "invisible-db-"+suffix, false)

	// Connector user with no grants.
	createTestUser(t, dataStore, "connector-"+suffix, "connpass123", []string{store.RoleConnector})
	connToken := loginUser(t, server, "connector-"+suffix, "connpass123")

	router := gin.New()
	router.Use(server.authMiddleware())
	router.GET("/api/v1/databases", server.handleListDatabases)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil)
	req.Header.Set("Authorization", "Bearer "+connToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	dbs := resp["databases"].([]any)
	for _, entry := range dbs {
		db := entry.(map[string]any)
		// Non-admin must never see the 'host' or 'port' fields.
		assert.Nil(t, db["host"], "non-admin response must not include 'host'")
		// Non-admin response must not include the listable field (DatabaseLimitedResponse).
		_, hasListable := db["listable"]
		assert.False(t, hasListable, "non-admin response must not include 'listable' field")
	}

	// Find names in result — invisible-db must not appear.
	names := make([]string, 0, len(dbs))
	for _, entry := range dbs {
		if name, ok := entry.(map[string]any)["name"].(string); ok {
			names = append(names, name)
		}
	}
	assert.Contains(t, names, "visible-db-"+suffix)
	assert.NotContains(t, names, "invisible-db-"+suffix)
}

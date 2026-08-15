package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// renameRouter wires the endpoints the rename tests exercise. Kept separate
// from kubernetesRouter so these tests don't inherit its tunnel-only routes.
func renameRouter(server *Server) *gin.Engine {
	router := gin.New()
	router.Use(server.authMiddleware())
	router.POST("/api/v1/servers", server.requireAdmin(), server.handleCreateDatabase)
	router.GET("/api/v1/servers/:uid", server.handleGetDatabase)
	router.PUT("/api/v1/servers/:uid", server.requireAdmin(), server.handleUpdateDatabase)
	router.DELETE("/api/v1/servers/:uid", server.requireAdmin(), server.handleDeleteDatabase)

	return router
}

func renameTestAdmin(t *testing.T) (*gin.Engine, string, *store.Store) {
	t.Helper()

	server, dataStore := setupTestServer(t)
	server.encryptionKey = dbTestEncryptionKey

	createTestUser(t, dataStore, "rename-admin", "adminpass123", []string{store.RoleAdmin})
	token := loginUser(t, server, "rename-admin", "adminpass123")

	return renameRouter(server), token, dataStore
}

func renamePayload(name string) map[string]any {
	return map[string]any{
		"name":          name,
		"host":          "127.0.0.1",
		"port":          5432,
		"database_name": "prod",
		"username":      "pg",
		"password":      "secret",
		"protocol":      "postgresql",
		"ssl_mode":      "disable",
	}
}

// createRenameServer POSTs a server and returns its uid.
func createRenameServer(t *testing.T, router *gin.Engine, token, name string) string {
	t.Helper()

	w, data := doJSON(t, router, "POST", "/api/v1/servers", token, renamePayload(name))
	require.Equal(t, 200, w.Code, w.Body.String())

	uid, ok := data["uid"].(string)
	require.True(t, ok, "created server has no uid: %s", w.Body.String())

	return uid
}

// TestUpdateServerRenames is the feature: PUT /servers/:uid accepts a new name,
// and the row answers to it afterwards. Before this, name was settable at
// creation only, so fixing a badly chosen connection target took a hand-written
// UPDATE against the storage database.
func TestUpdateServerRenames(t *testing.T) {
	t.Parallel()

	router, token, _ := renameTestAdmin(t)
	uid := createRenameServer(t, router, token, "before_rename")

	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"name": "after_rename",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	w, data := doJSON(t, router, "GET", "/api/v1/servers/"+uid, token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "after_rename", data["name"])
}

// TestUpdateServerRenameLeavesOtherFieldsAlone guards the wiring: a rename must
// not smuggle in a reset of everything the request omitted.
func TestUpdateServerRenameLeavesOtherFieldsAlone(t *testing.T) {
	t.Parallel()

	router, token, _ := renameTestAdmin(t)
	uid := createRenameServer(t, router, token, "rename_only")

	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"name": "rename_only_now",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	w, data := doJSON(t, router, "GET", "/api/v1/servers/"+uid, token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "rename_only_now", data["name"])
	assert.Equal(t, "127.0.0.1", data["host"])
	assert.InDelta(t, 5432, data["port"], 0)
	assert.Equal(t, "prod", data["database_name"])
	assert.Equal(t, "pg", data["username"])
}

// TestUpdateServerRenameRejectsNonSlug pins the update path to the very same
// slug gate creation enforces: the name is what clients type on all five
// protocols, so PUT must not be a way around POST's validation.
func TestUpdateServerRenameRejectsNonSlug(t *testing.T) {
	t.Parallel()

	router, token, _ := renameTestAdmin(t)
	uid := createRenameServer(t, router, token, "slug_gate")

	// Sequential rather than subtests: each attempt is asserted not to have
	// touched the row, which only means anything before the next one runs.
	for _, bad := range []string{"my-server", "MyServer", "abyla_abypocs (R/O)", "my.server"} {
		w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
			"name": bad,
		})
		require.Equal(t, 400, w.Code, "rename to %q: %s", bad, w.Body.String())
		assert.Contains(t, w.Body.String(), "slug", "rename to %q", bad)
	}

	w, data := doJSON(t, router, "GET", "/api/v1/servers/"+uid, token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "slug_gate", data["name"])
}

// TestUpdateServerRenameConflictIsA409 covers both halves of the unique
// constraint. servers_name_key is global — it spans soft-deleted rows — so the
// name of a *deleted* server is still taken, and that is the case that would
// otherwise surface as an opaque 500.
func TestUpdateServerRenameConflictIsA409(t *testing.T) {
	t.Parallel()

	router, token, _ := renameTestAdmin(t)
	uid := createRenameServer(t, router, token, "conflict_subject")

	createRenameServer(t, router, token, "conflict_live")

	deletedUID := createRenameServer(t, router, token, "conflict_deleted")
	w, _ := doJSON(t, router, "DELETE", "/api/v1/servers/"+deletedUID, token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())

	for _, taken := range []string{"conflict_live", "conflict_deleted"} {
		w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
			"name": taken,
		})
		require.Equal(t, 409, w.Code, "rename to %q: %s", taken, w.Body.String())
		assert.Contains(t, w.Body.String(), "already exists", "rename to %q", taken)
	}

	w, data := doJSON(t, router, "GET", "/api/v1/servers/"+uid, token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "conflict_subject", data["name"])
}

// TestUpdateServerRenameIsAudited: a rename moves the connection target for
// every client, so it has to be attributable — the new name lands in the
// database.updated audit details, in clear (it is a public identifier, not a
// credential).
func TestUpdateServerRenameIsAudited(t *testing.T) {
	t.Parallel()

	router, token, dataStore := renameTestAdmin(t)
	uid := createRenameServer(t, router, token, "audited_before")

	w, _ := doJSON(t, router, "PUT", "/api/v1/servers/"+uid, token, map[string]any{
		"name": "audited_after",
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	eventType := "database.updated"
	events, err := dataStore.ListAuditEvents(context.Background(), store.AuditFilter{
		EventType: &eventType,
		Limit:     10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, events, "no database.updated audit entry was written")

	var details struct {
		DatabaseUID   string `json:"database_uid"`
		UpdatedFields struct {
			Name *string `json:"name"`
		} `json:"updated_fields"`
	}
	require.NoError(t, json.Unmarshal(events[0].Details, &details))

	assert.Equal(t, uid, details.DatabaseUID)
	require.NotNil(t, details.UpdatedFields.Name, "the rename is missing from the audit details")
	assert.Equal(t, "audited_after", *details.UpdatedFields.Name)
	// The performer is recorded, which is what makes the rename attributable.
	assert.NotNil(t, events[0].PerformedBy)
}

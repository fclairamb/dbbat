package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestGrantDefinition_LegacyGroupFieldNamesAccepted pins the one-release
// compatibility window for the `group_uids` → `user_group_uids` and
// `approver_group_uids` → `approver_user_group_uids` rename: the old spellings
// are still *read* from a request body, and responses emit only the new ones.
func TestGrantDefinition_LegacyGroupFieldNamesAccepted(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "gdlegacy"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	scopeGroup, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "scope-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	approverGroup, err := dataStore.CreateUserGroup(ctx,
		&store.UserGroup{Name: "approvers-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	router := grantDefinitionsRouter(server)

	body := validCreateDefinitionBody("legacy-"+suffix, "legacy-"+suffix)
	body["group_uids"] = []string{scopeGroup.UID.String()}
	body["approver_group_uids"] = []string{approverGroup.UID.String()}

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.ElementsMatch(t, []any{scopeGroup.UID.String()}, resp["user_group_uids"],
		"the deprecated group_uids must still be read on input")
	require.ElementsMatch(t, []any{approverGroup.UID.String()}, resp["approver_user_group_uids"],
		"the deprecated approver_group_uids must still be read on input")

	_, hasLegacy := resp["group_uids"]
	require.False(t, hasLegacy, "responses must not emit the deprecated group_uids")

	_, hasLegacyApprover := resp["approver_group_uids"]
	require.False(t, hasLegacyApprover, "responses must not emit the deprecated approver_group_uids")

	uid, ok := resp["uid"].(string)
	require.True(t, ok)

	// The same shim on PATCH.
	w, resp = doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken, map[string]any{
		"group_uids":          []string{},
		"approver_group_uids": []string{},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, resp["user_group_uids"])
	require.Empty(t, resp["approver_user_group_uids"])
}

// TestGrantDefinition_NewGroupFieldNamesWinOverLegacy pins the tie-break: a
// body carrying both spellings uses the new one. The old name is a
// compatibility shim, never an override.
func TestGrantDefinition_NewGroupFieldNamesWinOverLegacy(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "gdboth"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	newGroup, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "new-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	oldGroup, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "old-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	router := grantDefinitionsRouter(server)

	body := validCreateDefinitionBody("both-"+suffix, "both-"+suffix)
	body["user_group_uids"] = []string{newGroup.UID.String()}
	body["group_uids"] = []string{oldGroup.UID.String()}

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.ElementsMatch(t, []any{newGroup.UID.String()}, resp["user_group_uids"])
}

// TestGrantDefinition_RetiredDatabaseUIDsRejected pins that the removed
// per-database scope is a hard 400 rather than a silently ignored field.
// Ignoring it would drop a scope restriction on the floor — failing *open* —
// which is exactly what this endpoint must never do.
func TestGrantDefinition_RetiredDatabaseUIDsRejected(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	suffix := "gdretired"

	createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	db := createTestDBEntry(t, dataStore, "retired-db-"+suffix, true)

	router := grantDefinitionsRouter(server)

	body := validCreateDefinitionBody("retired-"+suffix, "retired-"+suffix)
	body["database_uids"] = []string{db.UID.String()}

	w, resp := doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, body)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, "VALIDATION_ERROR", resp["code"])
	require.Contains(t, resp["message"], "server_group_uids")

	// And on PATCH, which is where a stale client is most likely to round-trip
	// the field it read from an older response.
	clean := validCreateDefinitionBody("retired-ok-"+suffix, "retired-ok-"+suffix)

	w, resp = doJSON(t, router, http.MethodPost, "/api/v1/grant-definitions", adminToken, clean)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	uid, ok := resp["uid"].(string)
	require.True(t, ok)

	w, _ = doJSON(t, router, http.MethodPatch, "/api/v1/grant-definitions/"+uid, adminToken, map[string]any{
		"database_uids": []string{db.UID.String()},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// TestGrantDefinition_ServerGroupScopeIsLive pins the payoff of the whole
// change: a database added to a scoped server group becomes requestable
// against that definition immediately, with no edit to the definition and
// therefore no new version.
func TestGrantDefinition_ServerGroupScopeIsLive(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "gdlive"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")
	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})

	inGroup := createTestDBEntry(t, dataStore, "live-in-"+suffix, true)
	joinsLater := createTestDBEntry(t, dataStore, "live-later-"+suffix, true)

	group, err := dataStore.CreateServerGroup(ctx, &store.ServerGroup{Name: "live-" + suffix})
	require.NoError(t, err)
	require.NoError(t, dataStore.AddServerToGroup(ctx, group.UID, inGroup.UID))

	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "live-scope-" + suffix,
		Slug:            "live-scope-" + suffix,
		DurationSeconds: 3600,
		Controls:        []string{store.ControlReadOnly},
		ServerGroupUIDs: []uuid.UUID{group.UID},
		CreatedBy:       admin.UID,
	})
	require.NoError(t, err)

	router := grantDefinitionsRouter(server)

	assign := func(databaseUID string) int {
		w, _ := doJSON(t, router, http.MethodPost, "/api/v1/grants", adminToken, map[string]any{
			"grant_definition_id": def.UID.String(),
			"user_id":             target.UID.String(),
			"database_id":         databaseUID,
		})

		return w.Code
	}

	require.Equal(t, http.StatusOK, assign(inGroup.UID.String()))
	require.Equal(t, http.StatusBadRequest, assign(joinsLater.UID.String()),
		"a database outside the scoped group is out of scope")

	// Membership is live: no definition edit, no new version, and the second
	// database is in scope.
	require.NoError(t, dataStore.AddServerToGroup(ctx, group.UID, joinsLater.UID))

	require.Equal(t, http.StatusOK, assign(joinsLater.UID.String()),
		"adding the server to the scoped group must widen the definition immediately")

	reloaded, err := dataStore.GetGrantDefinition(ctx, def.UID)
	require.NoError(t, err)
	require.True(t, reloaded.IsLive(), "widening the scope must not have versioned the definition")
}

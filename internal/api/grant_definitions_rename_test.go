package api

import (
	"context"
	"net/http"
	"testing"

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

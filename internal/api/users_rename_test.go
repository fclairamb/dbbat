package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// The user surface carries the same one-release compatibility window as the
// grant-definition surface (see grant_definitions_rename_test.go): server
// groups made a bare "group" ambiguous, so `group_uids` became
// `user_group_uids` on input and the user-detail response's `groups` key
// became `user_groups`. These tests pin both halves of that promise — the old
// spelling still *works*, and no response emits it.

// TestUpdateUser_LegacyGroupUIDsAccepted pins that a client written against
// the pre-rename API can still set a user's group membership.
func TestUpdateUser_LegacyGroupUIDsAccepted(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "ulegacy"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "legacy-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	router := newUsersTestRouter(server)

	w, _ := doJSON(t, router, http.MethodPut, "/api/v1/users/"+target.UID.String(), adminToken, map[string]any{
		"group_uids": []string{group.UID.String()},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	membership, err := dataStore.ListUserGroupUIDs(ctx, target.UID)
	require.NoError(t, err)
	require.Equal(t, []string{group.UID.String()}, uidStrings(membership),
		"the deprecated group_uids must still be read on input")

	// And the explicit-empty case, which is how a caller clears membership.
	w, _ = doJSON(t, router, http.MethodPut, "/api/v1/users/"+target.UID.String(), adminToken, map[string]any{
		"group_uids": []string{},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	membership, err = dataStore.ListUserGroupUIDs(ctx, target.UID)
	require.NoError(t, err)
	require.Empty(t, membership)
}

// TestUpdateUser_NewGroupFieldNameWinsOverLegacy pins the same precedence the
// grant-definition bodies use: sending both spellings applies the new one. The
// old name is a compatibility shim, never an override.
func TestUpdateUser_NewGroupFieldNameWinsOverLegacy(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "uboth"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	newGroup, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "new-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	oldGroup, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "old-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	router := newUsersTestRouter(server)

	w, _ := doJSON(t, router, http.MethodPut, "/api/v1/users/"+target.UID.String(), adminToken, map[string]any{
		"user_group_uids": []string{newGroup.UID.String()},
		"group_uids":      []string{oldGroup.UID.String()},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	membership, err := dataStore.ListUserGroupUIDs(ctx, target.UID)
	require.NoError(t, err)
	require.Equal(t, []string{newGroup.UID.String()}, uidStrings(membership))
}

// TestGetUser_EmitsUserGroupsNotGroups pins the response half of the rename:
// the user-detail body names the collection `user_groups`, and the old bare
// `groups` key is gone rather than duplicated alongside it.
func TestGetUser_EmitsUserGroupsNotGroups(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "udetail"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "detail-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)
	require.NoError(t, dataStore.AddUserToUserGroup(ctx, group.UID, target.UID))

	router := newUsersTestRouter(server)

	w, resp := doJSON(t, router, http.MethodGet, "/api/v1/users/"+target.UID.String(), adminToken, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	groups, ok := resp["user_groups"].([]any)
	require.True(t, ok, "the user detail response must carry user_groups")
	require.Len(t, groups, 1)

	_, hasLegacy := resp["groups"]
	require.False(t, hasLegacy, "responses must not emit the deprecated bare groups key")
}

// uidStrings renders uids for comparison without importing a sorting helper
// into every assertion.
func uidStrings(uids []uuid.UUID) []string {
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		out = append(out, uid.String())
	}

	return out
}

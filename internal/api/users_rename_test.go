package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// The user surface carries the same pre-rename spelling as the
// grant-definition surface (see grant_definitions_rename_test.go): server
// groups made a bare "group" ambiguous, so `group_uids` became
// `user_group_uids` on input and the user-detail response's `groups` key
// became `user_groups`. These tests pin both halves of the rename — the old
// input spelling is now a hard 400, and no response emits it.

// TestUpdateUser_RetiredGroupUIDsRejected pins that the pre-rename
// `group_uids` spelling is refused with a 400 rather than silently applied or
// silently ignored. Ignoring it would drop a group-membership change on the
// floor — failing *open* — which is exactly what this endpoint must never do;
// mirrors TestGrantDefinition_RetiredGroupFieldNamesRejected.
func TestUpdateUser_RetiredGroupUIDsRejected(t *testing.T) {
	t.Parallel()

	server, dataStore := setupTestServer(t)
	ctx := context.Background()
	suffix := "uretiredgroup"

	admin := createTestUser(t, dataStore, "admin-"+suffix, "adminpass123", []string{store.RoleAdmin})
	target := createTestUser(t, dataStore, "target-"+suffix, "targetpass123", []string{store.RoleConnector})
	adminToken := loginUser(t, server, "admin-"+suffix, "adminpass123")

	group, err := dataStore.CreateUserGroup(ctx, &store.UserGroup{Name: "retired-" + suffix, CreatedBy: &admin.UID})
	require.NoError(t, err)

	router := newUsersTestRouter(server)

	w, resp := doJSON(t, router, http.MethodPut, "/api/v1/users/"+target.UID.String(), adminToken, map[string]any{
		"group_uids": []string{group.UID.String()},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, "VALIDATION_ERROR", resp["code"])
	require.Contains(t, resp["message"], "user_group_uids")

	membership, err := dataStore.ListUserGroupUIDs(ctx, target.UID)
	require.NoError(t, err)
	require.Empty(t, membership, "a rejected request must not have applied any membership change")
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

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestGrantDefinitionAppliesTo(t *testing.T) {
	t.Parallel()

	groupA := uuid.New()
	groupB := uuid.New()
	sgA := uuid.New()
	sgB := uuid.New()

	tests := []struct {
		name            string
		def             GrantDefinition
		userGroupUIDs   []uuid.UUID
		serverGroupUIDs []uuid.UUID
		want            bool
	}{
		{
			// The backwards-compatibility case: every pre-scoping definition
			// has empty arrays and must keep applying to everyone.
			name:            "unscoped applies to everyone",
			def:             GrantDefinition{},
			userGroupUIDs:   nil,
			serverGroupUIDs: []uuid.UUID{sgA},
			want:            true,
		},
		{
			name:            "groups only, member",
			def:             GrantDefinition{UserGroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs:   []uuid.UUID{groupB, groupA},
			serverGroupUIDs: []uuid.UUID{sgA},
			want:            true,
		},
		{
			name:            "groups only, non-member",
			def:             GrantDefinition{UserGroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs:   []uuid.UUID{groupB},
			serverGroupUIDs: []uuid.UUID{sgA},
			want:            false,
		},
		{
			name:            "groups only, user in no group",
			def:             GrantDefinition{UserGroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs:   nil,
			serverGroupUIDs: []uuid.UUID{sgA},
			want:            false,
		},
		{
			name:            "server groups only, in scope",
			def:             GrantDefinition{ServerGroupUIDs: []uuid.UUID{sgA, sgB}},
			userGroupUIDs:   nil,
			serverGroupUIDs: []uuid.UUID{sgB},
			want:            true,
		},
		{
			name:            "server groups only, out of scope",
			def:             GrantDefinition{ServerGroupUIDs: []uuid.UUID{sgA}},
			userGroupUIDs:   []uuid.UUID{groupA},
			serverGroupUIDs: []uuid.UUID{sgB},
			want:            false,
		},
		{
			name:            "both axes must pass",
			def:             GrantDefinition{UserGroupUIDs: []uuid.UUID{groupA}, ServerGroupUIDs: []uuid.UUID{sgA}},
			userGroupUIDs:   []uuid.UUID{groupA},
			serverGroupUIDs: []uuid.UUID{sgB},
			want:            false,
		},
		{
			// Fail-closed: a group that was deleted leaves a dangling uid in
			// the scope array, which must match nobody rather than everybody.
			name:            "dangling group uid fails closed",
			def:             GrantDefinition{UserGroupUIDs: []uuid.UUID{uuid.New()}},
			userGroupUIDs:   []uuid.UUID{groupA, groupB},
			serverGroupUIDs: []uuid.UUID{sgA},
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.def.AppliesTo(tt.userGroupUIDs, tt.serverGroupUIDs); got != tt.want {
				t.Errorf("AppliesTo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserGroupCRUD(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "groups_crud")

	created, err := store.CreateUserGroup(ctx, &UserGroup{
		Name:        "Data Analysts",
		Description: "Self-serve read-only",
		CreatedBy:   &admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateUserGroup() error = %v", err)
	}

	if created.UID == uuid.Nil {
		t.Fatal("UID = uuid.Nil")
	}

	// Name uniqueness is case-insensitive.
	if _, err := store.CreateUserGroup(ctx, &UserGroup{Name: "data analysts"}); !errors.Is(err, ErrUserGroupDuplicate) {
		t.Errorf("CreateUserGroup() duplicate error = %v, want ErrUserGroupDuplicate", err)
	}

	fetched, err := store.GetUserGroup(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetUserGroup() error = %v", err)
	}

	if fetched.Name != "Data Analysts" {
		t.Errorf("Name = %q, want Data Analysts", fetched.Name)
	}

	fetched.Name = "Analysts"
	if err := store.UpdateUserGroup(ctx, fetched); err != nil {
		t.Fatalf("UpdateUserGroup() error = %v", err)
	}

	groups, err := store.ListUserGroups(ctx)
	if err != nil {
		t.Fatalf("ListUserGroups() error = %v", err)
	}

	if len(groups) != 1 || groups[0].Name != "Analysts" {
		t.Errorf("ListUserGroups() = %+v, want one group named Analysts", groups)
	}

	if err := store.DeleteUserGroup(ctx, created.UID); err != nil {
		t.Fatalf("DeleteUserGroup() error = %v", err)
	}

	if _, err := store.GetUserGroup(ctx, created.UID); !errors.Is(err, ErrUserGroupNotFound) {
		t.Errorf("GetUserGroup() after delete error = %v, want ErrUserGroupNotFound", err)
	}

	if err := store.DeleteUserGroup(ctx, created.UID); !errors.Is(err, ErrUserGroupNotFound) {
		t.Errorf("DeleteUserGroup() twice error = %v, want ErrUserGroupNotFound", err)
	}
}

func TestUserGroupMembership(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "grpmember", "hash", []string{RoleConnector})
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	group, err := store.CreateUserGroup(ctx, &UserGroup{Name: "SRE"})
	if err != nil {
		t.Fatalf("CreateUserGroup() error = %v", err)
	}

	if err := store.AddUserToUserGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToUserGroup() error = %v", err)
	}

	// Adding twice must be a no-op, not a unique violation.
	if err := store.AddUserToUserGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToUserGroup() twice error = %v", err)
	}

	uids, err := store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 1 || uids[0] != group.UID {
		t.Errorf("ListUserGroupUIDs() = %v, want [%v]", uids, group.UID)
	}

	members, err := store.ListUserGroupMembers(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListUserGroupMembers() error = %v", err)
	}

	if len(members) != 1 || members[0].UID != user.UID {
		t.Errorf("ListUserGroupMembers() = %+v, want the single member", members)
	}

	groupsForUser, err := store.ListUserGroupsForUser(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupsForUser() error = %v", err)
	}

	if len(groupsForUser) != 1 || groupsForUser[0].UID != group.UID {
		t.Errorf("ListUserGroupsForUser() = %+v, want the single group", groupsForUser)
	}

	if err := store.RemoveUserFromUserGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("RemoveUserFromUserGroup() error = %v", err)
	}

	uids, err = store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 0 {
		t.Errorf("ListUserGroupUIDs() after removal = %v, want empty", uids)
	}
}

func TestSetGroupMembersAndSetUserGroups(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	userA, err := store.CreateUser(ctx, "setgrp_a", "hash", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	userB, err := store.CreateUser(ctx, "setgrp_b", "hash", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	groupA, err := store.CreateUserGroup(ctx, &UserGroup{Name: "alpha"})
	if err != nil {
		t.Fatalf("CreateUserGroup() error = %v", err)
	}

	groupB, err := store.CreateUserGroup(ctx, &UserGroup{Name: "beta"})
	if err != nil {
		t.Fatalf("CreateUserGroup() error = %v", err)
	}

	if err := store.SetUserGroupMembers(ctx, groupA.UID, []uuid.UUID{userA.UID, userB.UID}); err != nil {
		t.Fatalf("SetUserGroupMembers() error = %v", err)
	}

	members, err := store.ListUserGroupMemberUIDs(ctx, groupA.UID)
	if err != nil {
		t.Fatalf("ListUserGroupMemberUIDs() error = %v", err)
	}

	if len(members) != 2 {
		t.Fatalf("ListUserGroupMemberUIDs() = %v, want 2 members", members)
	}

	// Replacing wholesale drops the members not in the new set.
	if err := store.SetUserGroupMembers(ctx, groupA.UID, []uuid.UUID{userB.UID}); err != nil {
		t.Fatalf("SetUserGroupMembers() replace error = %v", err)
	}

	members, err = store.ListUserGroupMemberUIDs(ctx, groupA.UID)
	if err != nil {
		t.Fatalf("ListUserGroupMemberUIDs() error = %v", err)
	}

	if len(members) != 1 || members[0] != userB.UID {
		t.Errorf("ListUserGroupMemberUIDs() = %v, want only userB", members)
	}

	if err := store.SetUserGroups(ctx, userA.UID, []uuid.UUID{groupA.UID, groupB.UID}); err != nil {
		t.Fatalf("SetUserGroups() error = %v", err)
	}

	uids, err := store.ListUserGroupUIDs(ctx, userA.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 2 {
		t.Errorf("ListUserGroupUIDs() = %v, want 2 groups", uids)
	}

	if err := store.SetUserGroups(ctx, userA.UID, nil); err != nil {
		t.Fatalf("SetUserGroups() clear error = %v", err)
	}

	uids, err = store.ListUserGroupUIDs(ctx, userA.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 0 {
		t.Errorf("ListUserGroupUIDs() after clear = %v, want empty", uids)
	}
}

func TestUserGroupDeletionCascadesMembershipButNotScope(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "grp_cascade")

	user, err := store.CreateUser(ctx, "cascade_member", "hash", nil)
	if err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}

	group, err := store.CreateUserGroup(ctx, &UserGroup{Name: "doomed"})
	if err != nil {
		t.Fatalf("CreateUserGroup() error = %v", err)
	}

	if err := store.AddUserToUserGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToUserGroup() error = %v", err)
	}

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "scoped-def",
		Slug:            "scoped-def",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		UserGroupUIDs:   []uuid.UUID{group.UID},
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	if err := store.DeleteUserGroup(ctx, group.UID); err != nil {
		t.Fatalf("DeleteUserGroup() error = %v", err)
	}

	// Membership cascades away…
	uids, err := store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 0 {
		t.Errorf("ListUserGroupUIDs() after group delete = %v, want empty", uids)
	}

	// …but the definition's scope keeps the dangling uid, so the definition
	// now matches nobody instead of silently reverting to "everyone".
	reloaded, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if len(reloaded.UserGroupUIDs) != 1 || reloaded.UserGroupUIDs[0] != group.UID {
		t.Fatalf("UserGroupUIDs = %v, want the dangling group uid preserved", reloaded.UserGroupUIDs)
	}

	if reloaded.AppliesToUserGroups(uids) {
		t.Error("definition with a dangling group scope should fail closed")
	}
}

func TestGrantDefinitionScopePersistence(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "scope_persist")

	groupUID := uuid.New()
	serverGroupUID := uuid.New()

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "persist-def",
		Slug:            "persist-def",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	// Unset scope round-trips as an empty (never nil) array.
	if len(def.UserGroupUIDs) != 0 || len(def.ServerGroupUIDs) != 0 {
		t.Errorf("new definition scope = %v/%v, want empty", def.UserGroupUIDs, def.ServerGroupUIDs)
	}

	def.UserGroupUIDs = []uuid.UUID{groupUID}
	def.ServerGroupUIDs = []uuid.UUID{serverGroupUID}

	updated, err := store.UpdateGrantDefinition(ctx, def)
	if err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	// The edit produced a new version; the scope has to be read off that one.
	reloaded, err := store.GetGrantDefinition(ctx, updated.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if len(reloaded.UserGroupUIDs) != 1 || reloaded.UserGroupUIDs[0] != groupUID {
		t.Errorf("UserGroupUIDs = %v, want [%v]", reloaded.UserGroupUIDs, groupUID)
	}

	if len(reloaded.ServerGroupUIDs) != 1 || reloaded.ServerGroupUIDs[0] != serverGroupUID {
		t.Errorf("ServerGroupUIDs = %v, want [%v]", reloaded.ServerGroupUIDs, serverGroupUID)
	}
}

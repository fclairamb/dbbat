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
	dbA := uuid.New()
	dbB := uuid.New()

	tests := []struct {
		name          string
		def           GrantDefinition
		userGroupUIDs []uuid.UUID
		databaseUID   uuid.UUID
		want          bool
	}{
		{
			// The backwards-compatibility case: every pre-scoping definition
			// has empty arrays and must keep applying to everyone.
			name:          "unscoped applies to everyone",
			def:           GrantDefinition{},
			userGroupUIDs: nil,
			databaseUID:   dbA,
			want:          true,
		},
		{
			name:          "groups only, member",
			def:           GrantDefinition{GroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs: []uuid.UUID{groupB, groupA},
			databaseUID:   dbA,
			want:          true,
		},
		{
			name:          "groups only, non-member",
			def:           GrantDefinition{GroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs: []uuid.UUID{groupB},
			databaseUID:   dbA,
			want:          false,
		},
		{
			name:          "groups only, user in no group",
			def:           GrantDefinition{GroupUIDs: []uuid.UUID{groupA}},
			userGroupUIDs: nil,
			databaseUID:   dbA,
			want:          false,
		},
		{
			name:          "databases only, in scope",
			def:           GrantDefinition{DatabaseUIDs: []uuid.UUID{dbA, dbB}},
			userGroupUIDs: nil,
			databaseUID:   dbB,
			want:          true,
		},
		{
			name:          "databases only, out of scope",
			def:           GrantDefinition{DatabaseUIDs: []uuid.UUID{dbA}},
			userGroupUIDs: []uuid.UUID{groupA},
			databaseUID:   dbB,
			want:          false,
		},
		{
			name:          "both axes must pass",
			def:           GrantDefinition{GroupUIDs: []uuid.UUID{groupA}, DatabaseUIDs: []uuid.UUID{dbA}},
			userGroupUIDs: []uuid.UUID{groupA},
			databaseUID:   dbB,
			want:          false,
		},
		{
			// Fail-closed: a group that was deleted leaves a dangling uid in
			// the scope array, which must match nobody rather than everybody.
			name:          "dangling group uid fails closed",
			def:           GrantDefinition{GroupUIDs: []uuid.UUID{uuid.New()}},
			userGroupUIDs: []uuid.UUID{groupA, groupB},
			databaseUID:   dbA,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.def.AppliesTo(tt.userGroupUIDs, tt.databaseUID); got != tt.want {
				t.Errorf("AppliesTo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserGroupCRUD(t *testing.T) { //nolint:paralleltest // shared migration lock
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

func TestUserGroupMembership(t *testing.T) { //nolint:paralleltest // shared migration lock
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

	if err := store.AddUserToGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToGroup() error = %v", err)
	}

	// Adding twice must be a no-op, not a unique violation.
	if err := store.AddUserToGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToGroup() twice error = %v", err)
	}

	uids, err := store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 1 || uids[0] != group.UID {
		t.Errorf("ListUserGroupUIDs() = %v, want [%v]", uids, group.UID)
	}

	members, err := store.ListGroupMembers(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListGroupMembers() error = %v", err)
	}

	if len(members) != 1 || members[0].UID != user.UID {
		t.Errorf("ListGroupMembers() = %+v, want the single member", members)
	}

	groupsForUser, err := store.ListGroupsForUser(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListGroupsForUser() error = %v", err)
	}

	if len(groupsForUser) != 1 || groupsForUser[0].UID != group.UID {
		t.Errorf("ListGroupsForUser() = %+v, want the single group", groupsForUser)
	}

	if err := store.RemoveUserFromGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("RemoveUserFromGroup() error = %v", err)
	}

	uids, err = store.ListUserGroupUIDs(ctx, user.UID)
	if err != nil {
		t.Fatalf("ListUserGroupUIDs() error = %v", err)
	}

	if len(uids) != 0 {
		t.Errorf("ListUserGroupUIDs() after removal = %v, want empty", uids)
	}
}

func TestSetGroupMembersAndSetUserGroups(t *testing.T) { //nolint:paralleltest // shared migration lock
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

	if err := store.SetGroupMembers(ctx, groupA.UID, []uuid.UUID{userA.UID, userB.UID}); err != nil {
		t.Fatalf("SetGroupMembers() error = %v", err)
	}

	members, err := store.ListGroupMemberUIDs(ctx, groupA.UID)
	if err != nil {
		t.Fatalf("ListGroupMemberUIDs() error = %v", err)
	}

	if len(members) != 2 {
		t.Fatalf("ListGroupMemberUIDs() = %v, want 2 members", members)
	}

	// Replacing wholesale drops the members not in the new set.
	if err := store.SetGroupMembers(ctx, groupA.UID, []uuid.UUID{userB.UID}); err != nil {
		t.Fatalf("SetGroupMembers() replace error = %v", err)
	}

	members, err = store.ListGroupMemberUIDs(ctx, groupA.UID)
	if err != nil {
		t.Fatalf("ListGroupMemberUIDs() error = %v", err)
	}

	if len(members) != 1 || members[0] != userB.UID {
		t.Errorf("ListGroupMemberUIDs() = %v, want only userB", members)
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

func TestUserGroupDeletionCascadesMembershipButNotScope(t *testing.T) { //nolint:paralleltest // shared migration lock
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

	if err := store.AddUserToGroup(ctx, group.UID, user.UID); err != nil {
		t.Fatalf("AddUserToGroup() error = %v", err)
	}

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "scoped-def",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		GroupUIDs:       []uuid.UUID{group.UID},
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

	if len(reloaded.GroupUIDs) != 1 || reloaded.GroupUIDs[0] != group.UID {
		t.Fatalf("GroupUIDs = %v, want the dangling group uid preserved", reloaded.GroupUIDs)
	}

	if reloaded.AppliesToGroups(uids) {
		t.Error("definition with a dangling group scope should fail closed")
	}
}

func TestGrantDefinitionScopePersistence(t *testing.T) { //nolint:paralleltest // shared migration lock
	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "scope_persist")

	groupUID := uuid.New()
	dbUID := uuid.New()

	def, err := store.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "persist-def",
		DurationSeconds: 3600,
		Controls:        []string{ControlReadOnly},
		CreatedBy:       admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantDefinition() error = %v", err)
	}

	// Unset scope round-trips as an empty (never nil) array.
	if len(def.GroupUIDs) != 0 || len(def.DatabaseUIDs) != 0 {
		t.Errorf("new definition scope = %v/%v, want empty", def.GroupUIDs, def.DatabaseUIDs)
	}

	def.GroupUIDs = []uuid.UUID{groupUID}
	def.DatabaseUIDs = []uuid.UUID{dbUID}

	if err := store.UpdateGrantDefinition(ctx, def); err != nil {
		t.Fatalf("UpdateGrantDefinition() error = %v", err)
	}

	reloaded, err := store.GetGrantDefinition(ctx, def.UID)
	if err != nil {
		t.Fatalf("GetGrantDefinition() error = %v", err)
	}

	if len(reloaded.GroupUIDs) != 1 || reloaded.GroupUIDs[0] != groupUID {
		t.Errorf("GroupUIDs = %v, want [%v]", reloaded.GroupUIDs, groupUID)
	}

	if len(reloaded.DatabaseUIDs) != 1 || reloaded.DatabaseUIDs[0] != dbUID {
		t.Errorf("DatabaseUIDs = %v, want [%v]", reloaded.DatabaseUIDs, dbUID)
	}
}

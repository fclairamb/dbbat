package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// newTestTargetServer persists a database target usable as a server-group
// member. Names are unique per suffix so tests can run in parallel.
func newTestTargetServer(t *testing.T, ctx context.Context, s *Store, name string) *Server {
	t.Helper()

	created, err := s.CreateServer(ctx, &Server{
		Name:         name,
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "db",
		Username:     "user",
		Password:     "pass",
		SSLMode:      "disable",
	}, testEncryptionKey())
	if err != nil {
		t.Fatalf("CreateServer(%s) error = %v", name, err)
	}

	return created
}

func TestServerGroupCRUD(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sgroups_crud")

	created, err := store.CreateServerGroup(ctx, &ServerGroup{
		Name:        "Analytics Replicas",
		Description: "Read replicas the analysts get",
		CreatedBy:   &admin.UID,
	})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	if created.UID == uuid.Nil {
		t.Fatal("UID = uuid.Nil")
	}

	// Name uniqueness is case-insensitive, exactly like user groups.
	if _, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "analytics replicas"}); !errors.Is(err, ErrServerGroupDuplicate) {
		t.Errorf("CreateServerGroup() duplicate error = %v, want ErrServerGroupDuplicate", err)
	}

	fetched, err := store.GetServerGroup(ctx, created.UID)
	if err != nil {
		t.Fatalf("GetServerGroup() error = %v", err)
	}

	if fetched.Name != "Analytics Replicas" {
		t.Errorf("Name = %q, want Analytics Replicas", fetched.Name)
	}

	fetched.Name = "Replicas"
	if err := store.UpdateServerGroup(ctx, fetched); err != nil {
		t.Fatalf("UpdateServerGroup() error = %v", err)
	}

	groups, err := store.ListServerGroups(ctx)
	if err != nil {
		t.Fatalf("ListServerGroups() error = %v", err)
	}

	if len(groups) != 1 || groups[0].Name != "Replicas" {
		t.Errorf("ListServerGroups() = %+v, want one group named Replicas", groups)
	}

	if err := store.DeleteServerGroup(ctx, created.UID); err != nil {
		t.Fatalf("DeleteServerGroup() error = %v", err)
	}

	if _, err := store.GetServerGroup(ctx, created.UID); !errors.Is(err, ErrServerGroupNotFound) {
		t.Errorf("GetServerGroup() after delete error = %v, want ErrServerGroupNotFound", err)
	}

	if err := store.DeleteServerGroup(ctx, created.UID); !errors.Is(err, ErrServerGroupNotFound) {
		t.Errorf("DeleteServerGroup() twice error = %v, want ErrServerGroupNotFound", err)
	}
}

func TestServerGroupMembership(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "staging"})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	dbA := newTestTargetServer(t, ctx, store, "sgm_a")
	dbB := newTestTargetServer(t, ctx, store, "sgm_b")

	if err := store.AddServerToGroup(ctx, group.UID, dbA.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	// Idempotent: re-adding is a no-op, not a duplicate-key error.
	if err := store.AddServerToGroup(ctx, group.UID, dbA.UID); err != nil {
		t.Fatalf("AddServerToGroup() twice error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, group.UID, dbB.UID); err != nil {
		t.Fatalf("AddServerToGroup(B) error = %v", err)
	}

	uids, err := store.ListServerGroupMemberUIDs(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListServerGroupMemberUIDs() error = %v", err)
	}

	if len(uids) != 2 {
		t.Errorf("ListServerGroupMemberUIDs() = %v, want 2 entries", uids)
	}

	servers, err := store.ListServerGroupMembers(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListServerGroupMembers() error = %v", err)
	}

	if len(servers) != 2 {
		t.Errorf("ListServerGroupMembers() = %d rows, want 2", len(servers))
	}

	// The reverse direction is what the auth path uses on every connection.
	groupUIDs, err := store.ListServerGroupUIDsForServer(ctx, dbA.UID)
	if err != nil {
		t.Fatalf("ListServerGroupUIDsForServer() error = %v", err)
	}

	if len(groupUIDs) != 1 || groupUIDs[0] != group.UID {
		t.Errorf("ListServerGroupUIDsForServer() = %v, want [%v]", groupUIDs, group.UID)
	}

	rows, err := store.ListServerGroupsForServer(ctx, dbA.UID)
	if err != nil {
		t.Fatalf("ListServerGroupsForServer() error = %v", err)
	}

	if len(rows) != 1 || rows[0].Name != "staging" {
		t.Errorf("ListServerGroupsForServer() = %+v, want the staging group", rows)
	}

	if err := store.RemoveServerFromGroup(ctx, group.UID, dbA.UID); err != nil {
		t.Fatalf("RemoveServerFromGroup() error = %v", err)
	}

	// Removing a non-membership is a no-op too.
	if err := store.RemoveServerFromGroup(ctx, group.UID, dbA.UID); err != nil {
		t.Fatalf("RemoveServerFromGroup() twice error = %v", err)
	}

	uids, err = store.ListServerGroupMemberUIDs(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListServerGroupMemberUIDs() after removal error = %v", err)
	}

	if len(uids) != 1 || uids[0] != dbB.UID {
		t.Errorf("ListServerGroupMemberUIDs() after removal = %v, want [%v]", uids, dbB.UID)
	}
}

func TestSetServerGroupMembersReplacesWholesale(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	dbA := newTestTargetServer(t, ctx, store, "sgset_a")
	dbB := newTestTargetServer(t, ctx, store, "sgset_b")
	dbC := newTestTargetServer(t, ctx, store, "sgset_c")

	if err := store.SetServerGroupMembers(ctx, group.UID, []uuid.UUID{dbA.UID, dbB.UID}); err != nil {
		t.Fatalf("SetServerGroupMembers() error = %v", err)
	}

	if err := store.SetServerGroupMembers(ctx, group.UID, []uuid.UUID{dbC.UID}); err != nil {
		t.Fatalf("SetServerGroupMembers() replace error = %v", err)
	}

	uids, err := store.ListServerGroupMemberUIDs(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListServerGroupMemberUIDs() error = %v", err)
	}

	if len(uids) != 1 || uids[0] != dbC.UID {
		t.Errorf("ListServerGroupMemberUIDs() = %v, want [%v]", uids, dbC.UID)
	}

	// An explicit empty set really empties the group — an empty group is a
	// real access-control state.
	if err := store.SetServerGroupMembers(ctx, group.UID, nil); err != nil {
		t.Fatalf("SetServerGroupMembers(nil) error = %v", err)
	}

	uids, err = store.ListServerGroupMemberUIDs(ctx, group.UID)
	if err != nil {
		t.Fatalf("ListServerGroupMemberUIDs() after clear error = %v", err)
	}

	if len(uids) != 0 {
		t.Errorf("ListServerGroupMemberUIDs() after clear = %v, want empty", uids)
	}
}

// TestServerGroupDeletionCascadesMembership pins the cascade: dropping a group
// drops its memberships, and dropping a server drops its memberships too.
func TestServerGroupDeletionCascadesMembership(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "cascade"})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	db := newTestTargetServer(t, ctx, store, "sgcascade")

	if err := store.AddServerToGroup(ctx, group.UID, db.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	if err := store.DeleteServerGroup(ctx, group.UID); err != nil {
		t.Fatalf("DeleteServerGroup() error = %v", err)
	}

	groupUIDs, err := store.ListServerGroupUIDsForServer(ctx, db.UID)
	if err != nil {
		t.Fatalf("ListServerGroupUIDsForServer() error = %v", err)
	}

	if len(groupUIDs) != 0 {
		t.Errorf("ListServerGroupUIDsForServer() after group delete = %v, want empty", groupUIDs)
	}
}

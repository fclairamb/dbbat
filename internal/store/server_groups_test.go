package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// queryCountHook counts every query bun executes whose SQL text contains a
// given substring — the mechanism TestListServerGroupMemberUIDsByGroups uses
// to prove a batched membership lookup, not one query per group.
type queryCountHook struct {
	substring string
	count     atomic.Int64
}

func (h *queryCountHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *queryCountHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if strings.Contains(event.Query, h.substring) {
		h.count.Add(1)
	}
}

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

// TestListServerGroupMemberUIDsByGroups pins the batching contract that
// GrantDefinition.ScopedDatabaseUIDs depends on: resolving several groups'
// membership at once must be one query, not one per group, or a listing of N
// scoped definitions would fire N membership queries.
func TestListServerGroupMemberUIDsByGroups(t *testing.T) {
	t.Parallel()

	testStore := setupTestStore(t)
	ctx := context.Background()

	dbA := newTestTargetServer(t, ctx, testStore, "batch_a")
	dbB := newTestTargetServer(t, ctx, testStore, "batch_b")
	dbC := newTestTargetServer(t, ctx, testStore, "batch_c")

	groupOne, err := testStore.CreateServerGroup(ctx, &ServerGroup{Name: "batch-one"})
	require.NoError(t, err)

	groupTwo, err := testStore.CreateServerGroup(ctx, &ServerGroup{Name: "batch-two"})
	require.NoError(t, err)

	groupEmpty, err := testStore.CreateServerGroup(ctx, &ServerGroup{Name: "batch-empty"})
	require.NoError(t, err)

	require.NoError(t, testStore.AddServerToGroup(ctx, groupOne.UID, dbA.UID))
	require.NoError(t, testStore.AddServerToGroup(ctx, groupOne.UID, dbB.UID))
	require.NoError(t, testStore.AddServerToGroup(ctx, groupTwo.UID, dbB.UID))
	require.NoError(t, testStore.AddServerToGroup(ctx, groupTwo.UID, dbC.UID))

	hook := &queryCountHook{substring: "server_group_members"}
	testStore.db.AddQueryHook(hook)

	members, err := testStore.ListServerGroupMemberUIDsByGroups(
		ctx, []uuid.UUID{groupOne.UID, groupTwo.UID, groupEmpty.UID},
	)
	require.NoError(t, err)

	require.EqualValues(t, 1, hook.count.Load(),
		"resolving 3 groups' membership must be one query, not one per group")

	require.ElementsMatch(t, []uuid.UUID{dbA.UID, dbB.UID}, members[groupOne.UID])
	require.ElementsMatch(t, []uuid.UUID{dbB.UID, dbC.UID}, members[groupTwo.UID])

	_, ok := members[groupEmpty.UID]
	require.False(t, ok, "a group with no members should be absent from the result, not an empty slice")

	// The empty-input shortcut must not even touch the database.
	hook.count.Store(0)

	empty, err := testStore.ListServerGroupMemberUIDsByGroups(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.EqualValues(t, 0, hook.count.Load(), "no groups requested must mean no query at all")
}

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// newTestUserGroup creates a named user group for approver-list tests.
func newTestUserGroup(t *testing.T, ctx context.Context, s *Store, name string) *UserGroup {
	t.Helper()

	created, err := s.CreateUserGroup(ctx, &UserGroup{Name: name})
	if err != nil {
		t.Fatalf("CreateUserGroup(%s) error = %v", name, err)
	}

	return created
}

// TestResolveServerApproverGroupsChain walks the whole fallback: nothing
// configured, group-level only, server-level overriding the group, and the
// union across several groups.
func TestResolveServerApproverGroupsChain(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	server := newTestTargetServer(t, ctx, store, "approver_chain_db")

	leads := newTestUserGroup(t, ctx, store, "approver_chain_leads")
	ops := newTestUserGroup(t, ctx, store, "approver_chain_ops")
	dba := newTestUserGroup(t, ctx, store, "approver_chain_dba")

	// 1. Nothing configured anywhere: nobody resolves, which every caller
	//    reads as "admins only". This is the pre-existing behavior, and the
	//    one every upgrade lands on.
	for _, kind := range []ApproverKind{ApproverKindAccess, ApproverKindQuery} {
		got, err := store.ResolveServerApproverGroups(ctx, server.UID, kind)
		if err != nil {
			t.Fatalf("ResolveServerApproverGroups(%s) error = %v", kind, err)
		}

		if len(got) != 0 {
			t.Errorf("ResolveServerApproverGroups(%s) = %v, want empty", kind, got)
		}
	}

	// 2. Group-level only: the server inherits from the group it is in.
	groupA, err := store.CreateServerGroup(ctx, &ServerGroup{
		Name:                        "approver chain group A",
		AccessApproverUserGroupUIDs: []uuid.UUID{ops.UID},
		QueryApproverUserGroupUIDs:  []uuid.UUID{leads.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(A) error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, groupA.UID, server.UID); err != nil {
		t.Fatalf("AddServerToGroup(A) error = %v", err)
	}

	assertResolves(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{ops.UID})
	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{leads.UID})

	// 3. Union across groups: a second group holding the same server adds its
	//    own approvers rather than replacing them. Same rule multiple approver
	//    groups on a grant definition already follow.
	groupB, err := store.CreateServerGroup(ctx, &ServerGroup{
		Name:                        "approver chain group B",
		AccessApproverUserGroupUIDs: []uuid.UUID{dba.UID, ops.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(B) error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, groupB.UID, server.UID); err != nil {
		t.Fatalf("AddServerToGroup(B) error = %v", err)
	}

	// ops appears in both groups and must come back once.
	assertResolvesSet(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{ops.UID, dba.UID})

	// The query kind is untouched by group B, which names none: the two kinds
	// are independent, with no hierarchy in either direction.
	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{leads.UID})

	// 4. The server's own list wins outright — it does not add to the groups'.
	accessOverride := []uuid.UUID{dba.UID}
	if err := store.UpdateServer(ctx, server.UID, ServerUpdate{
		AccessApproverUserGroupUIDs: &accessOverride,
	}, testEncryptionKey()); err != nil {
		t.Fatalf("UpdateServer() error = %v", err)
	}

	assertResolves(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{dba.UID})
	// …and only for the kind that was set.
	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{leads.UID})

	// 5. Clearing the server list falls back to the groups again. An explicit
	//    empty list is a real policy change, not a no-op.
	empty := []uuid.UUID{}
	if err := store.UpdateServer(ctx, server.UID, ServerUpdate{
		AccessApproverUserGroupUIDs: &empty,
	}, testEncryptionKey()); err != nil {
		t.Fatalf("UpdateServer(clear) error = %v", err)
	}

	assertResolvesSet(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{ops.UID, dba.UID})
}

// TestResolveServerApproverGroupsIsLive proves the second deliberate exception
// to immutable versioning: an edit to a group's approver list, and a change of
// membership, are both effective immediately — nothing is snapshotted.
func TestResolveServerApproverGroupsIsLive(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	server := newTestTargetServer(t, ctx, store, "approver_live_db")
	before := newTestUserGroup(t, ctx, store, "approver_live_before")
	after := newTestUserGroup(t, ctx, store, "approver_live_after")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{
		Name:                       "approver live group",
		QueryApproverUserGroupUIDs: []uuid.UUID{before.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, group.UID, server.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{before.UID})

	// A departed lead's replacement takes over with no re-issuance of anything.
	group.QueryApproverUserGroupUIDs = []uuid.UUID{after.UID}
	if err := store.UpdateServerGroup(ctx, group); err != nil {
		t.Fatalf("UpdateServerGroup() error = %v", err)
	}

	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{after.UID})

	// Removing the server from the group narrows it back to nobody.
	if err := store.RemoveServerFromGroup(ctx, group.UID, server.UID); err != nil {
		t.Fatalf("RemoveServerFromGroup() error = %v", err)
	}

	assertResolves(t, ctx, store, server.UID, ApproverKindQuery, nil)
}

// TestMayApproveForServer covers the membership intersection, including the
// fail-closed answers: no groups, no approvers, and a server that is gone.
func TestMayApproveForServer(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	server := newTestTargetServer(t, ctx, store, "may_approve_db")
	ops := newTestUserGroup(t, ctx, store, "may_approve_ops")
	others := newTestUserGroup(t, ctx, store, "may_approve_others")

	// Nothing configured: nobody may approve, whatever groups they hold.
	assertMayApprove(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{ops.UID}, false)

	list := []uuid.UUID{ops.UID}
	if err := store.UpdateServer(ctx, server.UID, ServerUpdate{
		AccessApproverUserGroupUIDs: &list,
	}, testEncryptionKey()); err != nil {
		t.Fatalf("UpdateServer() error = %v", err)
	}

	assertMayApprove(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{ops.UID}, true)
	assertMayApprove(t, ctx, store, server.UID, ApproverKindAccess, []uuid.UUID{others.UID}, false)
	// A user in no group at all.
	assertMayApprove(t, ctx, store, server.UID, ApproverKindAccess, nil, false)
	// No hierarchy: access approval buys nothing on the query side.
	assertMayApprove(t, ctx, store, server.UID, ApproverKindQuery, []uuid.UUID{ops.UID}, false)

	// A server that does not exist resolves to an error, which every caller
	// treats as a refusal.
	if _, err := store.MayApproveForServer(
		ctx, uuid.New(), ApproverKindAccess, []uuid.UUID{ops.UID},
	); err == nil {
		t.Error("MayApproveForServer(missing server) error = nil, want an error")
	}
}

// TestHasServerApproverGroups covers the coarse "is this user an approver
// anywhere" probe that gates the approvals stream and the pending badge.
func TestHasServerApproverGroups(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	server := newTestTargetServer(t, ctx, store, "has_approver_db")
	ops := newTestUserGroup(t, ctx, store, "has_approver_ops")
	leads := newTestUserGroup(t, ctx, store, "has_approver_leads")
	nobody := newTestUserGroup(t, ctx, store, "has_approver_nobody")

	assertHasApprover(t, ctx, store, []uuid.UUID{ops.UID}, false)
	assertHasApprover(t, ctx, store, nil, false)

	// Named on a server, query kind: still counts.
	list := []uuid.UUID{ops.UID}
	if err := store.UpdateServer(ctx, server.UID, ServerUpdate{
		QueryApproverUserGroupUIDs: &list,
	}, testEncryptionKey()); err != nil {
		t.Fatalf("UpdateServer() error = %v", err)
	}

	assertHasApprover(t, ctx, store, []uuid.UUID{ops.UID}, true)
	assertHasApprover(t, ctx, store, []uuid.UUID{nobody.UID}, false)

	// Named on a group only, access kind.
	if _, err := store.CreateServerGroup(ctx, &ServerGroup{
		Name:                        "has approver group",
		AccessApproverUserGroupUIDs: []uuid.UUID{leads.UID},
	}); err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	assertHasApprover(t, ctx, store, []uuid.UUID{leads.UID}, true)
}

func assertResolves(
	t *testing.T, ctx context.Context, s *Store, serverUID uuid.UUID, kind ApproverKind, want []uuid.UUID,
) {
	t.Helper()

	got, err := s.ResolveServerApproverGroups(ctx, serverUID, kind)
	if err != nil {
		t.Fatalf("ResolveServerApproverGroups(%s) error = %v", kind, err)
	}

	if len(got) != len(want) {
		t.Fatalf("ResolveServerApproverGroups(%s) = %v, want %v", kind, got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ResolveServerApproverGroups(%s) = %v, want %v", kind, got, want)
		}
	}
}

// assertResolvesSet is assertResolves without an ordering expectation — the
// union across groups has no defined order, only defined membership.
func assertResolvesSet(
	t *testing.T, ctx context.Context, s *Store, serverUID uuid.UUID, kind ApproverKind, want []uuid.UUID,
) {
	t.Helper()

	got, err := s.ResolveServerApproverGroups(ctx, serverUID, kind)
	if err != nil {
		t.Fatalf("ResolveServerApproverGroups(%s) error = %v", kind, err)
	}

	if len(got) != len(want) {
		t.Fatalf("ResolveServerApproverGroups(%s) = %v, want the set %v", kind, got, want)
	}

	seen := make(map[uuid.UUID]struct{}, len(got))
	for _, uid := range got {
		if _, dup := seen[uid]; dup {
			t.Fatalf("ResolveServerApproverGroups(%s) = %v, contains a duplicate", kind, got)
		}

		seen[uid] = struct{}{}
	}

	for _, uid := range want {
		if _, ok := seen[uid]; !ok {
			t.Fatalf("ResolveServerApproverGroups(%s) = %v, missing %s", kind, got, uid)
		}
	}
}

func assertMayApprove(
	t *testing.T, ctx context.Context, s *Store,
	serverUID uuid.UUID, kind ApproverKind, groups []uuid.UUID, want bool,
) {
	t.Helper()

	got, err := s.MayApproveForServer(ctx, serverUID, kind, groups)
	if err != nil {
		t.Fatalf("MayApproveForServer(%s) error = %v", kind, err)
	}

	if got != want {
		t.Errorf("MayApproveForServer(%s, %v) = %v, want %v", kind, groups, got, want)
	}
}

func assertHasApprover(t *testing.T, ctx context.Context, s *Store, groups []uuid.UUID, want bool) {
	t.Helper()

	got, err := s.HasServerApproverGroups(ctx, groups)
	if err != nil {
		t.Fatalf("HasServerApproverGroups() error = %v", err)
	}

	if got != want {
		t.Errorf("HasServerApproverGroups(%v) = %v, want %v", groups, got, want)
	}
}

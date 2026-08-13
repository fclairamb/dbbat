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

// TestResolveServerApproverGroupsByServers pins the point of the batched
// resolver: whatever the number of servers, resolution costs a fixed two
// queries — and it returns, per server, exactly what the per-server function
// returns for that server.
//
// The count is the contract. A listing of N pending items used to fire 2N round
// trips because the chain was asked once per row; if a future edit reintroduces
// a per-server lookup this test fails before anybody notices the latency.
func TestResolveServerApproverGroupsByServers(t *testing.T) {
	t.Parallel()

	testStore := setupTestStore(t)
	ctx := context.Background()

	own := newTestTargetServer(t, ctx, testStore, "batch_appr_own")
	grouped := newTestTargetServer(t, ctx, testStore, "batch_appr_grouped")
	twoGroups := newTestTargetServer(t, ctx, testStore, "batch_appr_two_groups")
	bare := newTestTargetServer(t, ctx, testStore, "batch_appr_bare")

	ops := newTestUserGroup(t, ctx, testStore, "batch_appr_ops")
	leads := newTestUserGroup(t, ctx, testStore, "batch_appr_leads")
	dba := newTestUserGroup(t, ctx, testStore, "batch_appr_dba")

	// A server naming its own access approvers — level 1, which wins outright.
	ownList := []uuid.UUID{ops.UID}
	if err := testStore.UpdateServer(ctx, own.UID, ServerUpdate{
		AccessApproverUserGroupUIDs: &ownList,
	}, testEncryptionKey()); err != nil {
		t.Fatalf("UpdateServer(own) error = %v", err)
	}

	// Level 2 for the other two, including a union across groups.
	groupA, err := testStore.CreateServerGroup(ctx, &ServerGroup{
		Name:                        "batch appr group A",
		AccessApproverUserGroupUIDs: []uuid.UUID{leads.UID},
		QueryApproverUserGroupUIDs:  []uuid.UUID{dba.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(A) error = %v", err)
	}

	groupB, err := testStore.CreateServerGroup(ctx, &ServerGroup{
		Name:                        "batch appr group B",
		AccessApproverUserGroupUIDs: []uuid.UUID{dba.UID, leads.UID},
	})
	if err != nil {
		t.Fatalf("CreateServerGroup(B) error = %v", err)
	}

	for _, m := range []struct {
		group  uuid.UUID
		server uuid.UUID
	}{
		{groupA.UID, grouped.UID},
		{groupA.UID, twoGroups.UID},
		{groupB.UID, twoGroups.UID},
		// The level-1 server is in a group too: its own list must still win.
		{groupA.UID, own.UID},
	} {
		if err := testStore.AddServerToGroup(ctx, m.group, m.server); err != nil {
			t.Fatalf("AddServerToGroup() error = %v", err)
		}
	}

	missing := uuid.New()
	all := []uuid.UUID{own.UID, grouped.UID, twoGroups.UID, bare.UID, missing}

	// Both queries name the approver columns, and nothing else the resolver
	// runs does, so this substring counts exactly the resolver's round trips.
	hook := &queryCountHook{substring: "approver_user_group_uids"}
	testStore.db.AddQueryHook(hook)

	resolved, err := testStore.ResolveServerApproverGroupsByServers(ctx, all, ApproverKindAccess)
	if err != nil {
		t.Fatalf("ResolveServerApproverGroupsByServers() error = %v", err)
	}

	if got := hook.count.Load(); got != 2 {
		t.Errorf("resolving %d servers took %d queries, want 2 (one for servers, one for their groups)",
			len(all), got)
	}

	assertSameUUIDSet(t, "own list wins", resolved[own.UID], []uuid.UUID{ops.UID})
	assertSameUUIDSet(t, "single group", resolved[grouped.UID], []uuid.UUID{leads.UID})
	// leads is named by both groups and must come back once.
	assertSameUUIDSet(t, "union across groups", resolved[twoGroups.UID], []uuid.UUID{leads.UID, dba.UID})
	assertSameUUIDSet(t, "nothing configured", resolved[bare.UID], nil)

	if _, present := resolved[bare.UID]; !present {
		t.Error("a server that exists but names nobody must have an entry, not be absent")
	}

	if _, present := resolved[missing]; present {
		t.Error("a server that does not exist must be absent from the result")
	}

	// The batched answer and the per-server answer are the same answer: the
	// single-server function is a wrapper over this one, so any divergence here
	// would mean the chain got implemented twice.
	for _, kind := range []ApproverKind{ApproverKindAccess, ApproverKindQuery} {
		batched, err := testStore.ResolveServerApproverGroupsByServers(
			ctx, []uuid.UUID{own.UID, grouped.UID, twoGroups.UID, bare.UID}, kind,
		)
		if err != nil {
			t.Fatalf("ResolveServerApproverGroupsByServers(%s) error = %v", kind, err)
		}

		for _, serverUID := range []uuid.UUID{own.UID, grouped.UID, twoGroups.UID, bare.UID} {
			single, err := testStore.ResolveServerApproverGroups(ctx, serverUID, kind)
			if err != nil {
				t.Fatalf("ResolveServerApproverGroups(%s) error = %v", kind, err)
			}

			assertSameUUIDSet(t, "batched vs per-server ("+string(kind)+")", batched[serverUID], single)
		}
	}

	// The per-server wrapper must still fail closed on a server that is gone.
	if _, err := testStore.ResolveServerApproverGroups(ctx, missing, ApproverKindAccess); err == nil {
		t.Error("ResolveServerApproverGroups(missing server) error = nil, want an error")
	}

	// And the empty-input shortcut must not touch the database at all.
	hook.count.Store(0)

	empty, err := testStore.ResolveServerApproverGroupsByServers(ctx, nil, ApproverKindAccess)
	if err != nil {
		t.Fatalf("ResolveServerApproverGroupsByServers(nil) error = %v", err)
	}

	if len(empty) != 0 {
		t.Errorf("ResolveServerApproverGroupsByServers(nil) = %v, want empty", empty)
	}

	if got := hook.count.Load(); got != 0 {
		t.Errorf("no servers requested took %d queries, want 0", got)
	}
}

// TestServerApproversMayApprove covers the decision half applied to an
// already-resolved answer — the same intersection MayApproveForServer makes,
// including every fail-closed direction.
func TestServerApproversMayApprove(t *testing.T) {
	t.Parallel()

	server := uuid.New()
	ops := uuid.New()
	others := uuid.New()

	resolved := ServerApprovers{server: {ops}}

	if !resolved.MayApprove(server, []uuid.UUID{others, ops}) {
		t.Error("MayApprove(named group) = false, want true")
	}

	if resolved.MayApprove(server, []uuid.UUID{others}) {
		t.Error("MayApprove(unrelated group) = true, want false")
	}

	if resolved.MayApprove(server, nil) {
		t.Error("MayApprove(no user groups) = true, want false")
	}

	if resolved.MayApprove(uuid.New(), []uuid.UUID{ops}) {
		t.Error("MayApprove(unknown server) = true, want false")
	}

	var absent ServerApprovers
	if absent.MayApprove(server, []uuid.UUID{ops}) {
		t.Error("MayApprove on a nil map = true, want false")
	}
}

// assertSameUUIDSet compares two uid lists as sets, and rejects duplicates in
// the first — the union rule promises membership, not order, but never repeats.
func assertSameUUIDSet(t *testing.T, what string, got, want []uuid.UUID) {
	t.Helper()

	if len(got) != len(want) {
		t.Errorf("%s = %v, want the set %v", what, got, want)

		return
	}

	seen := make(map[uuid.UUID]struct{}, len(got))

	for _, uid := range got {
		if _, dup := seen[uid]; dup {
			t.Errorf("%s = %v, contains a duplicate", what, got)

			return
		}

		seen[uid] = struct{}{}
	}

	for _, uid := range want {
		if _, ok := seen[uid]; !ok {
			t.Errorf("%s = %v, missing %s", what, got, uid)
		}
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

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGroupBoundGrantCoversServersAddedLater is the point of the whole
// feature: a grant bound to a server group authorizes a database that joined
// that group *after* the grant was issued, with no re-issuance and no edit.
//
// It runs against GetActiveGrant, which is the single function all five
// protocol proxies resolve their session's grant through — so proving it here
// proves it for PostgreSQL, Oracle, MySQL, MongoDB and SQL Server at once.
func TestGroupBoundGrantCoversServersAddedLater(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sgbound")

	user, anchor := createTestUserAndDatabase(t, ctx, store, "sgbound")
	joinsLater := newTestTargetServer(t, ctx, store, "sgbound_later")
	neverJoins := newTestTargetServer(t, ctx, store, "sgbound_never")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "bound-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, group.UID, anchor.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	def := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{
		Controls:        []string{ControlReadOnly},
		ServerGroupUIDs: []uuid.UUID{group.UID},
	})

	now := time.Now()
	grant := newTestGrant(t, ctx, store, def, user.UID, anchor.UID, admin.UID, now.Add(-time.Minute), now.Add(time.Hour))

	if grant.ServerGroupUID == nil || *grant.ServerGroupUID != group.UID {
		t.Fatalf("grant.ServerGroupUID = %v, want %v", grant.ServerGroupUID, group.UID)
	}

	// The anchor works, as it always did.
	if _, err := store.GetActiveGrant(ctx, user.UID, anchor.UID); err != nil {
		t.Fatalf("GetActiveGrant(anchor) error = %v", err)
	}

	// A database not in the group is not covered.
	if _, err := store.GetActiveGrant(ctx, user.UID, joinsLater.UID); !errors.Is(err, ErrNoActiveGrant) {
		t.Fatalf("GetActiveGrant(before joining) error = %v, want ErrNoActiveGrant", err)
	}

	// Membership is live: adding the server extends the grant that is already
	// running, with no new grant row.
	if err := store.AddServerToGroup(ctx, group.UID, joinsLater.UID); err != nil {
		t.Fatalf("AddServerToGroup(later) error = %v", err)
	}

	got, err := store.GetActiveGrant(ctx, user.UID, joinsLater.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant(after joining) error = %v, want the same grant", err)
	}

	if got.UID != grant.UID {
		t.Errorf("GetActiveGrant(after joining) = %v, want the already-live grant %v", got.UID, grant.UID)
	}

	// And a database that never joined is still refused.
	if _, err := store.GetActiveGrant(ctx, user.UID, neverJoins.UID); !errors.Is(err, ErrNoActiveGrant) {
		t.Fatalf("GetActiveGrant(never joined) error = %v, want ErrNoActiveGrant", err)
	}

	// Removing it again narrows the grant back, in the same live fashion.
	if err := store.RemoveServerFromGroup(ctx, group.UID, joinsLater.UID); err != nil {
		t.Fatalf("RemoveServerFromGroup() error = %v", err)
	}

	if _, err := store.GetActiveGrant(ctx, user.UID, joinsLater.UID); !errors.Is(err, ErrNoActiveGrant) {
		t.Fatalf("GetActiveGrant(after removal) error = %v, want ErrNoActiveGrant", err)
	}
}

// TestAnchorOnlyGrantIsUnaffectedByGroups pins the compatibility half: a grant
// issued from an unscoped definition binds to no group and keeps covering
// exactly one database, whatever the fleet's group membership looks like.
func TestAnchorOnlyGrantIsUnaffectedByGroups(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sganchor")
	user, anchor := createTestUserAndDatabase(t, ctx, store, "sganchor")
	sibling := newTestTargetServer(t, ctx, store, "sganchor_sibling")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "anchor-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	// Both databases are in a group; the definition simply does not scope on
	// it, so the grant must not pick the group up.
	for _, uid := range []uuid.UUID{anchor.UID, sibling.UID} {
		if err := store.AddServerToGroup(ctx, group.UID, uid); err != nil {
			t.Fatalf("AddServerToGroup() error = %v", err)
		}
	}

	def := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{Controls: []string{ControlReadOnly}})

	now := time.Now()
	grant := newTestGrant(t, ctx, store, def, user.UID, anchor.UID, admin.UID, now.Add(-time.Minute), now.Add(time.Hour))

	if grant.ServerGroupUID != nil {
		t.Errorf("grant from an unscoped definition bound to %v, want no binding", *grant.ServerGroupUID)
	}

	if _, err := store.GetActiveGrant(ctx, user.UID, sibling.UID); !errors.Is(err, ErrNoActiveGrant) {
		t.Errorf("GetActiveGrant(sibling) error = %v, want ErrNoActiveGrant", err)
	}
}

// TestPriorityRanksOverlappingServerGroups pins the ranking rule for the case
// group-bound grants introduce: two grants whose groups overlap on one
// database. On that database the higher priority wins; on a database only one
// group holds, that group's grant wins whatever its priority.
func TestPriorityRanksOverlappingServerGroups(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sgprio")
	user, shared := createTestUserAndDatabase(t, ctx, store, "sgprio")
	readOnlyOnly := newTestTargetServer(t, ctx, store, "sgprio_ro")

	// groupWrite holds only the shared database; groupRead holds both.
	groupWrite, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "write-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup(write) error = %v", err)
	}

	groupRead, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "read-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup(read) error = %v", err)
	}

	if err := store.SetServerGroupMembers(ctx, groupWrite.UID, []uuid.UUID{shared.UID}); err != nil {
		t.Fatalf("SetServerGroupMembers(write) error = %v", err)
	}

	if err := store.SetServerGroupMembers(ctx, groupRead.UID, []uuid.UUID{shared.UID, readOnlyOnly.UID}); err != nil {
		t.Fatalf("SetServerGroupMembers(read) error = %v", err)
	}

	writeDef := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{
		ServerGroupUIDs: []uuid.UUID{groupWrite.UID},
	})

	readDef := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{
		Controls:        []string{ControlReadOnly},
		ServerGroupUIDs: []uuid.UUID{groupRead.UID},
	})

	now := time.Now()

	writeGrant := newTestGrant(t, ctx, store, writeDef,
		user.UID, shared.UID, admin.UID, now.Add(-time.Minute), now.Add(time.Hour))

	readGrant := newTestGrant(t, ctx, store, readDef,
		user.UID, readOnlyOnly.UID, admin.UID, now.Add(-time.Minute), now.Add(2*time.Hour))

	if writeGrant.Priority <= readGrant.Priority {
		t.Fatalf("fixture is wrong: write priority %d must beat read priority %d",
			writeGrant.Priority, readGrant.Priority)
	}

	// On the overlap, the writable grant wins on priority — even though the
	// read-only grant lasts longer, which is the next tie-break down.
	got, err := store.GetActiveGrant(ctx, user.UID, shared.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant(shared) error = %v", err)
	}

	if got.UID != writeGrant.UID {
		t.Errorf("GetActiveGrant(shared) = %v, want the higher-priority write grant %v", got.UID, writeGrant.UID)
	}

	// Outside the overlap, only the read-only group covers the database, so
	// its grant is the answer regardless of the other's higher rank.
	got, err = store.GetActiveGrant(ctx, user.UID, readOnlyOnly.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant(read-only) error = %v", err)
	}

	if got.UID != readGrant.UID {
		t.Errorf("GetActiveGrant(read-only) = %v, want the read grant %v", got.UID, readGrant.UID)
	}

	if !got.IsReadOnly() {
		t.Error("the grant selected outside the overlap must carry the read-only shape")
	}
}

// TestGroupBoundGrantQuotaSpansTheGroup pins the quota decision: one budget
// for the grant, consumed across every database in its group, rather than one
// budget per database.
func TestGroupBoundGrantQuotaSpansTheGroup(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sgquota")
	user, dbA := createTestUserAndDatabase(t, ctx, store, "sgquota")
	dbB := newTestTargetServer(t, ctx, store, "sgquota_b")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "quota-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	if err := store.SetServerGroupMembers(ctx, group.UID, []uuid.UUID{dbA.UID, dbB.UID}); err != nil {
		t.Fatalf("SetServerGroupMembers() error = %v", err)
	}

	maxQueries := int64(10)

	def := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{
		Controls:        []string{ControlReadOnly},
		MaxQueryCounts:  &maxQueries,
		ServerGroupUIDs: []uuid.UUID{group.UID},
	})

	now := time.Now()
	grant := newTestGrant(t, ctx, store, def, user.UID, dbA.UID, admin.UID, now.Add(-time.Hour), now.Add(time.Hour))

	// One query on each database, both on *unstamped* connections — the
	// fallback attribution path, which is the half that used to be pinned to
	// the anchor database.
	for _, dbUID := range []uuid.UUID{dbA.UID, dbB.UID} {
		conn, err := store.CreateConnection(ctx, user.UID, dbUID, "10.0.0.9")
		if err != nil {
			t.Fatalf("CreateConnection() error = %v", err)
		}

		// Clear the auth-time stamp so this exercises the (user, database)
		// fallback rather than the grant_uid shortcut.
		if _, err := store.db.NewUpdate().
			Model((*Connection)(nil)).
			Set("grant_uid = NULL").
			Where("uid = ?", conn.UID).
			Exec(ctx); err != nil {
			t.Fatalf("clear grant stamp: %v", err)
		}

		if _, err := store.CreateQuery(ctx, &Query{
			ConnectionID: conn.UID,
			SQLText:      "SELECT 1",
			ExecutedAt:   now,
		}); err != nil {
			t.Fatalf("CreateQuery() error = %v", err)
		}
	}

	reloaded, err := store.GetGrantByUID(ctx, grant.UID)
	if err != nil {
		t.Fatalf("GetGrantByUID() error = %v", err)
	}

	if reloaded.QueryCount != 2 {
		t.Errorf("QueryCount = %d, want 2 — the budget spans every database in the group", reloaded.QueryCount)
	}
}

// TestApprovedRequestBindsTheGrantToTheServerGroup pins the other
// materialization path: a grant issued by approving a request is bound to the
// definition's server group inside the same transaction that issues it, so it
// widens with the group exactly like an admin-assigned one.
func TestApprovedRequestBindsTheGrantToTheServerGroup(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	admin := createTestAdmin(t, ctx, store, "sgreq")
	user, anchor := createTestUserAndDatabase(t, ctx, store, "sgreq")
	joinsLater := newTestTargetServer(t, ctx, store, "sgreq_later")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "req-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateServerGroup() error = %v", err)
	}

	if err := store.AddServerToGroup(ctx, group.UID, anchor.UID); err != nil {
		t.Fatalf("AddServerToGroup() error = %v", err)
	}

	def := newTestGrantDefinition(t, ctx, store, admin.UID, GrantDefinition{
		Controls:        []string{ControlReadOnly},
		ServerGroupUIDs: []uuid.UUID{group.UID},
	})

	req, err := store.CreateGrantRequest(ctx, &GrantRequest{
		UserID:            user.UID,
		GrantDefinitionID: def.UID,
		DatabaseID:        anchor.UID,
	})
	if err != nil {
		t.Fatalf("CreateGrantRequest() error = %v", err)
	}

	grant, _, err := store.ApproveGrantRequest(ctx, req.UID, admin.UID)
	if err != nil {
		t.Fatalf("ApproveGrantRequest() error = %v", err)
	}

	if grant.ServerGroupUID == nil || *grant.ServerGroupUID != group.UID {
		t.Fatalf("approved grant ServerGroupUID = %v, want %v", grant.ServerGroupUID, group.UID)
	}

	if err := store.AddServerToGroup(ctx, group.UID, joinsLater.UID); err != nil {
		t.Fatalf("AddServerToGroup(later) error = %v", err)
	}

	got, err := store.GetActiveGrant(ctx, user.UID, joinsLater.UID)
	if err != nil {
		t.Fatalf("GetActiveGrant(after joining) error = %v", err)
	}

	if got.UID != grant.UID {
		t.Errorf("GetActiveGrant(after joining) = %v, want the approved grant %v", got.UID, grant.UID)
	}
}

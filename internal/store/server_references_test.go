package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetServerReferencesCountsLiveAccess pins the number the edit dialog puts
// in front of an admin before they move a server's target: a revoked grant is
// not access, an expired one is not access, and a grant bound to a group that
// holds the row is.
func TestGetServerReferencesCountsLiveAccess(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "refs_live")
	now := time.Now()

	empty, err := store.GetServerReferences(ctx, database.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), empty.ActiveGrants, "a fresh row is reached by nothing")
	assert.Equal(t, int64(0), empty.ServerGroups)

	// One live grant anchored on the row.
	live, err := createGrantWithShape(t, ctx, store, testGrantSpec{
		UserID:     user.UID,
		DatabaseID: database.UID,
		GrantedBy:  user.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	require.NoError(t, err)

	// One already expired, which must not count.
	_, err = createGrantWithShape(t, ctx, store, testGrantSpec{
		UserID:     user.UID,
		DatabaseID: database.UID,
		GrantedBy:  user.UID,
		StartsAt:   now.Add(-4 * time.Hour),
		ExpiresAt:  now.Add(-2 * time.Hour),
	})
	require.NoError(t, err)

	refs, err := store.GetServerReferences(ctx, database.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), refs.ActiveGrants, "only the live grant reaches the new target")

	// Revoking it takes it out of the count, exactly as it takes it out of
	// what the proxy admits.
	require.NoError(t, store.RevokeGrant(ctx, live.UID, user.UID))

	refs, err = store.GetServerReferences(ctx, database.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), refs.ActiveGrants, "a revoked grant reaches nothing")
}

// TestGetServerReferencesCountsGroupBoundAccess covers the half the anchor
// column alone would miss: a grant issued against *another* server in a group
// this row belongs to covers this row too, so an edit here moves it.
func TestGetServerReferencesCountsGroupBoundAccess(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, anchor := createTestUserAndDatabase(t, ctx, store, "refs_group_anchor")
	_, sibling := createTestUserAndDatabase(t, ctx, store, "refs_group_sibling")

	group, err := store.CreateServerGroup(ctx, &ServerGroup{Name: "refs-group", CreatedBy: &user.UID})
	require.NoError(t, err)
	require.NoError(t, store.AddServerToGroup(ctx, group.UID, anchor.UID))
	require.NoError(t, store.AddServerToGroup(ctx, group.UID, sibling.UID))

	now := time.Now()
	def := newTestGrantDefinition(t, ctx, store, user.UID, GrantDefinition{
		ServerGroupUIDs: []uuid.UUID{group.UID},
	})
	newTestGrant(t, ctx, store, def, user.UID, anchor.UID, user.UID, now.Add(-time.Hour), now.Add(time.Hour))

	// The sibling was never granted anything directly, yet the grant reaches
	// it through the group — so editing its host moves that grant.
	refs, err := store.GetServerReferences(ctx, sibling.UID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), refs.ActiveGrants, "a group-bound grant reaches every member")
	assert.Equal(t, int64(1), refs.ServerGroups, "the row is carried by one group")
}

// TestServerHasHistoryIsWiderThanLiveness is the protocol guard's predicate: a
// revoked grant and a closed session are both history, even though neither is
// live access.
func TestServerHasHistoryIsWiderThanLiveness(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	user, database := createTestUserAndDatabase(t, ctx, store, "history_probe")

	used, err := store.ServerHasHistory(ctx, database.UID)
	require.NoError(t, err)
	assert.False(t, used, "a pristine row may still change protocol")

	conn, err := store.CreateConnection(ctx, user.UID, database.UID, "10.0.0.7")
	require.NoError(t, err)
	require.NoError(t, store.CloseConnection(ctx, conn.UID))

	used, err = store.ServerHasHistory(ctx, database.UID)
	require.NoError(t, err)
	assert.True(t, used, "a closed session is history the protocol must not re-label")

	// And a grant alone is enough, with no session at all.
	_, other := createTestUserAndDatabase(t, ctx, store, "history_grant_only")
	now := time.Now()
	grant, err := createGrantWithShape(t, ctx, store, testGrantSpec{
		UserID:     user.UID,
		DatabaseID: other.UID,
		GrantedBy:  user.UID,
		StartsAt:   now.Add(-time.Hour),
		ExpiresAt:  now.Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, store.RevokeGrant(ctx, grant.UID, user.UID))

	used, err = store.ServerHasHistory(ctx, other.UID)
	require.NoError(t, err)
	assert.True(t, used, "even a revoked grant is a reference the row already carries")
}

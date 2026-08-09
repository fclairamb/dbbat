package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/events"
	"github.com/fclairamb/dbbat/internal/store"
)

// newServerGroupFixture builds an MCP server whose single grant is bound to a
// server group, with the group's membership and the server rows under the
// test's control. Coverage is the point here, not execution, so the executor
// is left unused.
func newServerGroupFixture(
	t *testing.T,
	anchor uuid.UUID,
	groupUID uuid.UUID,
	members []uuid.UUID,
	servers map[uuid.UUID]*store.Server,
) (*Server, *Caller) {
	t.Helper()

	userUID := uuid.New()
	def := store.GrantDefinition{Controls: []string{store.ControlReadOnly}}

	grant := store.Grant{
		UID:            uuid.New(),
		UserID:         userUID,
		DatabaseID:     anchor,
		ServerGroupUID: &groupUID,
		StartsAt:       time.Now().Add(-time.Minute),
		ExpiresAt:      time.Now().Add(time.Hour),
		Definition:     &def,
	}

	st := &fakeGrantStore{
		grants:       []store.Grant{grant},
		servers:      servers,
		groupMembers: map[uuid.UUID][]uuid.UUID{groupUID: members},
	}

	broker := events.New()

	s := New(Deps{
		Store:    st,
		Broker:   func() *events.Broker { return broker },
		Executor: &fakeExecutor{calls: make(chan ExecRequest, 4)},
	})

	return s, &Caller{User: &store.User{UID: userUID, Username: testUsername}, APIKey: testAPIKey}
}

func listedNames(t *testing.T, s *Server, caller *Caller) []string {
	t.Helper()

	_, out, err := s.toolListDatabases(caller)(context.Background(), nil, ListDatabasesInput{})
	require.NoError(t, err)

	names := make([]string, 0, len(out.Databases))
	for _, d := range out.Databases {
		names = append(names, d.Name)
	}

	return names
}

// TestListDatabasesFollowsLiveServerGroupMembership pins that the agent-facing
// listing tells the truth about a group-bound grant: it advertises the group's
// *current* membership, including servers that joined after the grant was
// issued.
func TestListDatabasesFollowsLiveServerGroupMembership(t *testing.T) {
	t.Parallel()

	anchor := uuid.New()
	joinedLater := uuid.New()
	groupUID := uuid.New()

	s, caller := newServerGroupFixture(t, anchor, groupUID,
		[]uuid.UUID{anchor, joinedLater},
		map[uuid.UUID]*store.Server{
			anchor:      {UID: anchor, Name: "prod-pg", Protocol: store.ProtocolPostgreSQL},
			joinedLater: {UID: joinedLater, Name: "prod-replica", Protocol: store.ProtocolPostgreSQL},
		})

	require.ElementsMatch(t, []string{"prod-pg", "prod-replica"}, listedNames(t, s, caller))
}

// TestListDatabasesDropsAServerThatLeftTheGroup is the half that matters. A
// bound grant covers its group's live membership *instead of* the database it
// was issued for, so once the anchor leaves the group the proxy refuses it —
// and the listing must stop advertising it. Otherwise the agent plans against
// a database every one of its statements will be denied on.
func TestListDatabasesDropsAServerThatLeftTheGroup(t *testing.T) {
	t.Parallel()

	anchor := uuid.New()
	stillIn := uuid.New()
	groupUID := uuid.New()

	s, caller := newServerGroupFixture(t, anchor, groupUID,
		// The anchor is no longer a member.
		[]uuid.UUID{stillIn},
		map[uuid.UUID]*store.Server{
			anchor:  {UID: anchor, Name: "left-the-group", Protocol: store.ProtocolPostgreSQL},
			stillIn: {UID: stillIn, Name: "prod-replica", Protocol: store.ProtocolPostgreSQL},
		})

	require.Equal(t, []string{"prod-replica"}, listedNames(t, s, caller))
}

// TestListDatabasesKeepsTheAnchorForAnUnboundGrant pins the other side: a
// grant with no server group covers exactly its anchor, and group membership
// elsewhere in the fleet is none of its business.
func TestListDatabasesKeepsTheAnchorForAnUnboundGrant(t *testing.T) {
	t.Parallel()

	anchor := uuid.New()
	userUID := uuid.New()
	def := store.GrantDefinition{Controls: []string{store.ControlReadOnly}}

	st := &fakeGrantStore{
		grants: []store.Grant{{
			UID:        uuid.New(),
			UserID:     userUID,
			DatabaseID: anchor,
			StartsAt:   time.Now().Add(-time.Minute),
			ExpiresAt:  time.Now().Add(time.Hour),
			Definition: &def,
		}},
		servers: map[uuid.UUID]*store.Server{
			anchor: {UID: anchor, Name: "solo-db", Protocol: store.ProtocolPostgreSQL},
		},
	}

	broker := events.New()
	s := New(Deps{
		Store:    st,
		Broker:   func() *events.Broker { return broker },
		Executor: &fakeExecutor{calls: make(chan ExecRequest, 4)},
	})

	caller := &Caller{User: &store.User{UID: userUID, Username: testUsername}, APIKey: testAPIKey}

	require.Equal(t, []string{"solo-db"}, listedNames(t, s, caller))
}

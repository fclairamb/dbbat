package shared_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// fakeTargetStore is an in-memory TargetStore: a handful of server rows plus
// the set of (user, server) pairs that have an active grant.
type fakeTargetStore struct {
	servers []store.Server
	granted map[uuid.UUID]map[uuid.UUID]bool
	listErr error
}

func (f *fakeTargetStore) GetServerByName(_ context.Context, name string) (*store.Server, error) {
	for i := range f.servers {
		if f.servers[i].Name == name {
			return &f.servers[i], nil
		}
	}

	return nil, store.ErrServerNotFound
}

func (f *fakeTargetStore) ListServersByDatabaseName(_ context.Context, dbName string) ([]store.Server, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	var out []store.Server

	for i := range f.servers {
		if f.servers[i].DatabaseName == dbName {
			out = append(out, f.servers[i])
		}
	}

	return out, nil
}

func (f *fakeTargetStore) GetActiveGrant(_ context.Context, userID, dbID uuid.UUID) (*store.Grant, error) {
	if f.granted[userID][dbID] {
		return &store.Grant{UID: uuid.New(), UserID: userID, DatabaseID: dbID}, nil
	}

	return nil, store.ErrGrantNotFound
}

func (f *fakeTargetStore) grant(userID uuid.UUID, servers ...*store.Server) {
	if f.granted == nil {
		f.granted = map[uuid.UUID]map[uuid.UUID]bool{}
	}

	if f.granted[userID] == nil {
		f.granted[userID] = map[uuid.UUID]bool{}
	}

	for _, s := range servers {
		f.granted[userID][s.UID] = true
	}
}

// errFakeStore is the canonical failure a fake store returns; a static error
// keeps the linter happy and makes the intent ("the store is down") explicit.
var errFakeStore = errors.New("fake store failure")

func srv(name, dbName, protocol string) store.Server {
	return store.Server{UID: uuid.New(), Name: name, DatabaseName: dbName, Protocol: protocol}
}

func pgOnly(protocol string) bool { return protocol == store.ProtocolPostgreSQL }

func TestParseUsername(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw      string
		wantUser string
		wantHint string
	}{
		{"alice", "alice", ""},
		{"alice#demo_ro", "alice", "demo_ro"},
		{"florent.clairambault#demo_datalake_ro", "florent.clairambault", "demo_datalake_ro"},
		// A '#' inside the username itself: the LAST one is the separator.
		{"od#d#srv", "od#d", "srv"},
		// Trailing separator selects nothing.
		{"alice#", "alice", ""},
		{"", "", ""},
	}

	for _, tc := range cases {
		user, hint := shared.ParseUsername(tc.raw)
		assert.Equal(t, tc.wantUser, user, "user for %q", tc.raw)
		assert.Equal(t, tc.wantHint, hint, "hint for %q", tc.raw)
	}
}

func TestResolveTarget_NothingRequested(t *testing.T) {
	t.Parallel()

	st := &fakeTargetStore{}
	_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
		UserID:           uuid.New(),
		ProtocolAccepted: pgOnly,
	})
	require.ErrorIs(t, err, shared.ErrNoDatabaseRequested)
}

func TestResolveTarget_Rung1ExactServerName(t *testing.T) {
	t.Parallel()

	ro := srv("demo_datalake_ro", "demo_datalake", store.ProtocolPostgreSQL)
	st := &fakeTargetStore{servers: []store.Server{ro}}
	user := uuid.New()

	// No grant needed: rung 1 is the pre-existing rule, and the grant check
	// happens after resolution in every proxy.
	got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
		UserID:           user,
		RequestedDB:      "demo_datalake_ro",
		ProtocolAccepted: pgOnly,
	})
	require.NoError(t, err)
	assert.Equal(t, ro.UID, got.UID)
}

func TestResolveTarget_ServerNameWinsOverDatabaseName(t *testing.T) {
	t.Parallel()

	// "shared" is BOTH the name of one server and the upstream database_name
	// of another. The name must win, or a database_name would be a way to
	// shadow somebody else's server.
	named := srv("shared", "other_upstream", store.ProtocolPostgreSQL)
	shadow := srv("decoy", "shared", store.ProtocolPostgreSQL)
	st := &fakeTargetStore{servers: []store.Server{named, shadow}}
	user := uuid.New()
	st.grant(user, &shadow)

	got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
		UserID:           user,
		RequestedDB:      "shared",
		ProtocolAccepted: pgOnly,
	})
	require.NoError(t, err)
	assert.Equal(t, named.UID, got.UID, "the exact server-name match must win")
}

func TestResolveTarget_Rung2Hint(t *testing.T) {
	t.Parallel()

	ro := srv("demo_datalake_ro", "demo_datalake", store.ProtocolPostgreSQL)
	st := &fakeTargetStore{servers: []store.Server{ro}}
	user := uuid.New()
	st.grant(user, &ro)

	t.Run("hint plus the real database name", func(t *testing.T) {
		t.Parallel()

		got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			ServerHint:       "demo_datalake_ro",
			RequestedDB:      "demo_datalake",
			ProtocolAccepted: pgOnly,
		})
		require.NoError(t, err)
		assert.Equal(t, ro.UID, got.UID)
	})

	t.Run("hint alone", func(t *testing.T) {
		t.Parallel()

		got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			ServerHint:       "demo_datalake_ro",
			ProtocolAccepted: pgOnly,
		})
		require.NoError(t, err)
		assert.Equal(t, ro.UID, got.UID)
	})

	t.Run("IDE reconnect to a database the server does not expose", func(t *testing.T) {
		t.Parallel()

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			ServerHint:       "demo_datalake_ro",
			RequestedDB:      "postgres",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrDatabaseNotExposed)
		assert.Contains(t, err.Error(), `server "demo_datalake_ro" exposes database "demo_datalake", not "postgres"`)
	})

	t.Run("the mismatch detail is withheld from a caller without a grant", func(t *testing.T) {
		t.Parallel()

		// Same probe, by somebody who holds no grant on that server: the
		// answer must not leak the upstream database name.
		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           uuid.New(),
			ServerHint:       "demo_datalake_ro",
			RequestedDB:      "postgres",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
		assert.NotContains(t, err.Error(), "demo_datalake\"")
	})

	t.Run("unknown server in the hint", func(t *testing.T) {
		t.Parallel()

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			ServerHint:       "nope",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
	})

	t.Run("hint naming a server on another protocol", func(t *testing.T) {
		t.Parallel()

		mysqlSrv := srv("analytics", "analytics_db", store.ProtocolMySQL)
		other := &fakeTargetStore{servers: []store.Server{mysqlSrv}}

		_, err := shared.ResolveTarget(context.Background(), other, shared.TargetRequest{
			UserID:           user,
			ServerHint:       "analytics",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
	})
}

func TestResolveTarget_Rung3Grants(t *testing.T) {
	t.Parallel()

	ro := srv("demo_datalake_ro", "demo_datalake", store.ProtocolPostgreSQL)
	rw := srv("demo_datalake_rw", "demo_datalake", store.ProtocolPostgreSQL)
	foreign := srv("someone_elses", "private_db", store.ProtocolPostgreSQL)
	mysqlTwin := srv("mysql_datalake", "demo_datalake", store.ProtocolMySQL)

	t.Run("single granted candidate resolves", func(t *testing.T) {
		t.Parallel()

		st := &fakeTargetStore{servers: []store.Server{ro, rw, foreign}}
		user := uuid.New()
		st.grant(user, &ro)

		got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "demo_datalake",
			ProtocolAccepted: pgOnly,
		})
		require.NoError(t, err)
		assert.Equal(t, ro.UID, got.UID)
	})

	t.Run("the _ro/_rw twins are ambiguous", func(t *testing.T) {
		t.Parallel()

		st := &fakeTargetStore{servers: []store.Server{ro, rw}}
		user := uuid.New()
		st.grant(user, &ro, &rw)

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "demo_datalake",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetAmbiguous)
		assert.Contains(t, err.Error(), "demo_datalake_ro")
		assert.Contains(t, err.Error(), "demo_datalake_rw")
		assert.Contains(t, err.Error(), "<user>#<server>")
	})

	t.Run("a server the caller holds no grant on is invisible", func(t *testing.T) {
		t.Parallel()

		st := &fakeTargetStore{servers: []store.Server{foreign}}
		user := uuid.New() // no grants at all

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "private_db",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
	})

	t.Run("a granted server on another protocol is not reachable here", func(t *testing.T) {
		t.Parallel()

		st := &fakeTargetStore{servers: []store.Server{mysqlTwin}}
		user := uuid.New()
		st.grant(user, &mysqlTwin)

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "demo_datalake",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
	})

	t.Run("MySQL family accepts both mysql and mariadb", func(t *testing.T) {
		t.Parallel()

		maria := srv("maria_target", "shopdb", store.ProtocolMariaDB)
		st := &fakeTargetStore{servers: []store.Server{maria}}
		user := uuid.New()
		st.grant(user, &maria)

		got, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "shopdb",
			ProtocolAccepted: store.IsMySQLFamily,
		})
		require.NoError(t, err)
		assert.Equal(t, maria.UID, got.UID)
	})

	t.Run("a store failure is not-found, never an open door", func(t *testing.T) {
		t.Parallel()

		st := &fakeTargetStore{servers: []store.Server{ro}, listErr: errFakeStore}
		user := uuid.New()
		st.grant(user, &ro)

		_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
			UserID:           user,
			RequestedDB:      "demo_datalake",
			ProtocolAccepted: pgOnly,
		})
		require.ErrorIs(t, err, shared.ErrTargetNotFound)
	})
}

func TestResolveTarget_NilProtocolPredicateFailsClosed(t *testing.T) {
	t.Parallel()

	ro := srv("demo_datalake_ro", "demo_datalake", store.ProtocolPostgreSQL)
	st := &fakeTargetStore{servers: []store.Server{ro}}
	user := uuid.New()
	st.grant(user, &ro)

	_, err := shared.ResolveTarget(context.Background(), st, shared.TargetRequest{
		UserID:      user,
		RequestedDB: "demo_datalake_ro",
	})
	require.Error(t, err)
}

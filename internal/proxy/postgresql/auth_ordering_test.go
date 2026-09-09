package postgresql

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// The fleet these tests describe: one dbbat entry named `leak_ro` that exposes
// the upstream database `leakdb`. Neither string may be readable by anyone who
// has not proven they are `leakuser`.
const (
	leakEntryName   = "leak_ro"
	leakUpstreamDB  = "leakdb"
	leakUsername    = "leakuser"
	leakPassword    = "leak-password"
	leakBadPassword = "not-the-password"
)

// captureConn stands in for the client socket on the write side only: the
// authentication path reads through s.clientReader (a scripted byte stream) and
// writes through s.clientConn, so recording the writes is enough to see exactly
// what a client would have been told.
type captureConn struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.buf.Write(p)
}

func (c *captureConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *captureConn) Close() error             { return nil }
func (c *captureConn) LocalAddr() net.Addr      { return &net.TCPAddr{IP: net.IPv4zero} }
func (c *captureConn) RemoteAddr() net.Addr     { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1)} }

func (c *captureConn) SetDeadline(time.Time) error      { return nil }
func (c *captureConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureConn) SetWriteDeadline(time.Time) error { return nil }

// said returns everything the proxy wrote to the client, as one string. The
// frames are binary but every message body is plain text, so a substring search
// over the whole stream is the strictest possible "was this word ever said".
func (c *captureConn) said() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.buf.String()
}

// leakFixture is a store holding exactly one user, one entry and one grant
// binding them — the smallest fleet in which the resolver has something
// worth leaking.
type leakFixture struct {
	store *store.Store
	entry *store.Server
}

func newLeakFixture(t *testing.T) *leakFixture {
	t.Helper()

	ctx := context.Background()
	dataStore := newCopyTestStore(t)

	hash, err := crypto.HashPassword(leakPassword)
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, leakUsername, hash, []string{store.RoleConnector})
	require.NoError(t, err)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}

	entry, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         leakEntryName,
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: leakUpstreamDB,
		Username:     "upstream",
		Password:     "upstream",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, key)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, entry.UID, []string{})
	require.NoError(t, err)

	return &leakFixture{store: dataStore, entry: entry}
}

// handshake scripts a whole client side — StartupMessage then PasswordMessage —
// and runs authenticate() against it, returning the session, its error, and
// everything the client was told.
func (f *leakFixture) handshake(t *testing.T, username, database, password string) (*Session, string, error) {
	t.Helper()

	startup := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": username, "database": database},
	}

	script, err := startup.Encode(nil)
	require.NoError(t, err)

	script, err = (&pgproto3.PasswordMessage{Password: password}).Encode(script)
	require.NoError(t, err)

	conn := &captureConn{}

	session := &Session{
		ctx:          context.Background(),
		store:        f.store,
		logger:       slog.Default(),
		clientConn:   conn,
		clientReader: bufio.NewReader(bytes.NewReader(script)),
	}

	authErr := session.authenticate()

	return session, conn.said(), authErr
}

// TestAuthenticate_TargetResolutionOrdering is the regression test for the
// pre-auth information disclosure the `user#server` selector introduced.
//
// The resolver's messages are rich on purpose — "server X exposes database Y,
// not Z" is the whole point of the selector, because it is what an IDE's
// per-database reconnect needs to be told. But every one of them is a fact
// about the fleet, and the identity in a PostgreSQL StartupMessage is a claim
// until the password verifies it. Sending them before the password exchange
// turned `user=victim#some_entry` into an anonymous read of that entry's
// upstream database name.
//
// So: nothing about the fleet before the password, everything after it.
func TestAuthenticate_TargetResolutionOrdering(t *testing.T) {
	t.Parallel()

	f := newLeakFixture(t)

	t.Run("resolution details are never pre-auth", func(t *testing.T) {
		t.Parallel()
		assertNothingLeakedPreAuth(t, f)
	})

	t.Run("a grant holder still gets the actionable message", func(t *testing.T) {
		t.Parallel()
		assertActionableMessagePostAuth(t, f)
	})

	t.Run("the happy path still resolves, after the password", func(t *testing.T) {
		t.Parallel()
		assertHappyPathResolves(t, f)
	})
}

func assertNothingLeakedPreAuth(t *testing.T, f *leakFixture) {
	t.Helper()

	// Every case below asks for a database the entry does not expose — the
	// resolution that produces the richest message — and varies only the
	// password.
	for _, tc := range []struct {
		name     string
		username string
		database string
	}{
		{
			name:     "user#entry selector",
			username: leakUsername + shared.UsernameServerSeparator + leakEntryName,
			database: "postgres",
		},
		{
			// A name nothing in the fleet answers to: post-auth this is the
			// bare "database not found", which must not be pre-auth either —
			// it is still the answer to "does this name exist here?".
			name:     "bare name nothing exposes",
			username: leakUsername,
			database: "no_such_database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, said, err := f.handshake(t, tc.username, tc.database, leakBadPassword)

			require.ErrorIs(t, err, ErrInvalidPassword)
			assert.Contains(t, said, "authentication failed")

			// The two secrets: the entry's name and the upstream database it
			// exposes. Neither may appear, in any framing.
			assert.NotContains(t, said, leakEntryName)
			assert.NotContains(t, said, leakUpstreamDB)

			// Nor may the shapes the resolver's messages take — including the
			// `<user>#<server>` hint the ambiguity message offers, which is
			// itself a statement that several entries share a database name.
			for _, leak := range []string{
				"exposes", "not found", "ambiguous", "no valid grant",
				shared.UsernameServerSeparator,
			} {
				assert.NotContainsf(t, said, leak,
					"a caller who has not authenticated was told %q", leak)
			}
		})
	}
}

// assertActionableMessagePostAuth is the other half: deferring the message must
// not delete it. This is exactly what DataGrip does after reading pg_database —
// it reconnects with database=postgres — and the user has to be told what went
// wrong rather than handed a different database.
func assertActionableMessagePostAuth(t *testing.T, f *leakFixture) {
	t.Helper()

	_, said, err := f.handshake(t,
		leakUsername+shared.UsernameServerSeparator+leakEntryName, "postgres", leakPassword)

	require.ErrorIs(t, err, shared.ErrDatabaseNotExposed)

	// Both sides of the mismatch, named — that is what makes the message
	// actionable instead of merely correct.
	assert.Contains(t, said, leakEntryName)
	assert.Contains(t, said, leakUpstreamDB)
	assert.Contains(t, said, "postgres")
}

// assertHappyPathResolves pins the ordering from the other end: moving
// resolution behind the password exchange must still leave the session fully
// set up — database, grant and quotas all checked — before authenticate()
// returns.
func assertHappyPathResolves(t *testing.T, f *leakFixture) {
	t.Helper()

	session, said, err := f.handshake(t,
		leakUsername+shared.UsernameServerSeparator+leakEntryName, leakUpstreamDB, leakPassword)

	require.NoError(t, err)
	assert.True(t, session.authenticated)
	require.NotNil(t, session.database)
	assert.Equal(t, f.entry.UID, session.database.UID)
	require.NotNil(t, session.grant)

	// The '#' selects an entry; it never becomes part of the identity.
	require.NotNil(t, session.user)
	assert.Equal(t, leakUsername, session.user.Username)

	// The only thing written is the cleartext-password request: no error frame.
	assert.NotContains(t, strings.ToLower(said), "fatal")
}

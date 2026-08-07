package mssql

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// Fixture credentials. The client password is what a SQL client puts in its
// connection string; the upstream one is what dbbat stores and replays, and the
// test asserts the two never get confused.
const (
	fixtureUser         = "florent"
	fixturePassword     = "client-p@ssw0rd"
	fixtureAPIKey       = "dbb_mssqltestkey0123456789abcdef"
	fixtureUpstreamUser = "sa"
	fixtureUpstreamPass = "upstream-Secret-1"
	fixtureDBEntry      = "reporting"
	fixtureRealDatabase = "AdventureWorks"
)

// newTestStore spins up a throwaway PostgreSQL store. Only dbbat's own storage
// is needed — the SQL Server side is the in-process fake — so this stays out of
// the integration-tagged suite and runs under `make test` on any architecture.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	dataStore, err := store.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { dataStore.Close() })

	require.NoError(t, dataStore.Migrate(ctx))

	return dataStore
}

// grantFullAccess gives a user a live, unrestricted grant on a database.
// Every grant is an instance of a definition, so the definition comes first;
// the slug is unique per call because a definition slug is unique among live
// rows.
func grantFullAccess(t *testing.T, dataStore *store.Store, userUID, databaseUID uuid.UUID) {
	t.Helper()

	ctx := context.Background()

	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            "mssql-test-" + databaseUID.String(),
		Slug:            "mssql-test-" + databaseUID.String(),
		DurationSeconds: int64((24 * time.Hour).Seconds()),
		Controls:        []string{},
		CreatedBy:       userUID,
	})
	require.NoError(t, err)

	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:            userUID,
		DatabaseID:        databaseUID,
		GrantDefinitionID: def.UID,
		Definition:        def,
		GrantedBy:         userUID,
		StartsAt:          time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)
}

// authFixture is a proxy wired to a real store and a fake SQL Server.
type authFixture struct {
	store     *store.Store
	fake      *fakeUpstream
	authCache *cache.AuthCache
	proxyAddr string
	user      *store.User
	database  *store.Server
}

// newAuthFixture builds the whole chain: store, user, API key, a server row
// pointing at the fake upstream, a live grant, and the proxy in front of it.
func newAuthFixture(t *testing.T, upstreamEncryption byte, sslMode string) *authFixture {
	t.Helper()

	return newAuthFixtureWithDump(t, upstreamEncryption, sslMode, "")
}

// newAuthFixtureWithDump is newAuthFixture with session captures enabled.
func newAuthFixtureWithDump(t *testing.T, upstreamEncryption byte, sslMode, dumpDir string) *authFixture {
	t.Helper()

	ctx := context.Background()

	dataStore := newTestStore(t)
	fake := newFakeUpstream(t, upstreamEncryption)

	hash, err := crypto.HashPassword(fixturePassword)
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, fixtureUser, hash, []string{store.RoleConnector})
	require.NoError(t, err)

	encryptionKey := make([]byte, 32)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	_, err = dataStore.CreateAPIKeyWithValue(ctx, user.UID, "test-key", fixtureAPIKey, nil, encryptionKey)
	require.NoError(t, err)

	host, port := fake.addr()

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         fixtureDBEntry,
		Host:         host,
		Port:         port,
		DatabaseName: fixtureRealDatabase,
		Username:     fixtureUpstreamUser,
		Password:     fixtureUpstreamPass,
		Protocol:     store.ProtocolMSSQL,
		SSLMode:      sslMode,
	}, encryptionKey)
	require.NoError(t, err)

	grantFullAccess(t, dataStore, user.UID, database.UID)

	dumpConfig := config.DumpConfig{}
	if dumpDir != "" {
		dumpConfig = config.DumpConfig{
			Dir:       dumpDir,
			MaxSize:   config.DefaultDumpMaxSize,
			Retention: config.DefaultDumpRetention,
		}
	}

	// A live auth cache, because that is what main.go passes: the Argon2id
	// verification path a real deployment takes is the cached one.
	authCache := cache.NewAuthCache(cache.AuthCacheConfig{Enabled: true, TTLSeconds: 60, MaxSize: 16})

	proxy, err := NewServer(dataStore, encryptionKey, dumpConfig, authCache,
		config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}}, testLogger())
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	proxy.setListener(listener)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go proxy.handleConnection(conn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })

	return &authFixture{
		store:     dataStore,
		fake:      fake,
		authCache: authCache,
		proxyAddr: listener.Addr().String(),
		user:      user,
		database:  database,
	}
}

// login drives a full handshake and LOGIN7 through the proxy and returns the
// client, its login response, and what a driver would make of it.
func (f *authFixture) login(t *testing.T, username, password, database string) (*testClient, loginOutcome) {
	t.Helper()

	client := dialTestClient(t, f.proxyAddr)
	client.prelogin(t, encryptNotSup, false)

	login := sampleLogin7()
	login.UserName = username
	login.Password = password
	login.Database = database

	client.sendLogin7(t, login)

	return client, scanLoginResponse(client.readReply(t))
}

// requireLoginFailure asserts the login was refused, and returns the decoded
// error so the caller can pin the number and the wording.
func requireLoginFailure(t *testing.T, outcome loginOutcome) tdsMessage {
	t.Helper()

	require.NotNil(t, outcome.Failure, "the login should have been refused")
	require.False(t, outcome.Acked)

	return *outcome.Failure
}

func TestSessionAuthenticatesAValidUser(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)

	require.Nil(t, outcome.Failure)
	assert.True(t, outcome.Acked, "the client is served the upstream's own LOGINACK")

	// The upstream saw the *stored* credentials, never the client's.
	upstreamLogin := fixture.fake.lastLogin()
	require.NotNil(t, upstreamLogin)
	assert.Equal(t, fixtureUpstreamUser, upstreamLogin.UserName)
	assert.Equal(t, fixtureUpstreamPass, upstreamLogin.Password)
	assert.Equal(t, fixtureRealDatabase, upstreamLogin.Database,
		"the LOGIN7 database field names the dbbat entry; the real database comes from the row")
	assert.Contains(t, upstreamLogin.AppName, "dbbat/")
	assert.Contains(t, upstreamLogin.AppName, "@"+fixtureUser)

	// And the audit row landed, with the upstream leg's encryption recorded.
	connections, err := fixture.store.ListConnections(context.Background(), store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, fixture.user.UID, connections[0].UserID)
	assert.Equal(t, fixture.database.UID, connections[0].DatabaseID)
	assert.False(t, connections[0].UpstreamTLS)
}

func TestSessionRecordsAnEncryptedUpstreamLeg(t *testing.T) {
	t.Parallel()

	// The client leg is plaintext (the listener has TLS disabled) and the
	// upstream leg is encrypted: the two negotiate independently, and
	// upstream_tls reports the upstream one.
	fixture := newAuthFixture(t, encryptOn, "require")

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, outcome.Acked)

	connections, err := fixture.store.ListConnections(context.Background(), store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.True(t, connections[0].UpstreamTLS)
}

func TestSessionAcceptsAnAPIKeyAsThePassword(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	_, outcome := fixture.login(t, fixtureUser, fixtureAPIKey, fixtureDBEntry)

	require.Nil(t, outcome.Failure)
	assert.True(t, outcome.Acked)
}

func TestSessionRejectsAnAPIKeyBelongingToSomeoneElse(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	other, err := fixture.store.CreateUser(context.Background(), "someone-else", "x", []string{store.RoleConnector})
	require.NoError(t, err)
	require.NotNil(t, other)

	_, outcome := fixture.login(t, "someone-else", fixtureAPIKey, fixtureDBEntry)

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberLoginFailed, failure.Number)
	assert.Zero(t, fixture.fake.loginCount(), "no upstream session may be opened for a refused login")
}

// TestSessionAuthFailuresAreIndistinguishable is the property the spec calls
// for: a wrong password and a user that does not exist must look the same to
// the client, down to the error number and the wording.
func TestSessionAuthFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	cases := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", fixtureUser, "not-the-password"},
		{"unknown user", "ghost", fixturePassword},
		{"unknown user with a wrong password", "ghost", "not-the-password"},
		{"an API key that does not exist", fixtureUser, "dbb_nosuchkey0123456789abcdefghij"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, outcome := fixture.login(t, tc.username, tc.password, fixtureDBEntry)

			failure := requireLoginFailure(t, outcome)
			assert.Equal(t, errNumberLoginFailed, failure.Number)
			assert.Equal(t, errSeverityUserError, failure.Class)
			assert.Equal(t, fmt.Sprintf(authFailedMessage, tc.username), failure.Message,
				"the message must depend on nothing but the username the client supplied")
			assert.Zero(t, fixture.fake.loginCount(), "a refused login never reaches the upstream")
		})
	}
}

func TestSessionRejectsAnUnsupportedAuthType(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	client := dialTestClient(t, fixture.proxyAddr)
	client.prelogin(t, encryptNotSup, false)

	login := sampleLogin7()
	login.UserName = fixtureUser
	login.Password = fixturePassword
	login.Database = fixtureDBEntry
	// Windows integrated authentication: NTLM/Kerberos, which dbbat v1 refuses.
	login.OptionFlags2 |= optionFlags2IntSecurity

	client.sendLogin7(t, login)

	failure := requireLoginFailure(t, scanLoginResponse(client.readReply(t)))
	assert.Equal(t, errNumberLoginFailed, failure.Number)
	assert.Contains(t, failure.Message, "integrated")
	assert.Contains(t, failure.Message, "SQL login")
	assert.Zero(t, fixture.fake.loginCount())
}

func TestSessionRejectsAnUnknownDatabase(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, "no-such-entry")

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberCannotOpenDBName, failure.Number)
	assert.Contains(t, failure.Message, "no SQL Server database is registered")
}

func TestSessionRejectsAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, "")

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberCannotOpenDBName, failure.Number)
	assert.Contains(t, failure.Message, "named no database")
}

// TestSessionRejectsADatabaseOnAnotherProtocol closes the hole where a
// PostgreSQL entry could be reached through the SQL Server listener just
// because the name matched.
func TestSessionRejectsADatabaseOnAnotherProtocol(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	encryptionKey := make([]byte, 32)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	other, err := fixture.store.CreateServer(context.Background(), &store.Server{
		Name:         "a-postgres-entry",
		Host:         "127.0.0.1",
		Port:         5432,
		DatabaseName: "postgres",
		Username:     "postgres",
		Password:     "postgres",
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, encryptionKey)
	require.NoError(t, err)

	grantFullAccess(t, fixture.store, fixture.user.UID, other.UID)

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, "a-postgres-entry")

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberCannotOpenDBName, failure.Number)
}

func TestSessionRejectsAUserWithoutAGrant(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	hash, err := crypto.HashPassword("other-password")
	require.NoError(t, err)

	_, err = fixture.store.CreateUser(context.Background(), "ungranted", hash, []string{store.RoleConnector})
	require.NoError(t, err)

	_, outcome := fixture.login(t, "ungranted", "other-password", fixtureDBEntry)

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberCannotOpenDBName, failure.Number)
	assert.Contains(t, failure.Message, "no active grant")
	assert.Zero(t, fixture.fake.loginCount())
}

// TestSessionReportsAnUpstreamFailureWithoutLeakingIt proves the client is told
// the class of failure and nothing about the target.
func TestSessionReportsAnUpstreamFailureWithoutLeakingIt(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")
	fixture.fake.rejectLogin = true

	_, outcome := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)

	failure := requireLoginFailure(t, outcome)
	assert.Equal(t, errNumberProxyMessage, failure.Number)
	assert.Contains(t, failure.Message, "could not open a session on the upstream")
	assert.NotContains(t, failure.Message, fixtureUpstreamUser)
	assert.NotContains(t, failure.Message, fixture.database.Host)
}

// TestSessionVerifiesThroughTheAuthCache pins that the proxy goes through
// internal/cache rather than deriving Argon2id on every connect. A reconnect
// loop is the normal shape of SQL Server traffic (connection pools), and
// Argon2id is expensive by design.
func TestSessionVerifiesThroughTheAuthCache(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t, encryptNotSup, "disable")

	_, first := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, first.Acked)

	hits, misses, size := fixture.authCache.Stats()
	assert.Zero(t, hits)
	assert.Equal(t, int64(1), misses)
	assert.Equal(t, 1, size)

	_, second := fixture.login(t, fixtureUser, fixturePassword, fixtureDBEntry)
	require.True(t, second.Acked)

	hits, _, _ = fixture.authCache.Stats()
	assert.Equal(t, int64(1), hits, "the second connect must be served from the cache")

	// A wrong password is a different cache key, and must still be refused.
	_, wrong := fixture.login(t, fixtureUser, "not-the-password", fixtureDBEntry)
	assert.NotNil(t, wrong.Failure)
}

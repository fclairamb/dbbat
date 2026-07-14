//go:build integration

package mongodb

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

const defaultMongoImage = "mongo:7"

func mongoImage() string {
	if img := os.Getenv("MONGO_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultMongoImage
}

const (
	fixtureUser = "dbbattest"
	fixturePass = "dbbattest"
	rootUser    = "root"
	rootPass    = "rootpw"
	testDBName  = "testdb"
)

// runMongoContainer starts a MongoDB container with root credentials and
// returns its bound host/port.
func runMongoContainer(ctx context.Context, t *testing.T, image string) (testcontainers.Container, string, int) {
	t.Helper()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"27017/tcp"},
			Env: map[string]string{
				"MONGO_INITDB_ROOT_USERNAME": rootUser,
				"MONGO_INITDB_ROOT_PASSWORD": rootPass,
			},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("27017/tcp"),
				wait.ForLog("Waiting for connections"),
			).WithStartupTimeoutDefault(120 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start mongo container (%s)", image)

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "27017")
	require.NoError(t, err)

	t.Logf("Mongo container ready: image=%s host=%s port=%s", image, host, port.Port())

	return c, host, int(port.Num())
}

type fixture struct {
	t         *testing.T
	store     *store.Store
	proxy     *Server
	proxyAddr string
	user      *store.User
	dbUID     string
	encKey    []byte
}

func setupFixture(ctx context.Context, t *testing.T) *fixture {
	t.Helper()

	return setupFixtureWithDumpDir(ctx, t, "")
}

func setupFixtureWithDumpDir(ctx context.Context, t *testing.T, dumpDir string) *fixture {
	t.Helper()

	upstream, upstreamHost, upstreamPort := runMongoContainer(ctx, t, mongoImage())
	t.Cleanup(func() { _ = upstream.Terminate(context.Background()) })

	pgContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:15-alpine",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_DB":       "dbbat_test",
				"POSTGRES_USER":     "test",
				"POSTGRES_PASSWORD": "test",
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	pgHost, _ := pgContainer.Host(ctx)
	pgPort, _ := pgContainer.MappedPort(ctx, "5432")
	pgDSN := fmt.Sprintf("postgres://test:test@%s:%s/dbbat_test?sslmode=disable", pgHost, pgPort.Port())

	dataStore, err := store.New(ctx, pgDSN)
	require.NoError(t, err)
	t.Cleanup(func() { dataStore.Close() })
	require.NoError(t, dataStore.Migrate(ctx))

	hash, err := crypto.HashPassword(fixturePass)
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, fixtureUser, hash, []string{"connector"})
	require.NoError(t, err)

	encKey := make([]byte, 32)
	for i := range encKey {
		encKey[i] = byte(i + 1)
	}

	db, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         testDBName,
		Host:         upstreamHost,
		Port:         upstreamPort,
		DatabaseName: testDBName,
		Username:     rootUser,
		Password:     rootPass,
		Protocol:     store.ProtocolMongoDB,
		SSLMode:      "disable",
	}, encKey)
	require.NoError(t, err)

	_, err = dataStore.CreateGrant(ctx, &store.Grant{
		UserID:     user.UID,
		DatabaseID: db.UID,
		GrantedBy:  user.UID,
		Controls:   []string{},
		StartsAt:   time.Now().Add(-time.Hour),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)

	queryStorage := config.QueryStorageConfig{StoreResults: true, MaxResultRows: 1000, MaxResultBytes: 1 * 1024 * 1024}

	dumpCfg := config.DumpConfig{}
	if dumpDir != "" {
		dumpCfg = config.DumpConfig{Dir: dumpDir, MaxSize: config.DefaultDumpMaxSize, Retention: config.DefaultDumpRetention}
	}

	proxy, err := NewServer(dataStore, encKey, queryStorage, dumpCfg, nil, config.MongoConfig{}, slog.Default())
	require.NoError(t, err)

	go func() { _ = proxy.Start("127.0.0.1:0") }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownCtx)
	})

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 2*time.Second, 50*time.Millisecond, "proxy never started listening")

	return &fixture{
		t:         t,
		store:     dataStore,
		proxy:     proxy,
		proxyAddr: proxy.Addr().String(),
		user:      user,
		dbUID:     db.UID.String(),
		encKey:    encKey,
	}
}

// dialThrough connects the official Go driver through the proxy using SASL
// PLAIN over TLS with skip-verify.
func (f *fixture) dialThrough(username, password string) *mongo.Client {
	f.t.Helper()

	opts := options.Client().
		SetHosts([]string{f.proxyAddr}).
		SetAuth(options.Credential{
			AuthMechanism: "PLAIN",
			AuthSource:    testDBName,
			Username:      username,
			Password:      password,
		}).
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}). //nolint:gosec // self-signed proxy cert
		SetDirect(true).
		SetServerSelectionTimeout(8 * time.Second).
		SetTimeout(10 * time.Second)

	client, err := mongo.Connect(opts)
	require.NoError(f.t, err)

	return client
}

func (f *fixture) replaceGrant(ctx context.Context, controls []string) {
	f.t.Helper()

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(f.t, err)

	for _, g := range grants {
		require.NoError(f.t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
	}

	dbUID, err := uuid.Parse(f.dbUID)
	require.NoError(f.t, err)

	_, err = f.store.CreateGrant(ctx, &store.Grant{
		UserID:     f.user.UID,
		DatabaseID: dbUID,
		GrantedBy:  f.user.UID,
		Controls:   controls,
		StartsAt:   time.Now().Add(-time.Hour),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	require.NoError(f.t, err)
}

// ---------- Tests ----------

// TestIntegration_ProxyAuth_Password connects and pings through the proxy with
// a dbbat password. The Go driver's SDAM also opens monitoring connections that
// never authenticate — exercising the pre-auth path automatically.
func TestIntegration_ProxyAuth_Password(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	require.NoError(t, client.Ping(ctx, nil))
}

// TestIntegration_ProxyAuth_APIKey authenticates with a dbb_ API key as the
// password.
func TestIntegration_ProxyAuth_APIKey(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	_, plainKey, err := f.store.CreateAPIKey(ctx, f.user.UID, "test-key", nil, f.encKey)
	require.NoError(t, err)

	client := f.dialThrough(fixtureUser, plainKey)
	defer func() { _ = client.Disconnect(ctx) }()

	require.NoError(t, client.Ping(ctx, nil))
}

// TestIntegration_WrongPassword surfaces AuthenticationFailed (code 18).
func TestIntegration_WrongPassword(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	client := f.dialThrough(fixtureUser, "wrongpassword")
	defer func() { _ = client.Disconnect(ctx) }()

	err := client.Ping(ctx, nil)
	require.Error(t, err, "wrong password must fail")
	assert.Contains(t, strings.ToLower(err.Error()), "auth")
}

// TestIntegration_FindInsert verifies a write + read round-trip through the
// proxy and that the query log + result rows are captured.
func TestIntegration_FindInsert(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	coll := client.Database(testDBName).Collection("widgets")

	_, err := coll.InsertOne(ctx, bson.D{{Key: "name", Value: "gadget"}, {Key: "qty", Value: 7}})
	require.NoError(t, err)

	var got bson.M
	require.NoError(t, coll.FindOne(ctx, bson.D{{Key: "name", Value: "gadget"}}).Decode(&got))
	assert.Equal(t, "gadget", got["name"])

	time.Sleep(400 * time.Millisecond)

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 100})
	require.NoError(t, err)

	var sawInsert, sawFind bool
	var findUID string
	for i := range queries {
		if strings.HasPrefix(queries[i].SQLText, "insert ") {
			sawInsert = true
		}
		if strings.HasPrefix(queries[i].SQLText, "find ") {
			sawFind = true
			findUID = queries[i].UID.String()
		}
	}

	assert.True(t, sawInsert, "insert should be logged")
	assert.True(t, sawFind, "find should be logged")

	if findUID != "" {
		uid, _ := uuid.Parse(findUID)
		rows, err := f.store.GetQueryRows(ctx, uid, "", 10)
		require.NoError(t, err)
		assert.NotEmpty(t, rows.Rows, "find should capture cursor rows")
	}
}

// TestIntegration_ReadOnlyGrant_BlocksWrite verifies a read_only grant rejects
// an insert at the proxy.
func TestIntegration_ReadOnlyGrant_BlocksWrite(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrant(ctx, []string{"read_only"})

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	// A read still works.
	require.NoError(t, client.Ping(ctx, nil))

	_, err := client.Database(testDBName).Collection("widgets").
		InsertOne(ctx, bson.D{{Key: "x", Value: 1}})
	require.Error(t, err, "insert must be refused under read-only grant")
}

// TestIntegration_BlockDDL_BlocksCreateIndex verifies block_ddl rejects
// createIndexes.
func TestIntegration_BlockDDL_BlocksCreateIndex(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)
	f.replaceGrant(ctx, []string{"block_ddl"})

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	_, err := client.Database(testDBName).Collection("widgets").Indexes().
		CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "name", Value: 1}}})
	require.Error(t, err, "createIndexes must be refused under block_ddl grant")
}

// TestIntegration_DBViolation_Denied verifies a command against a disallowed
// database (local) is denied.
func TestIntegration_DBViolation_Denied(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	err := client.Database("local").Collection("startup_log").
		FindOne(ctx, bson.D{}).Err()
	require.Error(t, err, "access to the local database must be denied")
}

// TestIntegration_SessionDump verifies a per-connection dump file is written.
func TestIntegration_SessionDump(t *testing.T) {
	ctx := context.Background()
	dumpDir := t.TempDir()
	f := setupFixtureWithDumpDir(ctx, t, dumpDir)

	client := f.dialThrough(fixtureUser, fixturePass)
	require.NoError(t, client.Ping(ctx, nil))
	_, err := client.Database(testDBName).Collection("widgets").InsertOne(ctx, bson.D{{Key: "a", Value: 1}})
	require.NoError(t, err)
	require.NoError(t, client.Disconnect(ctx))

	time.Sleep(400 * time.Millisecond)

	entries, err := os.ReadDir(dumpDir)
	require.NoError(t, err)

	var dumpFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".dbbat-dump") {
			dumpFile = filepath.Join(dumpDir, e.Name())

			break
		}
	}

	require.NotEmpty(t, dumpFile, "expected a .dbbat-dump file in %s", dumpDir)

	stat, err := os.Stat(dumpFile)
	require.NoError(t, err)
	assert.Greater(t, stat.Size(), int64(0), "dump file should not be empty")
}

// TestIntegration_RevocationKillsSession verifies revoking the grant mid-session
// tears the live connection down (the watchdog force-closes both conns).
func TestIntegration_RevocationKillsSession(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	client := f.dialThrough(fixtureUser, fixturePass)
	defer func() { _ = client.Disconnect(ctx) }()

	coll := client.Database(testDBName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.D{{Key: "before", Value: 1}})
	require.NoError(t, err)

	// Revoke like the API does: mark revoked + signal the live-session registry.
	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)
	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
		f.store.Revocations().Revoke(g.UID)
	}

	// The watchdog closes the conns within a poll interval; the next op fails.
	require.Eventually(t, func() bool {
		_, e := coll.InsertOne(ctx, bson.D{{Key: "after", Value: 2}})

		return e != nil
	}, 5*time.Second, 200*time.Millisecond, "revoked session should be torn down")
}

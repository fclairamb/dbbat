//go:build integration

package mysql

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	gosqlmysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// Default test images. Override with env vars to test alternative versions
// (e.g. MARIADB_TEST_IMAGE=mariadb:11 for the latest MariaDB).
const (
	defaultMySQLImage   = "mysql:8.4"
	defaultMariaDBImage = "mariadb:10.11"
)

func mysqlImage() string {
	if img := os.Getenv("MYSQL_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultMySQLImage
}

func mariadbImage() string {
	if img := os.Getenv("MARIADB_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultMariaDBImage
}

// runMySQLContainer starts a MySQL or MariaDB container with predictable
// test credentials and returns its bound host/port.
//
// The testcontainers MySQL module hard-codes a "MySQL Community Server"
// log wait that doesn't match MariaDB, so we override the wait strategy
// when the image looks like MariaDB.
func runMySQLContainer(ctx context.Context, t *testing.T, image string) (testcontainers.Container, string, int) {
	t.Helper()

	opts := []testcontainers.ContainerCustomizer{
		tcmysql.WithDatabase("testdb"),
		tcmysql.WithUsername("root"),
		tcmysql.WithPassword("rootpw"),
	}

	if strings.Contains(strings.ToLower(image), "mariadb") {
		// MariaDB logs "ready for connections" before the socket is
		// actually accepting traffic, so layer a TCP-listener check on top.
		opts = append(opts, testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("ready for connections"),
				wait.ForListeningPort("3306/tcp"),
			).WithStartupTimeoutDefault(120*time.Second),
		))
	}

	c, err := tcmysql.Run(ctx, image, opts...)
	require.NoError(t, err, "start mysql container (%s)", image)

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "3306")
	require.NoError(t, err)

	t.Logf("MySQL container ready: image=%s host=%s port=%s", image, host, port.Port())

	return c, host, int(port.Num())
}

// fixture wires up: PG storage container, dbbat store, a user/database/grant,
// and a started MySQL proxy. Returns the proxy's listen address and a
// teardown that stops everything.
type fixture struct {
	t            *testing.T
	store        *store.Store
	proxy        *Server
	proxyAddr    string
	upstreamHost string
	upstreamPort int
	username     string
	password     string
	dbName       string
}

const fixtureUser = "dbbattest"
const fixturePass = "dbbattest"

func setupFixture(ctx context.Context, t *testing.T, mysqlImage, dbProtocol string) *fixture {
	t.Helper()

	upstreamContainer, upstreamHost, upstreamPort := runMySQLContainer(ctx, t, mysqlImage)
	t.Cleanup(func() { _ = upstreamContainer.Terminate(context.Background()) })

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
				WithOccurrence(2).
				WithStartupTimeout(60 * time.Second),
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

	encryptionKey := make([]byte, 32)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	db, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "testdb",
		Host:         upstreamHost,
		Port:         upstreamPort,
		DatabaseName: "testdb",
		Username:     "root",
		Password:     "rootpw",
		Protocol:     dbProtocol,
		SSLMode:      "disable",
	}, encryptionKey)
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

	queryStorage := config.QueryStorageConfig{
		StoreResults:   true,
		MaxResultRows:  1000,
		MaxResultBytes: 1 * 1024 * 1024,
	}

	proxy, err := NewServer(dataStore, encryptionKey, queryStorage, config.DumpConfig{},
		nil, config.MySQLConfig{}, slog.Default())
	require.NoError(t, err)

	go func() { _ = proxy.Start("127.0.0.1:0") }()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownCtx)
	})

	require.Eventually(t, func() bool { return proxy.Addr() != nil },
		2*time.Second, 50*time.Millisecond, "proxy never started listening")

	return &fixture{
		t:            t,
		store:        dataStore,
		proxy:        proxy,
		proxyAddr:    proxy.Addr().String(),
		upstreamHost: upstreamHost,
		upstreamPort: upstreamPort,
		username:     fixtureUser,
		password:     fixturePass,
		dbName:       "testdb",
	}
}

// dialTLS returns a *sql.DB connected through the proxy with TLS skip-verify
// enabled (the proxy auto-generates a self-signed cert).
func (f *fixture) dialTLS() *sql.DB {
	f.t.Helper()

	return f.dialWithTLSConfig("dbbat-skip-verify")
}

// dialPlain connects without TLS.
func (f *fixture) dialPlain() *sql.DB {
	f.t.Helper()

	cfg := f.driverConfig()
	cfg.TLSConfig = "false"
	cfg.AllowCleartextPasswords = true
	cfg.AllowNativePasswords = false

	return f.openWithConfig(cfg)
}

func (f *fixture) dialWithTLSConfig(name string) *sql.DB {
	cfg := f.driverConfig()
	cfg.TLSConfig = name
	cfg.AllowCleartextPasswords = true

	return f.openWithConfig(cfg)
}

func (f *fixture) driverConfig() *gosqlmysql.Config {
	host, port, _ := net.SplitHostPort(f.proxyAddr)

	cfg := gosqlmysql.NewConfig()
	cfg.User = f.username
	cfg.Passwd = f.password
	cfg.Net = "tcp"
	cfg.Addr = host + ":" + port
	cfg.DBName = f.dbName
	cfg.AllowNativePasswords = false // force caching_sha2/clear path

	return cfg
}

func (f *fixture) openWithConfig(cfg *gosqlmysql.Config) *sql.DB {
	connector, err := gosqlmysql.NewConnector(cfg)
	require.NoError(f.t, err)

	return sql.OpenDB(connector)
}

// init registers a TLS skip-verify config under the name "dbbat-skip-verify"
// for use with go-sql-driver/mysql's tls=<name> config parameter.
func init() {
	_ = gosqlmysql.RegisterTLSConfig("dbbat-skip-verify", &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // testing self-signed proxy cert
		MinVersion:         tls.VersionTLS12,
	})
}

// ---------- Tests ----------

// TestIntegration_MySQLContainer is a sanity check: the MySQL testcontainer
// works and we can reach it directly without the proxy. If this fails, all
// other tests will too — fail fast.
func TestIntegration_MySQLContainer(t *testing.T) {
	ctx := context.Background()

	c, host, port := runMySQLContainer(ctx, t, mysqlImage())
	defer func() { _ = c.Terminate(ctx) }()

	dsn := fmt.Sprintf("root:rootpw@tcp(%s:%d)/testdb", host, port)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))

	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	t.Logf("MySQL version: %s", version)
}

// TestIntegration_ProxyHandshake_TLS exercises the caching_sha2_password
// path through TLS termination. The proxy auto-generates a self-signed cert,
// the client uses TLS with skip-verify.
func TestIntegration_ProxyHandshake_TLS(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	db := f.dialTLS()
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))

	var one int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	assert.Equal(t, 1, one)
}

// TestIntegration_QueryAndCapture verifies result rows are captured into
// the store after a SELECT through the proxy.
func TestIntegration_QueryAndCapture(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	db := f.dialTLS()
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT 1, 'hello', NULL")
	require.NoError(t, err)

	var (
		i int
		s string
		n sql.NullString
	)
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&i, &s, &n))
	require.NoError(t, rows.Close())

	assert.Equal(t, 1, i)
	assert.Equal(t, "hello", s)
	assert.False(t, n.Valid)

	// Allow async write to land
	time.Sleep(200 * time.Millisecond)

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 10})
	require.NoError(t, err)

	var found bool

	for _, q := range queries {
		if strings.Contains(q.SQLText, "SELECT 1") {
			found = true

			break
		}
	}

	assert.True(t, found, "expected SELECT 1 to be logged in queries table")
}

// TestIntegration_PreparedStatement_BinaryRowCapture exercises the binary
// protocol path (COM_STMT_PREPARE + COM_STMT_EXECUTE) through the proxy and
// verifies the captured rows match what the client received.
//
// go-sql-driver/mysql uses binary protocol for any query with bind args.
// Without InterpolateParams, this test forces COM_STMT_EXECUTE rather than
// inline-substituted COM_QUERY.
func TestIntegration_PreparedStatement_BinaryRowCapture(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	db := f.dialTLS()
	defer db.Close()

	stmt, err := db.PrepareContext(ctx, "SELECT ? + ?, ?")
	require.NoError(t, err)
	defer stmt.Close()

	var sum int

	var label string

	require.NoError(t, stmt.QueryRowContext(ctx, 7, 35, "binary").Scan(&sum, &label))
	assert.Equal(t, 42, sum)
	assert.Equal(t, "binary", label)

	// Allow async write to land.
	time.Sleep(300 * time.Millisecond)

	queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 50})
	require.NoError(t, err)

	var executeQuery *store.Query

	for i := range queries {
		if strings.Contains(queries[i].SQLText, "SELECT ? + ?, ?") &&
			!strings.HasPrefix(queries[i].SQLText, "PREPARE:") {
			executeQuery = &queries[i]

			break
		}
	}

	require.NotNil(t, executeQuery, "expected COM_STMT_EXECUTE entry in queries log")

	result, err := f.store.GetQueryRows(ctx, executeQuery.UID, "", 10)
	require.NoError(t, err)
	require.NotEmpty(t, result.Rows, "binary-protocol rows must be captured")

	t.Logf("captured row: %s", string(result.Rows[0].RowData))
	assert.Contains(t, string(result.Rows[0].RowData), "42",
		"captured row should include the computed sum")
	assert.Contains(t, string(result.Rows[0].RowData), "binary",
		"captured row should include the string literal")
}

// TestIntegration_ReadOnlyGrant_BlocksWrite verifies that a grant with
// read_only control rejects an INSERT statement at the proxy layer.
func TestIntegration_ReadOnlyGrant_BlocksWrite(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)

	// Replace the existing grant with a read-only one.
	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)
	require.NotEmpty(t, grants)

	user, err := f.store.GetUserByUsername(ctx, fixtureUser)
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, user.UID))
	}

	databases, err := f.store.ListDatabases(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, databases)

	_, err = f.store.CreateGrant(ctx, &store.Grant{
		UserID:     user.UID,
		DatabaseID: databases[0].UID,
		GrantedBy:  user.UID,
		Controls:   []string{"read_only"},
		StartsAt:   time.Now().Add(-time.Hour),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)

	db := f.dialTLS()
	defer db.Close()

	_, err = db.ExecContext(ctx, "CREATE TABLE if not exists t (x int)")
	require.Error(t, err, "DDL must be refused under read-only grant")
}

// TestIntegration_LoadDataInfile_Blocked verifies LOAD DATA INFILE is
// rejected — both the regex pattern and (eventually) the protocol-level
// LOCAL_INFILE response.
func TestIntegration_LoadDataInfile_Blocked(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)
	db := f.dialTLS()
	defer db.Close()

	_, err := db.ExecContext(ctx, "LOAD DATA INFILE '/etc/passwd' INTO TABLE t")
	require.Error(t, err, "LOAD DATA INFILE must be refused")
}

// TestIntegration_MariaDB exercises the same proxy path with a MariaDB
// upstream and the protocol="mariadb" routing.
func TestIntegration_MariaDB(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mariadbImage(), store.ProtocolMariaDB)
	db := f.dialTLS()
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))

	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))

	t.Logf("MariaDB version through proxy: %s", version)
	assert.Contains(t, strings.ToLower(version), "mariadb",
		"expected mariadb in version banner")
}

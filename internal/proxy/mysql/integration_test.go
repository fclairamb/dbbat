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
	"path/filepath"
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
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
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

// mysqlWaitStrategy decides a MySQL/MariaDB container is usable only once an
// *authenticated* round trip succeeds.
//
// Both server families log their "ready" line and bind the port before the
// entrypoint has finished applying MYSQL_ROOT_PASSWORD, so the log/port pair
// says nothing about whether the credentials the tests use already work — the
// same window that makes the MongoDB fixture flake. It also leaves the default
// path on the module's 60s startup budget, which a cold mysql:8.4 initdb can
// blow through on a loaded daemon (that is how TestIntegration_MySQLContainer,
// which runs no dbbat code at all, manages to fail).
//
// `mariadb` is tried before `mysql` because MariaDB 11 deprecates the mysql-*
// symlinks; mysql:8.4 has no `mariadb` binary and falls through to the second
// command. Errors are muted so only the exit code decides.
func mysqlWaitStrategy(logLine string) wait.Strategy {
	const query = `-uroot -prootpw -e "SELECT 1"`

	// Per-strategy budget, then an overall cap: three independent 120s budgets
	// would let a genuinely broken container hang the suite for six minutes.
	const (
		perStrategyTimeout = 120 * time.Second
		overallDeadline    = 180 * time.Second
	)

	return wait.ForAll(
		wait.ForLog(logLine),
		wait.ForListeningPort("3306/tcp"),
		wait.ForExec([]string{
			"sh", "-c",
			"mariadb " + query + " 2>/dev/null || mysql " + query + " 2>/dev/null",
		}).
			// Polls are sequential, so the interval is pure dead time between
			// attempts rather than a guard against overlapping execs.
			WithPollInterval(250*time.Millisecond).
			WithStartupTimeout(perStrategyTimeout),
	).
		WithStartupTimeoutDefault(perStrategyTimeout).
		WithDeadline(overallDeadline)
}

// runMySQLContainer starts a MySQL or MariaDB container with predictable
// test credentials and returns its bound host/port.
//
// The testcontainers MySQL module hard-codes a "MySQL Community Server"
// log wait that doesn't match MariaDB, so the ready log line is picked per
// family before being handed to the shared readiness strategy.
func runMySQLContainer(ctx context.Context, t *testing.T, image string) (testcontainers.Container, string, int) {
	t.Helper()

	// MariaDB never prints the module's "MySQL Community Server" line.
	logLine := "port: 3306  MySQL Community Server"
	if strings.Contains(strings.ToLower(image), "mariadb") {
		logLine = "ready for connections"
	}

	opts := []testcontainers.ContainerCustomizer{
		tcmysql.WithDatabase("testdb"),
		tcmysql.WithUsername("root"),
		tcmysql.WithPassword("rootpw"),
		testcontainers.WithWaitStrategy(mysqlWaitStrategy(logLine)),
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

	return setupFixtureWithDumpDir(ctx, t, mysqlImage, dbProtocol, "")
}

func setupFixtureWithDumpDir(ctx context.Context, t *testing.T, mysqlImage, dbProtocol, dumpDir string) *fixture {
	t.Helper()

	return setupFixtureWith(ctx, t, mysqlImage, dbProtocol, dumpDir, "disable")
}

// setupFixtureWithSSLMode builds the fixture with a specific upstream ssl_mode,
// which is what the upstream-TLS tests vary.
func setupFixtureWithSSLMode(ctx context.Context, t *testing.T, mysqlImage, dbProtocol, sslMode string) *fixture {
	t.Helper()

	return setupFixtureWith(ctx, t, mysqlImage, dbProtocol, "", sslMode)
}

func setupFixtureWith(ctx context.Context, t *testing.T, mysqlImage, dbProtocol, dumpDir, sslMode string) *fixture {
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

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         "testdb",
		Host:         upstreamHost,
		Port:         upstreamPort,
		DatabaseName: "testdb",
		Username:     "root",
		Password:     "rootpw",
		Protocol:     dbProtocol,
		SSLMode:      sslMode,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, db.UID, []string{})
	require.NoError(t, err)

	queryStorage := config.QueryStorageConfig{
		StoreResults:   true,
		MaxResultRows:  1000,
		MaxResultBytes: 1 * 1024 * 1024,
	}

	dumpCfg := config.DumpConfig{}
	if dumpDir != "" {
		dumpCfg = config.DumpConfig{
			Dir:       dumpDir,
			MaxSize:   config.DefaultDumpMaxSize,
			Retention: config.DefaultDumpRetention,
		}
	}

	proxy, err := NewServer(dataStore, encryptionKey, queryStorage, dumpCfg,
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

// replaceGrantWithControls revokes every active grant on the fixture and issues
// one carrying exactly the named controls, so a test can drive the proxy under a
// specific control set.
func replaceGrantWithControls(ctx context.Context, t *testing.T, f *fixture, controls ...string) {
	t.Helper()

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)
	require.NotEmpty(t, grants)

	user, err := f.store.GetUserByUsername(ctx, fixtureUser)
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, user.UID))
	}

	databases, err := f.store.ListServers(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, databases)

	_, err = testsupport.CreateGrantWithControls(ctx, t, f.store, user.UID, databases[0].UID, controls)
	require.NoError(t, err)
}

// TestIntegration_ReadOnlyGrant_BlocksWrite verifies that a grant with
// read_only control rejects an INSERT statement at the proxy layer.
func TestIntegration_ReadOnlyGrant_BlocksWrite(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)

	replaceGrantWithControls(ctx, t, f, store.ControlReadOnly)

	db := f.dialTLS()
	defer db.Close()

	_, err := db.ExecContext(ctx, "CREATE TABLE if not exists t (x int)")
	require.Error(t, err, "DDL must be refused under read-only grant")
}

// TestIntegration_ReadOnlyGrant_BlocksPreparedWrite is the live half of the
// dynamic-SQL escape: `PREPARE s FROM 'DELETE FROM t'` is a write whose first
// keyword is PREPARE, so the prefix-shaped classification behind read_only saw
// nothing and the DELETE ran a statement later, on the real server.
//
// It runs against a live MySQL rather than only against the validator because the
// property under test is that dbbat's static read agrees with what the *server*
// would have executed: the row count after the attempt is what proves it.
func TestIntegration_ReadOnlyGrant_BlocksPreparedWrite(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)

	// A table with one row, created while the grant is still full-write.
	direct := f.dialTLS()
	_, err := direct.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS prep_t (x int)")
	require.NoError(t, err)
	_, err = direct.ExecContext(ctx, "INSERT INTO prep_t VALUES (1)")
	require.NoError(t, err)
	require.NoError(t, direct.Close())

	replaceGrantWithControls(ctx, t, f, store.ControlReadOnly, store.ControlBlockDDL)

	db := f.dialTLS()
	defer db.Close()

	// The wrapper is refused, and so is the EXECUTE of a name that never got
	// prepared — the half of the pair that carries no statement text at all.
	_, err = db.ExecContext(ctx, "PREPARE s FROM 'DELETE FROM prep_t'")
	require.Error(t, err, "a prepared write must be refused under read_only")

	_, err = db.ExecContext(ctx, "EXECUTE s")
	require.Error(t, err, "and so must executing a name dbbat could not vouch for")

	// A payload dbbat cannot read statically is refused under these two controls.
	_, err = db.ExecContext(ctx, "SET @sql = 'DELETE FROM prep_t'")
	require.NoError(t, err, "setting a session variable is not a write")

	_, err = db.ExecContext(ctx, "PREPARE s FROM @sql")
	require.Error(t, err, "a payload dbbat cannot read must be refused under read_only")

	// A benign payload still runs: this is not a blanket refusal of PREPARE.
	_, err = db.ExecContext(ctx, "PREPARE ok FROM 'SELECT COUNT(*) FROM prep_t'")
	require.NoError(t, err, "a prepared read must still be allowed")

	var rows int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prep_t").Scan(&rows))
	require.Equal(t, 1, rows, "the row must still be there: nothing deleted it")
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

// TestIntegration_SessionDump verifies that when DBB_DUMP_DIR is configured
// the MySQL session writes a per-connection dump file containing post-auth
// command-phase traffic.
//
// The client leg is TLS (dialTLS), so this doubles as the end-to-end form of
// TestStartDumpIfConfigured_RecordsPlaintextAboveTLS: the capture is parsed and
// asserted to hold plaintext MySQL packets — the marker query legible, the
// stream cleanly framed — rather than TLS records. The unit test pins the wrap
// order against a real go-mysql handshake; this one would survive a go-mysql
// release that installed the *tls.Conn somewhere else entirely, because nothing
// here knows where it lives.
func TestIntegration_SessionDump(t *testing.T) {
	ctx := context.Background()

	dumpDir := t.TempDir()

	// The marker travels inside the COM_QUERY. It can only appear in the
	// capture if the recording point sits above TLS.
	const marker = "dbbat-plaintext-above-tls-marker"

	f := setupFixtureWithDumpDir(ctx, t, mysqlImage(), store.ProtocolMySQL, dumpDir)
	db := f.dialTLS()

	require.NoError(t, db.PingContext(ctx))

	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 99 /* "+marker+" */").Scan(&n))
	assert.Equal(t, 99, n)

	require.NoError(t, db.Close())

	// Allow the proxy to flush + close the dump.
	time.Sleep(300 * time.Millisecond)

	entries, err := os.ReadDir(dumpDir)
	require.NoError(t, err)

	var dumpFile string

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pcapng") {
			dumpFile = filepath.Join(dumpDir, e.Name())

			break
		}
	}

	require.NotEmpty(t, dumpFile, "expected a .dbbatdump file in %s", dumpDir)

	stat, err := os.Stat(dumpFile)
	require.NoError(t, err)
	assert.Greater(t, stat.Size(), int64(0), "dump file should not be empty after a query round-trip")

	t.Logf("dump file: %s (%d bytes)", dumpFile, stat.Size())

	pkts := readCapture(t, dumpFile)
	require.NotEmpty(t, pkts, "capture should hold the command phase")

	toServer := captureStream(pkts, dump.DirClientToServer)
	require.NotEmpty(t, toServer, "capture should hold client→server traffic")

	assert.Contains(t, string(toServer), marker,
		"the capture should hold the plaintext COM_QUERY, not TLS records")
	assert.NotEqual(t, byte(0x17), toServer[0],
		"capture starts with what looks like a TLS application-data record")
	assertMySQLFramed(t, toServer)
}

// assertMySQLFramed walks stream as a sequence of MySQL packets (3-byte
// little-endian payload length, sequence byte, payload) and fails unless it
// consumes exactly. TLS records do not frame that way, which is what makes this
// a check on where the tap sits rather than on the file merely being non-empty.
func assertMySQLFramed(t *testing.T, stream []byte) {
	t.Helper()

	const headerLen = 4

	for off := 0; off < len(stream); {
		require.LessOrEqual(t, off+headerLen, len(stream),
			"truncated MySQL packet header at offset %d", off)

		length := int(stream[off]) | int(stream[off+1])<<8 | int(stream[off+2])<<16

		require.LessOrEqual(t, off+headerLen+length, len(stream),
			"MySQL packet at offset %d declares %d payload bytes, past the end of the stream", off, length)

		off += headerLen + length
	}
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

// upstreamCipher returns the TLS cipher the *upstream* session is using, empty
// when that leg is plaintext. The query runs on the upstream connection the
// proxy opened, so its session status describes exactly the leg under test.
func (f *fixture) upstreamCipher(ctx context.Context, db *sql.DB) string {
	f.t.Helper()

	var name, cipher string
	require.NoError(f.t, db.QueryRowContext(ctx,
		"SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher))

	return cipher
}

// assertRecordedUpstreamTLS checks that the session wrote down which way it
// went. Under an opportunistic ssl_mode this row is the only place the answer
// exists.
func (f *fixture) assertRecordedUpstreamTLS(ctx context.Context, want bool) {
	f.t.Helper()

	require.Eventually(f.t, func() bool {
		conns, err := f.store.ListConnections(ctx, store.ConnectionFilter{Limit: 10})

		return err == nil && len(conns) > 0 && conns[0].UpstreamTLS == want
	}, 5*time.Second, 100*time.Millisecond, "connections.upstream_tls should be %v", want)
}

// TestIntegration_UpstreamTLS_Require verifies that ssl_mode=require actually
// encrypts the proxy→upstream leg: the upstream session must report a cipher.
func TestIntegration_UpstreamTLS_Require(t *testing.T) {
	ctx := context.Background()

	f := setupFixtureWithSSLMode(ctx, t, mysqlImage(), store.ProtocolMySQL, "require")
	db := f.dialTLS()
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))
	assert.NotEmpty(t, f.upstreamCipher(ctx, db),
		"upstream connection should be TLS-encrypted under ssl_mode=require")
	f.assertRecordedUpstreamTLS(ctx, true)
}

// TestIntegration_UpstreamTLS_Prefer is the opportunistic case this proxy did
// not have until the upstream-connect paths were unified: against a TLS-capable
// server, prefer must actually encrypt rather than quietly stay in plaintext.
func TestIntegration_UpstreamTLS_Prefer(t *testing.T) {
	ctx := context.Background()

	f := setupFixtureWithSSLMode(ctx, t, mysqlImage(), store.ProtocolMySQL, "prefer")
	db := f.dialTLS()
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))
	assert.NotEmpty(t, f.upstreamCipher(ctx, db),
		"upstream connection should be TLS-encrypted under ssl_mode=prefer against a TLS-capable server")
	f.assertRecordedUpstreamTLS(ctx, true)
}

// TestIntegration_UpstreamTLS_Disable is the counterpart: against the very same
// TLS-capable upstream, disable must stay in the clear.
func TestIntegration_UpstreamTLS_Disable(t *testing.T) {
	ctx := context.Background()

	f := setupFixtureWithSSLMode(ctx, t, mysqlImage(), store.ProtocolMySQL, "disable")
	db := f.dialTLS()
	defer db.Close()

	require.NoError(t, db.PingContext(ctx))
	assert.Empty(t, f.upstreamCipher(ctx, db),
		"upstream connection should stay plaintext under ssl_mode=disable")
	f.assertRecordedUpstreamTLS(ctx, false)
}

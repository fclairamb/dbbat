//go:build integration

package postgresql

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// defaultPGImage is the upstream PostgreSQL image the suite proxies to.
// Override with PG_TEST_IMAGE=postgres:17 (or postgres:14, …) to run the same
// matrix against another server version.
const defaultPGImage = "postgres:16-alpine"

// defaultStorageImage backs dbbat's own store. Kept separate from the upstream
// image so a matrix run against an exotic upstream doesn't also swap the
// storage engine underneath dbbat.
const defaultStorageImage = "postgres:15-alpine"

func pgImage() string {
	if img := os.Getenv("PG_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultPGImage
}

func storageImage() string {
	if img := os.Getenv("DBBAT_STORE_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultStorageImage
}

const (
	fixtureUser = "dbbattest"
	fixturePass = "dbbattest"
	upstreamDB  = "testdb"
	upstreamUsr = "postgres"
	upstreamPwd = "postgres"
)

// runPostgresContainer starts a PostgreSQL container and returns its bound
// host/port.
func runPostgresContainer(ctx context.Context, t *testing.T, image, dbName string) (testcontainers.Container, string, int) {
	t.Helper()

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_DB":       dbName,
				"POSTGRES_USER":     upstreamUsr,
				"POSTGRES_PASSWORD": upstreamPwd,
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start postgres container (%s)", image)

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "5432")
	require.NoError(t, err)

	t.Logf("PostgreSQL container ready: image=%s host=%s port=%s", image, host, port.Port())

	return c, host, int(port.Num())
}

// runTLSPostgresContainer starts a PostgreSQL container with ssl=on, using a
// self-signed cert generated here. The cert/key are mounted read-only and then
// copied+chowned by the container command, because Postgres refuses a key file
// that isn't owned by the server user with 0600 permissions — and bind mounts
// keep the host's ownership.
func runTLSPostgresContainer(ctx context.Context, t *testing.T, dbName string) (testcontainers.Container, string, int) {
	t.Helper()

	certPEM, keyPEM := selfSignedCert(t)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o644))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o644))

	const bootstrap = `cp /tls/server.crt /tls/server.key /var/lib/postgresql/ && ` +
		`chown postgres:postgres /var/lib/postgresql/server.crt /var/lib/postgresql/server.key && ` +
		`chmod 600 /var/lib/postgresql/server.key && ` +
		`exec docker-entrypoint.sh postgres -c ssl=on ` +
		`-c ssl_cert_file=/var/lib/postgresql/server.crt ` +
		`-c ssl_key_file=/var/lib/postgresql/server.key`

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        pgImage(),
			ExposedPorts: []string{"5432/tcp"},
			Entrypoint:   []string{"/bin/sh", "-c"},
			Cmd:          []string{bootstrap},
			Env: map[string]string{
				"POSTGRES_DB":       dbName,
				"POSTGRES_USER":     upstreamUsr,
				"POSTGRES_PASSWORD": upstreamPwd,
			},
			Files: []testcontainers.ContainerFile{
				{HostFilePath: certPath, ContainerFilePath: "/tls/server.crt", FileMode: 0o644},
				{HostFilePath: keyPath, ContainerFilePath: "/tls/server.key", FileMode: 0o644},
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start TLS postgres container")

	host, err := c.Host(ctx)
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "5432")
	require.NoError(t, err)

	t.Logf("TLS PostgreSQL container ready: host=%s port=%s", host, port.Port())

	return c, host, int(port.Num())
}

// selfSignedCert returns a PEM cert/key pair valid for localhost and 127.0.0.1.
func selfSignedCert(t *testing.T) ([]byte, []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM
}

// fixture wires up: a storage container + dbbat store, a user/database/grant,
// an upstream PostgreSQL container, and a started proxy.
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

// fixtureOpts tweaks what setupFixtureWith builds.
type fixtureOpts struct {
	// dumpDir, when non-empty, enables per-session dump files.
	dumpDir string
	// tlsUpstream starts the upstream Postgres with ssl=on and a self-signed cert.
	tlsUpstream bool
	// sslMode is the server row's ssl_mode (defaults to "disable").
	sslMode string
}

func setupFixtureWithDumpDir(ctx context.Context, t *testing.T, dumpDir string) *fixture {
	t.Helper()

	return setupFixtureWith(ctx, t, fixtureOpts{dumpDir: dumpDir})
}

func setupFixtureWith(ctx context.Context, t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()

	dumpDir := opts.dumpDir

	sslMode := opts.sslMode
	if sslMode == "" {
		sslMode = "disable"
	}

	var (
		upstream     testcontainers.Container
		upstreamHost string
		upstreamPort int
	)

	if opts.tlsUpstream {
		upstream, upstreamHost, upstreamPort = runTLSPostgresContainer(ctx, t, upstreamDB)
	} else {
		upstream, upstreamHost, upstreamPort = runPostgresContainer(ctx, t, pgImage(), upstreamDB)
	}

	t.Cleanup(func() { _ = upstream.Terminate(context.Background()) })

	storeContainer, storeHost, storePort := runPostgresContainer(ctx, t, storageImage(), "dbbat_test")
	t.Cleanup(func() { _ = storeContainer.Terminate(context.Background()) })

	storeDSN := fmt.Sprintf("postgres://%s:%s@%s/dbbat_test?sslmode=disable",
		upstreamUsr, upstreamPwd, net.JoinHostPort(storeHost, strconv.Itoa(storePort)))

	dataStore, err := store.New(ctx, storeDSN)
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

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         upstreamDB,
		Host:         upstreamHost,
		Port:         upstreamPort,
		DatabaseName: upstreamDB,
		Username:     upstreamUsr,
		Password:     upstreamPwd,
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      sslMode,
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

	proxy, err := NewServer(dataStore, encKey, queryStorage, dumpCfg, nil, config.PGConfig{}, slog.Default())
	require.NoError(t, err)

	go func() { _ = proxy.Start("127.0.0.1:0") }()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownCtx)
	})

	require.Eventually(t, func() bool { return proxy.Addr() != nil },
		5*time.Second, 50*time.Millisecond, "proxy never started listening")

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

// dsn builds a client DSN pointing at the proxy. sslmode is caller-chosen so
// tests can exercise both plaintext and proxy-terminated TLS.
func (f *fixture) dsn(username, password, sslMode string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
		username, password, f.proxyAddr, upstreamDB, sslMode)
}

// connect dials through the proxy with TLS required (the proxy auto-generates
// a self-signed cert, so verification is off).
func (f *fixture) connect(ctx context.Context, username, password string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, f.dsn(username, password, "require"))
}

// mustConnect fails the test if the fixture user can't connect.
func (f *fixture) mustConnect(ctx context.Context, password string) *pgx.Conn {
	f.t.Helper()

	conn, err := f.connect(ctx, fixtureUser, password)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = conn.Close(context.Background()) })

	return conn
}

// replaceGrant revokes the current grants and installs one with the given
// controls.
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

// TestIntegration_ProxyAuth_Password connects through the proxy with a dbbat
// password over proxy-terminated TLS and runs a trivial query.
func TestIntegration_ProxyAuth_Password(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)
}

// TestIntegration_ProxyAuth_Plaintext connects with sslmode=disable, covering
// the non-TLS path through the same handshake.
func TestIntegration_ProxyAuth_Plaintext(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn, err := pgx.Connect(ctx, f.dsn(fixtureUser, fixturePass, "disable"))
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.Background()) }()

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)
}

// TestIntegration_ProxyAuth_APIKey authenticates with a dbb_ API key used as
// the password.
func TestIntegration_ProxyAuth_APIKey(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	_, plainKey, err := f.store.CreateAPIKey(ctx, f.user.UID, "test-key", nil, f.encKey)
	require.NoError(t, err)

	conn := f.mustConnect(ctx, plainKey)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	assert.Equal(t, 1, got)
}

// TestIntegration_WrongPassword verifies a bad password is refused.
func TestIntegration_WrongPassword(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	_, err := f.connect(ctx, fixtureUser, "wrongpassword")
	require.Error(t, err, "wrong password must fail")
}

// TestIntegration_UnknownDatabase verifies a startup message naming a database
// dbbat doesn't know about is refused.
func TestIntegration_UnknownDatabase(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=require",
		fixtureUser, fixturePass, f.proxyAddr, "nosuchdb")

	_, err := pgx.Connect(ctx, dsn)
	require.Error(t, err, "unknown database must be refused")
}

// TestIntegration_QueryAndCapture verifies a write + read round-trip through
// the proxy, and that the simple-protocol queries and their result rows land in
// the query log.
func TestIntegration_QueryAndCapture(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	_, err := conn.Exec(ctx, "CREATE TABLE widgets (id serial primary key, name text, qty int)")
	require.NoError(t, err)

	_, err = conn.Exec(ctx, "INSERT INTO widgets (name, qty) VALUES ('gadget', 7)")
	require.NoError(t, err)

	var name string

	var qty int

	require.NoError(t, conn.QueryRow(ctx, "SELECT name, qty FROM widgets").Scan(&name, &qty))
	assert.Equal(t, "gadget", name)
	assert.Equal(t, 7, qty)

	var selectUID string

	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		var sawInsert, sawSelect bool

		for i := range queries {
			text := strings.ToUpper(queries[i].SQLText)
			if strings.HasPrefix(text, "INSERT INTO WIDGETS") {
				sawInsert = true
			}

			if strings.HasPrefix(text, "SELECT NAME, QTY") {
				sawSelect = true
				selectUID = queries[i].UID.String()
			}
		}

		return sawInsert && sawSelect
	}, 5*time.Second, 100*time.Millisecond, "insert and select should be logged")

	uid, err := uuid.Parse(selectUID)
	require.NoError(t, err)

	rows, err := f.store.GetQueryRows(ctx, uid, "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, rows.Rows, "select should capture result rows")
}

// TestIntegration_ExtendedProtocol_Capture exercises Parse/Bind/Execute
// (pgx's default) and asserts the parameterised query and its rows are logged.
func TestIntegration_ExtendedProtocol_Capture(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	_, err := conn.Exec(ctx, "CREATE TABLE prepared (id int, label text)")
	require.NoError(t, err)

	// pgx defaults to the extended protocol, so a parameterised statement is a
	// real Parse/Bind/Execute round-trip rather than the simple protocol.
	_, err = conn.Exec(ctx, "INSERT INTO prepared (id, label) VALUES ($1, $2)", 42, "answer")
	require.NoError(t, err)

	var label string
	require.NoError(t, conn.QueryRow(ctx, "SELECT label FROM prepared WHERE id = $1", 42).Scan(&label))
	assert.Equal(t, "answer", label)

	// The INSERT binds "answer" in text format, so it is captured verbatim.
	//
	// Note: pgx sends Parse with an empty ParameterOIDs list (it lets the server
	// infer the types), so binary-format parameters like the int4 42 are logged
	// opaquely as "(oid:0)<base64>" — the proxy never sees a resolved OID. That
	// is a known observability gap, tracked in
	// specs/todos/2026-07-21-resolve-bind-parameter-oids-from-parameterdescription.md.
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if !strings.Contains(strings.ToUpper(queries[i].SQLText), "INSERT INTO PREPARED") {
				continue
			}

			if queries[i].Parameters == nil {
				continue
			}

			for _, v := range queries[i].Parameters.Values {
				if v == "answer" {
					return true
				}
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond, "extended-protocol query should be logged with its bind parameters")

	// The SELECT's bind parameter is captured too, even though its type is not
	// resolvable (binary format, no declared OID).
	require.Eventually(t, func() bool {
		queries, err := f.store.ListQueries(ctx, store.QueryFilter{Limit: 200})
		if err != nil {
			return false
		}

		for i := range queries {
			if !strings.Contains(strings.ToUpper(queries[i].SQLText), "FROM PREPARED WHERE ID = $1") {
				continue
			}

			if queries[i].Parameters != nil && len(queries[i].Parameters.Values) == 1 {
				return true
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond, "extended-protocol SELECT should record one bind parameter")
}

// TestIntegration_ReadOnlyGrant_BlocksWrite verifies a read_only grant rejects
// a write at the proxy while still allowing reads.
func TestIntegration_ReadOnlyGrant_BlocksWrite(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	// Seed a table before the grant is narrowed.
	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE ro (id int)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"read_only"})

	conn := f.mustConnect(ctx, fixturePass)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT count(*) FROM ro").Scan(&got))
	assert.Equal(t, 0, got)

	_, err = conn.Exec(ctx, "INSERT INTO ro (id) VALUES (1)")
	require.Error(t, err, "insert must be refused under a read-only grant")
}

// TestIntegration_BlockDDL_BlocksCreateTable verifies block_ddl rejects DDL
// while ordinary DML still goes through.
func TestIntegration_BlockDDL_BlocksCreateTable(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE ddl (id int)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"block_ddl"})

	conn := f.mustConnect(ctx, fixturePass)

	_, err = conn.Exec(ctx, "INSERT INTO ddl (id) VALUES (1)")
	require.NoError(t, err, "DML should still be allowed under block_ddl")

	_, err = conn.Exec(ctx, "CREATE TABLE blocked_ddl (id int)")
	require.Error(t, err, "CREATE TABLE must be refused under a block_ddl grant")
}

// TestIntegration_BlockCopy_BlocksCopy verifies block_copy rejects COPY.
func TestIntegration_BlockCopy_BlocksCopy(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	seed := f.mustConnect(ctx, fixturePass)
	_, err := seed.Exec(ctx, "CREATE TABLE cp (id int)")
	require.NoError(t, err)
	require.NoError(t, seed.Close(ctx))

	f.replaceGrant(ctx, []string{"block_copy"})

	conn := f.mustConnect(ctx, fixturePass)

	_, err = conn.Exec(ctx, "COPY cp TO STDOUT")
	require.Error(t, err, "COPY must be refused under a block_copy grant")
}

// TestIntegration_SessionDump verifies a per-connection dump file is written.
func TestIntegration_SessionDump(t *testing.T) {
	ctx := context.Background()
	dumpDir := t.TempDir()
	f := setupFixtureWithDumpDir(ctx, t, dumpDir)

	conn, err := f.connect(ctx, fixtureUser, fixturePass)
	require.NoError(t, err)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))
	require.NoError(t, conn.Close(ctx))

	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dumpDir)
		if err != nil {
			return false
		}

		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".dbbat-dump") {
				continue
			}

			stat, err := os.Stat(filepath.Join(dumpDir, e.Name()))
			if err == nil && stat.Size() > 0 {
				return true
			}
		}

		return false
	}, 5*time.Second, 100*time.Millisecond, "expected a non-empty .dbbat-dump file in %s", dumpDir)
}

// TestIntegration_UpstreamTLS_Require verifies that ssl_mode=require actually
// encrypts the proxy→upstream leg: pg_stat_ssl for the backend serving our
// proxied session must report ssl=true.
func TestIntegration_UpstreamTLS_Require(t *testing.T) {
	ctx := context.Background()
	f := setupFixtureWith(ctx, t, fixtureOpts{tlsUpstream: true, sslMode: "require"})

	conn := f.mustConnect(ctx, fixturePass)

	var ssl bool
	require.NoError(t, conn.QueryRow(ctx,
		"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl))
	assert.True(t, ssl, "upstream connection should be TLS-encrypted under ssl_mode=require")
}

// TestIntegration_UpstreamTLS_Disable is the counterpart: with ssl_mode=disable
// against the very same TLS-capable upstream, no SSLRequest is sent and the
// backend reports an unencrypted connection.
func TestIntegration_UpstreamTLS_Disable(t *testing.T) {
	ctx := context.Background()
	f := setupFixtureWith(ctx, t, fixtureOpts{tlsUpstream: true, sslMode: "disable"})

	conn := f.mustConnect(ctx, fixturePass)

	var ssl bool
	require.NoError(t, conn.QueryRow(ctx,
		"SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&ssl))
	assert.False(t, ssl, "upstream connection should stay plaintext under ssl_mode=disable")
}

// TestIntegration_UpstreamTLS_VerifyFullRejectsSelfSigned verifies that
// verify-full really validates the upstream chain: the container's self-signed
// cert isn't in the system pool, so the session must fail rather than silently
// downgrade to an unverified tunnel.
func TestIntegration_UpstreamTLS_VerifyFullRejectsSelfSigned(t *testing.T) {
	ctx := context.Background()
	f := setupFixtureWith(ctx, t, fixtureOpts{tlsUpstream: true, sslMode: "verify-full"})

	_, err := f.connect(ctx, fixtureUser, fixturePass)
	require.Error(t, err, "verify-full must reject an untrusted upstream certificate")
}

// TestIntegration_RevocationKillsSession verifies revoking the grant mid-session
// tears the live connection down.
func TestIntegration_RevocationKillsSession(t *testing.T) {
	ctx := context.Background()
	f := setupFixture(ctx, t)

	conn := f.mustConnect(ctx, fixturePass)

	var got int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&got))

	grants, err := f.store.ListGrants(ctx, store.GrantFilter{ActiveOnly: true})
	require.NoError(t, err)

	for _, g := range grants {
		require.NoError(t, f.store.RevokeGrant(ctx, g.UID, f.user.UID))
		f.store.Revocations().Revoke(g.UID)
	}

	require.Eventually(t, func() bool {
		return conn.QueryRow(ctx, "SELECT 1").Scan(&got) != nil
	}, 10*time.Second, 250*time.Millisecond, "revoked session should be torn down")
}

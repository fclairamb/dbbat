//go:build integration

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// defaultMSSQLImage is the upstream image this suite runs against. Set
// MSSQL_TEST_IMAGE to try another edition or version.
//
// Note for anyone running this on an Apple Silicon laptop: Microsoft publishes
// this image for linux/amd64 only, so it runs under emulation if it runs at
// all. The CI job (ubuntu-24.04, amd64) is where it is expected to pass.
const defaultMSSQLImage = "mcr.microsoft.com/mssql/server:2022-latest"

// saPassword must satisfy SQL Server's password policy or the container exits
// during setup with a message about complexity requirements.
const saPassword = "dbbat-Integration-1"

func mssqlImage() string {
	if img := os.Getenv("MSSQL_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultMSSQLImage
}

// startUpstreamSQLServer boots a real SQL Server and returns its host:port.
func startUpstreamSQLServer(ctx context.Context, t *testing.T) string {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        mssqlImage(),
		ExposedPorts: []string{"1433/tcp"},
		Env: map[string]string{
			"ACCEPT_EULA":       "Y",
			"MSSQL_SA_PASSWORD": saPassword,
			"MSSQL_PID":         "Developer",
		},
		// "SQL Server is now ready for client connections" is logged once
		// recovery is complete; the port opens well before that.
		WaitingFor: wait.ForLog("SQL Server is now ready for client connections").
			WithStartupTimeout(10 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "1433/tcp")
	require.NoError(t, err)

	return net.JoinHostPort(host, port.Port())
}

// startProxy runs a proxy with no store on an ephemeral port. Every login that
// gets past the handshake is refused as an authentication failure, which is
// what the handshake-only cases below assert on.
func startProxy(t *testing.T, cfg config.MSSQLConfig) string {
	t.Helper()

	srv, err := NewServer(nil, nil, config.DumpConfig{}, nil, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	go func() {
		_ = srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 10*time.Second, 10*time.Millisecond)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = srv.Shutdown(ctx)
	})

	return srv.Addr().String()
}

// dsn builds a go-mssqldb connection string against addr.
func dsn(addr, encrypt string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf(
		"sqlserver://sa:%s@%s:%s?encrypt=%s&TrustServerCertificate=true&connection+timeout=30",
		saPassword, host, port, encrypt)
}

// TestUpstreamSQLServerIsReachable proves the container fixture works, so a
// failure in the proxy tests below cannot be blamed on the image.
func TestUpstreamSQLServerIsReachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	addr := startUpstreamSQLServer(ctx, t)

	db, err := sql.Open("sqlserver", dsn(addr, "false"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT @@VERSION").Scan(&version))
	assert.Contains(t, version, "Microsoft SQL Server")
}

// TestProxyHandshakeWithARealClient proves a genuine third-party driver gets
// all the way through PRELOGIN, the encapsulated TLS handshake and LOGIN7 in
// every encryption mode, and then reads back dbbat's refusal as a normal SQL
// error rather than hanging or failing to parse the stream.
func TestProxyHandshakeWithARealClient(t *testing.T) {
	tests := []struct {
		name    string
		encrypt string
		tlsOff  bool
	}{
		// encrypt=true -> ENCRYPT_ON: the whole session is inside TLS.
		{name: "encryption on", encrypt: "true"},
		// encrypt=false -> ENCRYPT_OFF: a TLS handshake still happens and
		// covers the login packet only, then the stream reverts to cleartext.
		{name: "login-packet-only encryption", encrypt: "false"},
		// encrypt=disable -> ENCRYPT_NOT_SUP: no handshake at all.
		{name: "no encryption", encrypt: "disable"},
		// A listener with TLS termination switched off must still serve a
		// client that does not insist on it.
		{name: "listener without tls", encrypt: "disable", tlsOff: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			addr := startProxy(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: tc.tlsOff}})

			db, err := sql.Open("sqlserver", dsn(addr, tc.encrypt))
			require.NoError(t, err)

			t.Cleanup(func() { _ = db.Close() })

			err = db.PingContext(ctx)
			require.Error(t, err, "a proxy without a store authenticates nobody")
			assert.Contains(t, err.Error(), "Login failed for user",
				"the driver must surface the proxy's own message, which means it parsed "+
					"the ERROR/DONE token stream")
		})
	}
}

// TestProxyRefusesMARS documents the v1 limitation: a client configured with
// MultipleActiveResultSets cannot connect, and the failure is diagnosable.
func TestProxyRefusesMARS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	addr := startProxy(t, config.MSSQLConfig{})

	db, err := sql.Open("sqlserver", dsn(addr, "disable")+"&MultipleActiveResultSets=true")
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	// The proxy answers MARS=0x00 in PRELOGIN; the driver either gives up or
	// continues without MARS and then hits the login refusal. Either way it
	// fails, and never hangs.
	err = db.PingContext(ctx)
	require.Error(t, err)
}

// TestProxyRefusesEncryptionWhenTLSIsDisabled covers the one row of the
// negotiation matrix that has to fail: a client requiring encryption against a
// listener that cannot provide it.
func TestProxyRefusesEncryptionWhenTLSIsDisabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	addr := startProxy(t, config.MSSQLConfig{TLS: config.TLSConfig{Disable: true}})

	db, err := sql.Open("sqlserver", dsn(addr, "true"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	err = db.PingContext(ctx)
	require.Error(t, err)
	assert.False(t, strings.Contains(err.Error(), "Login failed for user"),
		"the connection must be refused during negotiation, before any login")
}

// Fixture names for the end-to-end tests below. The dbbat login is deliberately
// different from the SQL Server one: the whole point of the proxy is that the
// client never learns the upstream credentials.
const (
	e2eDBBatUser     = "e2e-user"
	e2eDBBatPassword = "e2e-p@ssw0rd"
	e2eEntryName     = "e2e-sqlserver"
	e2eRealDatabase  = "master"
)

// startProxyWithStore runs a fully wired proxy — store, encryption key, auth —
// on an ephemeral port.
func startProxyWithStore(
	t *testing.T,
	cfg config.MSSQLConfig,
	dataStore *store.Store,
	encryptionKey []byte,
) string {
	t.Helper()

	srv, err := NewServer(dataStore, encryptionKey, config.DumpConfig{}, nil, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	go func() {
		_ = srv.Start("127.0.0.1:0")
	}()

	require.Eventually(t, func() bool { return srv.Addr() != nil }, 10*time.Second, 10*time.Millisecond)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = srv.Shutdown(ctx)
	})

	return srv.Addr().String()
}

// seedE2E creates the dbbat user, the SQL Server entry pointing at upstreamAddr
// and a live grant linking them, and returns the store plus its encryption key.
func seedE2E(ctx context.Context, t *testing.T, upstreamAddr, sslMode string) (*store.Store, []byte) {
	t.Helper()

	dataStore := newTestStore(t)

	hash, err := crypto.HashPassword(e2eDBBatPassword)
	require.NoError(t, err)

	user, err := dataStore.CreateUser(ctx, e2eDBBatUser, hash, []string{store.RoleConnector})
	require.NoError(t, err)

	encryptionKey := make([]byte, 32)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	host, portText, err := net.SplitHostPort(upstreamAddr)
	require.NoError(t, err)

	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	database, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         e2eEntryName,
		Host:         host,
		Port:         port,
		DatabaseName: e2eRealDatabase,
		Username:     "sa",
		Password:     saPassword,
		Protocol:     store.ProtocolMSSQL,
		SSLMode:      sslMode,
	}, encryptionKey)
	require.NoError(t, err)

	grantFullAccess(t, dataStore, user.UID, database.UID)

	return dataStore, encryptionKey
}

// proxyDSN builds a go-mssqldb connection string aimed at the proxy, using the
// dbbat credentials and naming the dbbat entry as the database.
func proxyDSN(addr, encrypt string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf(
		"sqlserver://%s:%s@%s:%s?database=%s&encrypt=%s&TrustServerCertificate=true&connection+timeout=30",
		url.QueryEscape(e2eDBBatUser), url.QueryEscape(e2eDBBatPassword),
		host, port, url.QueryEscape(e2eEntryName), encrypt)
}

// TestProxyRelaysQueriesEndToEnd is what stage 2 is for: a real driver
// authenticates against dbbat, dbbat opens its own session on a real SQL
// Server with the stored credentials, and a query's rows come back.
//
// The two rows of the table are the two upstream encryption outcomes, asserted
// on the connection row's upstream_tls so the connections UI is proved to
// report SQL Server sessions like the others. The client leg is varied
// independently of the upstream leg, because they negotiate independently.
func TestProxyRelaysQueriesEndToEnd(t *testing.T) {
	tests := []struct {
		name          string
		upstreamMode  string
		clientEncrypt string
		wantTLS       bool
	}{
		{name: "plaintext upstream", upstreamMode: "disable", clientEncrypt: "disable", wantTLS: false},
		{name: "encrypted upstream", upstreamMode: "require", clientEncrypt: "true", wantTLS: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			upstreamAddr := startUpstreamSQLServer(ctx, t)
			dataStore, encryptionKey := seedE2E(ctx, t, upstreamAddr, tc.upstreamMode)

			proxyAddr := startProxyWithStore(t, config.MSSQLConfig{}, dataStore, encryptionKey)

			db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, tc.clientEncrypt))
			require.NoError(t, err)

			t.Cleanup(func() { _ = db.Close() })

			var answer int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT 42").Scan(&answer))
			assert.Equal(t, 42, answer)

			// A multi-row result exercises the packet-at-a-time response relay
			// rather than a single small message.
			rows, err := db.QueryContext(ctx,
				"SELECT TOP 50 name FROM sys.objects ORDER BY name")
			require.NoError(t, err)

			names := 0

			for rows.Next() {
				var name string
				require.NoError(t, rows.Scan(&name))

				names++
			}

			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Positive(t, names, "rows must actually arrive through the relay")

			// The session must be on the connection list, with the upstream
			// leg's encryption recorded honestly.
			connections, err := dataStore.ListConnections(ctx, store.ConnectionFilter{Limit: 10})
			require.NoError(t, err)
			require.NotEmpty(t, connections)
			assert.Equal(t, tc.wantTLS, connections[0].UpstreamTLS)
		})
	}
}

// TestProxyRefusesBadCredentialsAgainstARealServer proves the refusal path is
// the same one a real driver reads, and that nothing reaches the upstream.
func TestProxyRefusesBadCredentialsAgainstARealServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	dataStore, encryptionKey := seedE2E(ctx, t, upstreamAddr, "disable")

	proxyAddr := startProxyWithStore(t, config.MSSQLConfig{}, dataStore, encryptionKey)

	dsnText := strings.Replace(proxyDSN(proxyAddr, "disable"),
		url.QueryEscape(e2eDBBatPassword), "wrong-password", 1)

	db, err := sql.Open("sqlserver", dsnText)
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	err = db.PingContext(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Login failed for user '"+e2eDBBatUser+"'")

	connections, err := dataStore.ListConnections(ctx, store.ConnectionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, connections, "a refused login opens no session and records nothing")
}

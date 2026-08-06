//go:build integration

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
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
//
// Stage 1 of the proxy never talks to it — there is no upstream leg yet. It is
// started anyway because this is the fixture stage 2 builds on, and because a
// handshake suite that has never seen a real server is a handshake suite that
// will surprise you later.
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

// startProxy runs the stage-1 proxy on an ephemeral port.
func startProxy(t *testing.T, cfg config.MSSQLConfig) string {
	t.Helper()

	srv, err := NewServer(cfg, slog.New(slog.DiscardHandler))
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

// TestProxyHandshakeWithARealClient is the point of this stage: a genuine
// third-party driver must get all the way through PRELOGIN, the encapsulated
// TLS handshake and LOGIN7, and must then read back the stub error as a normal
// SQL error rather than hanging or failing to parse the stream.
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
			require.Error(t, err, "stage 1 always refuses the login")
			assert.Contains(t, err.Error(), "not wired through",
				"the driver must surface the proxy's own message, which means it parsed "+
					"the ERROR/DONE token stream")
			assert.Contains(t, err.Error(), "stage 1 of 3")
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
	// continues without MARS and then hits the stage-1 stub. Either way it
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
	assert.False(t, strings.Contains(err.Error(), "not wired through"),
		"the connection must be refused during negotiation, before any login")
}

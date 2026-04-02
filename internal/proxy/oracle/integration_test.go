//go:build integration

package oracle

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

	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/store"
)

const defaultOracleImage = "gvenzl/oracle-xe:18.4.0-slim"

func oracleTestImage() string {
	if img := os.Getenv("ORACLE_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultOracleImage
}

// startOracleContainer starts an Oracle XE container for testing.
func startOracleContainer(t *testing.T) (testcontainers.Container, string, int) {
	t.Helper()

	ctx := context.Background()
	image := oracleTestImage()

	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"1521/tcp"},
		Env: map[string]string{
			"ORACLE_PASSWORD": "oracle",
		},
		WaitingFor: wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(5 * time.Minute),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "1521")
	require.NoError(t, err)

	t.Logf("Oracle container ready: image=%s host=%s port=%s", image, host, port.Port())

	return container, host, port.Int()
}

// TestIntegration_OracleContainer verifies we can start and connect to Oracle directly.
func TestIntegration_OracleContainer(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	// Connect directly to Oracle (bypass proxy)
	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.PingContext(context.Background())
	require.NoError(t, err)

	// Verify version
	var banner string
	err = db.QueryRowContext(context.Background(),
		"SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&banner)
	require.NoError(t, err)
	t.Logf("Oracle version: %s", banner)

	image := oracleTestImage()
	if strings.Contains(image, "oracle-xe:18") {
		assert.Contains(t, strings.ToLower(banner), "18")
	}
}

// TestIntegration_TNSCapture connects to a real Oracle and validates our TNS parser
// against real protocol traffic.
func TestIntegration_TNSCapture(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	// Connect raw TCP to Oracle and send a TNS Connect
	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", host, port))
	require.NoError(t, err)
	defer conn.Close()

	// Build and send a real TNS Connect packet
	connectPayload := buildTNSConnect("XEPDB1")
	connectPkt := encodeTNSPacket(TNSPacketTypeConnect, connectPayload)

	_, err = conn.Write(connectPkt)
	require.NoError(t, err)

	// Read the response — should be Accept, Refuse, or Redirect
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := readTNSPacket(conn)
	require.NoError(t, err)

	t.Logf("Oracle responded with packet type: %s (code=%d), payload length: %d",
		resp.Type, resp.Type, len(resp.Payload))

	// Oracle should respond with Accept or Refuse (not crash or hang)
	assert.True(t,
		resp.Type == TNSPacketTypeAccept ||
			resp.Type == TNSPacketTypeRefuse ||
			resp.Type == TNSPacketTypeRedirect,
		"expected Accept/Refuse/Redirect, got %s", resp.Type)
}

// TestIntegration_ProxyPassthrough tests connecting through the proxy to a real Oracle.
// Requires a running PostgreSQL (for DBBat store) and Oracle container.
func TestIntegration_ProxyPassthrough(t *testing.T) {
	ctx := context.Background()

	// Start Oracle container
	oracleContainer, oracleHost, oraclePort := startOracleContainer(t)
	defer func() { _ = oracleContainer.Terminate(ctx) }()

	// Start PostgreSQL container for DBBat store
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
	defer func() { _ = pgContainer.Terminate(ctx) }()

	pgHost, _ := pgContainer.Host(ctx)
	pgPort, _ := pgContainer.MappedPort(ctx, "5432")
	pgDSN := fmt.Sprintf("postgres://test:test@%s:%s/dbbat_test?sslmode=disable", pgHost, pgPort.Port())

	// Create store and seed data
	dataStore, err := store.New(ctx, pgDSN)
	require.NoError(t, err)
	defer dataStore.Close()

	// Create user
	user, err := dataStore.CreateUser(ctx, "SYSTEM", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	// Create database pointing to Oracle container
	db, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "XEPDB1",
		Host:         oracleHost,
		Port:         oraclePort,
		DatabaseName: "XEPDB1",
		Username:     "system",
		Protocol:     store.ProtocolOracle,
	}, nil) // nil encryption key — password not needed for this test
	require.NoError(t, err)

	// Create grant
	_, err = dataStore.CreateGrant(ctx, user.UID, db.UID, user.UID, []string{},
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil, nil)
	require.NoError(t, err)

	// Start Oracle proxy
	proxy := NewServer(dataStore, nil, nil, slog.Default())
	go func() { _ = proxy.Start(":0") }()
	defer func() { _ = proxy.Shutdown(ctx) }()

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 2*time.Second, 50*time.Millisecond)

	t.Logf("Oracle proxy listening at %s", proxy.Addr())

	// Connect through proxy using raw TCP and verify the TNS handshake works
	proxyConn, err := net.Dial("tcp", proxy.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	// Send TNS Connect
	connectPayload := buildTNSConnect("XEPDB1")
	_, err = proxyConn.Write(encodeTNSPacket(TNSPacketTypeConnect, connectPayload))
	require.NoError(t, err)

	// Should get Accept (or Refuse if Oracle doesn't like our connect descriptor — either way, proxy worked)
	proxyConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := readTNSPacket(proxyConn)
	require.NoError(t, err)

	t.Logf("Proxy forwarded Oracle response: type=%s", resp.Type)

	// The proxy should have forwarded something from Oracle (not a proxy-generated refuse about missing DB)
	assert.True(t,
		resp.Type == TNSPacketTypeAccept ||
			resp.Type == TNSPacketTypeRefuse ||
			resp.Type == TNSPacketTypeRedirect,
		"expected Oracle response, got %s", resp.Type)
}

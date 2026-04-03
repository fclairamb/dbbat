//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
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

	env := map[string]string{
		"ORACLE_PASSWORD": "oracle",
	}

	timeout := 5 * time.Minute
	if strings.Contains(image, "enterprise") {
		env = map[string]string{
			"ORACLE_SID": "ORCLCDB",
			"ORACLE_PDB": "ORCLPDB1",
			"ORACLE_PWD": "oracle",
		}
		timeout = 10 * time.Minute
	}

	req := testcontainers.ContainerRequest{
		Image:        image,
		ExposedPorts: []string{"1521/tcp"},
		Env:          env,
		WaitingFor:   wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(timeout),
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

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	defer db.Close()

	err = db.PingContext(context.Background())
	require.NoError(t, err)

	var banner string
	err = db.QueryRowContext(context.Background(),
		"SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&banner)
	require.NoError(t, err)
	t.Logf("Oracle version: %s", banner)
}

// TestIntegration_TNSCapture connects to a real Oracle and validates our TNS parser.
func TestIntegration_TNSCapture(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", host, port))
	require.NoError(t, err)
	defer conn.Close()

	connectPayload := buildTNSConnect("XEPDB1")
	connectPkt := encodeTNSPacket(TNSPacketTypeConnect, connectPayload)

	_, err = conn.Write(connectPkt)
	require.NoError(t, err)

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := readTNSPacket(conn)
	require.NoError(t, err)

	t.Logf("Oracle responded with packet type: %s (code=%d), payload length: %d",
		resp.Type, resp.Type, len(resp.Payload))

	assert.True(t,
		resp.Type == TNSPacketTypeAccept ||
			resp.Type == TNSPacketTypeRefuse ||
			resp.Type == TNSPacketTypeRedirect,
		"expected Accept/Refuse/Redirect, got %s", resp.Type)
}

// TestIntegration_ProxyPassthrough tests connecting through the proxy to a real Oracle.
func TestIntegration_ProxyPassthrough(t *testing.T) {
	ctx := context.Background()

	oracleContainer, oracleHost, oraclePort := startOracleContainer(t)
	defer func() { _ = oracleContainer.Terminate(ctx) }()

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

	dataStore, err := store.New(ctx, pgDSN)
	require.NoError(t, err)
	defer dataStore.Close()

	user, err := dataStore.CreateUser(ctx, "SYSTEM", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	db, err := dataStore.CreateDatabase(ctx, &store.Database{
		Name:         "XEPDB1",
		Host:         oracleHost,
		Port:         oraclePort,
		DatabaseName: "XEPDB1",
		Username:     "system",
		Protocol:     store.ProtocolOracle,
	}, nil)
	require.NoError(t, err)

	_, err = dataStore.CreateGrant(ctx, user.UID, db.UID, user.UID, []string{},
		time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour), nil, nil)
	require.NoError(t, err)

	queryStorage := config.QueryStorageConfig{
		StoreResults:   true,
		MaxResultRows:  100,
		MaxResultBytes: 1048576,
	}

	proxy := NewServer(dataStore, nil, nil, queryStorage, slog.Default())
	go func() { _ = proxy.Start(":0") }()
	defer func() { _ = proxy.Shutdown(ctx) }()

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 2*time.Second, 50*time.Millisecond)
	t.Logf("Oracle proxy listening at %s", proxy.Addr())

	proxyConn, err := net.Dial("tcp", proxy.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	connectPayload := buildTNSConnect("XEPDB1")
	_, err = proxyConn.Write(encodeTNSPacket(TNSPacketTypeConnect, connectPayload))
	require.NoError(t, err)

	proxyConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := readTNSPacket(proxyConn)
	require.NoError(t, err)

	t.Logf("Proxy forwarded Oracle response: type=%s", resp.Type)
	assert.True(t,
		resp.Type == TNSPacketTypeAccept ||
			resp.Type == TNSPacketTypeRefuse ||
			resp.Type == TNSPacketTypeRedirect,
		"expected Oracle response, got %s", resp.Type)
}

// --- Phase 3: Result capture integration tests ---

func TestIntegration_ConcurrentSessions(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	var wg sync.WaitGroup
	errs := make([]error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
			db, err := sql.Open("oracle", dsn)
			if err != nil {
				errs[idx] = err
				return
			}
			defer db.Close()

			var n int
			errs[idx] = db.QueryRowContext(context.Background(), fmt.Sprintf("SELECT %d FROM DUAL", idx)).Scan(&n)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		assert.NoError(t, err, "session %d failed", i)
	}
}

func TestIntegration_LargeResultSet(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), "SELECT LEVEL AS n FROM DUAL CONNECT BY LEVEL <= 10000")
	require.NoError(t, err)
	defer rows.Close()

	count := 0
	for rows.Next() {
		var n int
		rows.Scan(&n)
		count++
	}

	assert.Equal(t, 10000, count)
}

func TestIntegration_MultipleDataTypes(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.ExecContext(context.Background(), `CREATE TABLE test_types (
		num_col NUMBER(10,2), str_col VARCHAR2(100), date_col DATE,
		float_col BINARY_FLOAT, double_col BINARY_DOUBLE, char_col CHAR(10)
	)`)
	require.NoError(t, err)

	_, err = db.ExecContext(context.Background(), `INSERT INTO test_types VALUES (
		42.50, 'hello', DATE '2024-03-15', 3.14, 2.718281828, 'fixed'
	)`)
	require.NoError(t, err)

	rows, err := db.QueryContext(context.Background(), "SELECT * FROM test_types")
	require.NoError(t, err)
	defer rows.Close()

	require.True(t, rows.Next())

	cols := make([]interface{}, 6)
	ptrs := make([]interface{}, 6)
	for i := range cols {
		ptrs[i] = &cols[i]
	}
	require.NoError(t, rows.Scan(ptrs...))

	t.Logf("Results: %v", cols)
}

func TestIntegration_VersionDetection(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/XEPDB1", host, port)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	defer db.Close()

	var banner string
	err = db.QueryRowContext(context.Background(), "SELECT banner FROM v$version WHERE ROWNUM = 1").Scan(&banner)
	require.NoError(t, err)
	t.Logf("Oracle version: %s", banner)

	image := oracleTestImage()
	switch {
	case strings.Contains(image, "oracle-xe:18"):
		assert.Contains(t, strings.ToLower(banner), "18")
	case strings.Contains(image, "oracle-free:23"):
		assert.Contains(t, banner, "23")
	case strings.Contains(image, "enterprise:19"):
		assert.Contains(t, banner, "19c")
	}
}

// Silence unused import warning for json
var _ = json.Marshal

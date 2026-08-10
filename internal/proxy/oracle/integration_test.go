//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/sijms/go-ora/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/mcp"
	"github.com/fclairamb/dbbat/internal/store"
)

const defaultOracleImage = "gvenzl/oracle-xe:18.4.0-slim"

func oracleTestImage() string {
	if img := os.Getenv("ORACLE_TEST_IMAGE"); img != "" {
		return img
	}

	return defaultOracleImage
}

// oracleTestService returns the PDB service name exposed by the test image:
// gvenzl's XE images serve XEPDB1, the Free (23ai) images FREEPDB1, and the
// enterprise image is started below with ORACLE_PDB=ORCLPDB1.
// ORACLE_TEST_SERVICE overrides the guess.
func oracleTestService() string {
	if svc := os.Getenv("ORACLE_TEST_SERVICE"); svc != "" {
		return svc
	}

	image := oracleTestImage()

	switch {
	case strings.Contains(image, "enterprise"):
		return "ORCLPDB1"
	case strings.Contains(image, "oracle-free"):
		return "FREEPDB1"
	default:
		return "XEPDB1"
	}
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

	return container, host, int(port.Num())
}

// buildTNSConnect constructs a TNS Connect packet payload for the given service
// name. It used to live in upstream_auth.go, but upstream connection setup now
// goes through go-ora, so the only remaining user is this raw-socket test.
//
// The payload is a 50-byte Connect header followed by the connect descriptor,
// so the descriptor starts at offset 58 of the packet (8-byte TNS header + 50)
// — the canonical value Oracle listeners expect in the data-offset field.
// This is a simplified version — real Connect packets carry many more fields.
//
// The announced TNS version is deliberately 313 (12c-era): from 315 onwards
// Oracle switches to the extended Connect format (4-byte lengths, connect data
// sent after the announced packet length — see readExtendedConnectData), which
// this builder does not emit. Announcing 315+ here makes a 23ai listener drop
// the connection outright; 313 and below get a normal Accept.
func buildTNSConnect(serviceName string) []byte {
	descriptor := fmt.Sprintf(
		"(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=%s)"+
			"(CID=(PROGRAM=dbbat)(HOST=dbbat-test)(USER=dbbat))))",
		serviceName,
	)
	descriptorBytes := []byte(descriptor)
	headerLen := 50

	header := make([]byte, headerLen)
	// Version (2 bytes) — TNS 313; see the note above about 315+
	binary.BigEndian.PutUint16(header[0:2], 313)
	// Compatible version (2 bytes)
	binary.BigEndian.PutUint16(header[2:4], 300)
	// Service options (2 bytes)
	binary.BigEndian.PutUint16(header[4:6], 0)
	// SDU size (2 bytes) — 8192
	binary.BigEndian.PutUint16(header[6:8], 8192)
	// TDU size (2 bytes) — 65535
	binary.BigEndian.PutUint16(header[8:10], 65535)
	// Protocol characteristics (2 bytes)
	binary.BigEndian.PutUint16(header[10:12], 0x8001)
	// Max packets before ACK (2 bytes)
	binary.BigEndian.PutUint16(header[12:14], 0)
	// Byte order/endianness (2 bytes)
	binary.BigEndian.PutUint16(header[14:16], 1)
	// Data length (2 bytes) — connect descriptor length
	binary.BigEndian.PutUint16(header[16:18], uint16(len(descriptorBytes)))
	// Data offset (2 bytes) — offset from start of the TNS packet to the descriptor
	binary.BigEndian.PutUint16(header[18:20], uint16(tnsHeaderSize+headerLen))
	// Max receivable connect data (4 bytes)
	binary.BigEndian.PutUint32(header[20:24], 0)
	// Connect flags 0 and 1
	header[24] = 0x41
	header[25] = 0x41
	// Remaining bytes are zero (trace info, padding, etc.)

	result := make([]byte, 0, len(header)+len(descriptorBytes))
	result = append(result, header...)
	result = append(result, descriptorBytes...)

	return result
}

// TestIntegration_OracleContainer verifies we can start and connect to Oracle directly.
func TestIntegration_OracleContainer(t *testing.T) {
	container, host, port := startOracleContainer(t)
	defer func() { _ = container.Terminate(context.Background()) }()

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/%s", host, port, oracleTestService())
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

	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	require.NoError(t, err)
	defer conn.Close()

	connectPayload := buildTNSConnect(oracleTestService())
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

	// Server credentials are AES-256-GCM encrypted at rest, so both the store
	// and the proxy need the same 32-byte key.
	encryptionKey := []byte("0123456789012345678901234567890X")

	service := oracleTestService()
	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:              service,
		Host:              oracleHost,
		Port:              oraclePort,
		DatabaseName:      service,
		OracleServiceName: &service,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = createGrantWithControls(ctx, t, dataStore, user.UID, db.UID, []string{})
	require.NoError(t, err)

	queryStorage := config.QueryStorageConfig{
		StoreResults:   true,
		MaxResultRows:  100,
		MaxResultBytes: 1048576,
	}

	proxy := NewServer(dataStore, encryptionKey, nil, queryStorage, config.DumpConfig{}, slog.Default())
	go func() { _ = proxy.Start(":0") }()
	defer func() { _ = proxy.Shutdown(ctx) }()

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 2*time.Second, 50*time.Millisecond)
	t.Logf("Oracle proxy listening at %s", proxy.Addr())

	proxyConn, err := net.Dial("tcp", proxy.Addr().String())
	require.NoError(t, err)
	defer proxyConn.Close()

	connectPayload := buildTNSConnect(oracleTestService())
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

			dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/%s", host, port, oracleTestService())
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

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/%s", host, port, oracleTestService())
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

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/%s", host, port, oracleTestService())
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

	dsn := fmt.Sprintf("oracle://system:oracle@%s:%d/%s", host, port, oracleTestService())
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

// TestIntegration_MCPExecutesThroughTheProxy is the Oracle half of the MCP
// phase-2 spec: an agent's statement must reach the database by dialing dbbat's
// own Oracle listener as the API key's owner, never by an internal path.
//
// It therefore asserts the two things that would break if the loopback client
// were wired wrong — rows come back, and the statement shows up in the ordinary
// query history attributed to the key's user — rather than re-testing the
// enforcement the rest of this suite already covers.
//
// Note for whoever runs this: every containered Oracle test in this package
// currently dies at startup with ORA-00442 on an Apple Silicon workstation
// (XE refuses a second single-instance start under emulation). This one is no
// different; it is meant for the CI job that runs `make test-e2e-oracle`.
func TestIntegration_MCPExecutesThroughTheProxy(t *testing.T) {
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

	require.NoError(t, dataStore.Migrate(ctx))

	// The proxy matches the username on the wire against the API key's owner,
	// and the upstream credentials come from the server row — so the dbbat user
	// is deliberately not the Oracle one.
	//
	// Lowercase on purpose: Oracle clients uppercase the username on the wire and
	// the proxy lowercases it again before looking the dbbat user up
	// (session.authenticateClient), so a dbbat user stored as "SYSTEM" can never
	// be found. This fixture used to create exactly that and failed the whole
	// test at the first Execute with "user not found: SYSTEM".
	user, err := dataStore.CreateUser(ctx, "agent", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash", []string{"connector"})
	require.NoError(t, err)

	encryptionKey := []byte("0123456789012345678901234567890X")

	service := oracleTestService()
	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:              service,
		Host:              oracleHost,
		Port:              oraclePort,
		DatabaseName:      service,
		OracleServiceName: &service,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = createGrantWithControls(ctx, t, dataStore, user.UID, db.UID, []string{})
	require.NoError(t, err)

	_, plainKey, err := dataStore.CreateAPIKey(ctx, user.UID, "agent-key", nil, encryptionKey)
	require.NoError(t, err)

	proxy := NewServer(dataStore, encryptionKey, nil, config.QueryStorageConfig{}, config.DumpConfig{}, slog.Default())
	go func() { _ = proxy.Start("127.0.0.1:0") }()

	defer func() { _ = proxy.Shutdown(ctx) }()

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 5*time.Second, 50*time.Millisecond)

	executor := mcp.NewLoopbackExecutor(mcp.LoopbackListeners{Oracle: proxy.Addr().String()})

	result, err := executor.Execute(ctx, mcp.ExecRequest{
		Protocol: store.ProtocolOracle,
		// The dbbat entry's name, not the upstream SERVICE_NAME: the session
		// resolver does an exact GetServerByName lookup on it.
		Database:         service,
		UpstreamDatabase: service,
		Username:         user.Username,
		APIKey:           plainKey,
		SQL:              "SELECT 42 AS answer FROM dual",
		MaxRows:          10,
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.False(t, result.Truncated)
	assert.Equal(t, []string{"ANSWER"}, result.Columns)

	// The row cap is applied while reading, not after.
	capped, err := executor.Execute(ctx, mcp.ExecRequest{
		Protocol:         store.ProtocolOracle,
		Database:         service,
		UpstreamDatabase: service,
		Username:         user.Username,
		APIKey:           plainKey,
		SQL:              "SELECT LEVEL AS n FROM dual CONNECT BY LEVEL <= 100",
		MaxRows:          5,
	})
	require.NoError(t, err)
	assert.Len(t, capped.Rows, 5)
	assert.True(t, capped.Truncated, "a result set larger than max_rows must be reported as truncated")

	// Nothing about MCP is a special path, so the statements land in the
	// ordinary query history.
	require.Eventually(t, func() bool {
		queries, listErr := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
		if listErr != nil {
			return false
		}

		for i := range queries {
			if strings.Contains(queries[i].SQLText, "42 AS answer") {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond, "an agent's statement must be logged like any other")
}

// createGrantWithControls issues a grant carrying the given controls. Every
// grant is an *instance of a definition* and carries no shape of its own, so
// the definition has to exist first and be named by uid — an inline
// GrantDefinition on the grant is not enough (CreateGrant answers
// ErrGrantDefinitionRequired), which is what used to fail this suite at setup.
func createGrantWithControls(
	ctx context.Context,
	t *testing.T,
	dataStore *store.Store,
	userUID, databaseUID uuid.UUID,
	controls []string,
) (*store.Grant, error) {
	t.Helper()

	name := fmt.Sprintf("itest-%s", uuid.NewString()[:8])

	def, err := dataStore.CreateGrantDefinition(ctx, &store.GrantDefinition{
		Name:            name,
		Slug:            name,
		DurationSeconds: int64(24 * time.Hour / time.Second),
		Controls:        controls,
		CreatedBy:       userUID,
	})
	require.NoError(t, err)

	return dataStore.CreateGrant(ctx, &store.Grant{
		UserID:            userUID,
		DatabaseID:        databaseUID,
		GrantedBy:         userUID,
		GrantDefinitionID: def.UID,
		Definition:        def,
		StartsAt:          time.Now().Add(-time.Hour),
		ExpiresAt:         time.Now().Add(24 * time.Hour),
	})
}

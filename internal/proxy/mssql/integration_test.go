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

	"github.com/google/uuid"
	_ "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
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

	srv, err := NewServer(nil, nil, config.QueryStorageConfig{}, config.DumpConfig{}, nil, cfg, slog.New(slog.DiscardHandler))
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

// TestProxyHandshakeAtTLS13 is the regression guard for the opt-in ceiling.
//
// TLS 1.3 is the version the encapsulation makes awkward: the client's
// handshake ends on a *write*, so the framed→raw switch no longer lands on the
// same byte for both peers, and the classic failure is a hang rather than an
// error. Everything here is therefore bounded — a short context and a short
// driver-side connection timeout — so a regression fails in seconds instead of
// parking the suite.
//
// go-mssqldb is the only driver this can drive; the Microsoft ODBC and JDBC
// drivers are not available to this suite, and docs/mssql.md says so.
func TestProxyHandshakeAtTLS13(t *testing.T) {
	tests := []struct {
		name     string
		encrypt  string
		proxyMax string
		wantLogn bool
	}{
		// The whole session inside TLS 1.3.
		{
			name: "encryption on", encrypt: "true",
			proxyMax: config.MSSQLTLSMaxVersion13, wantLogn: true,
		},
		// ENCRYPT_OFF still runs a full handshake, then reverts to cleartext —
		// the revert is the step most likely to break on a version change.
		{
			name: "login-packet-only encryption", encrypt: "false",
			proxyMax: config.MSSQLTLSMaxVersion13, wantLogn: true,
		},
		// And the default really is a ceiling: a client that will not go below
		// 1.3 must be refused, not quietly downgraded.
		{
			name: "default ceiling refuses a 1.3-only client", encrypt: "true",
			proxyMax: "", wantLogn: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately tight: the failure mode under test is a hang.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			addr := startProxy(t, config.MSSQLConfig{TLSMaxVersion: tc.proxyMax})

			// tlsmin=1.3 puts the *client's* floor at 1.3, so the handshake can
			// only succeed if the proxy actually served 1.3. Without it Go
			// would pick a version and the assertion would prove nothing.
			db, err := sql.Open("sqlserver", dsn(addr, tc.encrypt)+"&tlsmin=1.3")
			require.NoError(t, err)

			t.Cleanup(func() { _ = db.Close() })

			err = db.PingContext(ctx)
			require.Error(t, err, "a proxy without a store authenticates nobody")

			if !tc.wantLogn {
				assert.NotContains(t, err.Error(), "Login failed for user",
					"a 1.2 ceiling must refuse the handshake, well before any login")

				return
			}

			assert.Contains(t, err.Error(), "Login failed for user",
				"reaching the login refusal means the TLS 1.3 handshake completed and "+
					"both ends switched from PRELOGIN framing to raw records on the same byte")
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

	return startProxyWithOptions(t, cfg, dataStore, encryptionKey, config.QueryStorageConfig{}, nil)
}

// startProxyWithOptions is startProxyWithStore plus stage 3's dependencies:
// result-row storage and the approval-hold collaborators.
func startProxyWithOptions(
	t *testing.T,
	cfg config.MSSQLConfig,
	dataStore *store.Store,
	encryptionKey []byte,
	queryStorage config.QueryStorageConfig,
	approvalDeps *shared.ApprovalDeps,
) string {
	t.Helper()

	srv, err := NewServer(dataStore, encryptionKey, queryStorage, config.DumpConfig{}, nil, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	if approvalDeps != nil {
		srv.SetApprovalDeps(*approvalDeps)
	}

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

	return seedE2EWith(ctx, t, upstreamAddr, sslMode, nil, nil)
}

// seedE2EWith is seedE2E with the grant definition's controls and approval
// patterns spelled out, which is what the enforcement tests below need.
func seedE2EWith(
	ctx context.Context,
	t *testing.T,
	upstreamAddr, sslMode string,
	controls, approvalPatterns []string,
) (*store.Store, []byte) {
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

	grantAccess(t, dataStore, user.UID, database.UID, controls, approvalPatterns)

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

// e2eTable is the throwaway table the enforcement tests below write to.
const e2eTable = "dbbat_stage3"

// createE2ETable builds the fixture table through a *direct* connection to the
// container, so the proxy's own controls never get in the way of setting up.
func createE2ETable(ctx context.Context, t *testing.T, upstreamAddr string) {
	t.Helper()

	db, err := sql.Open("sqlserver", dsn(upstreamAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	_, err = db.ExecContext(ctx, fmt.Sprintf(
		"IF OBJECT_ID('%s') IS NOT NULL DROP TABLE %s;"+
			"CREATE TABLE %s (id int NOT NULL, label nvarchar(100) NULL)", e2eTable, e2eTable, e2eTable))
	require.NoError(t, err)

	for i := 1; i <= 5; i++ {
		_, err = db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (id, label) VALUES (@p1, @p2)", e2eTable),
			i, fmt.Sprintf("étiquette %d", i))
		require.NoError(t, err)
	}
}

// TestProxyEnforcesReadOnlyOnBothStatementPaths is the regression test for the
// owner's binding decision on RPC.
//
// go-mssqldb sends a parameterless statement as a plain SQLBatch and a
// parameterised one as sp_executesql, so the two halves of this test really do
// exercise the two code paths against a real server. If RPC were log-only, the
// second half would succeed and read_only would be bypassable by anyone.
func TestProxyEnforcesReadOnlyOnBothStatementPaths(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	dataStore, encryptionKey := seedE2EWith(ctx, t, upstreamAddr, "disable",
		[]string{store.ControlReadOnly}, nil)

	proxyAddr := startProxyWithStore(t, config.MSSQLConfig{}, dataStore, encryptionKey)

	db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	// A read still works, so the refusals below are about the write and not
	// about the session being broken.
	var before int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&before))
	assert.Equal(t, 5, before)

	// Path 1: a plain SQLBatch.
	_, err = db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s", e2eTable))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")

	// Path 2: the same write wrapped in sp_executesql by the driver's
	// parameter handling.
	_, err = db.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE id = @p1", e2eTable), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only",
		"sp_executesql must be enforced, not merely logged")

	// Nothing was deleted by either path.
	var after int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&after))
	assert.Equal(t, 5, after, "a blocked write must never reach the upstream")

	// And both refusals are in the query history.
	queries, err := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
	require.NoError(t, err)

	blocked := 0

	for _, query := range queries {
		if query.Error != nil && strings.Contains(*query.Error, "read-only") {
			blocked++
		}
	}

	assert.Equal(t, 2, blocked, "both refusals must be logged")
}

// TestProxyAccountsForResults covers the response side against a real server:
// the row count the upstream reported lands on the query row, and the rows
// themselves are captured.
func TestProxyAccountsForResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	dataStore, encryptionKey := seedE2E(ctx, t, upstreamAddr, "disable")

	proxyAddr := startProxyWithOptions(t, config.MSSQLConfig{}, dataStore, encryptionKey,
		config.QueryStorageConfig{StoreResults: true, MaxResultRows: 100}, nil)

	db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	const selectSQL = "SELECT id, label FROM " + e2eTable + " ORDER BY id"

	rows, err := db.QueryContext(ctx, selectSQL)
	require.NoError(t, err)

	seen := 0

	for rows.Next() {
		var (
			id    int
			label string
		)

		require.NoError(t, rows.Scan(&id, &label))
		assert.Equal(t, fmt.Sprintf("étiquette %d", id), label)

		seen++
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 5, seen)

	var logged store.Query

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
		if err != nil {
			return false
		}

		for _, query := range queries {
			if query.SQLText == selectSQL && query.RowsAffected != nil {
				logged = query

				return true
			}
		}

		return false
	}, 30*time.Second, 100*time.Millisecond)

	require.NotNil(t, logged.RowsAffected)
	assert.Equal(t, int64(5), *logged.RowsAffected,
		"the DONE token's row count must land on the query row")
	assert.Nil(t, logged.Error)

	captured, err := dataStore.GetQueryRows(ctx, logged.UID, "", 20)
	require.NoError(t, err)
	require.Len(t, captured.Rows, 5, "the captured result rows must be stored")
	assert.JSONEq(t, `[1,"étiquette 1"]`, string(captured.Rows[0].RowData))

	// A write reports its affected rows through the same DONE token.
	result, err := db.ExecContext(ctx,
		fmt.Sprintf("UPDATE %s SET label = N'modifié' WHERE id <= 2", e2eTable))
	require.NoError(t, err)

	affected, err := result.RowsAffected()
	require.NoError(t, err)
	assert.Equal(t, int64(2), affected)
}

// TestProxyHoldsAStatementForApproval drives a real driver into an approval
// hold. The behavior that matters is what the client does *while* parked: it
// must simply wait, with no timeout and no torn-down connection, and then get
// its real answer once a human approves.
func TestProxyHoldsAStatementForApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	// `^DELETE` rather than `(?i)^DELETE`: a pattern starting with `(` does not
	// survive the store round-trip today and comes back as a bare `(?i)`, which
	// matches every statement — including the warm-up SELECT below. See
	// specs/todos/2026-08-07-approval-patterns-starting-with-a-paren.md.
	dataStore, encryptionKey := seedE2EWith(ctx, t, upstreamAddr, "disable",
		nil, []string{`^DELETE`})

	registry := approval.NewRegistry()

	proxyAddr := startProxyWithOptions(t, config.MSSQLConfig{}, dataStore, encryptionKey,
		config.QueryStorageConfig{}, &shared.ApprovalDeps{
			Enabled:      true,
			Store:        dataStore,
			Registry:     registry,
			Logger:       slog.New(slog.DiscardHandler),
			PollInterval: 200 * time.Millisecond,
		})

	db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	// A statement that does not match the pattern runs normally, which also
	// warms the connection pool so the hold below is not racing a handshake.
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&count))
	require.Equal(t, 5, count)

	type execOutcome struct {
		affected int64
		err      error
	}

	outcomes := make(chan execOutcome, 1)

	go func() {
		result, err := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = 3", e2eTable))
		if err != nil {
			outcomes <- execOutcome{err: err}

			return
		}

		affected, err := result.RowsAffected()
		outcomes <- execOutcome{affected: affected, err: err}
	}()

	// The statement is persisted as pending while it hangs.
	var pendingUID uuid.UUID

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
		if err != nil {
			return false
		}

		for _, query := range queries {
			if query.ApprovalStatus != nil && *query.ApprovalStatus == store.ApprovalPending {
				pendingUID = query.UID

				return true
			}
		}

		return false
	}, 60*time.Second, 200*time.Millisecond)

	// Still parked a good while later: a hold has no timeout, and the client
	// has neither timed out nor been disconnected.
	select {
	case outcome := <-outcomes:
		t.Fatalf("the client was answered while parked: %+v", outcome)
	case <-time.After(5 * time.Second):
	}

	stillThere := 0
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = 3", e2eTable)).Scan(&stillThere))
	assert.Equal(t, 1, stillThere, "a parked statement must not reach the upstream")

	// A human approves, and the statement runs for real.
	users, err := dataStore.ListUsers(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, users)

	approver := users[0].UID
	require.True(t, registry.Resolve(approval.Decision{
		QueryUID: pendingUID,
		Status:   store.ApprovalApproved,
		By:       &approver,
		ByName:   "an approver",
		At:       time.Now(),
	}))

	select {
	case outcome := <-outcomes:
		require.NoError(t, outcome.err)
		assert.Equal(t, int64(1), outcome.affected,
			"the released statement must run and report its own row count")
	case <-time.After(60 * time.Second):
		t.Fatal("the released statement never completed")
	}

	gone := 1
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = 3", e2eTable)).Scan(&gone))
	assert.Equal(t, 0, gone)
}

// TestProxyReleasesAHoldWhenTheClientCancels drives a real driver into an
// approval hold and then cancels it, which is what a query timeout or a Ctrl-C
// does. The cancel arrives as a TDS ATTENTION on the held session's own socket
// — unlike every other protocol, where it comes in on a second connection —
// and it must end the hold there and then rather than after the client gives up
// and drops the socket.
func TestProxyReleasesAHoldWhenTheClientCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	// See the note on the pattern in TestProxyHoldsAStatementForApproval.
	dataStore, encryptionKey := seedE2EWith(ctx, t, upstreamAddr, "disable",
		nil, []string{`^DELETE`})

	proxyAddr := startProxyWithOptions(t, config.MSSQLConfig{}, dataStore, encryptionKey,
		config.QueryStorageConfig{}, &shared.ApprovalDeps{
			Enabled:      true,
			Store:        dataStore,
			Registry:     approval.NewRegistry(),
			Logger:       slog.New(slog.DiscardHandler),
			PollInterval: 200 * time.Millisecond,
		})

	db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	// Warms the pool, and proves the pattern does not match everything.
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&count))
	require.Equal(t, 5, count)

	stmtCtx, cancelStatement := context.WithCancel(ctx)
	defer cancelStatement()

	errs := make(chan error, 1)

	go func() {
		_, execErr := db.ExecContext(stmtCtx, fmt.Sprintf("DELETE FROM %s WHERE id = 3", e2eTable))
		errs <- execErr
	}()

	var pendingUID uuid.UUID

	require.Eventually(t, func() bool {
		queries, err := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
		if err != nil {
			return false
		}

		for _, query := range queries {
			if query.ApprovalStatus != nil && *query.ApprovalStatus == store.ApprovalPending {
				pendingUID = query.UID

				return true
			}
		}

		return false
	}, 60*time.Second, 200*time.Millisecond)

	// The client gives up: go-mssqldb sends an ATTENTION and waits for the
	// cancel acknowledgement before it hands the error back.
	cancelStatement()

	select {
	case execErr := <-errs:
		require.ErrorIs(t, execErr, context.Canceled,
			"the cancel must come back as a canceled context, not a broken connection")
	case <-time.After(30 * time.Second):
		t.Fatal("the client was still parked 30s after it canceled")
	}

	// The hold is settled rather than left pending forever, and it says who
	// ended it.
	require.Eventually(t, func() bool {
		query, err := dataStore.GetQuery(ctx, pendingUID)
		if err != nil || query.ApprovalStatus == nil {
			return false
		}

		return *query.ApprovalStatus == store.ApprovalAbandoned
	}, 30*time.Second, 200*time.Millisecond)

	query, err := dataStore.GetQuery(ctx, pendingUID)
	require.NoError(t, err)
	require.NotNil(t, query.ResolutionReason)
	assert.Equal(t, attentionCancelReason, *query.ResolutionReason)

	// The security property: a cancel cancels, it does not release. The row is
	// still there, so the DELETE never reached the upstream.
	stillThere := 0
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = 3", e2eTable)).Scan(&stillThere))
	assert.Equal(t, 1, stillThere, "a canceled statement must never reach the upstream")

	// And the connection survived it, which is the difference between an
	// acknowledged cancel and a dropped socket.
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&count))
	assert.Equal(t, 5, count)
}

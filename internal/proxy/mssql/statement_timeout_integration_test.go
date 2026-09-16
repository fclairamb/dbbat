//go:build integration

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// seedE2EWithGrantOptions is seedE2EWith with the grant definition's options
// spelled out — the statement-timeout suite is the only caller that needs one
// that is neither a control nor an approval pattern.
func seedE2EWithGrantOptions(
	ctx context.Context,
	t *testing.T,
	upstreamAddr, sslMode string,
	opts ...testsupport.GrantOption,
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

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, database.UID, nil, opts...)
	require.NoError(t, err)

	return dataStore, encryptionKey
}

// TestProxyTerminatesAnOverlongStatement is the whole feature on a protocol
// that has no server-side statement timeout at all: the watchdog is both
// layers, the TDS ATTENTION is the only cancel, and the session ends.
func TestProxyTerminatesAnOverlongStatement(t *testing.T) {
	ctx := context.Background()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	dataStore, encryptionKey := seedE2EWithGrantOptions(ctx, t, upstreamAddr, "disable",
		testsupport.WithStatementTimeout(1))

	proxyAddr := startProxyWithStore(t, config.MSSQLConfig{}, dataStore, encryptionKey)

	db, err := sql.Open("sqlserver", proxyDSN(proxyAddr, "disable"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	// Warm the pool so the measurement below is the statement's, not a
	// handshake's.
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&count))

	started := time.Now()

	_, err = db.ExecContext(ctx, "WAITFOR DELAY '00:00:30'")
	require.Error(t, err, "a 30s WAITFOR must not survive a 1s statement limit")

	elapsed := time.Since(started)
	assert.Less(t, elapsed, 15*time.Second,
		"the watchdog took %s to fire on a 1s limit (expected ~3s)", elapsed)

	requireTerminationRecorded(ctx, t, dataStore)
}

// requireTerminationRecorded asserts the full paper trail a dbbat-initiated
// statement-timeout teardown leaves: the reason on the connection row, the
// statement completed with an error naming the limit, and the chained audit
// entry that survives the row being deleted.
func requireTerminationRecorded(ctx context.Context, t *testing.T, dataStore *store.Store) {
	t.Helper()

	var terminated *store.Connection

	require.Eventually(t, func() bool {
		conns, err := dataStore.ListConnections(ctx, store.ConnectionFilter{})
		if err != nil {
			return false
		}

		for i := range conns {
			if conns[i].TerminationReason != nil &&
				*conns[i].TerminationReason == store.TerminationStatementTimeout {
				terminated = &conns[i]

				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"no connection row carries termination_reason=statement_timeout")

	require.NotNil(t, terminated.DisconnectedAt, "a terminated connection must also be closed")

	require.Eventually(t, func() bool {
		eventType := store.AuditEventConnectionTerminated

		events, err := dataStore.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if err != nil {
			return false
		}

		for i := range events {
			if strings.Contains(string(events[i].Details), terminated.UID.String()) {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond,
		"no connection.terminated audit entry was written")
}

// TestApprovalHoldTimeDoesNotCountAgainstTheStatementLimit is the rule the
// whole design hinges on, and the only place it can be observed end to end: a
// statement parked on a human for **longer than the limit** must still get its
// full allowance once released, because during the hold nothing was sent
// anywhere and the upstream was doing no work.
//
// Without this, a 30s limit with a 40s approval turnaround would make every
// gated statement fail — which would make the two features mutually exclusive.
func TestApprovalHoldTimeDoesNotCountAgainstTheStatementLimit(t *testing.T) {
	ctx := context.Background()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	createE2ETable(ctx, t, upstreamAddr)

	// A 5s statement limit, and a pattern that parks every DELETE. See the
	// approval suite for why the pattern does not start with `(?i)`.
	dataStore, encryptionKey := seedE2EWithGrantOptions(ctx, t, upstreamAddr, "disable",
		testsupport.WithApprovalPatterns(`^DELETE`),
		testsupport.WithStatementTimeout(5))

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

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s", e2eTable)).Scan(&count))

	outcomes := make(chan error, 1)

	go func() {
		_, execErr := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = 3", e2eTable))
		outcomes <- execErr
	}()

	var pendingUID uuid.UUID

	require.Eventually(t, func() bool {
		queries, qErr := dataStore.ListQueries(ctx, store.QueryFilter{Limit: 50})
		if qErr != nil {
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

	// Park it well past the 5s limit — plus the 2s watchdog grace — and check
	// the session is still alive rather than having been torn down for a
	// statement that never ran.
	time.Sleep(12 * time.Second)

	select {
	case outcome := <-outcomes:
		t.Fatalf("the held statement was answered while parked: %v", outcome)
	default:
	}

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
		require.NoError(t, outcome,
			"a statement held longer than the limit must still run once approved")
	case <-time.After(60 * time.Second):
		t.Fatal("the released statement never completed")
	}

	// And nothing was recorded as a timeout: the hold is not a violation.
	conns, err := dataStore.ListConnections(ctx, store.ConnectionFilter{})
	require.NoError(t, err)

	for i := range conns {
		assert.Nil(t, conns[i].TerminationReason,
			"an approved hold must not leave a termination reason")
	}
}

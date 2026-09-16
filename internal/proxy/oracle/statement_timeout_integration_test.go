//go:build integration

package oracle

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/mcp"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestIntegration_StatementTimeout_TerminatesTheSession is the whole feature on
// the protocol with the least to work with: Oracle has no in-band statement
// time limit, so there is no layer 1 at all and dbbat's watchdog is the entire
// mechanism.
//
// What this proves is that the **session ends** on time and the termination is
// recorded. It deliberately does *not* claim the upstream stopped working: the
// break/reset marker exchange dbbat sends first is documented as unverified
// (see docs/oracle.md), and proving the server abandoned the call would need to
// watch v$session after the teardown.
//
// The statement is driven through the MCP loopback executor rather than a raw
// TNS client for the same reason TestIntegration_MCPExecutesThroughTheProxy
// does: it is a real client speaking the real protocol, without this suite
// having to hand-roll a TTC conversation.
func TestIntegration_StatementTimeout_TerminatesTheSession(t *testing.T) {
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

	user, err := dataStore.CreateUser(ctx, "agent", "$argon2id$v=19$m=4096,t=3,p=1$salt$hash",
		[]string{"connector"})
	require.NoError(t, err)

	encryptionKey := []byte("0123456789012345678901234567890X")

	service := oracleTestService()

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:              "oracle_e2e",
		Host:              oracleHost,
		Port:              oraclePort,
		DatabaseName:      service,
		OracleServiceName: &service,
		Username:          "system",
		Password:          "oracle",
		Protocol:          store.ProtocolOracle,
	}, encryptionKey)
	require.NoError(t, err)

	_, err = testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, db.UID, nil,
		testsupport.WithStatementTimeout(2))
	require.NoError(t, err)

	_, plainKey, err := dataStore.CreateAPIKey(ctx, user.UID, "agent-key", nil, encryptionKey)
	require.NoError(t, err)

	proxy := NewServer(dataStore, encryptionKey, nil, config.QueryStorageConfig{},
		config.DumpConfig{}, slog.Default())
	go func() { _ = proxy.Start("127.0.0.1:0") }()

	defer func() { _ = proxy.Shutdown(ctx) }()

	require.Eventually(t, func() bool { return proxy.Addr() != nil }, 5*time.Second, 50*time.Millisecond)

	executor := mcp.NewLoopbackExecutor(mcp.LoopbackListeners{Oracle: proxy.Addr().String()})

	started := time.Now()

	_, err = executor.Execute(ctx, mcp.ExecRequest{
		Protocol:         store.ProtocolOracle,
		Database:         db.Name,
		UpstreamDatabase: service,
		Username:         user.Username,
		APIKey:           plainKey,
		SQL:              "BEGIN dbms_session.sleep(60); END;",
		// The executor never enforces this; it only needs the number to name
		// the limit back to the agent.
		StatementTimeout: 2 * time.Second,
		MaxRows:          10,
	})
	require.Error(t, err, "a 60s sleep must not survive a 2s statement limit")

	elapsed := time.Since(started)
	assert.Less(t, elapsed, 30*time.Second,
		"the watchdog took %s to fire on a 2s limit (expected ~4s)", elapsed)

	// The paper trail. The teardown writes it asynchronously, so poll.
	var terminated *store.Connection

	require.Eventually(t, func() bool {
		conns, listErr := dataStore.ListConnections(ctx, store.ConnectionFilter{})
		if listErr != nil {
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

		events, aErr := dataStore.ListAuditEvents(ctx, store.AuditFilter{EventType: &eventType})
		if aErr != nil {
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

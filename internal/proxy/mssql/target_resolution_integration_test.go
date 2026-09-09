//go:build integration

package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// resolutionRealDatabase is the upstream database the extra entries point at.
// `tempdb` always exists on a fresh SQL Server and is not the name of any dbbat
// entry the fixture creates, which is what makes the grant-scoped rung
// reachable — an exact entry-name match always wins ahead of it.
const resolutionRealDatabase = "tempdb"

// resolutionDSN builds a go-mssqldb connection string aimed at the proxy with
// an explicit login name and database — the two fields the ladder reads.
func resolutionDSN(addr, username, database string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf(
		"sqlserver://%s:%s@%s:%s?database=%s&encrypt=disable&TrustServerCertificate=true"+
			"&connection+timeout=30",
		url.QueryEscape(username), url.QueryEscape(e2eDBBatPassword),
		host, port, url.QueryEscape(database))
}

// registerResolutionTwin adds a SQL Server entry pointing at
// resolutionRealDatabase under a name of its own, optionally granted to the
// fixture user.
func registerResolutionTwin(
	ctx context.Context,
	t *testing.T,
	dataStore *store.Store,
	encryptionKey []byte,
	upstreamAddr, name string,
	granted bool,
) *store.Server {
	t.Helper()

	host, portText, err := net.SplitHostPort(upstreamAddr)
	require.NoError(t, err)

	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	db, err := dataStore.CreateServer(ctx, &store.Server{
		Name:         name,
		Host:         host,
		Port:         port,
		DatabaseName: resolutionRealDatabase,
		Username:     "sa",
		Password:     saPassword,
		Protocol:     store.ProtocolMSSQL,
		SSLMode:      "disable",
	}, encryptionKey)
	require.NoError(t, err)

	if granted {
		user, uerr := dataStore.GetUserByUsername(ctx, e2eDBBatUser)
		require.NoError(t, uerr)

		_, gerr := testsupport.CreateGrantWithControls(ctx, t, dataStore, user.UID, db.UID, nil)
		require.NoError(t, gerr)
	}

	return db
}

// TestIntegration_TargetResolution exercises the shared resolution ladder over
// real TDS: the dbbat entry named in the LOGIN7 database field (the pre-existing
// form), the entry selected from a `User Id=user#entry` login name with the real
// upstream database in Database, and the bare upstream name resolved against the
// caller's own grants.
func TestIntegration_TargetResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	upstreamAddr := startUpstreamSQLServer(ctx, t)
	dataStore, encryptionKey := seedE2E(ctx, t, upstreamAddr, "disable")
	proxyAddr := startProxyWithStore(t, config.MSSQLConfig{}, dataStore, encryptionKey)

	registerResolutionTwin(ctx, t, dataStore, encryptionKey, upstreamAddr, "tempdb_ro", true)

	openAs := func(t *testing.T, username, database string) *sql.DB {
		t.Helper()

		db, err := sql.Open("sqlserver", resolutionDSN(proxyAddr, username, database))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		return db
	}

	t.Run("entry name in the Database field still works", func(t *testing.T) {
		db := openAs(t, e2eDBBatUser, "tempdb_ro")

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current))
		assert.Equal(t, resolutionRealDatabase, current)
	})

	t.Run("user#entry with the real upstream database name", func(t *testing.T) {
		db := openAs(t, e2eDBBatUser+"#tempdb_ro", resolutionRealDatabase)

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current))
		assert.Equal(t, resolutionRealDatabase, current)
	})

	t.Run("reconnect to a database the entry does not expose is refused", func(t *testing.T) {
		db := openAs(t, e2eDBBatUser+"#tempdb_ro", "msdb")

		err := db.PingContext(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tempdb_ro")
		assert.Contains(t, err.Error(), "msdb")
	})

	t.Run("the bare upstream name resolves through the caller's single grant", func(t *testing.T) {
		db := openAs(t, e2eDBBatUser, resolutionRealDatabase)

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current))
		assert.Equal(t, resolutionRealDatabase, current)
	})

	t.Run("an entry the caller holds no grant on is invisible", func(t *testing.T) {
		registerResolutionTwin(ctx, t, dataStore, encryptionKey, upstreamAddr, "tempdb_nogrant", false)

		db := openAs(t, e2eDBBatUser, resolutionRealDatabase)

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&current))
		assert.Equal(t, resolutionRealDatabase, current)
	})

	t.Run("two granted twins make the bare name ambiguous", func(t *testing.T) {
		registerResolutionTwin(ctx, t, dataStore, encryptionKey, upstreamAddr, "tempdb_rw", true)

		ambiguous := openAs(t, e2eDBBatUser, resolutionRealDatabase)

		err := ambiguous.PingContext(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tempdb_ro")
		assert.Contains(t, err.Error(), "tempdb_rw")

		// …and the selector is how you get out of it.
		chosen := openAs(t, e2eDBBatUser+"#tempdb_rw", resolutionRealDatabase)
		require.NoError(t, chosen.PingContext(ctx))
	})
}

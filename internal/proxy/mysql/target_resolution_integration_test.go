//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// resolutionUpstreamDB is a second database created on the upstream server, so
// the suite has an upstream database name that is NOT also a dbbat entry name.
// An exact entry-name match always wins ahead of the grant-scoped rung, so
// without it that rung is unreachable.
const resolutionUpstreamDB = "analytics"

// dialAs opens a connection through the proxy under an explicit login name and
// database — the two fields this ladder is about.
func (f *fixture) dialAs(username, database string) *sql.DB {
	f.t.Helper()

	cfg := f.driverConfig()
	cfg.User = username
	cfg.DBName = database
	cfg.TLSConfig = "dbbat-skip-verify"
	cfg.AllowCleartextPasswords = true

	return f.openWithConfig(cfg)
}

// registerResolutionTwin adds a dbbat entry pointing at resolutionUpstreamDB
// under a name of its own — the `_ro` / `_rw` shape a real fleet has — and
// optionally grants it to the fixture user.
func (f *fixture) registerResolutionTwin(ctx context.Context, name string, granted bool) *store.Server {
	f.t.Helper()

	user, err := f.store.GetUserByUsername(ctx, fixtureUser)
	require.NoError(f.t, err)

	encryptionKey := make([]byte, 32)
	for i := range encryptionKey {
		encryptionKey[i] = byte(i + 1)
	}

	db, err := f.store.CreateServer(ctx, &store.Server{
		Name:         name,
		Host:         f.upstreamHost,
		Port:         f.upstreamPort,
		DatabaseName: resolutionUpstreamDB,
		Username:     "root",
		Password:     "rootpw",
		Protocol:     store.ProtocolMySQL,
		SSLMode:      "disable",
	}, encryptionKey)
	require.NoError(f.t, err)

	if granted {
		_, err = testsupport.CreateGrantWithControls(ctx, f.t, f.store, user.UID, db.UID, []string{})
		require.NoError(f.t, err)
	}

	return db
}

// TestIntegration_TargetResolution exercises the shared resolution ladder over
// the real MySQL wire protocol: the dbbat entry named in the database field (the
// pre-existing form), the entry selected from a `user#entry` login name with the
// real upstream database in the database field, and the bare upstream name
// resolved against the caller's own grants.
func TestIntegration_TargetResolution(t *testing.T) {
	ctx := context.Background()

	f := setupFixture(ctx, t, mysqlImage(), store.ProtocolMySQL)

	// A second database on the upstream, so `analytics` is a real database name
	// that no dbbat entry is called.
	bootstrap := f.dialTLS()
	_, err := bootstrap.ExecContext(ctx, "CREATE DATABASE "+resolutionUpstreamDB)
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())

	f.registerResolutionTwin(ctx, "analytics_ro", true)

	t.Run("entry name in the database field still works", func(t *testing.T) {
		db := f.dialAs(fixtureUser, "analytics_ro")
		defer db.Close()

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("user#entry with the real upstream database name", func(t *testing.T) {
		db := f.dialAs(fixtureUser+"#analytics_ro", resolutionUpstreamDB)
		defer db.Close()

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("reconnect to a database the entry does not expose is refused", func(t *testing.T) {
		db := f.dialAs(fixtureUser+"#analytics_ro", "mysql")
		defer db.Close()

		err := db.PingContext(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "analytics_ro")
		assert.Contains(t, err.Error(), "mysql")
	})

	t.Run("the bare upstream name resolves through the caller's single grant", func(t *testing.T) {
		db := f.dialAs(fixtureUser, resolutionUpstreamDB)
		defer db.Close()

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("an entry the caller holds no grant on is invisible", func(t *testing.T) {
		f.registerResolutionTwin(ctx, "analytics_nogrant", false)

		db := f.dialAs(fixtureUser, resolutionUpstreamDB)
		defer db.Close()

		var current string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("two granted twins make the bare name ambiguous", func(t *testing.T) {
		f.registerResolutionTwin(ctx, "analytics_rw", true)

		ambiguous := f.dialAs(fixtureUser, resolutionUpstreamDB)
		defer ambiguous.Close()

		err := ambiguous.PingContext(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "analytics_ro")
		assert.Contains(t, err.Error(), "analytics_rw")

		// …and the selector is how you get out of it.
		chosen := f.dialAs(fixtureUser+"#analytics_rw", resolutionUpstreamDB)
		defer chosen.Close()

		require.NoError(t, chosen.PingContext(ctx))
	})
}

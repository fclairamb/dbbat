//go:build integration

package postgresql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/testsupport"
	"github.com/fclairamb/dbbat/internal/store"
)

// resolutionUpstreamDB is a second database created on the upstream server, so
// the suite has an upstream name that is NOT also a dbbat entry name — the only
// way to reach the third resolution rung, since an exact entry-name match always
// wins ahead of it.
const resolutionUpstreamDB = "analytics"

// dsnFor builds a client DSN with an explicit username and database, which is
// exactly what these tests vary.
func (f *fixture) dsnFor(username, password, database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=require",
		username, password, f.proxyAddr, database)
}

// registerTwin adds a dbbat entry pointing at resolutionUpstreamDB under a name
// of its own — the `_ro` / `_rw` shape a real fleet has — and optionally grants
// it to the fixture user.
func (f *fixture) registerTwin(ctx context.Context, name string, granted bool) *store.Server {
	f.t.Helper()

	db, err := f.store.CreateServer(ctx, &store.Server{
		Name:         name,
		Host:         f.upstreamHost,
		Port:         f.upstreamPort,
		DatabaseName: resolutionUpstreamDB,
		Username:     upstreamUsr,
		Password:     upstreamPwd,
		Protocol:     store.ProtocolPostgreSQL,
		SSLMode:      "disable",
	}, f.encKey)
	require.NoError(f.t, err)

	if granted {
		_, err = testsupport.CreateGrantWithControls(ctx, f.t, f.store, f.user.UID, db.UID, []string{})
		require.NoError(f.t, err)
	}

	return db
}

// TestIntegration_TargetResolution exercises the resolution ladder end to end
// over a real wire protocol against a real PostgreSQL: what a DataGrip-shaped
// client actually sends (the real upstream database name in the database field,
// the dbbat entry selected from the username) and what it gets back when it
// reconnects to a database the entry does not expose.
func TestIntegration_TargetResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	f := setupFixture(ctx, t)

	// A second database on the upstream, so `analytics` is a real database name
	// that no dbbat entry is called.
	bootstrap := f.mustConnect(ctx, fixturePass)
	_, err := bootstrap.Exec(ctx, "CREATE DATABASE "+resolutionUpstreamDB)
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close(ctx))

	ro := f.registerTwin(ctx, "analytics_ro", true)

	t.Run("entry name in the database field still works", func(t *testing.T) {
		// Rung 1, the pre-existing contract: every connection string issued
		// before the username selector existed must keep working.
		conn, err := pgx.Connect(ctx, f.dsnFor(fixtureUser, fixturePass, "analytics_ro"))
		require.NoError(t, err)
		defer conn.Close(context.Background())

		var current string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("user#server with the real upstream database name", func(t *testing.T) {
		// Rung 2 — the form the UI hands out and the one DataGrip can sustain
		// across its per-database reconnects.
		conn, err := pgx.Connect(ctx,
			f.dsnFor(fixtureUser+"%23analytics_ro", fixturePass, resolutionUpstreamDB))
		require.NoError(t, err)
		defer conn.Close(context.Background())

		var current string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)

		// The '#' selects a server; it never becomes part of the identity. The
		// session is recorded against the plain dbbat user, and the upstream
		// was reached with the credential stored on the server row.
		conns, err := f.store.ListConnections(ctx, store.ConnectionFilter{Limit: 50})
		require.NoError(t, err)

		var found bool

		for i := range conns {
			if conns[i].DatabaseID == ro.UID {
				assert.Equal(t, f.user.UID, conns[i].UserID)

				found = true
			}
		}

		assert.True(t, found, "the session must be recorded against the bare dbbat user")
	})

	t.Run("user#server alone, with no database field", func(t *testing.T) {
		// libpq defaults the database to the username, which would be
		// "dbbattest#analytics_ro" — so ask for the entry's own name instead
		// and let the hint agree with it.
		conn, err := pgx.Connect(ctx,
			f.dsnFor(fixtureUser+"%23analytics_ro", fixturePass, "analytics_ro"))
		require.NoError(t, err)
		defer conn.Close(context.Background())

		var current string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("reconnect to a database the entry does not expose is refused", func(t *testing.T) {
		// This is precisely what DataGrip does after reading pg_database: it
		// opens a dedicated connection per database. It must be told what went
		// wrong, not silently handed a different database.
		_, err := pgx.Connect(ctx,
			f.dsnFor(fixtureUser+"%23analytics_ro", fixturePass, "postgres"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "analytics_ro")
		assert.Contains(t, err.Error(), "analytics")
		assert.Contains(t, err.Error(), "postgres")
	})

	t.Run("the bare upstream name resolves through the caller's single grant", func(t *testing.T) {
		// Rung 3: no selector at all, just the real database name.
		conn, err := pgx.Connect(ctx, f.dsnFor(fixtureUser, fixturePass, resolutionUpstreamDB))
		require.NoError(t, err)
		defer conn.Close(context.Background())

		var current string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("an entry the caller holds no grant on is invisible to rung 3", func(t *testing.T) {
		ungranted := f.registerTwin(ctx, "analytics_nogrant", false)
		require.NotEqual(t, ro.UID, ungranted.UID)

		// Still exactly one *granted* candidate, so this must still resolve to
		// analytics_ro rather than becoming ambiguous.
		conn, err := pgx.Connect(ctx, f.dsnFor(fixtureUser, fixturePass, resolutionUpstreamDB))
		require.NoError(t, err)
		defer conn.Close(context.Background())

		var current string
		require.NoError(t, conn.QueryRow(ctx, "SELECT current_database()").Scan(&current))
		assert.Equal(t, resolutionUpstreamDB, current)
	})

	t.Run("two granted twins make the bare name ambiguous", func(t *testing.T) {
		// The real-world shape: demo_datalake_ro and demo_datalake_rw both
		// point at demo_datalake, and the caller holds both.
		f.registerTwin(ctx, "analytics_rw", true)

		_, err := pgx.Connect(ctx, f.dsnFor(fixtureUser, fixturePass, resolutionUpstreamDB))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "analytics_ro")
		assert.Contains(t, err.Error(), "analytics_rw")

		// …and the selector is how you get out of it.
		conn, cerr := pgx.Connect(ctx,
			f.dsnFor(fixtureUser+"%23analytics_rw", fixturePass, resolutionUpstreamDB))
		require.NoError(t, cerr)
		defer conn.Close(context.Background())
	})
}

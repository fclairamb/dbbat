package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/fclairamb/dbbat/internal/migrations"
)

// TestGrantDefinitionNotifyChannelMigrationRoundTrip migrates to head, then
// runs the notify_channel down and up files by hand: the column must go away
// and come back as a nullable text column (NULL = default channel). It needs
// its own container because it drops a column the shared one's tests use.
func TestGrantDefinitionNotifyChannelMigrationRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_migration_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)

	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	migrator := migrate.NewMigrator(db, migrations.Migrations)
	require.NoError(t, migrator.Init(ctx))

	_, err = migrator.Migrate(ctx)
	require.NoError(t, err)

	columnType := func() (string, string) {
		var dataType, nullable string

		err := db.NewRaw(`SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_name = 'grant_definitions' AND column_name = 'notify_channel'`).
			Scan(ctx, &dataType, &nullable)
		if err != nil {
			return "", ""
		}

		return dataType, nullable
	}

	dataType, nullable := columnType()
	require.Equal(t, "text", dataType)
	require.Equal(t, "YES", nullable)

	run := func(file string) {
		body, err := os.ReadFile("../migrations/sql/" + file)
		require.NoError(t, err)

		_, err = db.ExecContext(ctx, string(body))
		require.NoError(t, err)
	}

	run("20261104000000_grant_definition_notify_channel.down.sql")

	dataType, _ = columnType()
	require.Empty(t, dataType, "down must drop the column")

	run("20261104000000_grant_definition_notify_channel.up.sql")

	dataType, nullable = columnType()
	require.Equal(t, "text", dataType)
	require.Equal(t, "YES", nullable)
}

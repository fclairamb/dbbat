package migrations

import (
	"embed"

	"github.com/uptrace/bun/migrate"
)

//go:embed sql/*.sql
var sqlMigrations embed.FS

// Migrations contains all registered migrations
var Migrations = migrate.NewMigrations()

//nolint:gochecknoinits // init is idiomatic for migration discovery
func init() {
	if err := Migrations.Discover(sqlMigrations); err != nil {
		panic(err)
	}
}

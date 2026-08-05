package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/fclairamb/dbbat/internal/migrations"
)

// slugMigrationName is the up-migration under test — bun names a discovered
// SQL migration after its timestamp prefix only, dropping the descriptive
// suffix. The test fails loudly (rather than silently testing the wrong
// migration) if a later migration has since become the newest one.
const slugMigrationName = "20260805000000"

// TestGrantDefinitionSlugMigrationBackfill proves the up-migration's core
// claim: a grant_definitions row that predates the slug column ends up with
// slug = uid (as text) once the migration runs, and the resulting NOT NULL +
// UNIQUE constraints don't choke on that backfilled value.
//
// This drives bun's migrator directly through an explicit
// "apply every earlier migration, seed a legacy row, then apply this one"
// sequence, so it needs its own single-use Postgres container — the
// package's shared container (concurrently used by every other parallel
// test in this package) could not tolerate having its schema rolled
// part-way back and reapplied mid-suite.
func TestGrantDefinitionSlugMigrationBackfill(t *testing.T) {
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
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}

	t.Cleanup(func() {
		_ = pgContainer.Terminate(ctx)
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	// WithUpsert(true) is required by RunMigration, which we use below to
	// apply every earlier migration one at a time (each its own group) so
	// the final, normal Migrate() call applies only the slug migration.
	migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithUpsert(true))
	if err := migrator.Init(ctx); err != nil {
		t.Fatalf("migrator.Init: %v", err)
	}

	all := migrations.Migrations.Sorted()
	if len(all) == 0 {
		t.Fatal("no migrations discovered")
	}

	last := all[len(all)-1]
	if last.Name != slugMigrationName {
		t.Fatalf("expected %q to be the newest migration, newest is %q — "+
			"update this test now that a later migration has landed", slugMigrationName, last.Name)
	}

	// Apply every migration up to (but not including) the slug one, so
	// grant_definitions exists but has no slug column — the exact state a
	// pre-upgrade production database is in.
	for _, m := range all[:len(all)-1] {
		if err := migrator.RunMigration(ctx, m.Name); err != nil {
			t.Fatalf("RunMigration(%s): %v", m.Name, err)
		}
	}

	s := &Store{db: db}

	admin, err := s.CreateUser(ctx, "legacy-admin", "hash", []string{RoleAdmin})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Seed a "legacy" row the way it existed before this release: a raw
	// insert with no slug value, since the column doesn't exist yet at this
	// point in the migration sequence.
	var legacyUID uuid.UUID

	err = db.NewRaw(
		"INSERT INTO grant_definitions (name, duration_seconds, created_by) VALUES (?, ?, ?) RETURNING uid",
		"legacy-def", 3600, admin.UID,
	).Scan(ctx, &legacyUID)
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Apply the slug migration itself — this is the up.sql under test.
	if _, err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (slug): %v", err)
	}

	got, err := s.GetGrantDefinition(ctx, legacyUID)
	if err != nil {
		t.Fatalf("GetGrantDefinition: %v", err)
	}

	if got.Slug != legacyUID.String() {
		t.Errorf("backfilled slug = %q, want %q (the uid as text)", got.Slug, legacyUID.String())
	}

	// The migration's NOT NULL + UNIQUE constraints must also hold for a
	// fresh insert after the backfill — the migration wouldn't be safe to
	// ship if it left the column in a state new rows couldn't satisfy.
	if _, err := s.CreateGrantDefinition(ctx, &GrantDefinition{
		Name:            "post-migration",
		Slug:            "post-migration",
		DurationSeconds: 60,
		CreatedBy:       admin.UID,
	}); err != nil {
		t.Fatalf("CreateGrantDefinition after migration: %v", err)
	}

	// The down-migration must reverse cleanly too: it ran in its own group
	// (the plain Migrate() call above applied only the one pending
	// migration), so Rollback() undoes exactly it.
	if _, err := migrator.Rollback(ctx); err != nil {
		t.Fatalf("Rollback (slug): %v", err)
	}

	var slugColumnExists bool

	err = db.NewRaw(
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns "+
			"WHERE table_name = 'grant_definitions' AND column_name = 'slug')",
	).Scan(ctx, &slugColumnExists)
	if err != nil {
		t.Fatalf("check slug column: %v", err)
	}

	if slugColumnExists {
		t.Error("slug column still present after rollback")
	}

	// Rows written before the rollback (and the constraints on other
	// columns) must survive it untouched — this migration only ever
	// touches the slug column.
	var rowCount int

	err = db.NewRaw("SELECT count(*) FROM grant_definitions").Scan(ctx, &rowCount)
	if err != nil {
		t.Fatalf("count grant_definitions: %v", err)
	}

	if rowCount != 2 {
		t.Errorf("grant_definitions row count after rollback = %d, want 2 (legacy + post-migration)", rowCount)
	}
}

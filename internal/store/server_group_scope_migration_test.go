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

// serverGroupScopeMigrationName is the up-migration under test.
const serverGroupScopeMigrationName = "20260809010000"

// TestGrantDefinitionServerGroupScopeMigration pins the data migration that
// moves per-database definition scope onto server groups.
//
// The invariant is "no behavior change": every definition that listed
// databases must end up pointing at a server group holding exactly those
// databases. The fixture also pins the two properties that make the migration
// safe to ship — definitions sharing a scope share one group (versions of a
// lineage almost always do), and an unscoped definition stays unscoped rather
// than acquiring an empty group, which would fail closed instead of open.
func TestGrantDefinitionServerGroupScopeMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_server_group_scope_test"),
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

	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	db := bun.NewDB(sqldb, pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })

	migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithUpsert(true))
	if err := migrator.Init(ctx); err != nil {
		t.Fatalf("migrator.Init: %v", err)
	}

	all := migrations.Migrations.Sorted()

	index := -1

	for i, m := range all {
		if m.Name == serverGroupScopeMigrationName {
			index = i

			break
		}
	}

	if index < 0 {
		t.Fatalf("migration %q not found — has it been renamed?", serverGroupScopeMigrationName)
	}

	for _, m := range all[:index] {
		if err := migrator.RunMigration(ctx, m.Name); err != nil {
			t.Fatalf("RunMigration(%s): %v", m.Name, err)
		}
	}

	f := seedServerGroupScopeFixture(t, ctx, db)

	if err := migrator.RunMigration(ctx, all[index].Name); err != nil {
		t.Fatalf("RunMigration(%s): %v", all[index].Name, err)
	}

	scopeOf := func(defUID uuid.UUID) []uuid.UUID {
		t.Helper()

		var groups []uuid.UUID

		if err := db.NewRaw("SELECT server_group_uids FROM grant_definitions WHERE uid = ?", defUID).
			Scan(ctx, pgdialect.Array(&groups)); err != nil {
			t.Fatalf("read server_group_uids for %s: %v", defUID, err)
		}

		return groups
	}

	membersOf := func(groupUID uuid.UUID) []uuid.UUID {
		t.Helper()

		var members []uuid.UUID

		if err := db.NewRaw(
			"SELECT ARRAY(SELECT server_uid FROM server_group_members WHERE group_uid = ? ORDER BY 1)", groupUID,
		).Scan(ctx, pgdialect.Array(&members)); err != nil {
			t.Fatalf("read members of %s: %v", groupUID, err)
		}

		return members
	}

	// The scoped definition points at exactly one group, holding exactly the
	// databases it used to list.
	scoped := scopeOf(f.scopedDefinition)
	if len(scoped) != 1 {
		t.Fatalf("scoped definition server_group_uids = %v, want exactly one group", scoped)
	}

	members := membersOf(scoped[0])
	if len(members) != 2 {
		t.Fatalf("mirrored group holds %v, want the two databases the definition listed", members)
	}

	got := map[uuid.UUID]bool{members[0]: true, members[1]: true}
	if !got[f.dbA] || !got[f.dbB] {
		t.Errorf("mirrored group holds %v, want %v and %v", members, f.dbA, f.dbB)
	}

	// A second definition with the *same* scope reuses that group rather than
	// getting a near-duplicate of its own.
	if twin := scopeOf(f.twinDefinition); len(twin) != 1 || twin[0] != scoped[0] {
		t.Errorf("definition sharing a scope got %v, want the same group %v", twin, scoped[0])
	}

	// A different scope gets its own group.
	other := scopeOf(f.otherScopedDefinition)
	if len(other) != 1 || other[0] == scoped[0] {
		t.Errorf("definition with a different scope got %v, want a group of its own", other)
	}

	if m := membersOf(other[0]); len(m) != 1 || m[0] != f.dbC {
		t.Errorf("second group holds %v, want just %v", m, f.dbC)
	}

	// An unscoped definition must stay unscoped: an empty group would fail
	// closed where it used to mean "every database".
	if unscoped := scopeOf(f.unscopedDefinition); len(unscoped) != 0 {
		t.Errorf("unscoped definition got server_group_uids = %v, want empty", unscoped)
	}

	// The retired column is gone only after the mirroring above ran.
	if columnExists(t, ctx, db, "grant_definitions", "database_uids") {
		t.Error("grant_definitions.database_uids still exists after the migration")
	}

	// Re-running is a no-op: the migration recomputes its guard rather than
	// tracking state, which is what makes it safe to retry after a failure.
	if err := all[index].Up(ctx, migrator, &all[index]); err != nil {
		t.Fatalf("re-apply %s: %v", all[index].Name, err)
	}

	if again := scopeOf(f.scopedDefinition); len(again) != 1 || again[0] != scoped[0] {
		t.Errorf("re-apply moved the scoped definition: %v, want %v", again, scoped)
	}

	var groupCount int

	if err := db.NewRaw("SELECT count(*) FROM server_groups").Scan(ctx, &groupCount); err != nil {
		t.Fatalf("count server groups: %v", err)
	}

	if groupCount != 2 {
		t.Errorf("server_groups = %d after a second pass, want the same 2", groupCount)
	}

	// Rollback rebuilds database_uids from the group membership.
	if _, err := migrator.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	var restored []uuid.UUID

	if err := db.NewRaw(
		"SELECT ARRAY(SELECT unnest(database_uids) ORDER BY 1) FROM grant_definitions WHERE uid = ?",
		f.scopedDefinition,
	).Scan(ctx, pgdialect.Array(&restored)); err != nil {
		t.Fatalf("read restored database_uids: %v", err)
	}

	if len(restored) != 2 {
		t.Errorf("rollback restored database_uids = %v, want the original two databases", restored)
	}
}

// serverGroupScopeFixture records the uids the assertions recognize after the
// migration has rewritten the table.
type serverGroupScopeFixture struct {
	dbA, dbB, dbC uuid.UUID

	scopedDefinition      uuid.UUID
	twinDefinition        uuid.UUID
	otherScopedDefinition uuid.UUID
	unscopedDefinition    uuid.UUID
}

// seedServerGroupScopeFixture writes definitions carrying the pre-migration
// per-database scope, straight through SQL: the Go model no longer has the
// column, which is the whole point of the migration.
func seedServerGroupScopeFixture(t *testing.T, ctx context.Context, db *bun.DB) serverGroupScopeFixture {
	t.Helper()

	f := serverGroupScopeFixture{}

	var adminUID uuid.UUID

	if err := db.NewRaw(
		"INSERT INTO users (username, password_hash, roles) VALUES ('sgscope-admin', 'x', ARRAY['admin']::user_role[]) "+
			"RETURNING uid",
	).Scan(ctx, &adminUID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	newServer := func(name string) uuid.UUID {
		t.Helper()

		var uid uuid.UUID

		if err := db.NewRaw(
			"INSERT INTO servers (name, host, port, database_name, username, password_encrypted, protocol) "+
				"VALUES (?, 'localhost', 5432, 'db', 'u', 'x', 'postgresql') RETURNING uid", name,
		).Scan(ctx, &uid); err != nil {
			t.Fatalf("seed server %s: %v", name, err)
		}

		return uid
	}

	f.dbA = newServer("sgscope-a")
	f.dbB = newServer("sgscope-b")
	f.dbC = newServer("sgscope-c")

	newDefinition := func(slug string, scope []uuid.UUID) uuid.UUID {
		t.Helper()

		uid := uuid.New()

		if _, err := db.NewRaw(
			"INSERT INTO grant_definitions "+
				"(uid, lineage_uid, name, slug, duration_seconds, controls, database_uids, created_by) "+
				"VALUES (?, ?, ?, ?, 3600, ARRAY['read_only'], ?, ?)",
			uid, uid, slug, slug, pgdialect.Array(scope), adminUID,
		).Exec(ctx); err != nil {
			t.Fatalf("seed definition %s: %v", slug, err)
		}

		return uid
	}

	f.scopedDefinition = newDefinition("sgscope-scoped", []uuid.UUID{f.dbA, f.dbB})
	// Same scope, listed in the other order: the migration keys on the sorted
	// set, so this must land on the same group.
	f.twinDefinition = newDefinition("sgscope-twin", []uuid.UUID{f.dbB, f.dbA})
	f.otherScopedDefinition = newDefinition("sgscope-other", []uuid.UUID{f.dbC})
	f.unscopedDefinition = newDefinition("sgscope-unscoped", []uuid.UUID{})

	return f
}

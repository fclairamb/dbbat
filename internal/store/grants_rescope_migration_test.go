package store

import (
	"context"
	"database/sql"
	"strings"
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

// grantsRescopeMigrationName is the corrective migration under test.
const grantsRescopeMigrationName = "20260807010000"

// TestGrantsRescopeMismatchedDefinitionsMigration exercises the repair for
// environments that applied 20260806020000 while its backfill pass 2 still
// matched on shape alone, ignoring a definition's database_uids. Those
// environments never re-run the amended file — bun records a migration by name,
// not by content — so this migration is their only fix.
//
// It cannot be driven the way the pass-2 test is driven, because the buggy
// pass 2 no longer exists to produce the damage. So the damaged state is built
// by hand on the post-migration schema and the migration is re-applied over it:
// it is idempotent by construction (it recomputes the mismatch predicate rather
// than tracking state), which is also what makes it safe to ship to production,
// where it must find nothing to do.
func TestGrantsRescopeMismatchedDefinitionsMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_grant_rescope_migration_test"),
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

	migrator := migrate.NewMigrator(db, migrations.Migrations, migrate.WithUpsert(true))
	if err := migrator.Init(ctx); err != nil {
		t.Fatalf("migrator.Init: %v", err)
	}

	if _, err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var corrective *migrate.Migration

	for i, m := range migrations.Migrations.Sorted() {
		if m.Name == grantsRescopeMigrationName {
			corrective = &migrations.Migrations.Sorted()[i]

			break
		}
	}

	if corrective == nil {
		t.Fatalf("migration %q not found — has it been renamed?", grantsRescopeMigrationName)
	}

	// The corrective migration is written against the schema as it stood when
	// it was authored, and two later migrations moved that schema on:
	// approver_group_uids was renamed to approver_user_group_uids (server
	// groups made a bare "group" ambiguous) and database_uids was replaced by
	// server-group scoping. bun records a migration by name and never re-runs
	// it, so neither change matters in production — but this test *does*
	// re-run it, so it restores the schema shape the migration was written for
	// and puts it back afterwards.
	exec := func(query string) {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	exec("ALTER TABLE grant_definitions RENAME COLUMN approver_user_group_uids TO approver_group_uids")
	exec("ALTER TABLE grant_definitions ADD COLUMN database_uids uuid[] NOT NULL DEFAULT '{}'")

	t.Cleanup(func() {
		exec("ALTER TABLE grant_definitions DROP COLUMN IF EXISTS database_uids")
		exec("ALTER TABLE grant_definitions RENAME COLUMN approver_group_uids TO approver_user_group_uids")
	})

	f := seedRescopeFixture(t, ctx, db)

	// Re-apply. Everything the fixture describes as damaged is repaired; the
	// two grants that must not move are the no-op half.
	if err := corrective.Up(ctx, migrator, corrective); err != nil {
		t.Fatalf("re-apply %s: %v", corrective.Name, err)
	}

	assertRescoped(t, ctx, db, f)

	// Idempotent: a second pass has nothing left to find, so nothing may move
	// and no further stand-in may be created.
	before := definitionOf(t, ctx, db, f.orphanGrant)

	var standInsBefore int

	if err := db.NewRaw("SELECT count(*) FROM grant_definitions WHERE slug LIKE '%-rescoped-%'").
		Scan(ctx, &standInsBefore); err != nil {
		t.Fatalf("count stand-ins: %v", err)
	}

	if err := corrective.Up(ctx, migrator, corrective); err != nil {
		t.Fatalf("second re-apply: %v", err)
	}

	if got := definitionOf(t, ctx, db, f.orphanGrant); got != before {
		t.Errorf("a second pass moved a repaired grant: %s -> %s", before, got)
	}

	var standInsAfter int

	if err := db.NewRaw("SELECT count(*) FROM grant_definitions WHERE slug LIKE '%-rescoped-%'").
		Scan(ctx, &standInsAfter); err != nil {
		t.Fatalf("count stand-ins after: %v", err)
	}

	if standInsAfter != standInsBefore {
		t.Errorf("a second pass synthesized %d more definitions", standInsAfter-standInsBefore)
	}
}

// rescopeFixture is the hand-built damaged state: four grants, two of which the
// migration must repair and two of which it must not touch.
type rescopeFixture struct {
	adminUID uuid.UUID
	dbUID    uuid.UUID

	// coveringDefinition is unscoped and shares elsewhereDefinition's shape, so
	// it is where step 1 has to send a grant stranded on elsewhereDefinition.
	elsewhereDefinition uuid.UUID
	coveringDefinition  uuid.UUID
	strandedGrant       uuid.UUID

	// orphanDefinition is scoped elsewhere and no other definition shares its
	// shape, so orphanGrant has nowhere to go but a synthesized stand-in.
	orphanDefinition uuid.UUID
	orphanGrant      uuid.UUID

	// provenanceGrant is stranded on orphanDefinition too, but a grant request
	// names it: that link is recorded fact, not an inference, so it stays.
	provenanceGrant uuid.UUID

	// inScopeGrant is on the database orphanDefinition is scoped to. Nothing
	// about it is wrong.
	inScopeGrant uuid.UUID
}

func seedRescopeFixture(t *testing.T, ctx context.Context, db *bun.DB) rescopeFixture {
	t.Helper()

	f := rescopeFixture{}

	if err := db.NewRaw(
		"INSERT INTO users (username, password_hash, roles) VALUES (?, ?, ?) RETURNING uid",
		"rescope-admin", "hash", pgdialect.Array([]string{RoleAdmin}),
	).Scan(ctx, &f.adminUID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	insertDatabase := func(name string) uuid.UUID {
		t.Helper()

		var uid uuid.UUID

		if err := db.NewRaw(
			"INSERT INTO servers (name, host, port, database_name, username, password_encrypted, ssl_mode) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING uid",
			name, "localhost", 5432, "db", "user", []byte("enc"), "disable",
		).Scan(ctx, &uid); err != nil {
			t.Fatalf("seed database %s: %v", name, err)
		}

		return uid
	}

	f.dbUID = insertDatabase("rescope-db")
	otherDBUID := insertDatabase("rescope-db-elsewhere")

	insertDefinition := func(slug string, maxQueries int64, controls []string, databaseUIDs []uuid.UUID) uuid.UUID {
		t.Helper()

		if databaseUIDs == nil {
			databaseUIDs = []uuid.UUID{}
		}

		// The uid is generated here rather than by the column default because
		// lineage_uid is NOT NULL from 20260806020000 onward, and a definition
		// that is not a version of anything is the root of its own lineage.
		uid := uuid.New()

		if _, err := db.NewRaw(
			"INSERT INTO grant_definitions "+
				"(uid, lineage_uid, name, slug, duration_seconds, controls, max_query_counts, "+
				"database_uids, created_by) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			uid, uid, slug, slug, 3600, pgdialect.Array(controls), maxQueries,
			pgdialect.Array(databaseUIDs), f.adminUID,
		).Exec(ctx); err != nil {
			t.Fatalf("seed definition %s: %v", slug, err)
		}

		return uid
	}

	f.elsewhereDefinition = insertDefinition(
		"rescope-elsewhere", 100, []string{ControlReadOnly}, []uuid.UUID{otherDBUID},
	)
	f.coveringDefinition = insertDefinition(
		"rescope-covering", 100, []string{ControlReadOnly}, nil,
	)
	f.orphanDefinition = insertDefinition(
		"rescope-orphan", 42, []string{ControlBlockDDL}, []uuid.UUID{otherDBUID},
	)

	now := time.Now()

	insertGrant := func(databaseUID, definitionUID uuid.UUID) uuid.UUID {
		t.Helper()

		var uid uuid.UUID

		if err := db.NewRaw(
			"INSERT INTO access_grants "+
				"(user_id, database_id, grant_definition_id, granted_by, starts_at, expires_at) "+
				"VALUES (?, ?, ?, ?, ?, ?) RETURNING uid",
			f.adminUID, databaseUID, definitionUID, f.adminUID,
			now.Add(-time.Hour), now.Add(time.Hour),
		).Scan(ctx, &uid); err != nil {
			t.Fatalf("seed grant: %v", err)
		}

		return uid
	}

	f.strandedGrant = insertGrant(f.dbUID, f.elsewhereDefinition)
	f.orphanGrant = insertGrant(f.dbUID, f.orphanDefinition)
	f.provenanceGrant = insertGrant(f.dbUID, f.orphanDefinition)
	f.inScopeGrant = insertGrant(otherDBUID, f.orphanDefinition)

	if _, err := db.NewRaw(
		"INSERT INTO grant_requests (user_id, grant_definition_id, database_id, status, resulting_grant_id) "+
			"VALUES (?, ?, ?, ?, ?)",
		f.adminUID, f.orphanDefinition, f.dbUID, string(GrantRequestApproved), f.provenanceGrant,
	).Exec(ctx); err != nil {
		t.Fatalf("seed grant request: %v", err)
	}

	return f
}

func assertRescoped(t *testing.T, ctx context.Context, db *bun.DB, f rescopeFixture) {
	t.Helper()

	// Step 1: an active definition with the same shape whose scope does cover
	// the grant is preferred over synthesizing anything.
	if got := definitionOf(t, ctx, db, f.strandedGrant); got != f.coveringDefinition {
		t.Errorf("stranded grant points at %s, want the in-scope twin %s", got, f.coveringDefinition)
	}

	// Step 2: with no such definition, the grant lands on an inactive stand-in
	// carrying the same shape — pass 3's rule, applied late.
	orphanDef := definitionOf(t, ctx, db, f.orphanGrant)
	if orphanDef == f.orphanDefinition {
		t.Fatal("orphan grant is still pinned to the definition that excludes its database")
	}

	var (
		slug     string
		isActive bool
		controls []string
		quota    *int64
	)

	if err := db.NewRaw(
		"SELECT slug, is_active, controls, max_query_counts FROM grant_definitions WHERE uid = ?", orphanDef,
	).Scan(ctx, &slug, &isActive, pgdialect.Array(&controls), &quota); err != nil {
		t.Fatalf("read the synthesized stand-in: %v", err)
	}

	// The prefix matters beyond readability: the down-migration of
	// 20260806020000 deletes synthesized definitions by it, so a stand-in
	// missing it would survive a full rollback.
	if !strings.HasPrefix(slug, "legacy-grant-shape-") || !strings.Contains(slug, "-rescoped-") {
		t.Errorf("stand-in slug %q, want the legacy prefix and the rescoped marker", slug)
	}

	if isActive {
		t.Error("a synthesized stand-in must be inactive")
	}

	if len(controls) != 1 || controls[0] != ControlBlockDDL {
		t.Errorf("stand-in controls = %v, want the shape it replaced", controls)
	}

	if quota == nil || *quota != 42 {
		t.Errorf("stand-in max_query_counts = %v, want 42", quota)
	}

	// Provenance is a recorded fact, not an inference: a grant a request
	// materialized keeps its link even when the definition's scope was
	// narrowed afterwards.
	if got := definitionOf(t, ctx, db, f.provenanceGrant); got != f.orphanDefinition {
		t.Errorf("provenance-linked grant was moved to %s; it must keep %s", got, f.orphanDefinition)
	}

	// And a grant that was in scope all along is untouched.
	if got := definitionOf(t, ctx, db, f.inScopeGrant); got != f.orphanDefinition {
		t.Errorf("in-scope grant was moved to %s; it must keep %s", got, f.orphanDefinition)
	}
}

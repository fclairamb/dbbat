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

// grantsDefinitionMigrationName is the up-migration under test — bun names a
// discovered SQL migration after its timestamp prefix only, dropping the
// descriptive suffix.
const grantsDefinitionMigrationName = "20260806020000"

// TestGrantsReferenceDefinitionsMigration exercises the riskiest part of this
// change: the backfill that has to find a definition for every pre-existing
// grant before grant_definition_id can be made NOT NULL.
//
// The fixture deliberately contains one grant of each provenance the backfill
// distinguishes:
//
//   - a grant materialized from a definition, linked through
//     grant_requests.resulting_grant_id (pass 1);
//   - a grant whose shape happens to equal an existing definition's, with no
//     request behind it (pass 2);
//   - *two* ad-hoc grants sharing one shape nothing matches, which must
//     collapse onto a single synthesized definition (pass 3);
//   - a third ad-hoc grant with a different shape, which must get its own.
//
// On top of that it pins down pass 2's database-scope condition, the part that
// was wrong until 2026-08-07: a grant whose shape matches a definition scoped
// to *another* database must not be pinned to it, a definition scoped to the
// grant's own database is a normal match, and an unscoped definition still
// matches anything (the regression the scope predicate could most easily
// introduce, since almost every definition is unscoped).
//
// It then rolls the migration back and checks the shape data came home.
//
// Like the slug-migration test, this drives bun's migrator through an explicit
// "apply every earlier migration, seed legacy rows, then apply this one"
// sequence, so it needs its own single-use Postgres container.
func TestGrantsReferenceDefinitionsMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_grantdef_migration_test"),
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

	all := migrations.Migrations.Sorted()

	index := -1

	for i, m := range all {
		if m.Name == grantsDefinitionMigrationName {
			index = i

			break
		}
	}

	if index < 0 {
		t.Fatalf("migration %q not found — has it been renamed?", grantsDefinitionMigrationName)
	}

	for _, m := range all[:index] {
		if err := migrator.RunMigration(ctx, m.Name); err != nil {
			t.Fatalf("RunMigration(%s): %v", m.Name, err)
		}
	}

	fixture := seedPreMigrationGrants(t, ctx, db)

	// This migration runs on its own — RunMigration gives it its own group —
	// so the backfill assertions below see exactly what *it* produced. Running
	// the rest of the stack first would let 20260807010000, which repairs
	// out-of-scope links, mask a pass-2 regression.
	if err := migrator.RunMigration(ctx, all[index].Name); err != nil {
		t.Fatalf("RunMigration(%s): %v", all[index].Name, err)
	}

	assertBackfill(t, ctx, db, fixture)
	scopedDefinition := assertDatabaseScopeRespected(t, ctx, db, fixture)

	// Now the migrations layered on top, including the corrective one. It has
	// nothing to repair here, so nothing may move.
	if _, err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if got := assertDatabaseScopeRespected(t, ctx, db, fixture); got != scopedDefinition {
		t.Errorf(
			"the corrective migration re-pointed a grant pass 2 had already placed correctly: %s -> %s",
			scopedDefinition, got,
		)
	}

	// Two rollbacks: one for the group Migrate just created, one for this
	// migration's own group.
	for range 2 {
		if _, err := migrator.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
	}

	assertRollback(t, ctx, db, fixture)
}

// legacyGrantFixture records the uids the assertions need to recognize each
// seeded row after the migration has rewritten the table.
type legacyGrantFixture struct {
	adminUID uuid.UUID
	dbUID    uuid.UUID

	// otherDBUID is a second database no grant is ever issued on. It exists to
	// give the scope-restricted definitions below something to be scoped to
	// that is *not* dbUID.
	otherDBUID uuid.UUID

	// definitionUID is a real, pre-existing definition, unscoped (empty
	// database_uids). requestGrant was materialized from it (and is linked
	// through a grant request); matchingGrant merely happens to share its
	// shape.
	definitionUID uuid.UUID
	requestGrant  uuid.UUID
	matchingGrant uuid.UUID

	// elsewhereDefinition is scoped to otherDBUID, so it must never claim
	// excludedGrant even though their shapes are identical.
	elsewhereDefinition uuid.UUID
	excludedGrant       uuid.UUID

	// hereDefinition is scoped to dbUID and must claim includedGrant: an
	// explicit scope that *covers* the grant is a normal match.
	hereDefinition uuid.UUID
	includedGrant  uuid.UUID

	// Two definitions share one shape: the older one is scoped elsewhere, the
	// newer one is unscoped. Pass 2 picks the oldest candidate, so twinGrant
	// landing on the newer one proves the scope filter runs before that
	// ordering rather than after it.
	twinElsewhereDefinition uuid.UUID
	twinUnscopedDefinition  uuid.UUID
	twinGrant               uuid.UUID

	// adHocA and adHocB share one shape that matches no definition; adHocC has
	// a different unmatched shape.
	adHocA uuid.UUID
	adHocB uuid.UUID
	adHocC uuid.UUID
}

// seedPreMigrationGrants writes the pre-upgrade rows with raw SQL, since the
// Go models no longer describe the old columns.
func seedPreMigrationGrants(t *testing.T, ctx context.Context, db *bun.DB) legacyGrantFixture {
	t.Helper()

	f := legacyGrantFixture{}

	if err := db.NewRaw(
		"INSERT INTO users (username, password_hash, roles) VALUES (?, ?, ?) RETURNING uid",
		"legacy-admin", "hash", pgdialect.Array([]string{RoleAdmin}),
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

	f.dbUID = insertDatabase("legacy-db")
	f.otherDBUID = insertDatabase("legacy-db-elsewhere")

	// databaseUIDs is the definition's scope. A nil slice becomes the empty
	// array the column defaults to, i.e. "unscoped — covers every database".
	insertDefinition := func(slug string, maxQueries int64, controls []string, databaseUIDs []uuid.UUID) uuid.UUID {
		t.Helper()

		if databaseUIDs == nil {
			databaseUIDs = []uuid.UUID{}
		}

		var uid uuid.UUID

		if err := db.NewRaw(
			"INSERT INTO grant_definitions "+
				"(name, slug, duration_seconds, controls, max_query_counts, database_uids, created_by) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING uid",
			slug, slug, 3600, pgdialect.Array(controls), maxQueries,
			pgdialect.Array(databaseUIDs), f.adminUID,
		).Scan(ctx, &uid); err != nil {
			t.Fatalf("seed definition %s: %v", slug, err)
		}

		return uid
	}

	// A real definition: read_only with a query quota, unscoped.
	f.definitionUID = insertDefinition("read-only-1h", 100, []string{ControlReadOnly}, nil)

	// Scoped to the *other* database: a shape match that must be rejected.
	f.elsewhereDefinition = insertDefinition(
		"ddl-elsewhere", 500, []string{ControlBlockDDL}, []uuid.UUID{f.otherDBUID},
	)

	// Scoped to the grant's own database: a shape match that must be accepted.
	f.hereDefinition = insertDefinition(
		"copy-here", 250, []string{ControlBlockCopy}, []uuid.UUID{f.dbUID},
	)

	// Same shape, two definitions. The out-of-scope one is created first, so
	// pass 2's "oldest candidate wins" tiebreak would pick it if the scope
	// filter did not run.
	f.twinElsewhereDefinition = insertDefinition(
		"twin-elsewhere", 777, []string{ControlReadOnly}, []uuid.UUID{f.otherDBUID},
	)
	f.twinUnscopedDefinition = insertDefinition(
		"twin-unscoped", 777, []string{ControlReadOnly}, nil,
	)

	now := time.Now()

	insertGrant := func(controls []string, maxQueries *int64, patterns []string) uuid.UUID {
		t.Helper()

		if controls == nil {
			controls = []string{}
		}

		if patterns == nil {
			patterns = []string{}
		}

		var uid uuid.UUID

		if err := db.NewRaw(
			"INSERT INTO access_grants "+
				"(user_id, database_id, controls, granted_by, starts_at, expires_at, max_query_counts, approval_patterns) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING uid",
			f.adminUID, f.dbUID, pgdialect.Array(controls), f.adminUID,
			now.Add(-time.Hour), now.Add(time.Hour), maxQueries, pgdialect.Array(patterns),
		).Scan(ctx, &uid); err != nil {
			t.Fatalf("seed grant: %v", err)
		}

		return uid
	}

	hundred := int64(100)

	// Pass 1: materialized from the definition, linked by a grant request.
	f.requestGrant = insertGrant([]string{ControlReadOnly}, &hundred, nil)

	if _, err := db.NewRaw(
		"INSERT INTO grant_requests (user_id, grant_definition_id, database_id, status, resulting_grant_id) "+
			"VALUES (?, ?, ?, ?, ?)",
		f.adminUID, f.definitionUID, f.dbUID, string(GrantRequestApproved), f.requestGrant,
	).Exec(ctx); err != nil {
		t.Fatalf("seed grant request: %v", err)
	}

	// Pass 2: the same shape, but no request behind it. The definition is
	// unscoped, which is the overwhelmingly common case and the one the scope
	// predicate must keep matching.
	f.matchingGrant = insertGrant([]string{ControlReadOnly}, &hundred, nil)

	// Pass 2, database scope. All three grants are on f.dbUID.
	fiveHundred, twoFifty, sevenSevenSeven := int64(500), int64(250), int64(777)

	f.excludedGrant = insertGrant([]string{ControlBlockDDL}, &fiveHundred, nil)
	f.includedGrant = insertGrant([]string{ControlBlockCopy}, &twoFifty, nil)
	f.twinGrant = insertGrant([]string{ControlReadOnly}, &sevenSevenSeven, nil)

	// Pass 3: two ad-hoc grants sharing one unmatched shape (note the reversed
	// control order — sorted comparison has to see through that), and a third
	// with a shape of its own.
	f.adHocA = insertGrant([]string{ControlBlockCopy, ControlBlockDDL}, nil, nil)
	f.adHocB = insertGrant([]string{ControlBlockDDL, ControlBlockCopy}, nil, nil)
	f.adHocC = insertGrant(nil, nil, []string{`(?i)^DELETE`})

	return f
}

// definitionOf returns the definition uid a grant ended up pointing at.
func definitionOf(t *testing.T, ctx context.Context, db *bun.DB, grantUID uuid.UUID) uuid.UUID {
	t.Helper()

	var defUID uuid.UUID

	if err := db.NewRaw("SELECT grant_definition_id FROM access_grants WHERE uid = ?", grantUID).
		Scan(ctx, &defUID); err != nil {
		t.Fatalf("read grant_definition_id for %s: %v", grantUID, err)
	}

	return defUID
}

// assertDatabaseScopeRespected covers backfill pass 2's database-scope
// condition, which was missing until 2026-08-07: shape equality alone used to
// be enough to pin a grant to a definition whose database_uids excluded the
// grant's own database. Replaying that logic against the production dbbat
// database predicted 10 such grants across 4 databases, which is what got it
// fixed rather than merely documented.
//
// It returns the definition the out-of-scope grant landed on, so the caller can
// check the corrective migration 20260807010000 leaves it where it is.
func assertDatabaseScopeRespected(
	t *testing.T, ctx context.Context, db *bun.DB, f legacyGrantFixture,
) uuid.UUID {
	t.Helper()

	// A definition scoped to another database must not claim the grant, even
	// though the shape matches exactly. It falls through to pass 3 instead.
	excludedDef := definitionOf(t, ctx, db, f.excludedGrant)
	if excludedDef == f.elsewhereDefinition {
		t.Errorf(
			"grant on database %s was pinned to definition %s, whose scope is limited to %s",
			f.dbUID, f.elsewhereDefinition, f.otherDBUID,
		)
	}

	var (
		excludedSlug     string
		excludedIsActive bool
	)

	if err := db.NewRaw("SELECT slug, is_active FROM grant_definitions WHERE uid = ?", excludedDef).
		Scan(ctx, &excludedSlug, &excludedIsActive); err != nil {
		t.Fatalf("read the definition the out-of-scope grant landed on: %v", err)
	}

	if !strings.HasPrefix(excludedSlug, "legacy-grant-shape-") {
		t.Errorf("out-of-scope grant landed on %q, want a synthesized pass-3 definition", excludedSlug)
	}

	if excludedIsActive {
		t.Error("the definition synthesized for the out-of-scope grant must be inactive")
	}

	// A definition whose scope *lists* the grant's database is a normal match.
	if got := definitionOf(t, ctx, db, f.includedGrant); got != f.hereDefinition {
		t.Errorf("in-scope grant points at %s, want the definition scoped to its database %s", got, f.hereDefinition)
	}

	// Scope is filtered before the oldest-candidate tiebreak, not after: the
	// older twin is out of scope, so the newer unscoped one has to win.
	if got := definitionOf(t, ctx, db, f.twinGrant); got != f.twinUnscopedDefinition {
		t.Errorf(
			"grant with two same-shape candidates points at %s, want the unscoped %s (the out-of-scope twin is %s)",
			got, f.twinUnscopedDefinition, f.twinElsewhereDefinition,
		)
	}

	// The end-state invariant the corrective migration also enforces: nothing
	// anywhere in the table is pinned to a definition that excludes it.
	var mismatched int

	if err := db.NewRaw(
		"SELECT count(*) FROM access_grants ag "+
			"JOIN grant_definitions gd ON gd.uid = ag.grant_definition_id "+
			"WHERE cardinality(gd.database_uids) > 0 AND NOT (ag.database_id = ANY (gd.database_uids))",
	).Scan(ctx, &mismatched); err != nil {
		t.Fatalf("count scope-mismatched grants: %v", err)
	}

	if mismatched != 0 {
		t.Errorf("%d grants are pinned to a definition whose database scope excludes them", mismatched)
	}

	return excludedDef
}

func assertBackfill(t *testing.T, ctx context.Context, db *bun.DB, f legacyGrantFixture) {
	t.Helper()

	// Nothing may be left unlinked — the migration makes the column NOT NULL,
	// so a miss would have failed the migration outright, but assert the
	// intent rather than relying on that.
	var unlinked int

	if err := db.NewRaw("SELECT count(*) FROM access_grants WHERE grant_definition_id IS NULL").
		Scan(ctx, &unlinked); err != nil {
		t.Fatalf("count unlinked grants: %v", err)
	}

	if unlinked != 0 {
		t.Errorf("%d grants left without a definition", unlinked)
	}

	// Pass 1: provenance beats shape matching. Both this grant and
	// matchingGrant share a shape, so only the request link distinguishes
	// them — and here they happen to agree, which is the point: provenance
	// never contradicts a correct match.
	if got := definitionOf(t, ctx, db, f.requestGrant); got != f.definitionUID {
		t.Errorf("request-materialized grant points at %s, want the definition it came from %s", got, f.definitionUID)
	}

	// Pass 2: an identical shape reuses the real definition rather than
	// synthesizing a duplicate. That definition is unscoped, so this is also
	// the regression guard for the scope predicate: an empty database_uids has
	// to keep matching every database, or nearly every grant in a real
	// deployment would be dumped on pass 3 and deactivated.
	if got := definitionOf(t, ctx, db, f.matchingGrant); got != f.definitionUID {
		t.Errorf("shape-matched grant points at %s, want the matching unscoped definition %s", got, f.definitionUID)
	}

	// Pass 3: duplicates collapse onto one synthesized definition...
	adHocADef := definitionOf(t, ctx, db, f.adHocA)
	adHocBDef := definitionOf(t, ctx, db, f.adHocB)

	if adHocADef != adHocBDef {
		t.Errorf("two grants with the same shape got different definitions (%s vs %s)", adHocADef, adHocBDef)
	}

	if adHocADef == f.definitionUID {
		t.Error("an unmatched shape was pointed at the pre-existing definition")
	}

	// ...and a different shape gets its own.
	adHocCDef := definitionOf(t, ctx, db, f.adHocC)
	if adHocCDef == adHocADef {
		t.Error("two different shapes collapsed onto one synthesized definition")
	}

	// The synthesized definitions describe the shape they replaced, are
	// inactive, and are recognizable as synthesized.
	var (
		slug     string
		isActive bool
		controls []string
	)

	if err := db.NewRaw("SELECT slug, is_active, controls FROM grant_definitions WHERE uid = ?", adHocADef).
		Scan(ctx, &slug, &isActive, pgdialect.Array(&controls)); err != nil {
		t.Fatalf("read synthesized definition: %v", err)
	}

	if isActive {
		t.Error("a synthesized legacy definition must be inactive")
	}

	// Only the prefix is fixed; the rest is uid-derived. The prefix is what
	// makes these recognizable as machine-generated stand-ins rather than a
	// deliberate policy, and it is what the down-migration deletes by.
	if !strings.HasPrefix(slug, "legacy-grant-shape-") {
		t.Errorf("synthesized slug %q does not carry the legacy prefix", slug)
	}

	if len(controls) != 2 {
		t.Errorf("synthesized definition controls = %v, want the two blocked controls", controls)
	}

	// And the shape columns are gone from the grants themselves — the actual
	// goal of the migration.
	for _, column := range []string{
		"controls", "max_query_counts", "max_bytes_transferred",
		"approval_patterns", "approver_group_uids",
	} {
		var exists bool

		if err := db.NewRaw(
			"SELECT EXISTS (SELECT 1 FROM information_schema.columns "+
				"WHERE table_name = 'access_grants' AND column_name = ?)", column,
		).Scan(ctx, &exists); err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}

		if exists {
			t.Errorf("access_grants.%s still exists after the migration", column)
		}
	}

	// priority explicitly stays: it is instance-level ranking, not policy.
	var priorityExists bool

	if err := db.NewRaw(
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns "+
			"WHERE table_name = 'access_grants' AND column_name = 'priority')",
	).Scan(ctx, &priorityExists); err != nil {
		t.Fatalf("check priority column: %v", err)
	}

	if !priorityExists {
		t.Error("access_grants.priority was dropped; it must survive this migration")
	}

	// The slug uniqueness is now per live row, which is what lets an edit
	// archive a version without colliding.
	var archivedSlugCollides bool

	if err := db.NewRaw(
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'grant_definitions' "+
			"AND indexname = 'grant_definitions_live_slug_uniq')",
	).Scan(ctx, &archivedSlugCollides); err != nil {
		t.Fatalf("check live slug index: %v", err)
	}

	if !archivedSlugCollides {
		t.Error("the partial unique index on live slugs is missing")
	}
}

func assertRollback(t *testing.T, ctx context.Context, db *bun.DB, f legacyGrantFixture) {
	t.Helper()

	var linkExists bool

	if err := db.NewRaw(
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns "+
			"WHERE table_name = 'access_grants' AND column_name = 'grant_definition_id')",
	).Scan(ctx, &linkExists); err != nil {
		t.Fatalf("check grant_definition_id: %v", err)
	}

	if linkExists {
		t.Error("grant_definition_id still present after rollback")
	}

	// The shape came back onto the grants, read off the definition each was
	// pinned to.
	var controls []string

	if err := db.NewRaw("SELECT controls FROM access_grants WHERE uid = ?", f.adHocA).
		Scan(ctx, pgdialect.Array(&controls)); err != nil {
		t.Fatalf("read restored controls: %v", err)
	}

	if len(controls) != 2 {
		t.Errorf("restored controls = %v, want the two blocked controls", controls)
	}

	var maxQueries *int64

	if err := db.NewRaw("SELECT max_query_counts FROM access_grants WHERE uid = ?", f.matchingGrant).
		Scan(ctx, &maxQueries); err != nil {
		t.Fatalf("read restored quota: %v", err)
	}

	if maxQueries == nil || *maxQueries != 100 {
		t.Errorf("restored max_query_counts = %v, want 100", maxQueries)
	}

	// The synthesized stand-ins are gone; the real definition stays.
	var synthesized int

	if err := db.NewRaw("SELECT count(*) FROM grant_definitions WHERE slug LIKE 'legacy-grant-shape-%'").
		Scan(ctx, &synthesized); err != nil {
		t.Fatalf("count synthesized definitions: %v", err)
	}

	if synthesized != 0 {
		t.Errorf("%d synthesized definitions survived the rollback", synthesized)
	}

	var realDefinitions int

	if err := db.NewRaw("SELECT count(*) FROM grant_definitions WHERE uid = ?", f.definitionUID).
		Scan(ctx, &realDefinitions); err != nil {
		t.Fatalf("count real definitions: %v", err)
	}

	if realDefinitions != 1 {
		t.Error("the rollback removed a definition it did not create")
	}
}

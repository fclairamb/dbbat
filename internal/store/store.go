package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/migrations"
)

// Store provides access to the database
type Store struct {
	db          *bun.DB
	storageDSN  string                    // Parsed storage DSN for security validation
	authCache   *cache.AuthCache          // Optional auth cache for API key verification
	revocations *cache.RevocationRegistry // In-process fan-out of grant revocations to live proxy sessions
	instanceID  string                    // Identifies this process among the replicas sharing this store
	runID       string                    // Identifies this *run*: minted here, never configurable

	// chainKey is the HMAC key sealing the audit and query chains — an HKDF
	// subkey of the master encryption key, never the master key itself and
	// never written anywhere. Empty disables chaining. See chain.go.
	chainKey []byte

	// queryRetention is the configured DBB_QUERY_STORAGE_RETENTION. The sweep
	// itself takes its window as an argument (CleanupOldQueryRows), so this is
	// not what drives deletion; it is what lets *verification* tell a session
	// retention emptied from one somebody emptied. See checkEmptiedChain.
	queryRetention time.Duration

	// auditChain caches the head of the store-wide audit chain; queryChains
	// caches one head per live connection; rowChains caches one head per query
	// whose result rows are being captured.
	auditChain  chainState
	queryChains *queryChains
	rowChains   *queryChains
}

// Options configures Store creation.
type Options struct {
	// DropTablesFirst drops all tables before running migrations (for test mode)
	DropTablesFirst bool

	// InstanceID identifies this dbbat process among the replicas sharing the
	// store. It is stamped on every connection row this process opens, which is
	// what lets CloseOrphanedConnections reconcile *its own* leftovers without
	// touching another replica's live sessions. Empty disables that reconcile
	// rather than widening it.
	InstanceID string

	// EncryptionKey is the master AES-256 key (DBB_KEY / DBB_KEYFILE). The
	// store keeps only an HKDF subkey of it, used to seal the tamper-evident
	// audit and query chains. Empty leaves those rows unchained — which is
	// what every test store that does not care about the chain gets, and what
	// a serving process never gets, because config always resolves a key.
	EncryptionKey []byte

	// QueryRetention is the configured query-history retention window
	// (DBB_QUERY_STORAGE_RETENTION), zero when retention is disabled — the
	// default. Chain verification needs it to judge a session whose statements
	// are *all* gone: retention can only account for that when the session ran
	// entirely before the retention cutoff. Zero therefore means "nothing
	// legitimately deletes statements here", which is the strictest reading.
	QueryRetention time.Duration
}

// New creates a new Store instance and runs migrations
func New(ctx context.Context, dsn string, opts ...Options) (*Store, error) {
	var options Options
	if len(opts) > 0 {
		options = opts[0]
	}

	// Create connection using pgdriver
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))

	// Configure connection pool
	sqldb.SetMaxOpenConns(25)
	sqldb.SetMaxIdleConns(25)
	sqldb.SetConnMaxLifetime(5 * time.Minute)

	// Create bun.DB
	db := bun.NewDB(sqldb, pgdialect.New())

	// Test connection
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	s := &Store{
		db:          db,
		storageDSN:  dsn,
		revocations: cache.NewRevocationRegistry(),
		instanceID:  options.InstanceID,
		// Minted here rather than taken from Options: the run id must identify
		// one live process, and anything an operator can set — including
		// DBB_INSTANCE_ID — can end up shared by several replicas. UUIDv7 so
		// it also sorts by start time, which makes a registry dump readable.
		runID:          newUIDv7().String(),
		queryChains:    newQueryChains(),
		rowChains:      newQueryChains(),
		queryRetention: options.QueryRetention,
	}

	if len(options.EncryptionKey) > 0 {
		chainKey, err := crypto.DeriveAuditChainKey(options.EncryptionKey)
		if err != nil {
			_ = db.Close()

			return nil, fmt.Errorf("failed to derive the audit chain key: %w", err)
		}

		s.chainKey = chainKey
	}

	// Drop-then-migrate is one schema change, so it happens under a single hold
	// of the migration lock: a replica starting next waits for the finished
	// schema rather than observing the half-dropped one.
	err := s.withMigrationLock(ctx, func(ctx context.Context) error {
		// Drop all tables first if requested (for test mode)
		if options.DropTablesFirst {
			if err := s.DropAllTables(ctx); err != nil {
				return fmt.Errorf("failed to drop tables: %w", err)
			}
		}

		// Run migrations
		if err := s.runMigrations(ctx); err != nil {
			return fmt.Errorf("failed to run migrations: %w", err)
		}

		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close closes the database connection pool
func (s *Store) Close() {
	if err := s.db.Close(); err != nil {
		slog.ErrorContext(context.Background(), "failed to close database", slog.Any("error", err))
	}
}

// Health checks if the database is healthy
func (s *Store) Health(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// DB returns the underlying bun.DB for advanced operations
func (s *Store) DB() *bun.DB {
	return s.db
}

// SetAuthCache sets the authentication cache for API key verification.
func (s *Store) SetAuthCache(authCache *cache.AuthCache) {
	s.authCache = authCache
}

// SetInstanceID sets the identifier stamped on the connection rows this process
// opens. See Options.InstanceID.
func (s *Store) SetInstanceID(instanceID string) {
	s.instanceID = instanceID
}

// InstanceID returns the identifier this process stamps on connection rows.
func (s *Store) InstanceID() string {
	return s.instanceID
}

// SetRunID overrides the run id minted by New. Tests use it to impersonate
// another run — a previous one of this instance id, or a live peer sharing the
// id; a serving process never calls it, because a run id that is not unique to
// one live process defeats the whole point of having one.
func (s *Store) SetRunID(runID string) {
	s.runID = runID
}

// RunID returns the identifier of this run: unique per live process, unlike the
// instance id, which an operator can pin to the same value on every replica.
func (s *Store) RunID() string {
	return s.runID
}

// currentRunID is the run id to stamp on the rows this process writes, or nil
// when it has none — a zero-value Store, which only tests build. NULL then
// carries the same meaning as it does on a row written before run tracking
// existed: no run vouches for it, so it is judged by its instance id alone.
func (s *Store) currentRunID() *string {
	if s.runID == "" {
		return nil
	}

	runID := s.runID

	return &runID
}

// Revocations returns the process-wide grant-revocation registry that live
// proxy sessions register with and the API's revoke handler signals. Always
// non-nil for a store built via New. Nil-safe on both a nil *Store receiver and
// a zero-value store (some tests build sessions without a real store); the
// returned registry's own methods are also nil-safe, so callers never have to
// nil-check.
func (s *Store) Revocations() *cache.RevocationRegistry {
	if s == nil {
		return nil
	}

	return s.revocations
}

// migrationAdvisoryLockKey is the PostgreSQL advisory-lock key that serializes
// the schema step across every process sharing this storage database. The value
// is the ASCII bytes of "DBBAT": arbitrary, but stable across dbbat versions
// (changing it would let an old and a new replica migrate concurrently) and
// unlikely to collide with another application's advisory locks.
const migrationAdvisoryLockKey int64 = 0x4442424154

const (
	// migrationLockWait bounds how long we wait for a peer replica to finish its
	// own migrations before giving up on the lock. Generous: a cold database
	// applying every migration from scratch is the slow case we must not cut off.
	migrationLockWait = 5 * time.Minute

	// migrationLockPoll is how often we retry pg_try_advisory_lock while waiting.
	migrationLockPoll = 100 * time.Millisecond
)

// withMigrationLock runs fn while holding the advisory lock that serializes
// schema changes across the replicas sharing this database.
//
// It exists because bun's migrator is not concurrency-safe on a *fresh*
// database: Init issues CREATE TABLE IF NOT EXISTS for bun_migrations and
// bun_migration_locks, and two of those racing inside PostgreSQL fail with
// `duplicate key value violates unique constraint "pg_class_relname_nsp_index"`
// rather than politely no-opping. Migrate is no safer — nothing stops two
// replicas from applying the same pending migration at once.
//
// The lock is taken on a connection pinned out of the pool, because a
// session-level advisory lock belongs to the session that took it: taking it on
// one pooled connection and releasing it on another would leak it for the
// lifetime of the process. fn runs on the regular pool, so it cannot deadlock
// against the pinned session — and PostgreSQL advisory locks are re-entrant
// within a session anyway, so even a nested acquisition would just stack.
//
// Not being able to lock is never fatal. A role without rights to the advisory
// lock functions, a database that does not implement them, or a peer holding
// the lock for longer than migrationLockWait all fall through to running the
// migrations unserialized — exactly the behavior that predates this lock.
func (s *Store) withMigrationLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not pin a connection for the migration lock, migrating unserialized",
			slog.Any("error", err))

		return fn(ctx)
	}

	locked := s.acquireMigrationLock(ctx, conn)

	defer func() {
		if locked {
			// Released on a context detached from ctx: a caller canceling mid-migration
			// must still give the lock back, or the next replica waits out the full
			// migrationLockWait for nothing.
			if _, err := conn.ExecContext(context.WithoutCancel(ctx),
				"SELECT pg_advisory_unlock(?)", migrationAdvisoryLockKey); err != nil {
				slog.WarnContext(ctx, "failed to release the migration advisory lock",
					slog.Any("error", err))
			}
		}

		if err := conn.Close(); err != nil {
			slog.WarnContext(ctx, "failed to return the migration lock connection to the pool",
				slog.Any("error", err))
		}
	}()

	return fn(ctx)
}

// acquireMigrationLock polls pg_try_advisory_lock on the pinned connection until
// it wins, the wait budget runs out, or the context ends. It reports whether the
// lock is actually held — the caller unlocks only then, so a failed acquisition
// can never unlock a peer's hold. Polling rather than blocking in
// pg_advisory_lock keeps a stuck peer from hanging startup forever.
func (s *Store) acquireMigrationLock(ctx context.Context, conn bun.Conn) bool {
	deadline := time.Now().Add(migrationLockWait)
	waited := false

	for {
		var acquired bool

		err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(?)", migrationAdvisoryLockKey).Scan(&acquired)
		if err != nil {
			// The functions are missing, or the role may not call them: nothing to
			// retry, so stop asking and let the migrations run unserialized.
			slog.WarnContext(ctx, "could not take the migration advisory lock, migrating unserialized",
				slog.Any("error", err))

			return false
		}

		if acquired {
			if waited {
				slog.InfoContext(ctx, "migration advisory lock acquired after waiting for another replica")
			}

			return true
		}

		if time.Now().After(deadline) {
			slog.WarnContext(ctx, "gave up waiting for the migration advisory lock, migrating unserialized",
				slog.Duration("waited", migrationLockWait))

			return false
		}

		if !waited {
			slog.InfoContext(ctx, "another replica is migrating, waiting for the migration advisory lock")

			waited = true
		}

		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "context ended while waiting for the migration advisory lock, migrating unserialized",
				slog.Any("error", ctx.Err()))

			return false
		case <-time.After(migrationLockPoll):
		}
	}
}

// runMigrations runs the database schema migrations
func (s *Store) runMigrations(ctx context.Context) error {
	migrator := migrate.NewMigrator(s.db, migrations.Migrations)

	// Initialize bun_migrations table
	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("failed to init migrator: %w", err)
	}

	// Run pending migrations
	group, err := migrator.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	if group.IsZero() {
		slog.DebugContext(ctx, "No new migrations to run")
	} else {
		slog.InfoContext(ctx, "Migrations applied", slog.Int64("group", group.ID), slog.Int("migrations", len(group.Migrations)))
	}

	return nil
}

// Migrate runs all pending migrations (for CLI command)
func (s *Store) Migrate(ctx context.Context) error {
	return s.withMigrationLock(ctx, s.runMigrations)
}

// Rollback rolls back the last migration group
func (s *Store) Rollback(ctx context.Context) error {
	return s.withMigrationLock(ctx, s.rollback)
}

func (s *Store) rollback(ctx context.Context) error {
	migrator := migrate.NewMigrator(s.db, migrations.Migrations)

	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("failed to init migrator: %w", err)
	}

	group, err := migrator.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("failed to rollback migrations: %w", err)
	}

	if group.IsZero() {
		slog.InfoContext(ctx, "No migrations to rollback")
	} else {
		slog.InfoContext(ctx, "Migrations rolled back", slog.Int64("group", group.ID), slog.Int("migrations", len(group.Migrations)))
	}

	return nil
}

// MigrationInfo contains information about a migration
type MigrationInfo struct {
	Name       string
	MigratedAt time.Time
}

// DropAllTables drops all application tables and types (for test mode)
// This should be called BEFORE migrations to ensure a fresh start
func (s *Store) DropAllTables(ctx context.Context) error {
	// Tables to drop in order (respecting foreign key constraints)
	// Must be in reverse dependency order
	tables := []string{
		"query_rows",
		"queries",
		"connections",
		"instances",
		"grant_requests",
		"grant_definitions",
		"access_grants",
		"api_keys",
		"audit_log",
		"oauth_states",
		"user_identities",
		"user_group_members",
		"user_groups",
		"server_group_members",
		"server_groups",
		"global_parameters",
		"servers",
		"databases", // pre-20260716120000 table name; drop in case of a stale dev DB
		"users",
		"bun_migrations",
		"bun_migration_locks",
	}

	for _, table := range tables {
		_, err := s.db.NewDropTable().
			Table(table).
			IfExists().
			Cascade().
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("failed to drop table %s: %w", table, err)
		}
	}

	// Drop custom types
	types := []string{
		"user_role",
	}

	for _, typeName := range types {
		_, err := s.db.ExecContext(ctx, "DROP TYPE IF EXISTS "+typeName+" CASCADE")
		if err != nil {
			return fmt.Errorf("failed to drop type %s: %w", typeName, err)
		}
	}

	slog.InfoContext(ctx, "All tables and types dropped for test mode")
	return nil
}

// MigrationStatus returns the status of all migrations
func (s *Store) MigrationStatus(ctx context.Context) ([]MigrationInfo, error) {
	var result []MigrationInfo

	// Reads the migration state, but Init still creates the bookkeeping tables on
	// a fresh database, so it races a starting replica exactly like Migrate does.
	err := s.withMigrationLock(ctx, func(ctx context.Context) error {
		migrator := migrate.NewMigrator(s.db, migrations.Migrations)

		if err := migrator.Init(ctx); err != nil {
			return fmt.Errorf("failed to init migrator: %w", err)
		}

		ms, err := migrator.MigrationsWithStatus(ctx)
		if err != nil {
			return fmt.Errorf("failed to get migration status: %w", err)
		}

		result = make([]MigrationInfo, len(ms))
		for i, m := range ms {
			result[i] = MigrationInfo{
				Name:       m.Name,
				MigratedAt: m.MigratedAt,
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// DSNComponents holds parsed PostgreSQL DSN components for comparison
type DSNComponents struct {
	Host   string
	Port   string
	Server string
}

// parsePostgresDSN parses a PostgreSQL DSN and extracts host, port, and database.
// Supports both URL format (postgres://...) and key-value format (host=... port=...).
func parsePostgresDSN(dsn string) (*DSNComponents, error) {
	// Try URL format first
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to parse DSN URL: %w", err)
		}

		host := u.Hostname()
		port := u.Port()
		if port == "" {
			port = "5432"
		}

		database := strings.TrimPrefix(u.Path, "/")

		return &DSNComponents{
			Host:   normalizeHost(host),
			Port:   port,
			Server: database,
		}, nil
	}

	// Parse key-value format
	components := &DSNComponents{
		Port: "5432", // default
	}

	// Split on spaces, handling potential quoted values
	parts := strings.Fields(dsn)
	for _, part := range parts {
		idx := strings.Index(part, "=")
		if idx == -1 {
			continue
		}
		key := strings.ToLower(part[:idx])
		value := part[idx+1:]
		// Remove quotes if present
		value = strings.Trim(value, "'\"")

		switch key {
		case "host":
			components.Host = normalizeHost(value)
		case "port":
			components.Port = value
		case "dbname", "database":
			components.Server = value
		}
	}

	return components, nil
}

// normalizeHost normalizes host names to allow comparison.
// Treats localhost, 127.0.0.1, and ::1 as equivalent.
func normalizeHost(host string) string {
	host = strings.ToLower(host)
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return "localhost"
	default:
		return host
	}
}

// MatchesStorageDSN checks if a target database configuration matches the storage DSN.
// Returns true if the target appears to be the same database as DBBat storage.
func (s *Store) MatchesStorageDSN(host string, port int, databaseName string) bool {
	storage, err := parsePostgresDSN(s.storageDSN)
	if err != nil {
		// If we can't parse the storage DSN, err on the side of caution
		slog.WarnContext(context.Background(), "failed to parse storage DSN for comparison", slog.Any("error", err))
		return false
	}

	targetPort := fmt.Sprintf("%d", port)
	targetHost := normalizeHost(host)

	return storage.Host == targetHost &&
		storage.Port == targetPort &&
		storage.Server == databaseName
}

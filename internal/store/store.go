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
	"github.com/fclairamb/dbbat/internal/migrations"
)

// Store provides access to the database
type Store struct {
	db         *bun.DB
	storageDSN string           // Parsed storage DSN for security validation
	authCache  *cache.AuthCache // Optional auth cache for API key verification
}

// Options configures Store creation.
type Options struct {
	// DropTablesFirst drops all tables before running migrations (for test mode)
	DropTablesFirst bool
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

	s := &Store{db: db, storageDSN: dsn}

	// Drop all tables first if requested (for test mode)
	if options.DropTablesFirst {
		if err := s.DropAllTables(ctx); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to drop tables: %w", err)
		}
	}

	// Run migrations
	if err := s.runMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return s, nil
}

// Close closes the database connection pool
func (s *Store) Close() {
	if err := s.db.Close(); err != nil {
		slog.Error("failed to close database", "error", err)
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
		slog.Debug("No new migrations to run")
	} else {
		slog.Info("Migrations applied", "group", group.ID, "migrations", len(group.Migrations))
	}

	return nil
}

// Migrate runs all pending migrations (for CLI command)
func (s *Store) Migrate(ctx context.Context) error {
	return s.runMigrations(ctx)
}

// Rollback rolls back the last migration group
func (s *Store) Rollback(ctx context.Context) error {
	migrator := migrate.NewMigrator(s.db, migrations.Migrations)

	if err := migrator.Init(ctx); err != nil {
		return fmt.Errorf("failed to init migrator: %w", err)
	}

	group, err := migrator.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("failed to rollback migrations: %w", err)
	}

	if group.IsZero() {
		slog.Info("No migrations to rollback")
	} else {
		slog.Info("Migrations rolled back", "group", group.ID, "migrations", len(group.Migrations))
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
		"access_grants",
		"api_keys",
		"audit_logs",
		"databases",
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

	slog.Info("All tables and types dropped for test mode")
	return nil
}

// MigrationStatus returns the status of all migrations
func (s *Store) MigrationStatus(ctx context.Context) ([]MigrationInfo, error) {
	migrator := migrate.NewMigrator(s.db, migrations.Migrations)

	if err := migrator.Init(ctx); err != nil {
		return nil, fmt.Errorf("failed to init migrator: %w", err)
	}

	ms, err := migrator.MigrationsWithStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get migration status: %w", err)
	}

	result := make([]MigrationInfo, len(ms))
	for i, m := range ms {
		result[i] = MigrationInfo{
			Name:       m.Name,
			MigratedAt: m.MigratedAt,
		}
	}

	return result, nil
}

// DSNComponents holds parsed PostgreSQL DSN components for comparison
type DSNComponents struct {
	Host     string
	Port     string
	Database string
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
			Host:     normalizeHost(host),
			Port:     port,
			Database: database,
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
			components.Database = value
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
		slog.Warn("failed to parse storage DSN for comparison", "error", err)
		return false
	}

	targetPort := fmt.Sprintf("%d", port)
	targetHost := normalizeHost(host)

	return storage.Host == targetHost &&
		storage.Port == targetPort &&
		storage.Database == databaseName
}

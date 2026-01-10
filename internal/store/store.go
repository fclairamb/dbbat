package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/fclairamb/dbbat/internal/migrations"
)

// Store provides access to the database
type Store struct {
	db *bun.DB
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

	s := &Store{db: db}

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

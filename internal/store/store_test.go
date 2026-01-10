package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	testContainer       *postgres.PostgresContainer
	testDSN             string
	containerOnce       sync.Once
	errContainerStartup error
	containerCtx        context.Context
	containerCancel     context.CancelFunc
)

// setupPostgresContainer starts a PostgreSQL container for testing.
// The container is reused across all tests in the package.
func setupPostgresContainer(t *testing.T) string {
	t.Helper()

	containerOnce.Do(func() {
		containerCtx, containerCancel = context.WithCancel(context.Background())

		testContainer, errContainerStartup = postgres.Run(containerCtx,
			"postgres:15-alpine",
			postgres.WithDatabase("dbbat_test"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if errContainerStartup != nil {
			return
		}

		testDSN, errContainerStartup = testContainer.ConnectionString(containerCtx, "sslmode=disable")
	})

	if errContainerStartup != nil {
		t.Fatalf("failed to start postgres container: %v", errContainerStartup)
	}

	return testDSN
}

// setupTestStore creates a Store for testing and cleans up tables.
func setupTestStore(t *testing.T) *Store {
	t.Helper()

	dsn := setupPostgresContainer(t)
	ctx := context.Background()

	store, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// Clean up tables in correct order (respecting foreign keys)
	cleanupTables := []string{
		"query_rows",
		"queries",
		"connections",
		"access_grants",
		"audit_log",
		"databases",
		"users",
	}

	for _, table := range cleanupTables {
		_, err := store.db.ExecContext(ctx, "DELETE FROM "+table)
		if err != nil {
			store.Close()
			t.Fatalf("failed to clean up table %s: %v", table, err)
		}
	}

	t.Cleanup(func() {
		store.Close()
	})

	return store
}

func TestNew(t *testing.T) {
	dsn := setupPostgresContainer(t)
	ctx := context.Background()

	t.Run("valid DSN", func(t *testing.T) {
		store, err := New(ctx, dsn)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		defer store.Close()

		if store.db == nil {
			t.Error("New() db is nil")
		}
	})

	t.Run("invalid DSN", func(t *testing.T) {
		_, err := New(ctx, "postgres://invalid:invalid@localhost:9999/nonexistent?connect_timeout=1")
		if err == nil {
			t.Error("New() expected error for invalid DSN")
		}
	})
}

func TestHealth(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	err := store.Health(ctx)
	if err != nil {
		t.Errorf("Health() error = %v", err)
	}
}

func TestDB(t *testing.T) {
	store := setupTestStore(t)

	db := store.DB()
	if db == nil {
		t.Error("DB() returned nil")
	}
}

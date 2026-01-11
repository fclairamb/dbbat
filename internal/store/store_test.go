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

func TestParsePostgresDSN(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		wantHost string
		wantPort string
		wantDB   string
		wantErr  bool
	}{
		{
			name:     "URL format with explicit port",
			dsn:      "postgres://user:pass@myhost:5433/mydb",
			wantHost: "myhost",
			wantPort: "5433",
			wantDB:   "mydb",
		},
		{
			name:     "URL format with default port",
			dsn:      "postgres://user:pass@myhost/mydb",
			wantHost: "myhost",
			wantPort: "5432",
			wantDB:   "mydb",
		},
		{
			name:     "URL format with localhost",
			dsn:      "postgres://user:pass@localhost:5432/dbbat",
			wantHost: "localhost",
			wantPort: "5432",
			wantDB:   "dbbat",
		},
		{
			name:     "URL format with 127.0.0.1 normalized to localhost",
			dsn:      "postgres://user:pass@127.0.0.1:5432/dbbat",
			wantHost: "localhost",
			wantPort: "5432",
			wantDB:   "dbbat",
		},
		{
			name:     "URL format with IPv6 localhost normalized",
			dsn:      "postgres://user:pass@::1:5432/dbbat",
			wantHost: "localhost",
			wantPort: "5432",
			wantDB:   "dbbat",
		},
		{
			name:     "postgresql:// scheme",
			dsn:      "postgresql://user:pass@myhost:5432/mydb",
			wantHost: "myhost",
			wantPort: "5432",
			wantDB:   "mydb",
		},
		{
			name:     "key-value format",
			dsn:      "host=myhost port=5433 dbname=mydb user=test password=test",
			wantHost: "myhost",
			wantPort: "5433",
			wantDB:   "mydb",
		},
		{
			name:     "key-value format with default port",
			dsn:      "host=myhost dbname=mydb user=test password=test",
			wantHost: "myhost",
			wantPort: "5432",
			wantDB:   "mydb",
		},
		{
			name:     "key-value format with localhost",
			dsn:      "host=localhost port=5432 dbname=dbbat user=test password=test",
			wantHost: "localhost",
			wantPort: "5432",
			wantDB:   "dbbat",
		},
		{
			name:     "key-value format with 127.0.0.1 normalized",
			dsn:      "host=127.0.0.1 port=5432 dbname=dbbat user=test password=test",
			wantHost: "localhost",
			wantPort: "5432",
			wantDB:   "dbbat",
		},
		{
			name:     "key-value format with quoted values",
			dsn:      "host='myhost' port='5433' dbname='mydb' user='test'",
			wantHost: "myhost",
			wantPort: "5433",
			wantDB:   "mydb",
		},
		{
			name:     "key-value format with database key",
			dsn:      "host=myhost port=5432 database=mydb user=test",
			wantHost: "myhost",
			wantPort: "5432",
			wantDB:   "mydb",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePostgresDSN(tt.dsn)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePostgresDSN() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if got.Host != tt.wantHost {
				t.Errorf("parsePostgresDSN() Host = %v, want %v", got.Host, tt.wantHost)
			}
			if got.Port != tt.wantPort {
				t.Errorf("parsePostgresDSN() Port = %v, want %v", got.Port, tt.wantPort)
			}
			if got.Database != tt.wantDB {
				t.Errorf("parsePostgresDSN() Database = %v, want %v", got.Database, tt.wantDB)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"localhost", "localhost"},
		{"LOCALHOST", "localhost"},
		{"127.0.0.1", "localhost"},
		{"::1", "localhost"},
		{"myhost.example.com", "myhost.example.com"},
		{"MYHOST.EXAMPLE.COM", "myhost.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := normalizeHost(tt.input); got != tt.want {
				t.Errorf("normalizeHost(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMatchesStorageDSN(t *testing.T) {
	tests := []struct {
		name       string
		storageDSN string
		host       string
		port       int
		dbName     string
		want       bool
	}{
		{
			name:       "exact match URL format",
			storageDSN: "postgres://user:pass@myhost:5432/dbbat",
			host:       "myhost",
			port:       5432,
			dbName:     "dbbat",
			want:       true,
		},
		{
			name:       "localhost vs 127.0.0.1 match",
			storageDSN: "postgres://user:pass@localhost:5432/dbbat",
			host:       "127.0.0.1",
			port:       5432,
			dbName:     "dbbat",
			want:       true,
		},
		{
			name:       "127.0.0.1 vs localhost match",
			storageDSN: "postgres://user:pass@127.0.0.1:5432/dbbat",
			host:       "localhost",
			port:       5432,
			dbName:     "dbbat",
			want:       true,
		},
		{
			name:       "different host",
			storageDSN: "postgres://user:pass@myhost:5432/dbbat",
			host:       "otherhost",
			port:       5432,
			dbName:     "dbbat",
			want:       false,
		},
		{
			name:       "different port",
			storageDSN: "postgres://user:pass@myhost:5432/dbbat",
			host:       "myhost",
			port:       5433,
			dbName:     "dbbat",
			want:       false,
		},
		{
			name:       "different database",
			storageDSN: "postgres://user:pass@myhost:5432/dbbat",
			host:       "myhost",
			port:       5432,
			dbName:     "other_db",
			want:       false,
		},
		{
			name:       "key-value format match",
			storageDSN: "host=myhost port=5432 dbname=dbbat user=test password=test",
			host:       "myhost",
			port:       5432,
			dbName:     "dbbat",
			want:       true,
		},
		{
			name:       "key-value format no match",
			storageDSN: "host=myhost port=5432 dbname=dbbat user=test password=test",
			host:       "myhost",
			port:       5432,
			dbName:     "other_db",
			want:       false,
		},
		{
			name:       "default port in DSN vs explicit 5432",
			storageDSN: "postgres://user:pass@myhost/dbbat",
			host:       "myhost",
			port:       5432,
			dbName:     "dbbat",
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Store{storageDSN: tt.storageDSN}
			if got := s.MatchesStorageDSN(tt.host, tt.port, tt.dbName); got != tt.want {
				t.Errorf("MatchesStorageDSN() = %v, want %v", got, tt.want)
			}
		})
	}
}

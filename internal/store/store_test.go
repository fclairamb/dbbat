package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun/driver/pgdriver"
)

// testTemplateDB is migrated once and then only ever read as the source of
// CREATE DATABASE ... TEMPLATE. Nothing may connect to it afterwards:
// PostgreSQL refuses to copy a template another session is attached to.
const testTemplateDB = "dbbat_store_template"

// testMaxConns is the connection budget one test store gets.
//
// store.New sizes its pool for a server, not for a hundred of them: the whole
// package now runs in parallel, every test opens its own store, and 25
// connections each blows past PostgreSQL's max_connections long before the
// tests do anything interesting ("sorry, too many clients already"). A test
// store issues a handful of statements at a time, so a small pool costs it
// nothing. testContainerMaxConns raises the server-side ceiling to match.
const (
	testMaxConns          = 4
	testContainerMaxConns = "300"
)

// testChainMasterKey is the master key every test store derives its chain key
// from. A fixed 32-byte test vector, not a secret.
var testChainMasterKey = bytes.Repeat([]byte{0x2a}, 32)

var (
	testContainer       *postgres.PostgresContainer
	testDSN             string
	testAdminDB         *sql.DB
	testDBSeq           atomic.Uint64
	containerOnce       sync.Once
	errContainerStartup error
	containerCtx        context.Context
	containerCancel     context.CancelFunc

	errTemplateBusy = errors.New("template database still has open connections")
)

// setupPostgresContainer starts the PostgreSQL container shared by this package
// and prepares the migrated template every test store is cloned from. It
// returns the DSN of the container's default database, which is only useful to
// tests that want to point somewhere else on the same server.
func setupPostgresContainer(t *testing.T) string {
	t.Helper()

	containerOnce.Do(prepareTestContainer)

	if errContainerStartup != nil {
		t.Fatalf("failed to start postgres container: %v", errContainerStartup)
	}

	return testDSN
}

// prepareTestContainer boots the container, opens the admin connection used to
// create and drop the per-test databases, and migrates the template.
func prepareTestContainer() {
	containerCtx, containerCancel = context.WithCancel(context.Background())

	testContainer, errContainerStartup = postgres.Run(containerCtx,
		"postgres:15-alpine",
		postgres.WithDatabase("dbbat_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithCmdArgs("-c", "max_connections="+testContainerMaxConns),
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
	if errContainerStartup != nil {
		return
	}

	// Attached to the container's default database, never to the template.
	testAdminDB = sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(testDSN)))
	testAdminDB.SetMaxOpenConns(testMaxConns)
	testAdminDB.SetMaxIdleConns(testMaxConns)

	if _, err := testAdminDB.ExecContext(containerCtx, "CREATE DATABASE "+testTemplateDB); err != nil {
		errContainerStartup = fmt.Errorf("failed to create the template database: %w", err)

		return
	}

	// Migrate exactly once, here. Every test then clones the finished schema,
	// which PostgreSQL does as a file copy instead of replaying the migrations.
	migrateStore, err := New(containerCtx, testDatabaseDSN(testTemplateDB))
	if err != nil {
		errContainerStartup = fmt.Errorf("failed to migrate the template database: %w", err)

		return
	}

	migrateStore.Close()

	errContainerStartup = waitForTemplateIdle(containerCtx)
}

// testDatabaseDSN points the container DSN at another database on the same
// server.
func testDatabaseDSN(name string) string {
	parsed, err := url.Parse(testDSN)
	if err != nil {
		panic("test container DSN is not a URL: " + err.Error())
	}

	parsed.Path = "/" + name

	return parsed.String()
}

// waitForTemplateIdle blocks until no backend is attached to the template.
// Closing a *sql.DB hands its connections back for teardown; the server-side
// backends disappear a moment later, and CREATE DATABASE ... TEMPLATE fails
// outright if one is still there.
func waitForTemplateIdle(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)

	for {
		var backends int

		row := testAdminDB.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_stat_activity WHERE datname = $1", testTemplateDB)
		if err := row.Scan(&backends); err != nil {
			return fmt.Errorf("failed to count template backends: %w", err)
		}

		if backends == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return errTemplateBusy
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// setupTestStoreNoCleanup is setupTestStore. The two names date from when every
// test shared one database and the only choice was whether to wipe it on the
// way in; both now hand out a private database, so there is nothing left to
// clean up either way.
func setupTestStoreNoCleanup(t *testing.T) *Store {
	t.Helper()

	return setupTestStore(t)
}

// setupTestStore gives the calling test its own already-migrated database.
//
// Tests in this package run in parallel against one shared container. They used
// to share a single database, and setupTestStore wiped every table on the way
// in — which deleted the rows a parallel test was still using, cascading a
// users row into that test's user_identities and api_keys. A database per test
// removes the shared state instead of trying to time the wipes; it is cheap
// because CREATE DATABASE ... TEMPLATE copies the already-migrated files rather
// than replaying the migrations.
func setupTestStore(t *testing.T) *Store {
	t.Helper()

	setupPostgresContainer(t)

	ctx := context.Background()
	// Built from a literal prefix and a counter: a database name is an
	// identifier, so it cannot go through a placeholder.
	name := fmt.Sprintf("dbbat_store_%d", testDBSeq.Add(1))

	if _, err := testAdminDB.ExecContext(ctx,
		"CREATE DATABASE "+name+" TEMPLATE "+testTemplateDB); err != nil {
		t.Fatalf("failed to create test database %s: %v", name, err)
	}

	// Every test store seals its audit and query chains, so the whole package
	// exercises the chained write paths rather than only the tests that care.
	store, err := New(ctx, testDatabaseDSN(name), Options{EncryptionKey: testChainMasterKey})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	store.db.SetMaxOpenConns(testMaxConns)
	store.db.SetMaxIdleConns(testMaxConns)

	t.Cleanup(func() {
		store.Close()

		// FORCE: a test that failed mid-call can leave a connection behind, and
		// a pinned database would fail the drop and leak.
		if _, err := testAdminDB.ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("failed to drop test database %s: %v", name, err)
		}
	})

	return store
}

func TestNew(t *testing.T) {
	t.Parallel()

	dsn := setupPostgresContainer(t)
	ctx := context.Background()

	t.Run("valid DSN", func(t *testing.T) {
		t.Parallel()

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
		t.Parallel()

		_, err := New(ctx, "postgres://invalid:invalid@localhost:9999/nonexistent?connect_timeout=1")
		if err == nil {
			t.Error("New() expected error for invalid DSN")
		}
	})
}

func TestHealth(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)
	ctx := context.Background()

	err := store.Health(ctx)
	if err != nil {
		t.Errorf("Health() error = %v", err)
	}
}

func TestDB(t *testing.T) {
	t.Parallel()

	store := setupTestStore(t)

	db := store.DB()
	if db == nil {
		t.Error("DB() returned nil")
	}
}

func TestParsePostgresDSN(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

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
			if got.Server != tt.wantDB {
				t.Errorf("parsePostgresDSN() Server = %v, want %v", got.Server, tt.wantDB)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			if got := normalizeHost(tt.input); got != tt.want {
				t.Errorf("normalizeHost(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMatchesStorageDSN(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			s := &Store{storageDSN: tt.storageDSN}
			if got := s.MatchesStorageDSN(tt.host, tt.port, tt.dbName); got != tt.want {
				t.Errorf("MatchesStorageDSN() = %v, want %v", got, tt.want)
			}
		})
	}
}

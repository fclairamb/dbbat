package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// freshDatabaseDSN creates an empty database on the shared test container and
// returns a DSN pointing at it. Every test that needs to observe what happens on
// a *fresh* schema needs its own database: the package-level one is migrated
// once at container startup, so it can never exercise the first-run path again.
func freshDatabaseDSN(t *testing.T, name string) string {
	t.Helper()

	ctx := context.Background()
	admin := setupTestStoreNoCleanup(t)

	// Identifier, not a value, so it cannot be a placeholder. The name is built
	// from a test-controlled prefix and a timestamp, and rejected below if it
	// contains anything but the characters that makes safe.
	if strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		t.Fatalf("unsafe database name %q", name)
	}

	if _, err := admin.DB().ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatalf("failed to drop leftover database %s: %v", name, err)
	}

	if _, err := admin.DB().ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("failed to create database %s: %v", name, err)
	}

	t.Cleanup(func() {
		// FORCE so a store whose Close was skipped by a failing test cannot pin
		// the database and fail the drop.
		if _, err := admin.DB().ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("failed to drop database %s: %v", name, err)
		}
	})

	parsed, err := url.Parse(setupPostgresContainer(t))
	if err != nil {
		t.Fatalf("failed to parse container DSN: %v", err)
	}

	parsed.Path = "/" + name

	return parsed.String()
}

// TestNewMigratesConcurrentlyOnFreshDatabase is the regression test for the
// first-run migration race: several processes (here, goroutines) calling New at
// the same moment against a database that has never been migrated.
//
// Before New serialised its schema step on an advisory lock, this failed
// reliably — bun's migrator Init issues CREATE TABLE IF NOT EXISTS, and two of
// those racing inside PostgreSQL surface as
// `duplicate key value violates unique constraint "pg_class_relname_nsp_index"`
// rather than one of them quietly no-opping.
func TestNewMigratesConcurrentlyOnFreshDatabase(t *testing.T) {
	dsn := freshDatabaseDSN(t, fmt.Sprintf("dbbat_race_%d", time.Now().UnixNano()))

	const racers = 8

	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		stores = make([]*Store, racers)
		errs   = make([]error, racers)
	)

	for i := range racers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			stores[i], errs[i] = New(context.Background(), dsn)
		}()
	}

	close(start)
	wg.Wait()

	defer func() {
		for _, s := range stores {
			if s != nil {
				s.Close()
			}
		}
	}()

	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d failed to create the store: %v", i, err)
		}

		if err == nil && stores[i] == nil {
			t.Errorf("racer %d returned neither a store nor an error", i)
		}
	}

	if t.Failed() {
		return
	}

	// Every racer succeeding is not enough: the migrations must also have been
	// applied exactly once. Two racers slipping past each other would each append
	// their own row for the same migration name.
	var duplicates int

	err := stores[0].DB().NewRaw(
		"SELECT count(*) FROM (SELECT name FROM bun_migrations GROUP BY name HAVING count(*) > 1) dupes",
	).Scan(context.Background(), &duplicates)
	if err != nil {
		t.Fatalf("failed to count duplicate migrations: %v", err)
	}

	if duplicates != 0 {
		t.Errorf("expected each migration to be applied once, found %d applied more than once", duplicates)
	}
}

// TestWithMigrationLockSerialisesAndReleases checks the lock's own contract:
// only one holder at a time, and the lock is handed back on every exit path,
// including the one where the guarded work fails.
func TestWithMigrationLockSerialisesAndReleases(t *testing.T) {
	store := setupTestStoreNoCleanup(t)
	ctx := context.Background()

	errBoom := errors.New("boom") //nolint:err113 // sentinel used only by this test

	if err := store.withMigrationLock(ctx, func(context.Context) error {
		return errBoom
	}); err == nil {
		t.Fatal("expected withMigrationLock to surface the callback's error")
	}

	// The failing call above must not have kept the lock: a second acquisition on
	// a *different* connection would otherwise block until the wait budget runs
	// out and then silently proceed unlocked.
	var (
		held    bool
		mu      sync.Mutex
		overlap bool
		wg      sync.WaitGroup
	)

	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := store.withMigrationLock(ctx, func(context.Context) error {
				mu.Lock()

				if held {
					overlap = true
				}

				held = true
				mu.Unlock()

				time.Sleep(20 * time.Millisecond)

				mu.Lock()
				held = false
				mu.Unlock()

				return nil
			}); err != nil {
				t.Errorf("withMigrationLock returned an error: %v", err)
			}
		}()
	}

	wg.Wait()

	if overlap {
		t.Error("two callbacks held the migration lock at the same time")
	}
}

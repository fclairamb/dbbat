# De-flake parallel API tests racing on first-run migrations

## Goal
Make `setupTestServer` (internal/api/password_reset_test.go) safe for
parallel tests that are the first to touch a fresh test container.

## Why
Each test calls `store.New`, which runs migrations. When two parallel tests
are the first users of the shared PostgreSQL testcontainer (easy to hit with
a narrow `-run` filter, e.g.
`go test ./internal/api/ -run 'TestOAuthLoginRedirectRoundTrip|TestFindOrCreateOAuthUser'`),
both run `migrator.Init()` concurrently and one dies with:

```
failed to init migrator: ERROR: duplicate key value violates unique
constraint "pg_class_relname_nsp_index" (SQLSTATE=23505)
```

The full-suite run usually escapes it by luck of scheduling, so this mostly
bites focused local runs, but it is a real race.

## Implementation
- Simplest: run migrations once right after the container starts, inside the
  `containerOnce.Do` block in `setupPostgresContainer`, so `store.New` calls
  from tests find the schema already in place (migrator init becomes a
  no-op read).
- Alternative: guard `store.New`'s migration step with a PostgreSQL advisory
  lock (`pg_advisory_lock`) so concurrent replicas — tests or real ones —
  serialize; that would also harden multi-replica production startups.

No GitHub issue filed yet — one should be created.

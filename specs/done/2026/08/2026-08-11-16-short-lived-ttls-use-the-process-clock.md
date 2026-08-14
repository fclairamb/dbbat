# Short-lived TTLs are stamped from the process clock but expired by the store's

## Goal

Audit the remaining rows whose `expires_at` is written from `time.Now()` in Go
and then filtered with `expires_at > NOW()` in SQL, and decide for each whether
it should be stamped from the database clock (`Store.Now` / `dbNow`, added in
`internal/store/store.go`) like grant windows now are.

No GitHub issue filed yet — one should be.

## Why

`specs/todos/2026-08-11-04-flaky-server-group-grant-binding-test.md` found that
a grant window stamped from the process clock is refused by the auth path for
as long as dbbat's clock runs ahead of PostgreSQL's, because the admission test
(`starts_at <= NOW()`) is evaluated by PostgreSQL. Grant issuance was fixed;
the same two-clock pattern is still used by every short TTL in the store:

- `internal/store/device_auth.go:86` — `ExpiresAt: time.Now().Add(DeviceAuthTTL)`,
  read back with `expires_at > NOW()` (four call sites).
- `internal/store/oauth_exchange.go:53` — `time.Now().Add(LoginExchangeTTL)`,
  same filter.
- `internal/store/oauth_states.go` — the OAuth state rows, written by their
  caller and swept with `expires_at <= NOW()`.

These are the security-relevant direction of the same bug: a process running
*behind* its store shortens the TTL (a device authorization that dies before
the device polls), one running *ahead* lengthens it (a login state that stays
redeemable past its intended life). The windows are seconds-to-minutes long, so
a few milliseconds of skew is not urgent — but it is the same defect, and the
helper to fix it now exists.

## Implementation

- For each site, replace the Go-stamped `expires_at` with a database-stamped
  one. Inside a transaction use `dbNow(ctx, tx)`; outside, either `Store.Now`
  or, cheaper, an SQL-side `NOW() + make_interval(secs => ?)` value on the
  insert so no extra round trip is needed.
- Prefer the SQL-side form where the row is inserted in one statement — it is
  one clock and one trip.
- Regression coverage: `setupTestStoreWithClockSkew` (in
  `internal/store/store_test.go`) shadows `NOW()` on a per-test database and
  already makes this class of bug deterministic — a device-auth test under a
  few seconds of skew is enough.
- While there: check the proxy side for the mirror case (a live session
  comparing `grant.ExpiresAt`, stamped by the store, against `time.Now()`),
  and decide whether cutting a session a few milliseconds early or late is
  worth closing.

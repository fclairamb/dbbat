---
model: opus
effort: medium
---

# Login throttling is per username only, so anyone can keep an account locked out

## Problem

Found by the security scan of 2026-10-30 (finding L6).

Severity: Low. Triggered by an unauthenticated network client that knows a username (`admin` always exists).

`authFailureTracker` keys only on the username (`internal/api/middleware.go:219-349`). From 10 failures, each new failure blocks the account for 5 minutes (`authBackoffDelays`, `middleware.go:241-249`), and it is checked before the password on `POST /auth/login` (`internal/api/auth.go:60-67`), on Basic Auth (`middleware.go:94-99`) and on the pre-login password change. One wrong password every 5 minutes, from anywhere, keeps the named account locked out of password login indefinitely. `/auth/login` also has no per-IP limit, so password spraying across many usernames is not throttled per source.

## Proposal

1. Key the backoff on `(username, client IP)` instead of the username alone, so an attacker's failures lock out the attacker's source, not the account. Add a per-IP limiter (`PreAuthMiddleware`) on `/auth/login` and `PUT /auth/password` against spraying. Both depend on a trustworthy client IP (`2026-10-30-12`).
2. Keep a coarse per-username ceiling (for example 100 failures per hour) as a backstop against distributed guessing, logged and audited rather than silently locking.

## Tests

- `internal/api/auth_failures_test.go`: failures from IP A do not block a correct login for the same username from IP B. This fails before the fix.
- Positive control: repeated failures from one IP are still throttled with `Retry-After`.

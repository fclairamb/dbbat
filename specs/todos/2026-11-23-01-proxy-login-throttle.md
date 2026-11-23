---
model: opus
effort: high
pr: batch
automerge: false
---

# The proxy listeners let anyone guess dbbat passwords with no throttle, ceiling or audit trail

## Problem

Found by the security scan of 2026-11-23 (finding M1). Severity: Medium. Anyone who can reach a proxy port can trigger it, no account needed.

The PostgreSQL, MySQL/MariaDB and SQL Server listeners authenticate a dbbat **user password** (the same Argon2id hash the REST API checks), not only API keys:

- PostgreSQL: `internal/proxy/postgresql/auth.go:167-213` (`verifyPassword`, cleartext exchange, then `authCache.VerifyPassword` / `crypto.VerifyPassword`).
- MySQL/MariaDB: `internal/proxy/mysql/auth.go:104-130` (`verifyCredentials`).
- SQL Server: `internal/proxy/mssql/auth.go:96-124` (`verifyCredentials`).

None of them applies any of the protections the API puts around the same check:

- per-source backoff: `checkAuthThrottle`, `internal/api/middleware.go:490`;
- the per-username failure ceiling: `failAuth` / `noteAuthFailure`, `middleware.go:509-560`;
- the chained `auth.failure_ceiling` audit entry: `internal/store/auth_audit.go:11`.

The only bound is the 30s pre-auth socket deadline. MongoDB adds a fixed 250ms delay per failure (`internal/proxy/mongodb/auth.go:20`), but it has no counter either, and parallel connections make the delay moot. Oracle authenticates API keys only, which have far too much entropy to guess.

So an attacker on the network opens connections in a loop against any username (`admin` included) and an accepted database name. Each attempt costs one TCP connection and one Argon2id verify. There is no 429 and no lockout, and nothing reaches `audit_log`. A weak password falls to an online guessing rate the API exists to prevent, one port over. A guessed admin password is also a REST API login, unless the account is SSO-only.

## Proposal

1. Share the API's limiter instead of building a second one. Move the `authFailureTracker` logic (`internal/api/middleware.go`, per (username, source IP) backoff plus the per-username ceiling) into a package both `api` and the proxies can import, for example `internal/cache` next to `AuthCache`. Keep its single-process semantics and its sweeper.
2. Call it from the three password paths above, keyed on the user name and the client's source IP (`clientConn.RemoteAddr()`):
   - before the hash check, refuse with the protocol's ordinary authentication error while the source is in backoff (the same timing as the API, so it is not an oracle);
   - record a failure on a wrong password or an unknown user;
   - clear the source on success.
   The API-key branch (`isAPIKey`) can stay unthrottled, as it is on the API.
3. Write the same `auth.failure_ceiling` audit entry when the ceiling is reached, with a `listener` field (`postgresql` / `mysql` / `mssql` / `mongodb`), so a guessing campaign leaves evidence in the chain.
4. MongoDB: replace the fixed delay with the shared limiter, or put the limiter in front of it.
5. Document the behaviour in each protocol doc and in `website/docs` (the security page that describes the API's backoff).

## Tests

- `internal/cache` (or wherever the tracker lands): the existing tracker tests move with it unchanged. Positive control: a correct password after failures from another IP still succeeds, as spec `2026-11-09-03` requires.
- `internal/proxy/postgresql`: N wrong passwords from one source, then the (N+1)th attempt is refused before any hash check, even with the right password while the backoff lasts. Use a hash-count hook or a fake store to prove the check didn't run. Fails before the change, where every attempt reaches `VerifyPassword`.
- Same for `internal/proxy/mysql` and `internal/proxy/mssql`, through `verifyCredentials`.
- Reaching the ceiling writes exactly one `auth.failure_ceiling` entry carrying `listener`.
- Positive control: a correct first attempt is never delayed, and an API-key login is unaffected.

## To verify

- Whether the auth cache's negative entries (`internal/cache/auth.go`) already short-circuit repeated identical wrong passwords. They would not help against distinct guesses.
- Whether the listeners see the real client IP behind a TCP load balancer (PROXY protocol is not supported). Without it, every client shares the balancer's address: document that, as `DBB_TRUSTED_PROXIES` does for the API.

## Open questions

- Should the proxy and API failures share one counter per username, or keep separate ones? Recommended: share it. It's the same secret, and two counters double the guessing budget.

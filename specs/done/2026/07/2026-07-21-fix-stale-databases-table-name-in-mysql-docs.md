# Fix the stale `databases` table name in `docs/mysql.md`

## Goal

Make `docs/mysql.md` refer to the `servers` table, not the long-renamed
`databases` table.

## Why

`docs/mysql.md:65-66` and `docs/mysql.md:166` still describe the upstream TLS
configuration as living on a `databases.ssl_mode` column. That table was renamed
to `servers` in `internal/migrations/sql/20260716120000_databases_to_servers.up.sql:14`,
so the documented column path no longer exists. Anyone following the doc to find
or set the value will look in the wrong place.

Found while auditing the sibling spec that corrected the stale upstream-TLS claim
in `docs/postgresql.md` — that spec deliberately scoped itself to PostgreSQL, so
this one was left behind.

No GitHub issue exists for this yet — one should be filed.

## Implementation

- Update the two sites in `docs/mysql.md` to say `servers.ssl_mode`.
- While there, verify the surrounding prose against
  `internal/proxy/mysql/` — in particular whether MySQL's upstream TLS behaviour
  per `ssl_mode` value actually matches what the doc claims, the same way
  `docs/postgresql.md` was reconciled against `internal/proxy/postgresql/upstream_tls.go`.
- Grep `docs/`, root `CLAUDE.md` and `website/` for any other surviving
  references to a `databases` table and fix them in the same pass.

## Implementation Plan

Docs-only change. No Go touched.

1. `docs/mysql.md:65` — replace `databases.ssl_mode` with `servers.ssl_mode`,
   and reconcile the upstream-TLS prose against
   `internal/proxy/mysql/upstream.go:89-98` (verified with go-mysql v1.16.0
   `client/conn.go:268` `UseSSL` and `client/auth.go:90` TLS-required check).
   Add a compact per-`ssl_mode` table matching the sibling `docs/postgresql.md`
   reconciliation, capturing the MySQL-specific nuance that `prefer`/`allow` do
   **not** attempt opportunistic TLS (they go straight to plaintext, unlike the
   PG proxy) and that `require`/verify modes fail if the upstream lacks TLS.
2. `docs/mysql.md:165` — replace the `databases` table reference with `servers`.
3. Other surviving renamed-table references found by grep:
   - `docs/mongodb.md:49` — `databases.protocol_data` → `servers.protocol_data`.
   Confirmed NOT to touch: `docs/mongodb.md:161` (`databases` array is the
   MongoDB `listDatabases` wire response, not the table) and prose plurals.
   Root `CLAUDE.md` and `website/` (excluding `node_modules`) have no stale
   table references.

Verification: docs-only, so no build/lint/test targets apply.

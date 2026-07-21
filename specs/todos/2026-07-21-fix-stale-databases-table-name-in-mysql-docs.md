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

---
model: sonnet
effort: medium
---

# Server names accept any string; enforce slug format `[a-z0-9_]+`

## Goal

Refuse to create a server whose name is not a slug matching `^[a-z0-9_]+$` —
no spaces, no uppercase, no punctuation.

## Why

The server name is not a display label: it is the **client-facing selector on
all five protocols**. It is what the user types as the "database name" in
their connection string, and every proxy resolves it with an exact
`GetServerByName` lookup:

- PostgreSQL: `internal/proxy/postgresql/auth.go:62`
- Oracle: `internal/proxy/oracle/session.go:574` (via the SERVICE_NAME)
- MongoDB: `internal/proxy/mongodb/auth.go:258`
- MySQL: `internal/proxy/mysql/auth.go:189`
- SQL Server: `internal/proxy/mssql/auth.go:148`

Yet the only validation on create is `binding:"required"`
(`internal/api/servers.go:19`) — any string is accepted, spaces and all. A
non-slug name costs reachability, not just aesthetics:

- Oracle EZ-Connect cannot carry a name with spaces or parens at all: the
  connection endpoint falls back to the raw upstream service name, which is
  ambiguous when shared — the live prod catch-22 of
  `2026-08-13-22-oracle-non-ez-safe-names-unreachable-via-shared-service.md`
  (`abyla_abymutualise02 (R/O)`).
- Names in connection URLs need percent-encoding; names on a CLI need shell
  quoting; MySQL database names with spaces need backtick-quoting. Every one
  of these is a support ticket waiting to happen.

Refusing the bad name at creation time is the cheap, permanent fix.

## Implementation

- **Validate in `Store.CreateServer`** (`internal/store/servers.go`), not just
  the HTTP handler — one choke point covers every caller. Add a typed
  `ErrServerNameInvalid` in `internal/store/errors.go` and map it to a 400 in
  the create handler's error switch (next to the `ErrServerNameConflict` →
  409 mapping at `internal/api/servers.go:286`).
- The update request deliberately has no `name` field
  (`internal/api/servers.go:61`), so create is the only gate today. If a
  rename path is ever added, the same store-level check must cover it.
- **OpenAPI**: add the `pattern` to the create-request schema in
  `internal/api/openapi.yml` so the constraint is documented, not just
  enforced.
- **Frontend**: mirror the check in the server creation form (`front/`) for
  immediate feedback instead of a round-trip 400.
- **Existing rows are grandfathered** — no migration. Renaming a server
  changes the selector every existing client connection string uses; doing
  that silently in a migration is worse than the disease. Consider surfacing
  non-conforming names in the admin UI (a warning on the server row) so
  operators rename deliberately.
- **Fixtures**: demo-mode seed data, test fixtures and e2e suites create
  servers by name — verify they all conform (several current test names use
  hyphens, e.g. `cluster-scope`, `bastion-excl` in
  `internal/store/servers_kubernetes_test.go` / `servers_ssh_test.go`).

## Open questions

- **Hyphens.** The requested format is `[a-z0-9_]+`, which excludes `-`.
  Underscore-only is the safest common denominator (an unquoted `-` is an
  operator in MySQL/Oracle identifier positions), but hyphenated names are
  common practice and used throughout the current test fixtures. Decide
  whether the class is `[a-z0-9_]` or `[a-z0-9_-]` before implementing; the
  spec as filed follows the stricter request.
- A maximum length (e.g. 63 bytes, the PostgreSQL identifier limit) may be
  worth adding in the same breath — cheap now, awkward later.

## Resolved open questions

> **Hyphens.** Decide whether the class is `[a-z0-9_]` or `[a-z0-9_-]`.

**Decision: underscore-only — no hyphens.** Enforce exactly
`^[a-z0-9_]{1,63}$`. This follows the original request and matches the naming
already adopted in production (`abyla_abymutualise02_ro`). Hyphenated test
fixtures must therefore be renamed rather than grandfathered in the regex:
rename `cluster-scope` → `cluster_scope`, `bastion-excl` → `bastion_excl` and
every other hyphenated (or uppercase, or space-bearing) fixture name in
`internal/store/servers_kubernetes_test.go`, `internal/store/servers_ssh_test.go`,
the rest of the Go suites, demo/test seed data and the Playwright e2e suites, so
the whole tree conforms to the rule it now enforces.

> A maximum length (e.g. 63 bytes, the PostgreSQL identifier limit) may be
> worth adding in the same breath.

**Decision: yes — cap at 63 bytes**, PostgreSQL's identifier limit and the
tightest of the five protocols. It is part of the same regex above, so a single
check covers charset and length; measure it in **bytes**, and since the charset
is ASCII-only the byte length and the rune count coincide. An over-long name is
the same typed `ErrServerNameInvalid` → 400 as a bad charset.

---
model: opus
effort: high
---

# Connection URLs name a database the proxy refuses, and DataGrip cannot work with the one it accepts

## Problem

Two layers, verified on the dev stack against the seeded server `proxy_target`
(upstream database `target`):

**1. The generated PostgreSQL / MySQL URL is refused by the proxy.**
`BuildConnectionURL` puts the upstream `database_name` in the path
([internal/api/connection_url.go:52](internal/api/connection_url.go:52),
[:70](internal/api/connection_url.go:70)), while every proxy resolves the
client's database field against the dbbat server **`name`**
([internal/proxy/postgresql/auth.go:62](internal/proxy/postgresql/auth.go:62),
[internal/proxy/mysql/auth.go:189](internal/proxy/mysql/auth.go:189),
[internal/proxy/mssql/auth.go:148](internal/proxy/mssql/auth.go:148)).

| Startup `database=` | Result |
|---|---|
| `target` — what the UI hands out | `FATAL: database not found` |
| `proxy_target` — the dbbat name | connects; `current_database()` returns `target` |
| `user=admin#proxy_target`, `database=target` | `authentication failed` — the hint exists only on MongoDB |

[internal/api/connection_url_test.go:29](internal/api/connection_url_test.go:29)
masks the bug: `makeDB` sets `Name` and `DatabaseName` to the same string.
Oracle is unaffected (it already advertises the dbbat name, see
[connection_url.go:117](internal/api/connection_url.go:117)); MongoDB carries
the dbbat name in `authSource` and the real one in the path, which is the shape
this spec generalizes.

**2. Even the accepted form does not work in DataGrip.** DataGrip treats a
PostgreSQL data source as a *server*: it lists `pg_database`, names the
current database from `current_database()` (`target`, not `proxy_target`), and
opens a **dedicated connection per database** with `database=<that name>`,
which the proxy cannot resolve. The tree stays empty. JetBrains ships "Single
database mode" (data source → Options) precisely for PgBouncer, whose pool
aliases have the same semantics — a workaround, not a fix. (The GUI run in
DataGrip 2026.2 could not be completed under click-only automation; the
per-database reconnect behaviour comes from JetBrains' documentation and the
PgBouncer precedent, and must be re-checked by hand once this lands.)

**Real-world shape (Stonal, 2026-09-09).** Every datalake is registered twice
with the same `database_name`: `demo_datalake_ro` and `demo_datalake_rw` both
target `demo_datalake`. So "resolve the real name through the user's grants"
is ambiguous for anyone holding both, and cannot be the primary mechanism.

## Proposal

Move the dbbat server selector into the **username**, the one slot every
client and driver preserves on every connection it opens, and let the database
field carry the real upstream name. The MongoDB proxy already has this ladder
(`internal/proxy/mongodb/auth.go` — `splitUserDBHint`, `resolveDatabase`:
`authSource` → `user#database` hint → single active grant); make it
protocol-wide.

Target form, which should work end to end in psql, JDBC, DBeaver and DataGrip:

```
postgresql://florent.clairambault%23demo_datalake_ro:{DBBAT_KEY}@db.stonal.io:5432/demo_datalake
```

(`#` is `%23` inside a URL userinfo — libpq and pgjdbc decode it; in an IDE the
user field is separate and `florent.clairambault#demo_datalake_ro` is typed
as-is.)

### Steps

1. **Fix the builder now** — PostgreSQL and MySQL/MariaDB branches of
   `BuildConnectionURL` emit `db.Name` in the path until step 3 replaces the
   form. Split the `makeDB` fixture so `Name != DatabaseName` and assert the
   path. Adjust the endpoint description in `internal/api/openapi.yml` and the
   e2e regex in `front/e2e/servers.spec.ts` if it pins the path.

2. **One shared resolver** — hoist `splitUserDBHint` from the MongoDB proxy
   into `internal/proxy/shared` with a resolver used by PostgreSQL
   (`auth.go`), MySQL (`OnAuthSuccess`) and SQL Server (`resolveDatabase`),
   in this order:
   1. requested database equals a server `name` → exact match wins (today's rule);
   2. username carries `user#server` → that server; the requested database
      must equal its `database_name` or be empty, else refuse with a message
      naming what the server exposes (this is what DataGrip's reconnect to
      `postgres` / `template1` hits);
   3. otherwise, among the caller's **active grants**, servers on this
      listener's protocol whose `database_name` equals the requested name:
      exactly one → use it; several → refuse naming the candidates and the
      `user#server` form; none → "database not found".

   Rung 3 is scoped to the caller's grants so it can never widen access, and
   the ordering stops a `database_name` from shadowing another server's `name`.
   The bare username is what lands in `connections`, `audit_log`, the Slack
   notifications and the approval gate; the upstream credential is the stored
   one, so `#` never reaches the target. MongoDB keeps its own entry point but
   calls the shared parse.

3. **What the UI hands out** — emit the `user%23server` + real database form
   for PostgreSQL and MySQL/MariaDB (SQL Server has no builder branch today —
   add one in the same shape, `Server=host,1434;Database=<real>;User Id=user#server`).
   Under the field, one line: "Database is the real upstream name; the
   `#demo_datalake_ro` suffix selects the dbbat server."

4. **Docs** — `website/docs/configuration/servers.md` (`name` is the selector,
   three ways to provide it), a short "IDEs (DataGrip, DBeaver)" section with
   the DataGrip "Single database mode" note for deployments not yet upgraded,
   and a cross-reference from `docs/mongodb.md` now that the ladder is shared.

5. **Tests** — unit tests for the resolver (each rung, ambiguity, shadowing,
   protocol mismatch, `#` in the recorded username); per-protocol integration
   tests connecting with `database=<real name>` and with `user#server`; an
   assertion that a reconnect to a database the server does not expose is
   refused with the explicit message.

### Out of scope

Rewriting `current_database()` / `pg_database` in flight: it breaks the
transparent-proxy contract and DataGrip issues dozens of catalog queries.
Renaming servers so `name == database_name` is impossible with the `_ro`/`_rw`
twins.

---
model: opus
effort: medium
---

# PostgreSQL: block_ddl does not refuse DO blocks and CALL

## Problem

Found by the security scan of 2026-10-30 (finding M1).

Severity: Medium. Triggered by any authenticated user holding a grant with `block_ddl` (and not `read_only`) on a PostgreSQL server.

On PostgreSQL, `block_ddl` is a prefix check only: `validateStatement` refuses a statement when `isDDLQuery` sees `CREATE`, `ALTER`, `DROP` or `TRUNCATE` as its first keyword (`internal/proxy/postgresql/intercept.go:155`, keyword list `internal/proxy/shared/validation.go:121`). Nothing on the server side enforces it: only `read_only` gets a session pin.

A `DO` block or a `CALL` starts with neither keyword, so it is forwarded, and its body can run any DDL (directly or through `EXECUTE`). dbbat already refuses `DO`/`CALL` as "code dbbat cannot see", but only under `read_only` (`readOnlyProcedurePattern`, `intercept.go:87`, applied at `intercept.go:136-139`). The other dialects fail closed on opaque dynamic SQL under `block_ddl` (`shared.RefusesOpaqueDynamicSQL`, `internal/proxy/shared/validation.go:594`). PostgreSQL is the one where a `block_ddl` grant can still drop or create tables.

## Proposal

1. In `validateStatement` (`internal/proxy/postgresql/intercept.go:125`), refuse a statement matching `readOnlyProcedurePattern` when the grant carries `block_ddl`, not only `read_only`. Reuse `shared.ErrReadOnlyProcedureCall` or add a block_ddl-specific error whose text names the control (preferred, so the refusal row says why).
2. Check the extended-protocol path goes through the same function (`handleParse` calls `validateStatement`), so the simple and extended paths cannot drift.
3. Update `docs/postgresql.md` and the `block_ddl` line of `website/docs/intro.md:91` to say that `DO` and `CALL` are refused under `block_ddl`.

## Tests

- `internal/proxy/postgresql/intercept_test.go`: under `block_ddl`, a `DO` block containing DDL, a `DO` block with dynamic `EXECUTE`, and a `CALL` are refused, through `handleQuery` and `handleParse`. This fails before the fix.
- Positive control: under `block_ddl`, `SELECT`, `INSERT` and `UPDATE` still pass. Under a full-write grant, `DO` and `CALL` still pass.
- Integration (`internal/proxy/postgresql/integration_test.go`): a `block_ddl` session sending a `DO` block that drops a table gets an error and the table still exists.

## Open questions

- Should `block_copy` also refuse `DO`/`CALL`? COPY to or from the client cannot run inside PL/pgSQL, so the residual is server-side COPY, which needs privileged roles. Recommended: no, keep the change to `block_ddl`.

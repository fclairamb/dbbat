---
model: opus
effort: medium
---

# SQL Server: sp_cursor positioned writes bypass read_only

## Problem

Found by the security scan of 2026-10-30 (finding M2).

Severity: Medium. Triggered by any authenticated user holding a `read_only` (or `block_ddl`) grant on a SQL Server server, when the server row's login can write.

`describeRPCRequest` forwards every procedure in `harmlessProcs` without any check, on the grounds that they "carry no statement" and that "the statement they act on was already checked when it was prepared or opened" (`internal/proxy/mssql/rpc.go:372-379`). The list includes `sp_cursor` (procedure id 1).

`sp_cursor` is not bookkeeping. Its `optype` argument requests positioned UPDATE, DELETE or INSERT on the cursor's base table, with the new values as parameters. The statement dbbat checked when the cursor was opened (`sp_cursoropen`, `sp_cursorprepexec`) is the cursor's `SELECT`, which passes `read_only`. The write that follows through `sp_cursor` is never checked, and the `queries` row only shows the RPC.

`docs/mssql.md` (the "Cursor bookkeeping" paragraph after the fail-closed list) describes `sp_cursor` as carrying "neither a statement nor a decision", which is wrong for its write optypes. On SQL Server `read_only` is the text control alone (`docs/mssql.md`, "read_only is a text control on SQL Server"), so nothing else stops the write.

## Proposal

1. Remove `spCursor` from `harmlessProcs` (`internal/proxy/mssql/rpc.go:376`).
2. In `describeRPCRequest`, handle `sp_cursor` explicitly: read its `optype` parameter. Refuse it under a restrictive grant (`read_only` or `block_ddl`, same predicate as the other fail-closed cases) when `optype` includes UPDATE, DELETE or INSERT, or cannot be read. Let REFRESH, LOCK, SETPOSITION and ABSOLUTE through as today.
3. Correct the cursor paragraph of `docs/mssql.md`.

## Tests

- `internal/proxy/mssql/rpc_test.go` / `intercept_test.go`: an `sp_cursor` RPC with optype UPDATE, DELETE and INSERT is refused under `read_only`. This fails before the fix. An unreadable `optype` is refused too.
- Positive control: `sp_cursor` with optype REFRESH or SETPOSITION, and `sp_cursorfetch` / `sp_cursorclose`, still pass under `read_only`. Every optype passes under a full-write grant.

## To verify

- The exact `optype` bit values and parameter position (TDS / `sp_cursor` reference): UPDATE 0x0001, DELETE 0x0002, INSERT 0x0004, REFRESH 0x0008, LOCK 0x0010, SETPOSITION 0x0020, ABSOLUTE 0x0040, combinable with flags.
- Whether `rpcRequest` exposes typed integer parameter values, or a small decoder is needed.

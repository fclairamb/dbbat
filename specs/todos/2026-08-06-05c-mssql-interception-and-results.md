---
model: opus
effort: high
---

# SQL Server proxy, stage 3: query interception, grant enforcement and result accounting

Stage 3 of 3, split from the original `2026-08-06-05-sql-server-proxy-support.md` on the
owner's instruction. **Depends on stages 1 and 2**
([05a](2026-08-06-05a-mssql-tds-framing-and-handshake.md),
[05b](2026-08-06-05b-mssql-auth-and-upstream.md)): the codec, handshake, client auth and
upstream relay must already work.

## Goal

Put SQL Server sessions on the same pipeline as the other four protocols: every statement
logged, grants and quotas enforced, approval holds honoured, result rows and byte counts
accounted for. After this stage SQL Server is no longer a second-class protocol — it is
subject to the same controls as PostgreSQL, Oracle, MySQL and MongoDB.

## Proposal

### Query interception

- **`SQLBatch` (type 0x01)** carries the SQL text as UCS-2. Decode it and feed it through
  the shared intercept/grant/approval pipeline exactly as `internal/proxy/mysql/intercept.go`
  does — the pipeline in `internal/proxy/shared/` is protocol-agnostic, so this is wiring,
  not new policy. Watch the encoding: UCS-2 is not UTF-8, and a naive conversion mangles
  non-ASCII identifiers and, worse, could let a crafted statement evade a pattern match.
  Round-trip the decode in tests with non-ASCII SQL.
- **`RPC` (type 0x03)** — `sp_executesql` and prepared statements. **Extract the statement
  text and enforce grants on it**, do not merely log it (decided; see Resolved open
  questions). Log-only here would let any client bypass `read_only` and `block_ddl` by
  wrapping a write in `sp_executesql`, which is a hole in an access-control path, not a
  gap in observability. Enforcement operates on the statement template text, which is the
  right granularity for read-only and DDL blocking; parameter values are captured for
  logging, not for matching.
  - Cover the RPC forms that actually carry SQL (`sp_executesql`, `sp_prepare`,
    `sp_prepexec`) and the procedure-name-by-id shorthand. A procedure call that carries no
    inline SQL still gets logged, and still passes through the grant check for
    `read_only`/`block_ddl` on whatever the call itself implies — if you cannot determine
    that for a given RPC form, fail closed and say so in `docs/mssql.md`.
- Approval holds work through the shared gate, so a held statement must park the TDS session
  without the client timing out or the connection being torn down — verify this specifically,
  since it is the behaviour most likely to break subtly under a protocol the gate has never
  driven before.

### Result accounting

Parse the response token stream far enough to fill the shared metrics, as
`internal/proxy/mysql/result.go` does:

- `COLMETADATA` for column shape, `ROW` and `NBCROW` for rows, `DONE` / `DONEPROC` /
  `DONEINPROC` for row counts and to detect statement boundaries, `ERROR` and `INFO` tokens
  for diagnostics.
- Fill the query row-count and byte-transferred metrics, and feed the shared `rowwriter`
  capture so captured result rows work for SQL Server like the others.
- An `ERROR` token must land on the query row's error field as a real diagnostic. Note the
  repo has been burned before by a decoder writing raw bytes into `queries.error` (see
  `specs/todos/2026-08-05-clean-up-fabricated-query-errors.md`) — the sanitiser
  `shared.SanitizeQueryError` exists for exactly this; use it.

### Docs and tests

- `docs/mssql.md`: the interception table (which TDS message types are intercepted, which
  are relayed untouched), the RPC enforcement decision and its limits, and anything a
  client can do that DBBat does not yet see.
- Unit tests for the UCS-2 decode, the RPC statement extraction (each supported form, plus
  one unsupported form asserting it fails closed), and the token-stream accounting against
  synthesized responses.
- Extend the `//go:build integration` suite from stages 1–2: a `read_only` grant blocking a
  write issued as a plain `SQLBatch` **and** the same write wrapped in `sp_executesql`
  (the regression test for the decision above), a row-count assertion, and an approval hold
  released mid-session.

## Resolved open questions

Inherited from the owner's answers of 2026-08-06 — see
[05a](2026-08-06-05a-mssql-tds-framing-and-handshake.md) for the full set. The one binding
on this stage:

- **RPC calls → extract the statement text and enforce grants on it**, not log-only.

No GitHub issue exists yet — one should be filed.

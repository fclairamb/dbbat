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

## Implementation Plan

Stage 2 left two seams (`clientMessageHook`, `serverPacketHook` in
`internal/proxy/mssql/relay.go`) and a partial token walker (`tokens.go`). The plan is
to fill them, not to touch the pumps.

### 1. Session dependencies (`server.go`, `session.go`, `main.go`)

`Server` grows what its four siblings already carry: `queryStorage
config.QueryStorageConfig` (a new `NewServer` parameter), `rowWriter *shared.RowWriter`
+ `SetRowWriter`, `approvalDeps shared.ApprovalDeps` + `SetApprovalDeps`. `main.go`'s
`startMSSQLProxy` passes them like `startMongoProxy` does.

`session` grows the per-session collaborators: a `shared.WatchedConn` +
`shared.CountingConn` under the raw socket (below TLS, so an approval hold parks on raw
records and the byte counters see the whole client leg), `shared.LimitGuard`,
`cache.RevocationHandle`, `shared.ApprovalGate`, `shared.StreamPublisher`, the
`lastBytesSnapshot`, and the held-query uid. The guard's watchdog starts in `serve`,
and `recordDisconnect` flushes trailing bytes, both exactly as MongoDB does.

One concurrency change is forced: a blocked statement is answered on the *client*
codec from the client→upstream pump, while the upstream→client pump also writes it.
A `clientWriteMu` on the session serialises the two.

### 2. Query interception (`intercept.go`, `rpc.go`)

`onClientMessage` classifies by TDS message type:

| Type | Treatment |
|------|-----------|
| `SQLBatch` 0x01 | ALL_HEADERS skipped, statement decoded from UCS-2LE, enforced |
| `RPC` 0x03 | parsed into requests; statement text extracted, enforced |
| `BulkLoad` 0x07 | refused under `read_only`/`block_copy` (belt and braces on `INSERT BULK`) |
| everything else | relayed untouched |

The enforcement pipeline is the shared one, in the MySQL order: revocation → quotas →
`shared.ValidateQuery` (+ a `block_copy` check for `BULK INSERT`) → approval hold →
forward. A refusal is answered on the client leg as an `ERROR` + `DONE(DONE_ERROR)`
token stream — the same shape a real SQL Server error takes, so the driver raises it
and keeps the connection.

RPC statement extraction (`rpc.go`) parses `NameLenProcID`, `OptionFlags` and the
`ParameterData` list (B_VARCHAR name, status byte, TYPE_INFO, value), for both the
by-name and the by-procid shorthand, and handles a multi-RPC batch. Forms that carry
SQL: `sp_executesql` (param 0), `sp_prepare` / `sp_prepexec` / `sp_cursorprepare`
(param 2), `sp_cursoropen` (param 1), `sp_cursorprepexec` (param 3),
`sp_prepexecrpc` (param 1). Parameter values are decoded for logging only.

`sp_execute` / `sp_cursorexecute` carry a *handle*, not SQL. The accountant records the
handle the upstream returns for each prepare (`RETURNVALUE` token) against the
statement text that was already validated, so an execute is enforced on the statement
it actually runs. An **unknown** handle, and any RPC naming a stored procedure whose
body dbbat cannot see, **fail closed** whenever the grant restricts anything
(`read_only` / `block_ddl` / `block_copy`) — documented in `docs/mssql.md`.

### 3. Result accounting (`tokens.go`, `result.go`)

`onServerPacket` feeds an incremental token walker: the upstream→client pump forwards
packet-at-a-time, so tokens split across packet boundaries and the walker keeps a
carry buffer with a hard cap. It handles `COLMETADATA` (full TYPE_INFO, including the
BYTELEN/USHORTLEN/LONGLEN/PLP families), `ROW`, `NBCROW`, `DONE`/`DONEPROC`/
`DONEINPROC`, `ERROR`, `INFO`, `RETURNVALUE`, and skips the rest by length. Anything
unmodelled desynchronises the walk *for that message only* — the walk is purely
observational and can never corrupt the relayed bytes.

A 13-byte rolling tail is kept per message: the last token of a TDS response is always
a DONE-family token, so the row count survives even a desynchronised walk.

On EOM the pending statement is recorded through the same async path MySQL and MongoDB
use: one `store.Query` row, captured rows through `shared.RowWriter`, byte delta from
the counting conn onto the connection stats and the in-session quota counters. An
`ERROR` token becomes the row's `error`, through `shared.SanitizeQueryError`.

While here: stage 2's `tokenFeatureExtAck` is `0xEE`, which is FEDAUTHINFO —
FEATUREEXTACK is `0xAE`. Fixed with a test, since the accountant needs both.

### 4. Docs and tests

`docs/mssql.md` gains the interception table, the RPC enforcement decision with its
limits, and a plain list of what a client can still do that dbbat does not see.

Unit tests (all under `make test`, on the stage-1/2 fake-upstream harness): UCS-2
decode round-trip with non-ASCII SQL, RPC extraction per supported form plus an
unsupported one asserting the fail-closed refusal, and token-stream accounting against
synthesized responses. The `//go:build integration` suite gains a `read_only` grant
blocking a write as a plain `SQLBatch` *and* the same write through `sp_executesql`, a
row-count assertion, and an approval hold released mid-session.

---
model: opus
effort: xhigh
---

# A statement running through dbbat has no time limit, so one investigation session can starve a production replica

## Problem

On 2026-09-15 a `postgresql-prod-prod-0-ro` RDS replica at Stonal lagged 20
minutes behind its primary. The cause was not application traffic: one dbbat
session (an AI agent's diagnostic under an approved grant) was running
`EXPLAIN (ANALYZE, BUFFERS)` and full aggregates over a large table in a loop,
at ~93% of the replica's active sessions and 100% I/O wait, until the volume's
EBS I/O balance ran out and WAL replay was throttled. It stopped only because
the queries eventually finished on their own.

dbbat today bounds a grant in *time* (`expires_at`), *volume*
(`max_query_counts`, `max_bytes_transferred`) and *shape* (`read_only`,
`block_ddl`, `block_copy`, approval patterns), but never a single statement's
*duration*. `grep -rn statement_timeout internal/` returns nothing. A read-only
grant with all quotas set can still hold a full table scan open for an hour.

The Slack thread converged on a design (Florent's closing message):

1. make the limit **configurable**,
2. **apply it when the upstream session opens** (server-side, so the database
   itself cancels the statement with a clean error), and
3. if a statement is still running **`timeout + 2s` later, disconnect the
   session** from dbbat's side.

Decisions taken with Florent while filing this spec:

- **Scope**: a global default (possibly unset) *and* a per-grant-definition
  value (possibly unset). Not per server.
- **No bypass**: a hard limit. The server-side setting exists for the error
  message; dbbat's own watchdog is the enforcement, and a client that unsets
  the server-side value gains nothing.
- **All five protocols** in the first version: the server-side knob where one
  exists, the watchdog and an upstream cancel everywhere.

## Proposal

### Configuration and resolution

Three layers, resolved per session at auth time, in this order:

| Layer | Where | Semantics |
|---|---|---|
| Grant definition | new nullable column `grant_definitions.statement_timeout_seconds` | `NULL` = inherit the global value; `0` = explicitly no limit for this definition; `> 0` = seconds |
| Global, store | new parameter group `limits`, key `statement_timeout` (Go duration string) in `global_parameters`, edited from the Settings page | wins over the env var when set, like `public.*` wins over `DBB_LISTEN_*` (`store.ResolvePublicEndpoints`, `internal/store/global_parameters.go:262`) |
| Global, env | `DBB_STATEMENT_TIMEOUT` (Go duration; empty or `0` = none) | the deployment default |

The `0` on a definition is deliberate: with a global limit set, it is the only
way to keep a dump or an ETL definition usable through dbbat. It is an admin
decision made at definition-edit time, which is what "no bypass" is meant to
protect. The UI labels it "No limit (overrides the global timeout)" and shows
the resolved global value next to the field.

Definitions are immutably versioned, so editing the value archives the row and
inserts a successor. Live grants keep the value they were issued under, exactly
like `duration_seconds` and the quotas. Expose it through `AccessGrant`:

```go
// StatementTimeout resolves the per-statement limit for this grant. global is
// the instance-wide default (zero = none). Returns 0 when nothing applies.
func (g *AccessGrant) StatementTimeout(global time.Duration) time.Duration
```

Plumbing to add, following `MaxBytesTransferred` end to end:

- migration `internal/migrations/sql/2026091600000_grant_definitions_statement_timeout.{up,down}.sql`
- `store.GrantDefinition.StatementTimeoutSeconds *int64` (`internal/store/models.go:988` area), `CreateGrantDefinition` / `UpdateGrantDefinition`
- API DTOs `internal/api/grant_definitions.go:81,156` with the
  `clear_statement_timeout_seconds` tri-state the other nullable fields use
  (`:209`), validation `>= 0` at `:325`, OpenAPI schema
- config: `Config.StatementTimeout` with koanf key `statement_timeout`, default
  empty, env mapping in `internal/config/config.go:1273` area, and the
  `CLAUDE.md` / `website/docs/configuration/index.md` env tables
- store parameters: `GroupLimits`, `KeyLimitsStatementTimeout` next to
  `GroupPublic` (`internal/store/global_parameters.go:104`), a typed getter,
  and a field on the Settings page (`front/src/routes/_authenticated/settings/index.tsx`)
- the resolved value has to reach every proxy at auth time; the proxies already
  receive `config.Config` and the store, so resolve once per session next to
  where `GetActiveGrant` is called (PG `internal/proxy/postgresql/auth.go:109`,
  MySQL `auth.go:209`, Mongo `auth.go:102`, MSSQL `auth.go:52`, Oracle
  `session.go:1138`). Read the store parameter through the auth cache so it is
  not one extra query per connection.

### Layer 1: the server-side setting, issued at session start

Where a protocol has a session-level knob, set it before the client is told it
is connected, at the same point the read-only pin is applied. A session that
cannot be pinned fails rather than running unbounded, same rule as
`ErrUpstreamReadOnlyMode`.

| Protocol | Statement | Where |
|---|---|---|
| PostgreSQL | `SET SESSION statement_timeout = '<ms>ms'` | `replayUpstreamStartup`, next to `setSessionReadOnly` (`internal/proxy/postgresql/upstream.go:85,144`). Generalize `setSessionReadOnly` into `runUpstreamSetup(stmts ...string)` |
| MySQL | `SET SESSION max_execution_time = <ms>` | right after `upstream.ConnectMySQL` in `connectUpstream` (`internal/proxy/mysql/upstream.go:42`); the conn is a `*client.Conn`, so `Execute` is available. MariaDB does not have `max_execution_time`; detect it from the server version string and use `SET SESSION max_statement_time = <s>` (seconds, fractional allowed). Note MySQL's setting only covers `SELECT`; the watchdog covers the rest |
| MongoDB | no session knob. Inject `maxTimeMS` into every forwarded command document (`find`, `aggregate`, `count`, `distinct`, `update`, `delete`, `findAndModify`, `getMore` and the generic case) when the client did not send one or sent a larger one | the interception point in `pumpClientToUpstream` (`internal/proxy/mongodb/intercept.go:40`), where the command is already decoded |
| Oracle | none in-band (a statement time limit is a Resource Manager plan, DBA-level; `CALL_TIMEOUT` is an OCI client setting) | watchdog only |
| SQL Server | none server-side (the query timeout is a client concept) | watchdog only |

Because the limit is hard, a client statement that would change the setting is
refused with a clear error, the way `readOnlyBypassPatterns` refuses un-setting
read-only (`internal/proxy/postgresql/intercept.go:26`):

- PG: `SET [SESSION|LOCAL] statement_timeout ...`, `RESET statement_timeout`,
  `SET ... TO DEFAULT`. Error text: `statement_timeout is managed by dbbat
  (limit 30s)`. `SHOW statement_timeout` stays allowed.
- MySQL: `SET [SESSION|GLOBAL|PERSIST] max_execution_time` /
  `max_statement_time`, and the `/*+ MAX_EXECUTION_TIME(n) */` optimizer hint
  when `n` exceeds the limit (a smaller `n` is fine).
- MongoDB: a client `maxTimeMS` larger than the limit is clamped, not refused.

This refusal is a courtesy, not the security boundary: without it, a client
that sets the value to 0 would only ever see the disconnect from layer 2, with
no explanation. The intercept runs through `validateStatement` so the simple
and extended paths cannot drift.

### Layer 2: the dbbat watchdog

The per-session `LimitGuard` (`internal/proxy/shared/limits.go:41`) already
polls every 250ms (`DefaultLimitPollInterval`) and tears the session down
through `onLimitViolation` on quota, expiry or revocation. Give it a statement
clock:

```go
// StatementClock marks the oldest statement currently executing upstream.
// Zero when the session is idle. Set when a statement is forwarded (after any
// approval hold resolves), cleared when its completion is observed.
type StatementClock struct{ since atomic.Int64 } // unix nanos

func (g *LimitGuard) WithStatementTimeout(limit, grace time.Duration, clock *StatementClock) *LimitGuard
```

`Check()` returns a new `ErrStatementTimeout` when `now - since > limit +
grace`, with `grace = 2s` (`StatementTimeoutGrace`, a constant, not a setting).
`Watch()`'s early return ("nothing to enforce") must account for the new limit.
The 250ms tick means the kill lands within 2.25s of the limit, which is the
"+2 secondes" of the thread.

The clock is driven by the per-protocol bookkeeping that already exists:

| Protocol | Start (forward to upstream) | Stop (completion observed) |
|---|---|---|
| PostgreSQL | `handleQuery` / `handleExecute` after `holdIfNeeded` returns (`intercept.go:107,286`) | `logQuery` (`intercept.go:424`). With the extended protocol several statements can be in flight (`extendedState.pendingQueries`); the clock holds the *oldest* start and is re-armed from the next pending one on completion. A `COPY` in progress is a statement |
| Oracle | the four cursor start sites (`intercept.go:369,529,690,784`) | `completeQuery` (`:1129`) |
| MySQL | `runIntercepted` (`intercept.go:184`) | `recordQuery` (`:492`) |
| SQL Server | `runStatement` (`intercept.go:275`) | the result path in `result.go` |
| MongoDB | `registerPending` (`intercept.go:214`) | `takePending` (`:221`); the oldest pending op is the reference, same as PG |

Time parked on an approval hold does **not** count: the clock starts when the
statement is actually sent upstream, and a held statement has not been. The
server-side setting agrees by construction (the server has not seen it).

On `ErrStatementTimeout`, `onLimitViolation` does two things in order:

1. **Cancel upstream first.** Closing the sockets is not enough: a PostgreSQL
   backend in a long sequential scan does not notice a dead client until it
   tries to send, and with `client_connection_check_interval` at its default
   of 0 the scan runs to completion, which is exactly the load this spec
   exists to stop. Per protocol, best effort with a short deadline:
   - PG: a `CancelRequest` carrying the upstream `BackendKeyData` dbbat
     already keeps (`upstream.PostgresUpstream.BackendKeyData`,
     `internal/proxy/upstream/postgres.go:65`), sent on a fresh connection
     through the same dial path (SSH / Kubernetes tunnels included, so it
     cannot be a bare `net.Dial`).
   - MySQL: `KILL QUERY <upstream connection id>` on a fresh upstream
     connection (`conn.GetConnectionID()` on the go-mysql client conn).
   - SQL Server: an Attention token on the existing upstream connection, then
     drain to the attention ack. The `armedCancel` / `attentionAckDue`
     machinery in `internal/proxy/mssql/intercept.go:476` is the model.
   - Oracle: the break/reset marker exchange (`docs/oracle.md`, "OCI
     break/reset"). Mark it unverified in the docs until the e2e suite proves
     it; the socket close is the fallback.
   - MongoDB: nothing (`killOp` needs privileges the proxied role usually
     lacks); the injected `maxTimeMS` is the cancel.
2. **Close both sockets** (`closeConns`), the existing teardown.

### Recording the termination

Every dbbat-initiated teardown is currently a bare WARN log line. Make it a
first-class fact, reused by the terminate-connection spec that follows:

- `connections.termination_reason text` (nullable): `statement_timeout`,
  `grant_expired`, `quota_exceeded`, `grant_revoked`, and later
  `admin_terminated`. Written in the same transaction as `disconnected_at` by
  `CloseConnection`. Surfaced on `GET /connections/{uid}` and on the connection
  detail page (`front/src/routes/_authenticated/connections/$uid.tsx`).
- the in-flight query row is completed with an error string such as
  `statement timeout: limit 30s, ran 32.1s, session terminated by dbbat` via
  `UpdateQueryCompletion`, so the queries page shows *which* statement did it.
- a chained audit entry `connection.terminated` (next to
  `AuditEventConnectionClosed`, `internal/store/connection_audit.go:45`) with
  connection uid, user, database, grant, reason, the query uid, the limit and
  the observed duration. Like the other two connection events it is excluded
  from an unfiltered `GET /api/v1/audit` and reachable with `?event_type=`.
- a `connection` stream event with state `terminated` and the reason
  (`shared.StreamPublisher.Connection`, `internal/proxy/shared/stream.go:152`),
  so the live connections page can show it immediately.

### MCP

Agent statements run through the same listener (`docs/mcp.md`), so the limit
applies to them unchanged, which is the case that started this. The MCP
executors deliberately carry no client-side timeout because of approval holds
(`internal/mcp/exec_mssql.go:34`); leave that. Map the resulting connection
error to a tool error that names the limit (`statement exceeded the 30s limit
of your grant and was cancelled`), so the agent learns to narrow its query
rather than retry it.

### Documentation

- `CLAUDE.md` env table and Access Control section, `docs/approvals.md`
  cross-reference (hold time excluded), `docs/postgresql.md` / `mysql.md` /
  `mongodb.md` / `oracle.md` / `mssql.md` (what is server-side, what is
  watchdog-only, what cancel is sent).
- `website/docs/configuration/index.md` and a short "Limits" section in the
  features docs.

### Tests

- `internal/proxy/shared`: `LimitGuard` with a fake clock: idle session never
  trips; clock set then cleared before the limit never trips; clock past
  `limit + grace` trips with `ErrStatementTimeout`; the oldest-of-several rule.
- Per-protocol integration suites (`make test-integration-*`), each with a
  1s definition limit:
  - PG `SELECT pg_sleep(5)` gets SQLSTATE `57014` and the session survives
    (layer 1); `SET statement_timeout = 0` is refused; with a test hook that
    skips the server-side SET, `pg_sleep(5)` ends in a disconnect within
    ~3.5s, `pg_stat_activity` no longer shows the backend (the cancel landed),
    the query row carries the error, the connection row carries the reason and
    the audit entry exists.
  - MySQL `SELECT SLEEP(5)` under `max_execution_time` (MariaDB variant if the
    suite has one); the `MAX_EXECUTION_TIME` hint above the limit refused.
  - MongoDB `{find, filter: {$where: "sleep(5000) || true"}}` gets
    `MaxTimeMSExpired`; a client `maxTimeMS: 60000` is clamped.
  - SQL Server `WAITFOR DELAY '00:00:05'` and Oracle `dbms_session.sleep(5)`
    end in a disconnect within the window (watchdog-only protocols).
  - An approval hold parked for longer than the limit, then approved, runs to
    completion (hold time excluded).
- API tests for the definition field round-trip and the `0` / `NULL`
  distinction; a Settings page e2e for the global field.

### Out of scope, noted for later

- Idle-in-transaction limits (`idle_in_transaction_session_timeout`) and a
  lock timeout. Different failure mode, same plumbing; a later spec.
- Per-server overrides. The thread floated a "script run at connection open
  for a set of servers"; the definition-level value covers the stated need and
  a generic init-script mechanism moves a trust boundary, so it stays out.

## Implementation Plan

Ordered, each step a commit.

1. **Migration + store model** — `grant_definitions.statement_timeout_seconds bigint`
   (nullable) and `connections.termination_reason text` (nullable) in one migration
   pair. `store.GrantDefinition.StatementTimeoutSeconds *int64`, carried through
   `CreateGrantDefinition` / `UpdateGrantDefinition` / `sameGrantDefinitionShape`, plus
   `(*AccessGrant).StatementTimeout(global time.Duration) time.Duration`.
2. **Global parameter + config** — `store.GroupLimits` / `KeyLimitsStatementTimeout`,
   `Store.GetLimits` / `SetLimits`, `store.ResolveStatementTimeout(param, cfg)`;
   `config.Config.StatementTimeout` (koanf `statement_timeout`, env
   `DBB_STATEMENT_TIMEOUT`) with a `StatementTimeoutDuration()` accessor.
3. **Cached resolver** — `shared.StatementTimeoutResolver`: store parameter with a
   short TTL, env fallback, one shared instance per proxy server so it is not a
   query per connection. Resolution helper `shared.ResolveStatementTimeout(grant, global)`.
4. **API DTOs** — `statement_timeout_seconds` on create/update grant-definition
   requests with the `clear_statement_timeout_seconds` tri-state, `>= 0` validation,
   OpenAPI schema. Settings endpoints for the global `limits.statement_timeout`.
5. **Shared LimitGuard clock (layer 2)** — `shared.StatementClock`,
   `(*LimitGuard).WithStatementTimeout(limit, grace, clock)`, `ErrStatementTimeout`,
   `StatementTimeoutGrace = 2s`, `Watch` early-return updated. Unit tests.
6. **Termination recording** — `store.TerminationReason*` constants,
   `Store.CloseConnectionWithReason`, `connections.termination_reason` surfaced on
   the connection DTO, `store.AuditEventConnectionTerminated` chained entry,
   `shared.ConnectionTerminated` stream state, and a protocol-agnostic
   `shared.Termination` record so each session can note "dbbat killed this, for
   this reason, on this statement" once and have close/audit/stream agree.
7. **PostgreSQL** — `runUpstreamSetup`, `SET SESSION statement_timeout`, intercept
   refusal patterns through `validateStatement`, clock start/stop in
   `handleQuery`/`handleExecute`/`logQuery`/`recordBlockedQuery`, `CancelRequest`
   cancel through `shared.DialUpstream`, then `closeConns`.
8. **MySQL** — `SET SESSION max_execution_time` (MariaDB: `max_statement_time`,
   seconds), hint/SET refusal, clock in `runIntercepted`/`recordQuery`,
   `KILL QUERY <id>` on a fresh upstream connection.
9. **MongoDB** — `maxTimeMS` injection/clamping in the forwarded command,
   clock from `registerPending`/`takePending`, no cancel (the injection is it).
10. **SQL Server** — watchdog only, Attention-token cancel on the live upstream
    connection, clock in `runStatement` and the result path.
11. **Oracle** — watchdog only, best-effort break/reset marker (documented
    unverified), clock at the cursor start sites and `completeQuery`.
12. **MCP** — map `ErrStatementTimeout` (and the wire error it produces) to a tool
    error naming the limit, in the `exec_*.go` executors.
13. **Frontend** — Settings page global field, grant-definition form field with the
    "No limit (overrides the global timeout)" `0` semantics, connection detail
    `termination_reason`.
14. **Docs** — `CLAUDE.md` env table + Access Control, `docs/*.md` per protocol,
    `docs/approvals.md` cross-reference, `website/docs/configuration/index.md`.
15. **Tests** — shared unit tests, API round-trip tests, per-protocol integration
    cases behind `//go:build integration`.

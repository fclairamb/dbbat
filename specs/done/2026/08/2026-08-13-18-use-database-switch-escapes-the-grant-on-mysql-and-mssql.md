# `USE <db>` as a text query escapes the grant's database on MySQL and SQL Server

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

**Depends on `specs/todos/2026-08-13-20-sql-comments-evade-every-oracle-blocked-pattern.md`,
which is scheduled to land first despite its higher number.** That spec adds a
literal-aware comment normaliser feeding a scratch copy that
`oracleBlockedPatterns` and `IsWriteQuery`/`IsDDLQuery` match against. The
"leading comments" shape listed below is the *same* problem — `USE/**/otherdb`
and `/* x */ USE otherdb` — so this spec must reuse that normaliser rather than
hand-rolling comment handling in a second place. Two implementations of "what
does this statement really say" is exactly the drift the Implementation section
warns about below. MySQL additionally accepts `# …` line comments and requires
whitespace after `--`; confirm the normaliser covers both before relying on it
here.

## Goal

Refuse a mid-session database switch on the MySQL and SQL Server proxies the way
the Oracle proxy now refuses `ALTER SESSION SET CONTAINER`, and the way the
MySQL proxy already refuses `COM_INIT_DB`. Today the *text* form — `USE otherdb`
sent as an ordinary query — is forwarded upstream unexamined on both protocols.

## Why

Found while implementing
`specs/todos/2026-08-13-14-alter-session-set-container-escapes-a-full-write-grant.md`,
whose last bullet asks whether the other four protocols have the same escape. Two
of them do; two do not. Measured, per protocol:

- **MySQL — HOLE.** `handler.UseDB` in
  `internal/proxy/mysql/intercept.go` refuses a post-auth switch with
  `ErrSwitchDatabaseDenied`, exactly as the phase-2 spec decided
  (`specs/done/2026/04/2026-04-25-mysql-phase2-query-interception.md`, "refuse
  `COM_INIT_DB` if it would change to a different database"). But `UseDB` is only
  reached from the `COM_INIT_DB` packet: go-mysql's `server/command.go` dispatch
  routes `COM_QUERY` straight to `HandleQuery`, so a client that sends the SQL
  text `USE otherdb` (which is what `mysql -e "USE otherdb"` and most drivers'
  `Exec` do) never touches `UseDB`. `USE` is not in `writeKeywords` or
  `ddlKeywords` and matches no `mysqlBlockedPatterns` entry, so
  `ValidateMySQLQuery` returns nil under *every* grant, read-only included, and
  the statement is executed on the upstream connection. If the upstream
  credentials can see another schema, the session moves there and every
  subsequent `queries` row names the granted database rather than the one the
  statement actually ran against.
- **SQL Server — HOLE, same shape and slightly worse.** `resolveDatabase`
  (`internal/proxy/mssql/auth.go`) reads the LOGIN7 database field as the dbbat
  row name and the upstream LOGIN7 is rewritten with the server row's
  `DatabaseName` — which sets the *initial* database context only; TDS pins
  nothing. Every batch then goes through `session.validate`
  (`internal/proxy/mssql/intercept.go`), which runs `shared.ValidateQuery` plus
  the bulk-copy pattern and nothing else: there is **no MSSQL blocked-pattern
  list at all**, unlike Oracle and MySQL. `USE otherdb` is an ordinary batch,
  passes, and switches the session. The same text inside `sp_executesql` takes
  the identical path. The proxy does not even *observe* the change: post-login
  `ENVCHANGE` (token `0xE3`) is never parsed — `scanToken`/`recordToken` in
  `internal/proxy/mssql/tokens.go` only read the login response. SQL Server
  additionally has *three-part naming* (`SELECT * FROM otherdb.dbo.t`), which
  reaches another database with no switch to intercept — a strictly harder
  problem that this spec should scope explicitly rather than pretend to solve.
- **PostgreSQL — no hole, and for the right reason.** The database is chosen once
  in the StartupMessage and cannot be renegotiated on an open connection; `\c` is
  a client-side *reconnect*, which sends a fresh StartupMessage through the proxy
  and re-runs `GetServerByName` + `GetActiveGrant`. The residual is `dblink` /
  `postgres_fdw` — neither is mentioned anywhere in the repo, and
  `SELECT * FROM dblink('dbname=otherdb', …)` would read another database of the
  same cluster under a full-write grant if the extension is installed. That needs
  preconditions the two holes above do not (`CREATE EXTENSION` is DDL, so it is
  refused under `read_only`/`block_ddl`), so it is environment-dependent rather
  than a confirmed hole — note it in `docs/` if this work touches the PG
  validators, do not chase it here.
- **MongoDB — no hole on the main path.** The target is the per-message `$db`,
  checked on *every* command by `mongoDatabaseAllowed` in
  `internal/proxy/shared/validation.go` against the session's `store.Server`, so
  there is no switch to make. Two narrower gaps found alongside it are filed
  separately as `specs/todos/2026-08-13-19-*.md`.

Worth recording, because it is what makes all four of these *audit* problems and
not just access problems: `queries` carries no database column of its own
(`internal/migrations/sql/20260107000000_initial_schema.up.sql`) — only
`connection_id`, and `connections.database_id` is pinned at auth. So a statement
that runs against another database is not merely permitted, it is attributed to
the granted one, with nothing in the row to contradict it.

The Oracle fix went in as an outright refusal because the escape is not a
function of grant controls — a full-write grant on one database is not a grant on
another. The same reasoning applies here, and unlike Oracle these two do not even
need a new policy decision: MySQL already made it and only enforces it on one of
the two paths a client can take.

## Implementation

- MySQL: catch the text form before it reaches the upstream. The natural home is
  `internal/proxy/shared/validation.go` — a `USE` matcher used by
  `ValidateMySQLQuery` — but the decision needs the session's target database,
  which `ValidateMySQLQuery(sql, grant)` does not carry. Either widen the
  signature or (simpler, and keeps the policy in one place) parse the target out
  in `handler.runIntercepted`/`HandleQuery` in
  `internal/proxy/mysql/intercept.go` and route it through the existing
  `handler.UseDB`, so `COM_INIT_DB` and `USE` share one decision and one
  `ErrSwitchDatabaseDenied`. Prefer the latter: two implementations of "may this
  session change database" is exactly the drift that becomes an authorization
  bug.
- Watch the shapes: `USE otherdb`, `use `otherdb``, `USE otherdb;`, leading
  comments, and `USE <the granted database>` — which must stay allowed, since
  clients emit it routinely on connect. Prepared statements (`HandleStmtPrepare`
  / `HandleStmtExecute`) take the same text, so the check belongs where both
  paths pass.
- SQL Server: same check on the batch path in `internal/proxy/mssql/`. Find where
  a SQL batch is validated (the `ValidateQuery` call site) and refuse a `USE`
  naming anything other than the session's database, with a TDS error the client
  can read — see the error-number table in `docs/mssql.md`.
- Three-part names on SQL Server: **out of scope here**, but record the decision.
  A grant is scoped to a server row, and dbbat cannot enforce a per-database
  boundary against arbitrary cross-database references without a real parser.
  Either document it as a known limitation in `docs/mssql.md` (next to the other
  "what is deliberately not enforced" notes) or file it separately.
- Tests: `internal/proxy/shared/validation_test.go` if the matcher lands there,
  otherwise the proxies' own unit tests next to the existing `UseDB` coverage.
  Pin the allowed case (`USE <granted db>`) as hard as the refused one, and pin
  that the refusal is independent of grant controls — a full-write grant must be
  refused too, which is the whole point.
- Docs: `docs/mysql.md` line ~175 currently says "`COM_INIT_DB` (USE database) is
  allowed but logged", which is doubly stale — it is refused, and the parenthesis
  equates it with a text `USE` that takes a different path. Fix it as part of
  this work.

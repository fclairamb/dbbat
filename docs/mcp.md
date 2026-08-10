# MCP: governed database access for AI agents

dbbat speaks the [Model Context Protocol](https://modelcontextprotocol.io), so
Claude Code, Claude Desktop, or anything MCP-capable can query databases
*through the governance pipeline that already exists*: scoped to the caller's
grants, time-boxed, every statement logged, and approval holds intact.

**On by default** (`DBB_MCP_ENABLED`), unlike approval holds. The endpoint is
API-key gated and grants an agent nothing its key holder could not already do
with `psql`.

---

## The design decision the rest of this page follows from

**An agent's statement is executed by dialing dbbat's own proxy listener over
loopback**, with a real protocol client, authenticating as the API key's owner
with that key as the password:

```
Claude ──HTTP/MCP──► dbbat API ──loopback protocol client──► dbbat proxy ──► your database
                                                              ▲
                                                              └── the same auth, grant,
                                                                  interception, approval
                                                                  and logging pipeline
                                                                  psql goes through
```

There is deliberately **no internal execution path**. Everything that makes
dbbat a control — `read_only`, `block_ddl`, `block_copy`, quotas, expiry,
revocation, query rows in `/queries`, the pcapng session capture, the
mid-flight approval gate — is not reimplemented for MCP and cannot drift from
the proxy, because MCP *is* a proxy client.

That is not incidental tidiness. A parallel execution path is precisely the bug
class fixed in #306 and #308, where statements reached a database without
passing the approval gate. If you are extending this package and find yourself
reaching for `*store.Store` or a driver pointed at a customer database in order
to run an agent's SQL, stop: the design has been broken.

The store is read for exactly one thing — listing the grants the caller holds,
which is metadata they can already fetch from `GET /api/v1/grants` — and never
to execute or authorize a statement.

### What this costs

One extra TCP connection per statement, on loopback, plus the proxy's own
per-session setup. In exchange, the enforcement surface of the MCP feature is
zero lines of new code.

---

## Connecting

The endpoint is Streamable HTTP at:

```
POST https://<your-dbbat>/api/v1/mcp
Authorization: Bearer dbb_…
```

Any existing `dbb_` API key works — the same one you would paste into a
connection string. Create one from the UI (**API Keys**) or with
`POST /api/v1/keys`.

Claude Code:

```bash
claude mcp add --transport http dbbat https://dbbat.example.com/api/v1/mcp \
  --header "Authorization: Bearer dbb_your_key_here"
```

`.mcp.json` / Claude Desktop:

```jsonc
{
  "mcpServers": {
    "dbbat": {
      "type": "http",
      "url": "https://dbbat.example.com/api/v1/mcp",
      "headers": { "Authorization": "Bearer dbb_your_key_here" }
    }
  }
}
```

There is no `dbbat mcp` stdio subcommand. The HTTP endpoint is the whole
surface; a local stdio bridge would only forward to it.

### API keys only

Basic Auth and browser session tokens reach the endpoint's middleware and are
then refused with `403`. The key here is not merely the caller's credential: it
is also the password the loopback protocol client authenticates to the proxy
with. Accepting Basic Auth would mean this endpoint could be driven with a
user's login password, and a browser session token has no business running SQL
on an agent's behalf.

### Stateless sessions

The server sets no `Mcp-Session-Id` and keeps no per-session state: every HTTP
request is re-authenticated, and the MCP server handed to the transport is
built fresh with its tool closures bound to *that request's* caller.

The practical consequence is the one that matters: **a revoked key stops
working on the next call**, not at the end of a session nobody is tracking.
`GET` and `DELETE /api/v1/mcp` therefore answer `405`; they are registered so a
client's transport probe gets the protocol's own answer instead of a `404` it
has to guess at.

---

## Supported protocols

All five. Each is a loopback client and a switch case; none of them adds
enforcement.

| Protocol | Client | Loopback leg | Statement |
|---|---|---|---|
| PostgreSQL | `pgx` | plaintext | SQL, `$1…` binds |
| MySQL / MariaDB | `go-mysql` | plaintext | SQL, `?` binds |
| Oracle | `go-ora` | plaintext (the listener speaks plain TNS) | SQL, `:1…` binds |
| Microsoft SQL Server | `go-mssqldb` | plaintext (`ENCRYPT_NOT_SUP`) | SQL, `@p1…` binds |
| MongoDB | `mongo-driver` | **TLS**, self-signed cert accepted | `<command> <extended JSON>`, no binds |

An SSH-only entry is not a database: it stays `supported: false` in
`list_databases`, and `query` refuses it. A protocol that is supported but
whose listener is disabled in this process (`DBB_LISTEN_ORA`,
`DBB_LISTEN_MONGO`, …) is refused too — there is nothing of ours to dial, and
the executor will not find another way to the database.

Adding a protocol means adding a case to `SupportedProtocol`, a loopback client
next to `exec_postgresql.go` / `exec_mysql.go`, and nothing else — no auth, no
grant check, no approval plumbing.

### What travels as the database name

Every proxy resolves its target with `GetServerByName`, so what the loopback
client puts in the connection string is always the **dbbat entry's name**:
PostgreSQL's startup `database`, MySQL's schema, Oracle's EZ-Connect service,
SQL Server's LOGIN7 database, MongoDB's `authSource`.

Oracle is the one worth spelling out: several dbbat entries may proxy one
mutualized upstream `SERVICE_NAME`, so sending the raw service name would
resolve to an arbitrary one of them.

MongoDB is the exception in the other direction. A MongoDB command carries its
own `$db` inside the message and the proxy forwards it verbatim, so that field
has to name the database as the *upstream* knows it. `ExecRequest` therefore
carries both names, and only the MongoDB client reads the second.

### Per-protocol notes

**Oracle.** The connection is `user/key@host:port/<dbbat name>` with
`PROGRAM=dbbat-mcp`. The approval gate is *not* at parity with the other four,
and the MCP layer does nothing about it: dbbat's TTC parser locates a statement
heuristically, so a frame whose SQL it cannot decode is forwarded ungated and
unlogged, and a piggyback re-execution naming a cursor dbbat never saw parsed
is forwarded ungated too. A cursor re-execution dbbat *did* see parsed is
gated, on every execution. See `docs/approvals.md`. The consequence for an
agent is the same as for a human at `sqlplus`, which is the point — but do not
read "Oracle is supported" as "Oracle holds are airtight".

**SQL Server.** The loopback leg asks for `encrypt=disable`. The proxy never
*requires* encryption on its client leg, whatever `DBB_MSSQL_TLS_DISABLE` says,
so no certificate decision arises and `DBB_MSSQL_TLS_MAX_VERSION` (1.2 by
default) never comes into it: there is no handshake to cap.

**MongoDB.** This leg is TLS, because the listener terminates TLS and SASL
PLAIN carries the `dbb_` key inside it. The certificate is dbbat's own
auto-generated self-signed one and the dial never leaves 127.0.0.1, so it is
not verified — the only `InsecureSkipVerify` in the package, and it is scoped
to this dial.

`sql` carries the command as `<command> <extended JSON>`:

```jsonc
{ "database": "prod-mongo", "sql": "find {\"find\":\"users\",\"filter\":{\"active\":true},\"limit\":10}" }
```

That is deliberately the same rendering the proxy writes into `/queries`, so an
approval pattern like `(?i)^delete ` means one thing whether the command came
from `mongosh` or from an agent. A separate `run_command` tool taking a
document was the alternative and was rejected: it would have grown the tool
surface and given patterns a second text to match against.

`find`, `aggregate`, `listCollections` and `listIndexes` are read through a
cursor — its `getMore` round trips go back through the proxy like any other
command — and the cursor is killed when the row cap is hit. Everything else is
one command and one reply document, reported as a single row, with `n` (what
write commands report) surfaced as `rows_affected`. `aggregate` needs its own
`"cursor":{}` option, exactly as it does from any driver: dbbat does not add it,
because adding it would change the text `/queries` records.

---

## Tools

Four, deliberately. Everything below is expressible as "things a `psql` session
through dbbat could do".

### `list_databases`

No arguments; the caller's grants are the scope. Returns, per database: name,
protocol, whether MCP can query it, the grant's expiry, its controls
(`read_only`, `block_ddl`, `block_copy`), how many approval patterns it carries,
and quota usage.

This is what an agent plans against. The expiry matters: a grant can run out
*mid-session*, and the failure then comes from the database, not from the tool
layer.

### `query`

```jsonc
{ "database": "prod-pg", "sql": "SELECT …", "params": [], "max_rows": 200 }
```

The statement is sent to the proxy **untouched**. dbbat never rewrites it —
rewriting would falsify what `/queries` records and what approval patterns
match, and a `LIMIT` silently appended by a tool layer is exactly the kind of
divergence that makes an audit log untrustworthy.

`params` are bind parameters (`$1…` on PostgreSQL, `?` on MySQL, `:1…` on
Oracle, `@p1…` on SQL Server). Using them is what an agent should do with
values; a non-empty `params` also forces the prepared-statement path. MongoDB
has no bind parameters — values live in the command document.

Rows come back as `[{column: value}]` plus a `columns` array for ordering.
Repeated column names are suffixed (`id`, `id_2`) so a row never loses a column
the agent can see in `columns`.

### Row capping is server-side

`max_rows` is clamped: unset gives 200, anything above 1000 gives 1000 with
`truncated: true`. **The cap is applied before the statement runs**, whatever
the agent asked for. A model's idea of a reasonable page size is not a server
limit.

On MySQL the no-parameter path streams and stops reading at the cap rather than
buffering the result set, so `SELECT * FROM events` cannot make the dbbat
process hold a table in memory. The parameterized path buffers (go-mysql only
offers prepared execution through its buffering API) and truncates afterwards.
Oracle, SQL Server and MongoDB all stop reading at the cap as well.

`rows_affected` is reported wherever the protocol reports it. On Oracle and SQL
Server that means the client has to choose `Exec` over `Query` up front, which
it does from the statement's leading keyword; a T-SQL write carrying an `OUTPUT`
clause takes the query path instead, since it returns rows. Guessing wrong there
can only omit `rows_affected` — never lose a row, and never change what the
proxy enforces, which is decided on the other end of the socket.

The row cap is a *response* limit, not a data-volume control. That is what the
grant's `max_bytes_transferred` quota is for, and it keeps running during MCP
sessions exactly as it does for any other client.

### `describe`

`{database, table?, schema?}` — lists the database's tables, or one table's
columns. Introspection runs through the same governed path as `query`, so it is
logged like everything else and can itself be held by an approval pattern that
matches `SELECT`.

| Protocol | Tables | Columns |
|---|---|---|
| PostgreSQL | `information_schema.tables` | `information_schema.columns` |
| MySQL / MariaDB | `information_schema.tables`, current schema | `information_schema.columns` |
| Oracle | `ALL_TABLES`, current schema | `ALL_TAB_COLUMNS` |
| SQL Server | `INFORMATION_SCHEMA.TABLES` | `INFORMATION_SCHEMA.COLUMNS` |
| MongoDB | `listCollections` | one **sampled document** |

Table and schema names travel as bind parameters, never interpolated. The input
comes from a model. MongoDB has neither SQL nor bind parameters, so the same
rule is kept the only other way it can be: the collection name is a *value* in
a document handed to the BSON encoder, never text concatenated into the
command.

Two protocol-specific truths the output states rather than hides. Oracle folds
unquoted identifiers to upper case, so the bound table name is folded the same
way (`UPPER(:1)`) and `DATA_DEFAULT` is not read at all — it is a `LONG`, and a
`LONG` in a catalog query is how a describe fails on a driver quirk instead of
returning columns. MongoDB has no column catalog, so a described collection
reports the top-level fields of **one sampled document**, and says so in
`message`: another document in the same collection may look nothing like it.

### `await_approval`

`{execution_id, timeout_seconds?}` — see below.

---

## Approval holds, and the agent that must not time out

A held statement blocks the wire connection until a human decides
(`docs/approvals.md`). That is fine for `psql`, which simply sits there, and
awkward for an MCP request, which cannot.

The shape:

1. `query` starts the statement on a loopback connection and waits **10
   seconds**.
2. If it comes back, you get rows. Ordinary statements never pay for any of
   this.
3. If it does not, the connection **stays parked in the background** and the
   tool returns a structured result instead of hanging:

```jsonc
{
  "status": "approval_pending",
  "execution_id": "9f2c…",
  "query_uid": "0192…",          // the statement a human is being shown
  "approval_pattern": "(?i)^\\s*DELETE",
  "message": "…call await_approval with this execution_id…"
}
```

4. `await_approval` blocks for up to 55 seconds (configurable per call, capped
   at 120) and then answers: the final result, or the same pending status
   again.

**The agent never silently times out.** Every return names the next action.
That is the whole contract, and it is why `await_approval` answers "still
pending" rather than letting a request die on an intermediary's idle timeout.

A statement that is slow but *not* held returns `status: "still_running"`
instead, with the same execution id. The distinction matters: an agent should
not go looking for an approver who does not exist.

Meanwhile the ordinary escalation runs: after `DBB_APPROVAL_SLACK_DELAY` the
hold posts to Slack with Approve/Deny buttons, exactly as it would for a human's
`psql` session. The approver sees the statement, the user and the database —
and self-approval is rejected, so an agent acting as its owner cannot wave its
own statement through.

### How the held query is identified

The `query_uid` comes from the `approvals/pending` event topic
(`internal/events`), matched on (user, database, statement) within the
execution's own lifetime, and claimed exclusively so two concurrent executions
of the same statement cannot adopt one hold.

It is matched that way because the loopback client never learns the connection
uid dbbat assigned it — the proxy mints it server-side and nothing on the wire
carries it back. Being wrong costs an agent a mislabelled uid in an advisory
message; it can never cause a statement to run when it should not, because
whether the statement ran is decided by the loopback connection returning, not
by this correlation.

### Why `await_approval` does not read the approval registry

`internal/approval.Registry` is a *delivery* mechanism: it wakes a parked proxy
session when a decision arrives. It answers "is a hold still parked here", not
"did the statement run and what did it return".

`await_approval` waits on the loopback execution itself instead, which is the
same thing the client waiting on a socket is doing. That is deliberately the
stronger source of truth: the statement's outcome is decided by the connection
returning, never by a second bookkeeping structure that could disagree with it.
The registry stays exactly what it is — the proxy's own plumbing — and the MCP
layer stays a client.

### Bounds on a parked execution

A hold has no timeout by design. An MCP execution does: after **30 minutes**
the loopback socket is closed, which ends the hold as `abandoned` — the
ordinary "the client gave up" outcome, with nothing forwarded upstream. A
finished execution stays collectable for 10 minutes so a late
`await_approval` gets its rows rather than a mystery.

### Multiple replicas

An execution lives on the replica that started it. If `await_approval` is
load-balanced to a different pod it answers `unknown execution id` explicitly,
telling the agent to look the statement up in dbbat rather than re-run it — a
retry would park a second connection on the same approver. Pin MCP traffic to
one replica (session affinity) if your agents lean on long holds.

---

## What is logged

Everything, in the ordinary places, because nothing here is a special path:

- a **connection** row per statement (one loopback session per `query`), owned
  by the API key's user;
- a **query** row with the SQL, parameters, duration and any approval state;
- **result rows** captured under the usual `DBB_QUERY_STORAGE_*` settings;
- a **pcapng capture** if `DBB_DUMP_DIR` is set;
- **audit** entries for every approval decision.

The loopback session advertises `application_name = dbbat-mcp`, which the proxy
folds into the upstream application name, so an agent's traffic is
distinguishable from a human's in the target database's own views
(`pg_stat_activity`, `performance_schema`).

---

## Configuration

| Variable | Description | Default |
|---|---|---|
| `DBB_MCP_ENABLED` | Register `POST/GET/DELETE /api/v1/mcp` | `true` |

Set it to `false` and the routes do not exist at all — a disabled feature
should not answer, not even with a `403`.

Related settings that shape what an agent can do, all pre-existing:
`DBB_APPROVAL_ENABLED`, `DBB_APPROVAL_SLACK_DELAY`, `DBB_LISTEN_PG`,
`DBB_LISTEN_MYSQL`, `DBB_LISTEN_ORA`, `DBB_LISTEN_MONGO`, `DBB_LISTEN_MSSQL`,
`DBB_QUERY_STORAGE_RETENTION`.

---

## Operating notes

- **Give agents their own user.** The MCP session acts as the key's owner, so
  attribution, grants and quotas are per-user. An agent sharing a human's key
  makes `/queries` unreadable and makes self-approval indistinguishable.
- **Approval patterns are the control that matters here.** An agent is a client
  that can write a `DELETE` it did not fully think through; `(?i)^\s*DELETE\s+FROM`
  on the definition is what puts a human in front of it.
- **Prefer short grants.** Nothing about MCP changes the expiry model, and an
  agent with a two-hour grant is exactly as bounded as a human with one.
- **The write deadline is extended, not cleared.** The API server's 15s write
  timeout would kill an `await_approval`; the MCP handler raises it to 5
  minutes per request so a wedged request still cannot pin a connection
  forever.

---

## Implementation map

| File | What lives there |
|---|---|
| `internal/mcp/doc.go` | the loopback-execution decision, stated once |
| `internal/mcp/server.go` | per-request MCP server, grant resolution, row-cap clamp, execution start |
| `internal/mcp/tools.go` | the four tools and their schemas |
| `internal/mcp/exec.go` | executor interface, protocol dispatch, loopback address resolution |
| `internal/mcp/exec_postgresql.go` | pgx loopback client |
| `internal/mcp/exec_mysql.go` | go-mysql loopback client |
| `internal/mcp/exec_sql.go` | the database/sql half Oracle and SQL Server share |
| `internal/mcp/exec_oracle.go` | go-ora loopback client |
| `internal/mcp/exec_mssql.go` | go-mssqldb loopback client |
| `internal/mcp/exec_mongodb.go` | mongo-driver loopback client, and the command-text shape |
| `internal/mcp/pending.go` | backgrounded executions, hold correlation, reaping |
| `internal/api/mcp.go` | route handler: API-key restriction, write deadline, caller injection |

Library: [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk),
the official Go SDK, at v1.7.0 — past 1.0, and it documents the stateless
one-server-per-request deployment this uses, including the schema cache that
makes it cheap.

# Approval holds and the live event stream

Two features that share one transport:

1. **A live event stream** clients subscribe to by topic (`GET /api/v1/stream`).
2. **Approval holds** — query patterns, carried by the grant, that suspend a
   statement mid-flight until a second human approves or denies it.

Approval gates the *statement*, not the *data*: an approved `SELECT` can still
read the whole table. This complements quotas and `read_only`; it does not
replace them.

**Ships off by default.** Set `DBB_APPROVAL_ENABLED=true` to turn it on. Until
then, approval patterns on grants are inert and nothing is ever held.

---

## The hold, in order

1. A statement arrives and passes every static control (`read_only`,
   `block_copy`, `block_ddl`, the protocol blocklists). Cheap deterministic
   denies stay cheap — the gate runs after them.
2. The statement is matched against the grant's `approval_patterns` (Go RE2,
   compiled once per session). No match ⇒ nothing changes.
3. On a match, dbbat **persists the query row immediately** with
   `approval_status = 'pending'`, so the held statement is visible in
   `/queries` and addressable by UID *while it hangs*.
4. The hold is published to `connection/<uid>/queries` and `approvals/pending`,
   and a Slack escalation timer is armed.
5. The session goroutine blocks. It wakes on exactly one of: **approved**,
   **denied**, **client gone**, **quota/expiry/revocation**, **shutdown**.
6. Approve ⇒ the statement is forwarded and runs normally. Deny ⇒ the client
   gets a protocol-native error carrying the approver's reason, over the same
   path `ErrDDLBlocked` uses. Anything else ⇒ `abandoned`, and **nothing is
   ever forwarded upstream**.

The gate fails closed. There is no path through it that forwards a statement
without an explicit approval decision naming that statement's UID.

## There is no approval timeout

A held query waits indefinitely. The only clocks are the client's own socket
timeout, the grant's expiry, and its quotas.

**Operator note.** Because dbbat imposes no timeout, the practical backstop for
an idle upstream transaction — and any locks it holds — is the *target
server's* own setting:

| Target | Setting |
|---|---|
| PostgreSQL | `idle_in_transaction_session_timeout` (and `statement_timeout`) |
| MySQL / MariaDB | `wait_timeout`, `innodb_lock_wait_timeout` |
| Oracle | profile `IDLE_TIME`, resource manager plans |
| MongoDB | `maxTimeMS` on the client side |

Set them. A hold that nobody answers otherwise parks an upstream connection for
as long as the client is willing to wait.

`POST /api/v1/queries/pending/deny-all` is the bulk safety valve — one admin
action that clears every parked statement after a bad deploy, during an
incident, or at the end of a shift.

## `abandoned` is not `denied`

The most common outcome in production is **not** a human decision: the client
gives up first (its own timeout, a Ctrl-C, a killed process), and the hold ends
as `abandoned`. That is correct and safe — nothing ran — but it must never read
as a rejection, or the workflow looks broken to whoever finally clicks Approve
on a query nobody is waiting for anymore.

`abandoned` therefore renders distinctly from `denied` everywhere: in the UI
badge, in the Slack message, and in the API (`approval_status`).

There is deliberately **no `timeout` status**. It would imply a clock dbbat
does not have.

## Client-disconnect detection

Since disconnect is the sole liveness bound on a hold, it is load-bearing, not
polish. Two mechanisms, both in `internal/proxy/shared/watch.go`:

- **An active read-watch while parked.** The session goroutine stops reading
  the client socket during a hold, so a FIN would otherwise sit unnoticed.
  `WatchedConn.Park()` spawns a reader that reads into a buffered pipe: EOF or
  any terminal error cancels the hold. Bytes a pipelining client sent
  meanwhile are **queued and replayed in stream order** once the session
  resumes — never dropped, never interpreted mid-hold.
  `WatchedConn` sits *below* any TLS layer, so it parks on raw records it never
  has to decrypt.
- **TCP keepalive on client connections** (30 s idle / 10 s interval / 3
  probes). A hard-killed client or a dead network path never produces a FIN;
  without keepalive, "until the client disconnects" silently means *forever*.

## Cancellation while held

A held query stays cancellable through each protocol's normal out-of-band path.
The held session's own socket is blocked by definition, so these all arrive
elsewhere:

| Protocol | Path | Result |
|---|---|---|
| PostgreSQL | `CancelRequest` on a **separate TCP connection**, routed by the BackendKeyData dbbat forwarded | hold → `abandoned`; the cancel is also relayed upstream |
| MySQL / MariaDB | `KILL [QUERY] <id>` from another connection, routed by the connection id dbbat assigned | hold → `abandoned`; the `KILL` still runs upstream |
| MongoDB | `killOperations` | hold → `abandoned` |
| Oracle | client disconnect / session kill | hold → `abandoned` |

## Quotas, expiry and revocation keep running

`shared.LimitGuard` is re-evaluated on a ticker for the whole duration of a
hold. A grant that expires, is revoked, or exhausts its byte quota while a
statement is parked ends the hold with that error. With no approval timeout,
this is the one remaining server-side bound, so the parked state must not
bypass it.

## Shutdown

Draining explicitly abandons every parked statement (`approval_drain` in
`main.go`, running before the servers stop) rather than hanging the shutdown or
silently letting the statements through.

## Who may approve

- Any user with the **`admin`** role, plus
- any member of a group listed in the grant's **`approver_group_uids`**.

**Self-approval is always rejected**, including for admins. Four eyes means
four eyes; an admin who could wave their own statements through would make the
control decorative.

Two independent checks guard substitution:

- The approver decides **by query UID** — exactly the statement they were
  shown.
- The waiting session **asserts the resolved UID is its own** before acting.
  Without it, a client able to influence timing could have somebody else's
  approval released against its statement (a TOCTOU substitution).

Every decision writes `audit_logs`, is returned by `GET /api/v1/queries`, and
is published on the stream as a resolution event carrying the approver's uid
and display name — so every watcher sees *who* unblocked it, not merely that it
unblocked.

## Configuring patterns

Patterns live on the **grant definition** only (no server-level patterns; that
is a possible follow-up, deliberately out of scope). They are mirrored onto
`AccessGrant` at materialization time, exactly as `controls` and quotas are —
the proxy session holds only a `*store.Grant`, and admin-created grants bypass
definitions entirely, so reading patterns off the definition at query time
would mean a join on the hot path *and* zero coverage for direct admin grants.

Direct admin grants accept `approval_patterns` / `approver_group_uids` on
`POST /api/v1/grants` for the same reason.

Patterns are Go `regexp` (RE2 — no catastrophic backtracking, which matters
when the input is attacker-influenced SQL). They are compiled at
definition-save time, so a bad pattern is a `400`, not a runtime surprise.

Bounds: at most 32 patterns per definition, 512 characters each.

### Write your patterns case-insensitively

**Matching is case-sensitive unless you make it otherwise.** The statement is
only *trimmed* before matching (`shared.NormalizeSQL`) — it is not
upper-cased. This differs from the static controls (`read_only`, `block_ddl`,
`block_copy`), which upper-case the statement before their keyword-prefix
checks and are therefore case-insensitive for free.

The divergence is deliberate: an approval pattern is a full regexp an operator
writes and then reads back against the SQL shown in `/queries`, so silently
matching against a rewritten string would make patterns behave differently from
what the UI displays. But it has one sharp edge:

```
^DELETE          ← matches "DELETE FROM users", NOT "delete from users"
(?i)^DELETE      ← matches both. Write this one.
```

Prefix essentially every pattern with `(?i)`. The definition form's placeholder
shows the same convention, and so does every example in this document:

```
(?i)^\s*DELETE\s+FROM
(?i)^\s*(GRANT|REVOKE)
```

A pattern that misses is a hold that never happens — it fails *open* relative
to your intent, so this is worth getting right.

```jsonc
// POST /api/v1/grant-definitions
{
  "name": "prod-oncall",
  "duration_seconds": 3600,
  "approval_patterns": [
    "(?i)^\\s*DELETE\\s+FROM",
    "(?i)^\\s*(GRANT|REVOKE)\\b",
    "(?i)^\\s*ALTER\\s+TABLE"
  ],
  "approver_group_uids": ["…sre group uid…"]
}
```

## Slack escalation

A hold still pending after `DBB_APPROVAL_SLACK_DELAY` (default `30s`, `0`
disables) posts to the configured Slack channel with the user, database,
matched pattern, elapsed time, truncated SQL and a deep link to
`${DBB_PUBLIC_URL}/app/connections/<uid>?watch=1`. Approve/Deny buttons appear
when an inbound interaction transport is configured (signing secret or Socket
Mode).

The trigger is **purely time-based** — no live-subscriber detection, no
cluster-wide presence bookkeeping. If an admin was watching, they had the delay
to act.

Because there is no approval timeout, the message would otherwise stay
actionable indefinitely, so it is **updated in place the moment the hold
resolves by any route** (UI, Slack, client disconnect, grant expiry). A stale
Approve button that silently no-ops is worse than no button, and here it could
linger for hours.

Exactly one replica posts per hold: the timer is owned by the replica running
the parked session, which is pinned to one pod anyway, so no election is
needed.

**Redaction.** SQL text in Slack is truncated to 500 characters and can be
switched off entirely with `DBB_APPROVAL_SLACK_SQL=false`. Slack is a lower
trust boundary than the dbbat UI, and this feature pipes production query text
into it.

---

## The event stream

`GET /api/v1/stream` upgrades to a WebSocket.

### Authentication

A normal `Authorization: Bearer …` header works for non-browser clients.
Browsers cannot set one on a WebSocket handshake, so the token may instead ride
the subprotocol:

```
Sec-WebSocket-Protocol: dbbat.auth.bearer.<token>
```

The server echoes the selected subprotocol back. This is deliberately *not* a
query parameter: a token in the URL leaks into access logs, proxy logs and
`Referer` headers.

### Envelope

```jsonc
// client → server
{"type": "subscribe",   "topic": "connection/<uid>/queries"}
{"type": "unsubscribe", "topic": "connection/<uid>/queries"}

// server → client
{"type": "subscribed",   "topic": "…", "error": null}
{"type": "unsubscribed", "topic": "…"}
{"type": "event", "topic": "…", "seq": 1234, "event": "query", "at": "…", "data": {…}}
{"type": "lagged", "dropped": 12}
```

`event` is one of `query`, `approval_pending`, `approval_resolved`,
`connection`. Every query-shaped `data` carries `query_uid`, `connection_uid`,
`sql_text`, `executed_at`, `approval_required` and `approval_status`;
resolution events additionally carry `resolved_by` (`{uid, username,
display_name}`), `resolved_at` and `resolution_reason`.

### Topics and authorization

| Topic | Payload | Who may subscribe |
|---|---|---|
| `connection/<uid>/queries` | every query on that connection | admin, viewer, or the connection's owner |
| `approvals/pending` | approval-pending queries, all connections | admin, or a member of an approver group |
| `connections` | connection open/close | admin |

Authorization is per-topic, evaluated **at subscribe time and re-checked on
every send**. A topic grants exactly what the equivalent REST endpoint would.
This matters more than it looks: `sql_text` routinely contains PII, credentials
and business data, so the stream must never become a wider read path than
`GET /api/v1/queries` already is.

Topic names are validated for shape before anything else happens (`connections`,
`approvals/pending`, or `connection/<uuid>/queries`, at most 128 characters), so
an unknown topic is answered `{"error":"unknown topic"}` without reaching the
store or the per-socket authorization memo. A socket may hold at most 64
subscriptions, and the memo itself is capped at 128 entries — together those
stop a client from making its own socket accumulate state.

There is deliberately **no global all-queries topic**. A busy proxy issues
thousands of statements a second, each carrying full SQL text; firehosing that
would be both a throughput problem and a data-exposure one.

### Backpressure

Each subscriber has a bounded send buffer (256 events). On overflow, events are
dropped and a `lagged` frame is emitted rather than blocking the proxy hot path
or growing unboundedly. **The `approvals/pending` topic is exempt from
dropping** — a lost pending event means a live database connection waits on
somebody who never learned about it.

Publishing is non-blocking and never returns an error to the session: **a
broken stream must never break a database connection.** That is structural,
not conventional: `Broker.Publish` only touches in-memory state. The per-topic
authorization re-check — which reads the store — runs in each socket's own
write loop, immediately before the write, so a slow database costs one client
latency and can never stall a proxy session parked on a hold. Decisions are
memoized for two seconds, bounding both the database load on a busy stream and
how long a reader whose access was revoked can keep receiving.

Clients treat the stream as best-effort for history and authoritative only for
"what is pending now". On any gap — a `lagged` frame or a reconnect — refetch
`GET /api/v1/queries/pending`.

### Multiple replicas

dbbat runs several pods. The session holding a parked query is on replica A
while the admin's socket is on replica B, so the in-process broker is not
enough. Events and decisions fan out over PostgreSQL `LISTEN`/`NOTIFY` on the
existing store connection (`dbbat_query_events`, `dbbat_approval_events`) — no
new infrastructure.

The payload is only the topic plus the query UID; receivers re-read the row.
That keeps SQL text out of the database's notification queue, and means a late
subscriber sees current state rather than a stale snapshot.

---

## Why WebSocket (a recorded trade-off)

The stream is ~99% server→client; the only client→server messages are
subscribe/unsubscribe. **SSE (`text/event-stream`) plus plain `POST /approve`
would fit better**: automatic browser reconnection, no upgrade handshake, no
ping/pong keepalive to hand-roll, no proxy/load-balancer upgrade configuration,
and it works with the existing gin middleware stack unchanged.

WebSocket was specified, so WebSocket is what ships. The mitigation is that
**nothing above the socket knows it is a socket**: the envelope above is
transport-agnostic, `internal/events` has no HTTP types in its API, and the
frontend's `useEventStream` hook exposes topics and callbacks with no WebSocket
in its public surface. A later swap is confined to `internal/api/stream.go` and
the hook's internals.

If you are deploying behind a load balancer or ingress that does not pass
WebSocket upgrades, the stream degrades to nothing and the UI falls back to
polling `GET /api/v1/queries/pending` every 60 seconds — the pending count
stays correct, only the live feed goes away.

## Per-protocol notes

| Protocol | Hook point | Notes |
|---|---|---|
| PostgreSQL | simple `Query`, and `Execute` (**not** `Parse`) | At Parse time the bind parameters aren't known and the SQL that ultimately runs is the portal's. Validated first — start here. |
| MySQL / MariaDB | `COM_QUERY` / `COM_STMT_EXECUTE`, inside `runIntercepted` | go-mysql owns the wire; the hold happens before `exec()`. |
| MongoDB | `OP_MSG` command dispatch | Matching runs against the rendered `<command> <extJSON>` text, which is what `/queries` shows. |
| Oracle | `OALL8` and the v315+ piggyback exec | Oracle clients are the least forgiving about a silent connection — expect `abandoned` more often here. |

## Schema

On `queries`: `approval_status` (`null|pending|approved|denied|abandoned`),
`approval_pattern`, `resolved_by`, `resolved_at`, `resolution_reason`, plus a
partial index on `approval_status = 'pending'`.

On `grant_definitions` and `access_grants`: `approval_patterns text[]`,
`approver_group_uids uuid[]`.

## Environment variables

| Variable | Description | Default |
|---|---|---|
| `DBB_APPROVAL_ENABLED` | Enable approval holds | `false` |
| `DBB_APPROVAL_SLACK_DELAY` | Delay before a pending hold escalates to Slack; `0` disables | `30s` |
| `DBB_APPROVAL_SLACK_SQL` | Include (truncated) SQL text in the Slack message | `true` |

Slack escalation additionally needs `DBB_SLACK_NOTIFY_BOT_TOKEN`,
`DBB_SLACK_NOTIFY_CHANNEL` and `DBB_PUBLIC_URL`; Approve/Deny buttons need
`DBB_SLACK_SIGNING_SECRET` or `DBB_SLACK_NOTIFY_APP_TOKEN`.

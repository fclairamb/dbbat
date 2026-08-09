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
2. The statement is matched against the `approval_patterns` of the definition
   the grant was issued from (Go RE2, compiled once per session). No match ⇒
   nothing changes.
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

The gate fails closed. Once a statement has been matched, there is no path
through the gate that forwards it without an explicit approval decision naming
that statement's UID — not a denial, not a lost decision, not a failure to even
persist the pending row (`ErrApprovalUnavailable`).

**The gate only sees statements the protocol layer hands it, and that is the
part with a caveat.** A statement must first be recognized as a statement and
its SQL text extracted; anything the interceptor cannot decode is forwarded
unmatched *and* unlogged. For four of the five protocols this is not much of a
gap — the wire format is parsed by the driver library that owns it, or (SQL
Server) a malformed request is refused outright. **Oracle is the exception**:
TTC is hand-rolled and the SQL is located heuristically, so a decode failure on
`OALL8`, the v315+ piggyback exec or the JDBC exec is deliberately treated as
"pass through" rather than "refuse" — an unparseable frame must not be able to
break a customer's connection. Two known gaps remain in what the gate *sees* on
Oracle:

- **An undecodable frame.** A statement dbbat cannot decode is neither held nor
  recorded. It is also not checked against `read_only`/`block_ddl`, so this is
  not specific to approvals — the static controls have exactly the same
  dependency on decoding.
- **A piggyback re-execution naming a cursor dbbat never saw parsed.** It is
  forwarded ungated under *any* grant, with a WARN. The two other re-execution
  frames — the SQL-less `OALL8` and an `OFETCH` that starts a fresh query —
  fail closed under a restrictive grant; the piggyback one deliberately does
  not. See "Re-executing a cursor" below.

If an Oracle client of yours is not showing up in `/queries` at all, that is the
first gap: treat missing query rows on Oracle as missing enforcement, and file it.

### Re-executing a cursor **is** gated

An Oracle client can re-run a statement it already parsed by naming the cursor
id alone, with no SQL text on the wire. Three frames do that — a **piggyback
re-execution** (TTC func `0x03`, sub-op `0x4e` for a SELECT and `0x04` for
anything else), the legacy SQL-less `OALL8`, and an `OFETCH` arriving when no
query is in flight. All three are gated against the SQL that cursor was parsed
with: the same normalize → static controls → hold order as the SQL-carrying
path, on **every** execution. A statement matched by an approval pattern is
therefore held again on each re-execution; approving it once does not buy a free
run for the rest of the session.

That covers a cursor dbbat **saw parsed**, which is what makes the SQL known.
The boundaries, all deliberate:

- **A fetch that continues a query already in flight is not re-gated.** It is
  more rows of a statement that has already been through the gate, and holding
  there would park a client mid-result-set. Only the fetch that starts a *fresh*
  pending query — the one that persists its own row in `/queries` — is gated.
- **A SQL-less `OALL8`, or an `OFETCH` starting a fresh query, naming an
  untracked cursor fails closed under a restrictive grant.** If the cursor id
  was never seen parsed on this session, dbbat does not know what the execution
  would run. When the grant carries
  **statement-shaped controls** — a non-empty approval-pattern set, `read_only`,
  or `block_ddl` — the execution is **refused** (`ORA-01031`), the same
  fail-closed shape the SQL Server proxy uses for an unknown prepared-statement
  handle. When the grant carries none of them, the frame is **forwarded** and a
  WARN is logged with the cursor id.

  That conditional refusal is a trade-off, not an oversight. An untracked cursor
  is not by itself an attack — dbbat may have attached mid-session, or the
  tracker entry may be gone — so refusing unconditionally would break permissive
  sessions for no security gain. Refusing exactly where a statement control
  exists keeps the guarantee that matters without that blast radius.

  Both frames answer identically on purpose: an execution dbbat cannot identify
  is the thing being refused, and the wire op carrying it must not be a cheaper
  way past the same grant. (The `OFETCH` half of that used to forward under any
  grant, with only a debug log.)

  One subtlety worth knowing before filing it as a bug: the approval-pattern
  half of "restrictive" is read off the grant regardless of
  `DBB_APPROVAL_ENABLED`. A grant carrying patterns while approvals are globally
  switched off will still refuse an untracked cursor, under a control that is
  inert. That is intentional — it errs fail-closed.
- **A re-execution *is* counted against `max_query_counts`.** Each of the three
  frames records its own `/queries` row, so each is checked against the quota
  before it runs. The check sits on the re-execution branch itself, not on the
  fetch path as a whole: a fetch continuing a result set already streaming is
  never refused by it, which is what keeps a client from being cut off
  mid-result-set. (The response leg's `LimitGuard` independently covers
  revocation, the byte quota and expiry; it does not know about
  `max_query_counts`, which is why the branch has to check it.)
- **A piggyback re-execution naming an untracked cursor is forwarded, not
  refused.** This is the opposite of the two frames above, and
  deliberately so: that frame is what an ordinary `execute()` loop sends, so
  failing it closed would refuse every second execution of a perfectly legal
  read-only session whenever dbbat missed the response that names the cursor.
  Filed as `specs/todos/2026-08-09-oracle-piggyback-reexec-unknown-cursor.md`.

**How often real clients do this: measured, and the answer is "constantly".**
Five captures against Oracle Free 23ai, one per client, are in
`internal/proxy/oracle/testdata/*_cursor_reexec.pcapng`:

| Client | Re-executes by cursor id? |
|--------|---------------------------|
| `go-ora` v3, prepared SELECT and prepared INSERT | yes, every run after the first |
| `python-oracledb` thin, a plain `cur.execute()` loop | yes — its statement cache does it with no prepared-statement API involved |
| JDBC thin (ojdbc11), cached `PreparedStatement` | yes |
| sqlplus / OCI thick | no — resends the full statement text every run |

So the gate is load-bearing, not theoretical: without it a client that parses
once and loops runs everything after the first execution outside `read_only`,
`block_ddl` and every approval pattern. That is no longer hypothetical either —
it is what dbbat did until the piggyback frame was routed through the gate,
because the shape the gate originally handled (the SQL-less `OALL8`) is the
**legacy pre-v315 framing and was not observed from any client tested**. It is
kept as defence in depth for older clients. See the client table and frame
layout in `docs/oracle.md`.

The enforcement tests run both ways: hand-built frames for the legacy shape
(`cursor_reexec_gate_test.go`) and the recordings replayed through the real
intercept paths for the modern one (`cursor_reexec_replay_test.go`).

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
  The SQL Server proxy is the exception: it has no `WatchedConn` at all, and
  keeps its client leg read by a goroutine sitting on the TDS codec *above*
  TLS. It has to, because a TDS cancel arrives in-band on the held session's
  own socket and a byte-level watcher below TLS would only ever see records.
  See `docs/mssql.md`.
- **TCP keepalive on client connections** (30 s idle / 10 s interval / 3
  probes). A hard-killed client or a dead network path never produces a FIN;
  without keepalive, "until the client disconnects" silently means *forever*.

## Cancellation while held

A held query stays cancellable through each protocol's normal cancel path. In
every protocol but one the held session's own socket is not being interpreted,
so the cancel has to arrive elsewhere:

| Protocol | Path | Result |
|---|---|---|
| PostgreSQL | `CancelRequest` on a **separate TCP connection**, routed by the BackendKeyData dbbat forwarded | hold → `abandoned`; the cancel is also relayed upstream |
| MySQL / MariaDB | `KILL [QUERY] <id>` from another connection, routed by the connection id dbbat assigned | hold → `abandoned`; the `KILL` still runs upstream |
| MongoDB | `killOperations` | hold → `abandoned` |
| SQL Server | `ATTENTION` **on the held connection itself** — what a driver sends on a cancelled context or a query timeout | hold → `abandoned`; the ATTENTION is *not* forwarded upstream, and the client gets the `DONE_ATTN` acknowledgement |
| Oracle | client disconnect / session kill | hold → `abandoned` |

SQL Server is in-band because TDS has no side channel: the cancel travels on the
same socket as the statement it cancels. That is why its proxy reads the client
leg from a goroutine of its own — see `docs/mssql.md`.

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
- any member of a group listed in **`approver_group_uids`** on the definition
  the grant was issued from.

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
is a possible follow-up, deliberately out of scope). A grant carries no shape
of its own: it pins the definition *version* it was issued from, and
`store.AccessGrant`'s accessors read the patterns and approver groups back off
that row. The proxy session still holds only a `*store.Grant` — the definition
is attached once, at authentication, so nothing joins on the query hot path.

Every grant comes from a definition, including one an admin assigns directly
with `POST /api/v1/grants`, so there is no longer any grant that approval
gating cannot reach.

Because definitions are immutably versioned — an edit archives the current row
and inserts a successor — editing a definition's patterns never changes what a
live session is gated on. The new patterns apply to grants issued from then on.

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

### Repairing patterns split by the pre-fix storage bug

Every dbbat before the fix in `internal/store/array.go` read `text[]` columns
back through `uptrace/bun`'s PostgreSQL array parser, which treats an element
whose first byte is `(` or `[` as a range literal and terminates it at the
matching bracket. A pattern stored correctly as `(?i)^DELETE` therefore came
back as **two** patterns, `(?i)` and `^DELETE` — and `(?i)` on its own is a
regexp that matches every statement, so such a definition put a hold on *every
query the grant ran*. It fails closed, not open, but it makes the documented
`(?i)…` form unusable.

The stored rows were only damaged if somebody **edited** the definition while
running an affected build: the UI read the split list and wrote it straight
back, and because definitions are immutably versioned, that saved a genuinely
split successor row. Definitions that were never re-saved are intact and need
nothing — upgrading is enough.

Find the suspects. A leading element that is nothing but an inline-flag group
(`(?i)`, `(?s)`, `(?im)`, …) is never something an operator would author on
purpose, so it is a reliable fingerprint:

```sql
SELECT uid, lineage_uid, slug, archived_at, approval_patterns
FROM grant_definitions
WHERE EXISTS (
  SELECT 1 FROM unnest(approval_patterns) AS p
  WHERE p ~ '^\(\?[a-zA-Z-]+\)$'
)
ORDER BY slug, archived_at NULLS FIRST;
```

Repair is deliberately **manual**, and dbbat ships no migration for it. The
split is deterministic, so re-joining `['(?i)', '^DELETE']` into
`'(?i)^DELETE'` is unambiguous *for that row* — but a definition legitimately
authored with two patterns, the first of which happens to be a bracketed group
(`['(a|b)', '^DROP']`), is byte-for-byte indistinguishable from a split one. An
automatic fix would silently rewrite correct definitions. Approval patterns are
a security control, so re-read each row the query above returns and edit it in
the UI (or with `PUT /api/v1/grant-definitions/{uid}`) to the patterns you
meant. Editing versions the definition as usual, so live grants keep running
against the version they were issued from until they are re-issued.

Archived rows (`archived_at IS NOT NULL`) are history: leave them. They are
never used to authorize anything, and rewriting them would falsify the record
of what a past grant was actually gated on.

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

A released statement then produces a *third* event — the `query` event
published once it has actually run — and that one **repeats the hold's
outcome**, resolver included. It has to: it is the last word on that query uid,
and a client folding events by uid would otherwise end up with a row that has
forgotten the statement was ever gated. The proxy's in-memory record knows
nothing about approval (the row was written by the gate, not by the session),
so the gate remembers the hold it just resolved and the publisher stamps it on.

### Topics and authorization

| Topic | Payload | Who may subscribe | What they then receive |
|---|---|---|---|
| `connection/<uid>/queries` | every query on that connection | admin, viewer, or the connection's owner | everything on the topic |
| `approvals/pending` | approval-pending queries, all connections | admin, viewer, or a member of an approver group | admins and viewers: everything. Anybody else: **only holds they are an approver for** |
| `connections` | connection open/close | admin | everything on the topic |

Authorization is evaluated **at subscribe time and re-checked on every send**,
and — for `approvals/pending` — **per event**, not merely per topic. A topic
grants exactly what the equivalent REST endpoint would. This matters more than
it looks: `sql_text` routinely contains PII, credentials and business data, so
the stream must never become a wider read path than `GET /api/v1/queries`
already is.

`approvals/pending` is the one topic where subscribing and receiving are
different questions. It is a single global fan-out carrying every held
statement in the fleet, so a topic-only rule would hand an approver of one
low-sensitivity grant the SQL text, target database and requesting user of
*every* hold on the instance. Each event is therefore filtered through the same
`mayViewQuery` helper `GET /api/v1/queries/pending` filters its rows through —
the same function, not a second implementation, so the two cannot drift. The
decision is memoized per connection for the same couple of seconds as the topic
decision, and anything unresolvable (no query uid, a query that no longer
exists, a store error) is a denial. Resolution events are filtered by exactly
the same rule, as are events forwarded from another replica.

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
not conventional: `Broker.Publish` only touches in-memory state. The
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
| SQL Server | `SQLBatch` / `RPC`, in the client→upstream pump's hook | The one protocol whose cancel is in-band, so the client leg is read by a separate goroutine for the whole session. |
| Oracle | `OALL8`, the v315+ piggyback exec, the JDBC thin driver's `func=0x11` / sub-op `0x69` exec, plus cursor re-executions (the piggyback `0x03`/`0x4e`+`0x04` frames every thin client sends, the legacy SQL-less `OALL8`, and an `OFETCH` that starts a new query) | All go through the same normalize → static controls → hold order; re-executions are gated against the SQL the cursor was parsed with. Oracle clients are the least forgiving about a silent connection — expect `abandoned` more often here. A frame whose SQL cannot be decoded is forwarded ungated; see the caveat under "The hold, in order". |

## Schema

On `queries`: `approval_status` (`null|pending|approved|denied|abandoned`),
`approval_pattern`, `resolved_by`, `resolved_at`, `resolution_reason`, plus a
partial index on `approval_status = 'pending'`.

On `grant_definitions`: `approval_patterns text[]`, `approver_group_uids
uuid[]`. **Not** on `access_grants` — a grant references its definition through
`grant_definition_id` and reads both from there.

## Environment variables

| Variable | Description | Default |
|---|---|---|
| `DBB_APPROVAL_ENABLED` | Enable approval holds | `false` |
| `DBB_APPROVAL_SLACK_DELAY` | Delay before a pending hold escalates to Slack; `0` disables | `30s` |
| `DBB_APPROVAL_SLACK_SQL` | Include (truncated) SQL text in the Slack message | `true` |

Slack escalation additionally needs `DBB_SLACK_NOTIFY_BOT_TOKEN`,
`DBB_SLACK_NOTIFY_CHANNEL` and `DBB_PUBLIC_URL`; Approve/Deny buttons need
`DBB_SLACK_SIGNING_SECRET` or `DBB_SLACK_NOTIFY_APP_TOKEN`.

---
model: opus
effort: xhigh
---

# Live query stream over WebSocket, with pattern-triggered human approval that holds the query

No GitHub issue yet — one should be filed before implementation starts.

## Problem

Today dbbat is *observational after the fact*: a query is validated against the
grant's static controls (`read_only`, `block_copy`, `block_ddl`, plus the
protocol-specific blocklists in `internal/proxy/shared/validation.go`), forwarded
upstream, and persisted asynchronously
(`internal/proxy/postgresql/intercept.go:362`). An admin discovers what happened
by refreshing `/queries`. There are exactly two outcomes for any statement:
allowed, or rejected by a regex/keyword rule.

That binary is too coarse for the cases that actually matter. `DELETE FROM
users` on production, a `GRANT` on a prod role, an `ALTER TABLE` during business
hours: these are not "always deny" (an on-call engineer legitimately needs them)
and not "always allow". They are "allow, *if a second human agrees right now*" —
four-eyes control on the statement itself. Static controls cannot express that,
and a grant broad enough to permit the emergency case is broad enough to permit
the accident.

There is also no live view at all. `GET /api/v1/queries` is a paginated
historical list; there is no way for an admin to watch a session as it happens,
which is the natural companion to (and prerequisite for) an approval workflow.

## Proposal

Two features that share one transport:

1. **A generic real-time event stream** clients subscribe to by topic.
2. **Approval holds**: query patterns, carried by the grant, that suspend a
   statement mid-flight until an admin approves or denies it — surfaced on the
   stream, and escalated to Slack when nobody is watching.

### 1. Transport — generic subscription stream

`GET /api/v1/stream` (WebSocket upgrade), authenticated by the existing session
cookie or `dbb_` API key via `internal/api/middleware.go`. `gorilla/websocket`
is already in the module graph (indirect, pulled in by Slack Socket Mode) —
promote it to a direct dependency.

The socket is a dumb pipe with client→server subscription control:

```jsonc
// client → server
{"type": "subscribe",   "topic": "connection/<uid>/queries"}
{"type": "unsubscribe", "topic": "connection/<uid>/queries"}
// server → client
{"type": "event", "topic": "...", "seq": 1234, "data": { ... }}
{"type": "subscribed", "topic": "...", "error": null}
```

Initial topics — the namespace is the extension point, nothing else about the
transport is query-specific:

| Topic | Payload | Who may subscribe |
|---|---|---|
| `connection/<uid>/queries` | every query on that connection | admin, or the connection's owner |
| `approvals/pending` | approval-pending queries, all connections | admin only |
| `connections` | connection open/close | admin |

**Authorization is per-topic, evaluated at subscribe time and re-checked on
send** — a topic subscription grants exactly what the equivalent REST endpoint
would. This matters more than it looks: `sql_text` routinely contains PII,
credentials and business data, so the stream must not become a wider read path
than `GET /api/v1/queries` already is.

**Backpressure**: bounded per-subscriber send buffer (e.g. 256 events). On
overflow, drop and emit `{"type":"lagged","dropped":N}` rather than blocking the
proxy hot path or growing unboundedly. `approvals/pending` is exempt from
dropping — those events must never be lost.

The proxy publishes to an in-process `Broker` (`internal/events/`). Publishing is
non-blocking and never returns an error to the session: **a broken stream must
never break a database connection.**

### 2. Approval patterns on the grant

**Decision: patterns live on the grant definition only** — not on the server. A
server-level ("everything on prod needs approval") set may come later; it is
deliberately out of scope here.

Add to `GrantDefinition` (`internal/store/models.go:436`):

```go
// SQL patterns (RE2) that suspend a matching statement until an admin or an
// approver-group member approves it. Empty = no approval gating.
ApprovalPatterns []string `bun:"approval_patterns,array,notnull,default:'{}'"`
// Groups whose members may resolve holds on grants built from this
// definition, *in addition to* admins. Empty = admins only.
ApproverGroupUIDs []uuid.UUID `bun:"approver_group_uids,array,notnull,default:'{}'"`
```

and **mirror both onto `AccessGrant`** (`internal/store/models.go:380`) at
materialization time, exactly as `Controls` / `MaxQueryCounts` already are. This
is the important structural decision: the proxy session holds an `*store.Grant`
and nothing else, and admin-created grants bypass definitions entirely — reading
patterns off the definition at query time would mean a join on the hot path and
zero coverage for direct admin grants.

Patterns are Go `regexp` (RE2 — no catastrophic backtracking, which matters when
the input is attacker-influenced SQL), compiled and cached once per session,
validated at definition-save time so a bad pattern is a 400 rather than a
runtime surprise. Matching happens on the same normalized SQL text the existing
validators use.

**Who may approve**: any user with the `admin` role, plus any member of a group
listed in `ApproverGroupUIDs`. `UserGroup` / `UserGroupMember`
(`internal/store/models.go:517`) already model this; reuse the membership lookup
that `GrantDefinition.AppliesToGroups` is built on. The check runs on the
approve/deny endpoint *and* gates who may subscribe to `approvals/pending`.

### 3. The hold

New shared helper in `internal/proxy/shared/` — call it `ApprovalGate` — invoked
from each protocol's intercept path, *after* the existing static validation
(cheap deterministic denies stay cheap) and *before* the statement is forwarded:

- `internal/proxy/postgresql/intercept.go:39` (simple query) and `:218`
  (`handleExecute` — **not** `handleParse`: at Parse time the bind parameters
  are not yet known, and the SQL that ultimately runs is the portal's)
- `internal/proxy/oracle/intercept.go:68` and `:122`
- `internal/proxy/mysql/intercept.go:155`
- `internal/proxy/mongodb/intercept.go:82`

On a pattern match the gate:

1. persists the query row **immediately** with `approval_status = 'pending'`
   (it must be visible in `/queries` and addressable by UID while it hangs, not
   only after it resolves — the current async-persist-on-completion path can't
   provide that);
2. publishes to `connection/<uid>/queries` and `approvals/pending`;
3. blocks the session goroutine on a channel select over `{approved, denied,
   client-gone, grant-revoked/expired, server-shutdown}`;
4. on approve → forward upstream and continue normally; on deny → return a
   protocol-native error carrying the approver's reason, using the same paths as
   `ErrDDLBlocked` and friends.

**Decision: there is no approval timeout.** A held query waits indefinitely; the
only clock is the client's own. The hold ends when it is approved, denied, or the
**client connection disconnects** — at which point the row is marked `abandoned`
and nothing is ever forwarded upstream. It still fails closed: no path through
the gate forwards a statement without an explicit approval.

That makes **client-disconnect detection load-bearing**, not a nice-to-have — it
is now the sole liveness bound on a parked query, an idle upstream connection,
and any locks it holds. Two things are required:

- **An active read-watch while parked.** The session goroutine is not reading the
  client socket during a hold, so a FIN sits unnoticed. Spawn a reader goroutine
  that reads into a buffered pipe: EOF/error cancels the hold, and any bytes a
  pipelining client sent meanwhile are queued and replayed once the session
  resumes (they must not be dropped, and must not be interpreted mid-hold).
- **TCP keepalive on client connections** (e.g. 30 s idle / 10 s interval / 3
  probes). Without it, a hard-killed client or a dead network path never produces
  a FIN and "until disconnect" means *forever*.

Note for operators, not a code change: with no dbbat-side timeout, the practical
backstop for an idle upstream transaction is the target server's own setting —
PostgreSQL `idle_in_transaction_session_timeout`, MySQL `wait_timeout`. Worth
calling out in `docs/approvals.md`. A bulk "deny all pending" admin action is a
cheap safety valve and should ship with the feature.

Things the implementation has to get right, none of which are optional:

- **Approve by query UID, and verify it.** The approver approves what they were
  shown; the waiting session must assert the resolved UID is *its* pending UID.
  Otherwise a client that can influence timing has a TOCTOU substitution.
- **The approver must not be the requester.** Four-eyes means four eyes; reject
  self-approval even when the requester is themselves an admin or approver-group
  member.
- **Record and broadcast the approver.** `approved_by` / `approved_at` (or
  `denied_by` / `denied_at` + reason) are persisted on the query row, written to
  `audit_logs`, returned by `GET /api/v1/queries`, and **published on the stream
  as a resolution event** carrying the approver's uid and display name — so every
  watcher sees who unblocked it, not just that it unblocked.
- **Cancellation while held.** PostgreSQL `CancelRequest` arrives on a
  *separate* TCP connection; MySQL has `KILL QUERY`; Mongo has
  `killOperations`. A held query must be cancellable by those paths too.
- **Quotas, expiry and revocation keep running.** `shared.LimitGuard` must still
  trip while a query is parked — a grant expiring mid-hold denies. With no
  approval timeout this is the one remaining server-side bound, so it must not be
  bypassed by the parked state.
- **Multi-instance.** dbbat runs multiple replicas in k8s. The session holding
  the query is on replica A; the admin's WebSocket is on replica B. The in-process
  broker is not enough: use **PostgreSQL `LISTEN`/`NOTIFY`** on the existing store
  connection to fan events and decisions between replicas (channel per topic
  prefix, payload = event UID, re-read from the table). No new infrastructure.
- **Shutdown.** Draining must deny (or explicitly abandon) held queries rather
  than hanging the shutdown or silently letting them through.

### 4. Slack escalation when nobody is watching

Reuse `internal/notify/slack.go` (`SlackNotifier`, `buildBlocks`,
`actionsBlock`) and the existing interaction paths
(`internal/api/slack_interactions.go`, `slack_socketmode.go`) — the grant-request
Approve/Deny flow is exactly this shape already.

**Decision: the trigger is purely time-based — a query still pending 30 s after
the hold started fires a Slack notification.** No live-subscriber detection, no
cluster-wide presence bookkeeping: if an admin was watching, they had 30 s to
act, and the hold resolving cancels the pending notification. This is
substantially simpler than presence-tracking and behaves identically in the case
that matters (nobody watching).

Implementation: a single timer per hold (`DBB_APPROVAL_SLACK_DELAY`, default
`30s`, `0` disables). On fire, post with the user, database, truncated SQL, a
deep-link to `${DBB_PUBLIC_URL}/connections/<uid>?watch=1`, and Approve/Deny
buttons when a signing secret is configured. Because there is no approval
timeout, the message stays actionable indefinitely — so it **must** be updated in
place the moment the hold resolves by any route (UI approval, deny, client
disconnect, grant expiry), showing the outcome and the approver. A stale
"Approve" button that silently no-ops is worse than no button, and here it can
linger for hours.

The timer must survive the fan-out: with multiple replicas, exactly one instance
should post. The replica owning the held session owns the timer — the session is
pinned to one instance anyway, so no election is needed.

**Redaction**: SQL text in Slack must be truncated and configurable-off. Slack is
a lower trust boundary than the dbbat UI, and this feature pipes production query
text into it.

### 5. Frontend

- `front/src/routes/_authenticated/connections/` — a live "watch" panel on the
  connection detail page, streaming queries as they arrive, pending ones pinned
  with Approve/Deny, an elapsed-time-held counter (not a countdown — nothing
  expires), and the resolver's name once resolved.
- A global pending-approvals indicator in the layout, fed by
  `approvals/pending`.
- One shared `useEventStream` hook in `front/src/hooks/` handling reconnect with
  backoff, resubscribe, and `lagged` gaps. On reconnect, refetch from REST — the
  stream is best-effort for history, authoritative only for "what is pending now".

### 6. Schema / API

- Migration in `internal/migrations/sql/`: on `queries` —
  `approval_status` (`null|pending|approved|denied|abandoned`; no `timeout`
  value, there is no timeout), `approval_pattern` (which pattern matched),
  `resolved_by uuid`, `resolved_at timestamptz`, `resolution_reason text`. On
  `grant_definitions` and `access_grants` — `approval_patterns text[]`,
  `approver_group_uids uuid[]`. Partial index on `approval_status = 'pending'`.
- `POST /api/v1/queries/{uid}/approve`, `POST /api/v1/queries/{uid}/deny`
  (`{reason}`), `GET /api/v1/queries/pending`, `POST
  /api/v1/queries/pending/deny-all`. All decisions write `audit_logs` and
  publish a resolution event.
- Stream payloads (`data`) — every query event carries `query_uid`,
  `connection_uid`, `sql_text`, `executed_at`, `approval_required` (bool) and
  `approval_status`; resolution events additionally carry `resolved_by`
  (`{uid, username, display_name}`), `resolved_at` and `resolution_reason`.
- New env vars, to add to the `CLAUDE.md` table: `DBB_APPROVAL_SLACK_DELAY`
  (default `30s`), `DBB_APPROVAL_SLACK_SQL` (include SQL text in Slack, default
  on).
- Update `internal/api/openapi.yml` and add `docs/approvals.md` covering the
  per-protocol hold semantics and the operator note on upstream idle timeouts.

## Honest assessment

The feature is worth building — four-eyes-on-statement is the thing dbbat's
position in the network makes uniquely possible, and nothing else in the product
can express it. Three caveats, in order of how much they should change the plan:

1. **WebSocket is probably the wrong transport for the requested shape.** The
   stream is ~99% server→client; the only client→server messages are
   subscribe/unsubscribe and the decisions. **SSE (`text/event-stream`) + plain
   `POST /approve`** gives automatic browser reconnection, no upgrade handshake,
   no ping/pong keepalive to hand-roll, no proxy/LB upgrade config, and works
   with the existing gin middleware stack unchanged — with subscriptions as a
   query param (`?topic=…`). WebSocket buys bidirectionality this feature does
   not really need, and costs a connection lifecycle to get right. I'd start with
   SSE and keep the topic envelope identical so swapping later is a transport
   change only. **This spec is written for WebSocket as requested; flag the
   decision before implementing.**

2. **"Notify every single query" does not survive contact with production.** A
   busy proxy issues thousands of statements per second, each carrying full SQL
   text. Firehosing that is a throughput problem *and* a data-exposure problem.
   The per-connection subscription model already in the request is the right
   answer — the spec above just makes it explicit that there is no global
   all-queries topic, only `connection/<uid>/queries` (opt-in, authorized) and
   the low-volume `approvals/pending`.

3. **Blocking a live database connection on a human is the risky part**, not the
   streaming — and with **no approval timeout** (the decision above), the client's
   own socket timeout is what ends most holds in practice. Expect the common
   failure mode to be: the client gives up first, dbbat is still parked, and the
   held statement is abandoned rather than denied. That is *correct* and safe, but
   it means the UI and Slack message must show `abandoned` distinctly from
   `denied` or the workflow looks broken to whoever finally clicks Approve on a
   query nobody is waiting for anymore. Disconnect detection (active read-watch +
   TCP keepalive) is therefore a hard requirement, not polish. Ship behind an
   off-by-default flag and validate on PostgreSQL first; Oracle and Mongo have the
   least forgiving client-side timeout behaviour and should come last.

One scope note: approval gates the *statement*, not the *data*. An approved
`SELECT` can still exfiltrate the whole table. This complements quotas and
`read_only`; it does not replace them.

## Decisions taken (previously open)

- **Patterns on the grant definition only.** Server-level patterns are a possible
  follow-up, not part of this work.
- **Approvers = any admin, plus members of the definition's
  `approver_group_uids`.** Self-approval is always rejected.
- **Slack fires on a 30 s timer**, not on subscriber-presence detection. This
  removes the cluster-wide presence bookkeeping entirely.
- **No approval timeout.** The hold ends on approve, deny, or client disconnect.
- **The approver is persisted and broadcast** on the stream with the resolution
  event.

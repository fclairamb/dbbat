---
model: opus
effort: high
---

# The connection detail page splits queries across three surfaces; holds and the live stream hide behind an opt-in toggle

## Problem

The connection detail page ([front/src/routes/_authenticated/connections/$uid.tsx](../../front/src/routes/_authenticated/connections/$uid.tsx))
currently shows the same underlying entity — this connection's queries — in
three disconnected surfaces:

1. **A "Live watch" panel** (`ConnectionWatchPanel`, mounted at
   [$uid.tsx:383](../../front/src/routes/_authenticated/connections/$uid.tsx#L383))
   that is **off by default**. It only streams after the user clicks
   "Watch live" or arrives via the `?watch=1` deep link
   ([$uid.tsx:54-57](../../front/src/routes/_authenticated/connections/$uid.tsx#L54)).
   Opening an *active* connection shows a dead panel with a call-to-action
   instead of what the connection is doing right now.
2. **Pending approval holds**, fetched via `usePendingApprovals({ enabled:
   watching })`
   ([ConnectionWatchPanel.tsx:180-183](../../front/src/components/shared/ConnectionWatchPanel.tsx#L180)).
   Because the fetch is gated on `watching`, a statement that was **already
   held before the page was opened is invisible** unless the visitor knows to
   click "Watch live" first. An admin walking the connections list to find out
   why a colleague is stuck sees nothing actionable. (The Slack deep link
   carries `?watch=1`, so that path works — every other path doesn't.)
3. **A separate "Queries" card** below the panel
   ([$uid.tsx:385-411](../../front/src/routes/_authenticated/connections/$uid.tsx#L385)),
   a REST-backed `DataTable` of the last 50 queries. A query that just
   streamed through the live feed shows up *again* here after a refetch, with
   different styling, different columns, and no indication the two rows are
   the same statement.

The result: two renderings of the same query, an approval workflow that can be
missed entirely, and a live feed (`watch-feed`,
[ConnectionWatchPanel.tsx:337-368](../../front/src/components/shared/ConnectionWatchPanel.tsx#L337))
that duplicates rather than complements the history table.

## Proposal

Collapse the three surfaces into **one query table** that is live by default
on active connections.

1. **Auto-start the stream for active connections.** When
   `connection.disconnected_at` is null, subscribe to
   `connection/<uid>/queries` on mount — no toggle required. Keep a
   "Pause" affordance (the existing toggle, inverted default) for users who
   don't want the page moving. Disconnected connections render the plain REST
   table with no stream. `?watch=1` stays accepted for backward compatibility
   (Slack notifications link to it) but becomes a no-op on active
   connections.

2. **Always fetch pending holds.** Enable `usePendingApprovals` (filtered to
   this connection, as the panel already does at
   [ConnectionWatchPanel.tsx:230-232](../../front/src/components/shared/ConnectionWatchPanel.tsx#L230))
   whenever the connection is active, independent of the stream. A hold that
   predates the page load is visible and actionable immediately.

3. **One intermixed table.** Merge the live feed into the existing `Queries`
   `DataTable`:
   - Seed from REST (`useQueries`, limit 50) and fold stream events in by
     `query_uid` — the merge groundwork already exists in `mergeFeedItem` /
     `keep`
     ([ConnectionWatchPanel.tsx:109-141](../../front/src/components/shared/ConnectionWatchPanel.tsx#L109)),
     which folds multiple events (held → resolved → completed) onto one row.
   - Sort by `executed_at` descending; refetches dedupe against streamed rows
     by uid, never duplicate them.
   - Live/held rows use the same columns as historical ones: Duration and
     Rows show `-` until completion; the Status column grows the
     `ApprovalStatusBadge` states (Awaiting approval / Approved / Denied /
     Abandoned) alongside the existing OK/Error badges.
   - A **held row is visually loud** — amber treatment like today's hold card
     ([ConnectionWatchPanel.tsx:286-333](../../front/src/components/shared/ConnectionWatchPanel.tsx#L286))
     — and carries the Approve / Deny buttons plus the `HeldFor` counter and
     matched pattern inline. Intermixed does not mean uniform: the hold must
     remain impossible to miss.
   - On stream gaps (`lagged` / reconnect), refetch **both** the query list
     and the pending set — the REST answer is authoritative, as the current
     panel already treats it
     ([ConnectionWatchPanel.tsx:203-213](../../front/src/components/shared/ConnectionWatchPanel.tsx#L203)).
   - Row navigation to `/queries/<uid>` keeps working for any row that has a
     `query_uid`.

### Simplifying observation (verify before relying on it)

While a statement is held, the client connection is blocked mid-flight, so no
*newer* query can arrive on that connection — a held row should be the newest
row and sits at the top of a `executed_at`-descending table for free, no
pinning logic needed. Verify this holds for every proxied protocol (MongoDB
multiplexing / pipelining is the one to check) before dropping the pinned
section; if some protocol can interleave, pin pending holds above the table
instead.

### Open questions

- Whether the hold's Approve / Deny actions live inline in the row or in an
  expanded row body — whichever reads better at table density, but they must
  not require a navigation.
- `ConnectionWatchPanel` likely dissolves into the page (or becomes the
  unified table component); the `watch-*` / `pending-approval-*` test ids are
  used by the E2E suite and the showcase project (`front/showcase/`), so
  either keep them or update both consumers in the same change.

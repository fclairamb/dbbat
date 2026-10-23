---
model: opus
effort: high
---

# Heavy transfers under a grant go unnoticed: add a Slack soft limit on bytes

GitHub Issue: https://github.com/fclairamb/dbbat/issues/415

## Source

Filed by stonalbot, 2026-10-05, from a Stonal incident. A user ran a chunked
`COPY ... TO STDOUT` export through the proxy on one **auto-approved** grant:
about 11.5 GiB in ~3 hours, over hundreds of short connections. With result
capture on, the dbbat store grew from ~8 GiB to ~20 GiB and nobody noticed for
days. A hard `max_bytes_transferred` is the wrong tool (it blocks legitimate
dumps, and with auto-approval the user just requests another grant).

The ask is a **soft limit**: let traffic through, and post a reply in the grant
request's Slack thread each time the grant's cumulative bytes cross another
multiple of a step, e.g. "Grant `<uid>` for @user on `<db>` has transferred 3 GiB
so far (last 1 GiB in 12 min). The traffic is not blocked." Suggested: a global
`DBB_SLACK_NOTIFY_BYTES_STEP` (`0` = off), a per-definition `notify_bytes_step`
override, accounting across all connections of the grant, one message per step
de-duplicated across restarts and replicas, a link to the grant in the UI, and a
fallback to `DBB_SLACK_NOTIFY_CHANNEL` for grants with no request thread. A
store-size alert is explicitly out of scope.

The report's reading of the code is right.

## Problem

- **The thread exists, but only on the request.** `grant_requests.slack_channel` /
  `slack_message_ts` (`internal/store/models.go:1355-1356`) are written by the
  notifier after the first post (`internal/notify/slack.go:233`,
  `internal/store/grant_requests.go:391`). The grant links back through
  `grant_requests.resulting_grant_id`. A grant assigned directly with
  `POST /api/v1/grants` has no request and no thread.
- **`PostThreadReply` is already the right primitive**
  (`internal/notify/slack.go:266`): best-effort, a no-op without coordinates.
- **Cumulative bytes are only ever computed, never stored on the grant.**
  `AccessGrant.BytesTransferred` is `bun:"-"` (`internal/store/models.go:885`),
  filled by the aggregate in `internal/store/grants.go:527-542`: a `SUM` of
  `connections.bytes_transferred` over the grant's stamped connections (plus the
  unstamped fallback), bounded by `starts_at` and the revocation/expiry upper
  bound. That aggregate is exactly the accounting the issue asks for (across all
  connections, not per connection), so the soft limit must reuse it rather than
  count on its own.
- **Bytes reach the store per query, not per packet.**
  `IncrementConnectionStats` / `IncrementConnectionBytes`
  (`internal/store/connections.go:841`, `:861`) run at query completion. A single
  multi-GiB `COPY` therefore counts only when it finishes. The chunked export in
  the incident is many queries, so it is caught; one giant statement is caught
  late. Acceptable for a soft signal; say so in the docs.
- **The proxies hold the grant in memory** (`s.grant.BytesTransferred += ...` in
  each `intercept.go`/`result.go`) and the `LimitGuard`
  (`internal/proxy/shared/limits.go:229`) only knows one session. Neither sees
  the grant's total across sessions and replicas, so the trigger cannot live in
  the proxy hot path.
- **Nothing persists "last step notified"**, so dedupe across restarts and
  replicas needs a column.
- Closest precedent for a periodic, replica-safe store sweep: the heartbeat loop
  in `heartbeat.go:209` (beat / reclaim / termination poll, each under
  `h.guarded`). Closest precedent for a Slack message about a session going wrong:
  `internal/notify/termination.go` and `DBB_SLACK_NOTIFY_TERMINATIONS`.

## Why a spec

Several rows of the classification table fire: it needs a **migration** (a column
on `access_grants` for the last notified step, and one on `grant_definitions` for
the override), a **new config variable** and a **definition API/OpenAPI field**,
it spans the store, the notifier, the heartbeat loop, the API and the front, and
the de-duplication across replicas hinges on **concurrency**.

## Proposal

1. **Config** (`internal/config/config.go`, `SlackNotifyConfig`): `BytesStep int64`
   (`koanf:"bytes_step"`, env `DBB_SLACK_NOTIFY_BYTES_STEP`), default `0` =
   disabled. Accept a plain byte count; also accept a `1GiB`-style suffix only if a
   parser is already in the dependency tree (no new dependency). Negative = startup
   failure. Only meaningful with a bot token. Add the row to CLAUDE.md's
   environment table and to `website/docs/configuration/`.
2. **Migration** (`internal/migrations/sql/`, up + down):
   - `grant_definitions.notify_bytes_step BIGINT NULL`. Same three states as
     `statement_timeout_seconds`: `NULL` inherits the instance default, `0`
     disables for this definition (a dump definition opts out), positive is the
     step. Definitions are immutably versioned, so it rides the existing
     archive-and-insert path like every other field.
   - `access_grants.bytes_notified_steps INTEGER NOT NULL DEFAULT 0`: how many
     steps have already been announced for this grant.
3. **API** (`internal/api/grant_definitions.go`): `notify_bytes_step` on create and
   update, with a `clear_notify_bytes_step` flag like
   `clear_max_bytes_transferred` (`grant_definitions.go:221`); validate `>= 0`.
   OpenAPI schema + description. Front: one field on the definition form next to
   `max_bytes_transferred`, with a `data-testid`.
4. **Sweep** (new `Store` method plus a step on the heartbeat loop):
   - On a ticker of its own on the same goroutine (every 60s; a constant, not a
     setting), under `h.guarded` with its own goroutine name.
   - Select **active** grants (not revoked, not expired) whose connections saw
     activity since the last sweep (`connections.last_activity_at`), so an idle
     fleet costs one cheap query. For each, resolve the effective step
     (definition override, else config), skip when `0`, compute the cumulative
     bytes with the **same** aggregate `populateGrantCounters` uses (factor it,
     do not copy the SQL), and derive `steps = bytes / step`.
   - Claim with a conditional update:
     `UPDATE access_grants SET bytes_notified_steps = ? WHERE uid = ? AND bytes_notified_steps < ?`.
     Only the replica whose update affects a row posts. That is the whole
     cross-replica dedupe; a restart rereads the column.
   - Several steps crossed in one sweep produce **one** message naming the
     current total, not one per step.
   - Throughput "last N GiB in M min": keep it simple. Store
     `bytes_notified_at TIMESTAMPTZ` next to the counter and report the bytes and
     the minutes since the previous notification (or since `starts_at` for the
     first).
5. **Notifier** (`internal/notify/`): `NotifyGrantBytes(ctx, grant, user, db,
   total, delta, since)`. Look up the request with `resulting_grant_id = grant.uid`;
   if it has a thread, `PostThreadReply`; otherwise post to the configured channel.
   Text: user (Slack mention when the user's Slack id is known, the same way the
   request message resolves it), database name, total in human units (GiB with one
   decimal), delta and duration since the previous notice, a link to
   `DBB_PUBLIC_URL` + the grant's page, and "The traffic is not blocked." Also
   write a `grant.bytes_threshold` audit entry so the signal survives without
   Slack.
6. **Tests**:
   - Store: the sweep claims a step once; two concurrent claims on the same step
     yield exactly one winner; crossing three steps in one sweep announces once
     and stores 3; a revoked or expired grant is skipped; `0` on the definition
     overrides a positive instance default.
   - Notifier: thread reply when the request has coordinates, channel post
     otherwise (fake Slack client, as `slack_test.go` does).
   - API: definition create/update/clear of `notify_bytes_step`, `-1` → 400.
7. **Docs**: a short section in `website/docs/features/access-control.md` next to
   quotas: what the soft limit does, that bytes count at statement completion, and
   that it never blocks.

Out of scope, per the issue: a store-size or captured-bytes alert.

## Open questions

- **Mention approvers or admins at a higher step?** The issue lists it as
  optional. Recommended: not in this spec. The thread already notifies whoever
  approved or watched the request; a mention policy (which step, which group) is
  a second design. File it as a follow-up if the plain reply proves too quiet.
- **Default value of `DBB_SLACK_NOTIFY_BYTES_STEP`?** Recommended: `0` (off), so an
  upgrade posts nothing new until an operator opts in, like every other new
  behaviour in this repo. Trade-off: the incident would not have been caught
  without configuration.

## Closing the issue

This spec closes #415. The implementing PR body **must** carry a `Closes #415`
line so the squash-merge closes it. When the spec is archived to `specs/done/`,
verify the issue is closed and, if the merge did not close it, close it by hand:

    gh issue close 415 --comment "Implemented by <PR or commit>; spec: <archived spec path>"

## Resolved open questions

- No approver or admin mention at a higher step in this spec; file a follow-up if the plain reply proves too quiet. (autopilot: recommended answer)
- `DBB_SLACK_NOTIFY_BYTES_STEP` defaults to `0` (off). (autopilot: recommended answer)

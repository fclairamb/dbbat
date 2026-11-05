---
model: sonnet
effort: medium
---

# Slack notify: post an auto-approved renewal as a reply in its first request's thread

GitHub Issue: https://github.com/fclairamb/dbbat/issues/466

## Source

Issue #466 (filed by stonalbot, 2026-11-04). Automated clients (AI agents, scheduled scans)
re-request the same short auto-approved grant (e.g. 1h read-only) many times a day, often
10-15 in a row. Each request posts a new top-level message in the notification channel, so
the requests that need a human (manual approval, read-write) get lost. Asks: when an
**auto-approved** request renews an earlier one (same user, same database, same definition,
earlier grant still active or expired within a configurable window,
`DBB_SLACK_NOTIFY_RENEWAL_WINDOW=24h`, `0` = off), post a compact thread reply under the
first request of the chain instead. Requests that need a decision, and the first
auto-approved request of a chain, stay top-level. Related: #464 (per-definition channel,
shipped in #465).

The report is right about the noise. One assumption is wrong: it says the thread root's
`slack_channel` / `slack_message_ts` "are already stored". For an auto-approved request they
never are (see Problem).

## Problem

- `internal/api/grant_requests.go:365`: an auto-approved request never fires a `Created`
  event. `autoApproveGrantRequest` (`grant_requests.go:720`) only fires `Approved`.
- `internal/notify/slack.go:295-306`: the `Approved` event goes through the update branch.
  The request has no coordinates, so it takes the fallback `PostMessageContext` and
  **discards** the returned `channel, ts`. `SetGrantRequestSlackMessage` is only called on the
  `Created` branch (`slack.go:284`). So no auto-approved request is a usable thread root today,
  and the byte-threshold replies (`internal/store/grant_bytes_notify.go:244`) and the
  self-revocation reply (`internal/api/grants.go:311`) fall back for those grants as well.
  The bytes one posts top-level, the revocation one posts nothing.
- No code looks up an earlier request of the same (user, database, definition lineage).
- The issue's idea that "the renewal stores the same coordinates" conflicts with the update
  branch: any later event on the renewal (none today for auto-approved, but the fallback path
  shows the shape) would `chat.update` the **root's** message with the renewal's blocks. The
  bytes and revocation replies read the same columns. So the renewal must record that its
  coordinates are a thread, not its own message.

## Why a spec

- New configuration (`DBB_SLACK_NOTIFY_RENEWAL_WINDOW`), plus probably a column to tell a
  thread reply from a top-level post (schema change, migration).
- Product decisions the reporter left open: does a renewal chain survive a definition
  version change or a channel change, and do bursts of concurrent requests need to be
  deduped.
- Touches the API handler, the notifier, the store and the config: more than 4 files.

## Proposal

1. **Persist the fallback post** (a fix on its own, worth doing first): in the `default`
   branch of `NotifyGrantRequest`, when the request has no coordinates, call
   `SetGrantRequestSlackMessage` with the `channel, ts` the fallback post returns. Then every
   auto-approved request becomes a thread root, and the bytes and revocation replies work for
   it too. Unit test with the existing fake Slack client: the `Approved`-without-coordinates
   event persists the coordinates.
2. **Config**: `SlackNotifyConfig.RenewalWindow` (`koanf:"renewal_window"`,
   `DBB_SLACK_NOTIFY_RENEWAL_WINDOW`), a Go duration, default `24h`, `0` = off. Malformed or
   negative fails at startup (same as `DBB_SLACK_NOTIFY_BYTES_STEP`). Add it to the env table
   in `CLAUDE.md` and to `website/docs/configuration/`.
3. **Store**: `Store.FindRenewalRoot(ctx, userID, databaseID, lineageUID, since)` returns the
   most recent **root** grant request (status `approved`, `slack_message_ts` set, not itself a
   reply) of the same user + database + definition lineage whose resulting grant is still
   active, or expired/revoked at or after `since`. Migration: add
   `grant_requests.slack_thread_root_uid uuid NULL REFERENCES grant_requests(uid) ON DELETE
   SET NULL`. A non-NULL value means "this request was posted as a reply in that thread", and
   its own `slack_channel` / `slack_message_ts` hold the **root's** coordinates, so the bytes
   and revocation replies land in the thread unchanged. The notifier never `chat.update`s a
   row whose `slack_thread_root_uid` is set: it posts a reply instead.
4. **Notifier**: in `NotifyGrantRequest`, for an auto-approved event (`isAutoApproved`,
   `slack.go:500`) with a window > 0, look up the root. When there is one in the same channel
   `channelFor` now resolves to, post a one-line `PostThreadReply`, e.g.
   `⚡ renewed by @user for 1h — "<justification>" · <link to request>`, without buttons, and
   record root + coordinates on the renewal. Otherwise post top-level as today. A lookup
   error falls back to top-level (fail visible, never silent).
5. **Tests**: store test for `FindRenewalRoot` (window edges, other user/database/lineage,
   revoked grant, a reply row is never returned as a root). Notifier test: first request
   top-level, second one threaded with root coordinates stored, window `0` always top-level,
   a pending (manual) request always top-level even with a root present.

## Open questions

1. **Definition versions**: does a renewal match on the definition *lineage* or on the exact
   version? Recommended: lineage. An admin edit (archive + successor) would otherwise restart
   every chain. Trade-off: a renewal under a widened definition (read-only becomes read-write
   but stays auto-approved) hides in the thread. To avoid that, break the chain when the
   controls differ (`read_only`, `block_ddl`, `block_copy`) between root and renewal.
2. **Concurrent bursts**: two requests within milliseconds can both find no root and both
   post top-level. Recommended: accept it (at worst one extra top-level message per burst)
   rather than adding a lock. The window makes the third request onwards thread correctly.
3. **Chain length**: should a chain roll over to a new top-level message after N replies or
   when the root is older than the window, even if renewals kept it alive? Recommended: roll
   over once the root itself is older than the window, so a daily scan gets one top-level
   message per day and never a months-old thread.

## Closing the issue

This spec closes #466. The implementing PR body **must** carry a `Closes #466` line so the
squash-merge closes it. When the spec is archived to `specs/done/`, verify the issue is
closed and, if the merge did not close it, close it by hand:

    gh issue close 466 --comment "Implemented by <PR or commit>; spec: <archived spec path>"

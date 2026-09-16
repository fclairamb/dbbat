---
model: sonnet
effort: medium
---

# When dbbat ends a session on its own, nobody is told

## Problem

The statement-timeout and terminate-connection specs give dbbat two new
reasons to tear a session down. Both are events an operator wants to hear
about: a timeout kill means a query was heavy enough to be a problem (the
2026-09-15 replica incident is the reference case, and Florent's take in the
thread was "un timeout bonus en plus sur dbbat avec de l'alerting"), and an
admin termination is something the rest of the team should see happen.

dbbat already posts to Slack for grant requests and approval-hold escalations
(`internal/notify/slack.go:190`, `internal/notify/approval.go:118`), with a
bot token, a channel and deep links back to the UI. Terminations get a WARN
log line.

## Proposal

### What fires

A Slack message on `DBB_SLACK_NOTIFY_CHANNEL` for every `connection.terminated`
with reason:

| Reason | Posted | Why |
|---|---|---|
| `statement_timeout` | yes | the operational signal this exists for |
| `admin_terminated` | yes | names the admin; a human action worth a trace |
| `quota_exceeded` | yes | a grant hit `max_bytes_transferred`; same shape as a timeout |
| `grant_revoked`, `grant_expired`, `instance_lost` | no | revocation already went through a human, expiry is routine, instance loss is an infra alert elsewhere |

### Message

Same block style as the grant-request notification (`buildBlocks`,
`internal/notify/slack.go:273`), no buttons:

> :octagonal_sign: **dbbat ended a session on `prod-datalake-ro`**
> User @florent (Slack mention when the user has a linked Slack id, via
> `slackMention`, `slack.go:410`) · grant "Diagnostic PARIS_HABITAT"
> Reason: statement exceeded the **30s** limit (ran 32.1s)
> `SELECT count(*) FROM data d JOIN properties p ON ...` (head, 200 chars)
> Open connection → `<DBB_PUBLIC_URL>/connections/<uid>`

The statement head is included only when `DBB_SLACK_NOTIFY_SQL` is true
(new, default `true`, mirroring `DBB_APPROVAL_SLACK_SQL`). The admin case reads
"Terminated by @admin: <their reason>".

### Wiring

- `notify.SlackNotifier.NotifyTermination(ctx, TerminationEvent)`, with
  `TerminationEvent{Connection, User, Database, Grant, Reason, QueryHead,
  Limit, Ran, TerminatedBy}`.
- The producer is the shared helper that records a termination (introduced by
  the statement-timeout spec, called from every protocol's teardown and from
  the terminate poller). Give it an optional `TerminationNotifier` interface
  in the same style as `shared.ApprovalEscalator`
  (`internal/proxy/shared/approval.go:78`), nil when Slack is not configured.
  Fire-and-forget in a `safe.RunGuarded` goroutine; a Slack outage must never
  delay a teardown.
- Config: `DBB_SLACK_NOTIFY_TERMINATIONS` (bool, default `true`; only
  meaningful when `DBB_SLACK_NOTIFY_BOT_TOKEN` is set) and
  `DBB_SLACK_NOTIFY_SQL`. Both in `config.SlackNotifyConfig`, the env table in
  `CLAUDE.md` and `website/docs/configuration/index.md`.

### Rate

A runaway client reconnecting in a loop and timing out every time would flood
the channel. Coalesce per `(user, database, reason)`: after the first message,
further terminations within 10 minutes are counted and posted as one
follow-up ("+7 more in the last 10 min") when the window closes. Keep the
counter in memory; it is per replica and that is acceptable.

### Tests

- Block builder unit tests (with and without SQL, admin vs timeout wording).
- The notifier is not called for excluded reasons.
- Coalescing: 8 events in a window produce 2 posts.
- The producer tolerates a nil notifier and a notifier that blocks.

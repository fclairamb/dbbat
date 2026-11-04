---
model: sonnet
effort: medium
---

# Route Slack notifications to a channel chosen per grant definition

GitHub Issue: https://github.com/fclairamb/dbbat/issues/464

## Source

Florent (#464): every grant request is posted to the one instance-wide channel
(`DBB_SLACK_NOTIFY_CHANNEL`). Once automated clients (AI agents, scheduled scans) request
auto-approved, low-risk grants all day, those posts bury the production requests a human
actually has to approve. He asks for several named channels in the config and a nullable
per-definition selection, plus a way to post nothing for very noisy definitions. Use case:
`prod-rw-1h` (manual) to `#dbbat-prod`, `nonprod-rw-1h` and `all-ro-1h` (auto-approved) to
`#dbbat-nonprod`.

The report is right: there is no routing at all today.

## Problem

- `config.SlackNotifyConfig` (`internal/config/config.go:661`) has a single `Channel string`.
- `NewSlackNotifier` copies it into `SlackNotifier.channel` (`internal/notify/slack.go:169`), and
  every post uses it: the grant request creation (`slack.go:225`), its fallback re-post when no
  prior message exists (`slack.go:249`), the bytes-step notice for a grant with no request
  (`internal/notify/grant_bytes.go:34`), terminations (`internal/notify/termination.go:86`, `:124`)
  and approval-hold escalations (`internal/notify/approval.go:124`).
- `GrantRequestEvent` already carries `Definition *store.GrantDefinition` (`slack.go:75`), so the
  creation path has what it needs to pick a channel without a new lookup.
- Follow-ups on a posted request already reuse the stored coordinates
  (`grant_requests.slack_channel`, `internal/store/grant_requests.go:413`; read back in
  `slack.go:242`, `internal/api/grants.go:310`, `internal/api/slack_interactions.go:457`,
  `internal/store/grant_bytes_notify.go:243`). Those keep working unchanged.
- The per-definition three-state precedent to copy is `notify_bytes_step`: migration
  `internal/migrations/sql/20261023000000_grant_bytes_soft_limit.up.sql`, model
  `internal/store/models.go:1096`, API `internal/api/grant_definitions.go:92/178/179/324`
  (with a `clear_notify_bytes_step` flag on update), OpenAPI `internal/api/openapi.yml:5560/5759/5931`,
  front `front/src/routes/_authenticated/grant-definitions/index.tsx`.

## Why a spec

- A DB migration (new column on `grant_definitions`).
- An API and OpenAPI change on grant definitions, plus a new config key.
- A product decision the reporter left open (how to say "post nothing", and which other
  notification kinds follow the definition's channel).

## Proposal

1. **Config.** Add `Channels map[string]string` (`koanf:"channels"`) to `SlackNotifyConfig`. Env
   form `DBB_SLACK_NOTIFY_CHANNELS=prod=#dbbat-prod,nonprod=#dbbat-nonprod` (parse it the way
   `DBB_OIDC_ROLE_MAPPING` is parsed: comma-separated `key=value`; the `slack_notify_*` prefix
   rule at `config.go:1400` maps the env name). Validate at startup: keys match
   `^[a-z0-9][a-z0-9_-]*$`, values non-empty, the key `none` is reserved. Malformed = startup
   failure. `Channel` stays the default and keeps its current meaning.
2. **Migration.** `grant_definitions.notify_channel text NULL`. `NULL` inherits the default
   channel; `'none'` posts nothing; any other value is a key of `Channels`. Being a column on the
   definition, it is part of the immutable versioning like every other field (an edit archives
   the row and the successor carries the new value).
3. **Model + API.** `NotifyChannel *string` on `store.GrantDefinition`; `notify_channel` on
   create/update/response, `clear_notify_channel` on update, mirroring `notify_bytes_step`.
   Validate on create/update that the value is `none` or a configured key: `400` naming the
   known keys otherwise. Expose the configured keys (not the channel ids) somewhere the UI can
   read them, e.g. a `notify_channels: ["prod","nonprod"]` field on the existing instance/settings
   payload the front already loads, so the definition form offers a select instead of a free text.
4. **Startup check against the store.** A stored definition naming a key no longer in the config
   (operator removed it) must not fail startup (that would brick every replica): log a WARN per
   live definition at startup, and at post time fall back to the default channel with a WARN.
5. **Notifier.** `SlackNotifier` keeps `channel` (default) and gains `channels map[string]string`.
   One helper `channelFor(def *store.GrantDefinition) (channel string, post bool)`. Use it in
   `NotifyGrantRequest` for `GrantActionCreated` and the no-prior-post fallback. With `none`,
   nothing is posted and `slack_channel` stays NULL, so later lifecycle updates (`default` branch)
   must also honour `none` instead of falling back to a fresh post in the default channel.
6. **Other notification kinds.** Bytes-step notices for a grant with no request and terminations
   carry a grant whose definition is known: route them through the same helper. Approval-hold
   escalations: same, via the hold's grant definition. See open question 2.
7. **Tests.** Config parsing (env and file, malformed cases); API validation (unknown key 400,
   `none` accepted, clear flag); notifier routing with a fake poster (default, named key, `none`
   suppresses creation and updates, unknown key falls back); a migration up/down round trip as the
   existing migration tests do.
8. **Docs.** `CLAUDE.md` env table (`DBB_SLACK_NOTIFY_CHANNELS`, and the `DBB_SLACK_NOTIFY_CHANNEL`
   row says it is the default), `website/docs/configuration/` Slack page, OpenAPI.

## Open questions

1. **How to say "post nothing"?** Recommended: the reserved key `none` (readable in the UI select
   and in audit diffs). Alternative: empty string `""` meaning none, which is easy to confuse with
   "unset" in JSON clients and forms.
2. **Do terminations, bytes-step notices and approval holds follow the definition's channel, or
   only grant requests?** Recommended: all of them follow it, so a `nonprod` definition never
   lands in the approvers' channel. Trade-off: with `none`, a statement-timeout termination or an
   approval hold on that definition would post nothing; holds need a human, so for holds `none`
   should fall back to the default channel rather than silently park a statement nobody sees.

## Closing the issue

This spec closes #464. The implementing PR body **must** carry a `Closes #464` line so the
squash-merge closes it. When the spec is archived to `specs/done/`, verify the issue is closed
and, if the merge did not close it, close it by hand:

    gh issue close 464 --comment "Implemented by <PR or commit>; spec: <archived spec path>"

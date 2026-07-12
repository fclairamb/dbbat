# Slack interactive grant approval (Approve / Deny buttons)

> Issue: TBD — per [specs/README.md](../README.md), create the GitHub issue and
> link it here before implementation.
>
> Follow-up to `specs/done/2026/05/04-slack-grant-request-notifications.md`,
> which explicitly deferred buttons to "Spec 05". All the plumbing that spec
> promised is now in place: bot token config, Block Kit rendering,
> `(slack_channel, slack_message_ts)` persistence, Slack-user → dbbat-user
> identity mapping, and atomic pending→decided store transitions.

## Goal

When a grant request is created, the Slack notification (already posted by
`internal/notify/slack.go`) additionally:

1. Mentions the requester and the admins:
   _"🔐 @requester requested access on **prod-pg** with **Read-only 1h**.
   @admin1, @admin2 — please approve or deny."_
2. Carries two buttons: **✅ Approve** and **❌ Deny**.

When an admin clicks a button, dbbat decides the request exactly as if the
admin had used the web UI (same store transition, same audit event, same
grant materialization), then:

- **updates the original message in place** — buttons removed, status badge
  and "Approved/Denied by" line shown (existing `chat.update` behavior), and
- **posts a reply in the message thread**: _"✅ This request has been
  *approved* by @admin1."_ / _"❌ This request has been *denied* by @admin1."_

Only dbbat admins may decide. Anyone else who clicks gets an **ephemeral**
error (visible only to them); the channel stays clean.

### Terminology

dbbat statuses are `approved` / `denied` / `cancelled` (DB CHECK constraint,
UI, audit events). The Slack copy uses the same words — not accept/refuse —
so Slack, UI, and audit log never disagree.

### Why update-in-place AND a thread reply

Message edits don't notify anyone; a thread reply gives requester and
watchers an actual notification. The in-place update is still required to
remove the buttons (otherwise stale buttons invite clicks on decided
requests). For consistency, `cancelled` also gets a thread reply
(_"🚫 This request has been *cancelled* by the requester."_).

### Transport decision: HTTPS interactivity webhook (not Socket Mode)

Slack button clicks reach dbbat via an inbound `POST` to a public HTTPS
endpoint, authenticated by Slack's signing secret. Socket Mode (outbound
WebSocket, no ingress needed) is explicitly out of scope for v1: it adds a
persistent-connection component (reconnect/backoff lifecycle) for one
feature, while the webhook is a stateless gin handler like everything else.

**Deployment caveat to document**: deep links only require *users' browsers*
to reach `DBB_PUBLIC_URL`; buttons require *Slack's servers* to reach it.
Intranet-only deployments keep today's link-through-UI flow by simply not
setting the signing secret — the feature degrades gracefully.

## Configuration

> **Editor's note (2026-07-09):** as first shipped, the config layer only read
> `DBB_SLACK_NOTIFY_SIGNING_SECRET`, not the `DBB_SLACK_SIGNING_SECRET`
> documented here. This has since been fixed: `DBB_SLACK_SIGNING_SECRET` is
> the canonical name and the `_NOTIFY_` form remains accepted as a legacy
> alias (see `specs/todos/2026-07-08-slack-signing-secret-env-var-name-mismatch.md`).

| Var                        | Description                                              | Required |
|----------------------------|----------------------------------------------------------|----------|
| `DBB_SLACK_SIGNING_SECRET` | Slack app signing secret. Empty → no buttons, no inbound endpoint (today's behavior). | no |

Validation at startup (same pattern as `ErrChannelMissing`):
`DBB_SLACK_SIGNING_SECRET` set while `DBB_SLACK_NOTIFY_BOT_TOKEN` is unset →
fail fast (buttons live on notification messages; interactivity without
notifications is meaningless).

Add the variable to the env table in `CLAUDE.md` and to the README/docs
Slack setup section.

## Files added / modified

**Add**
- `internal/api/slack_interactions.go` — signature verification + interaction
  handler (lives in `api`: it needs store, notifier, logger, config).
- `internal/api/slack_interactions_test.go`

**Modify**
- `internal/config/config.go` — add `SigningSecret` to `SlackNotifyConfig`.
- `internal/notify/slack.go` — mention line, actions block, thread replies;
  update the package doc comment (it currently says "no interactive buttons").
- `internal/notify/slack_test.go`
- `internal/api/server.go` — register `POST /api/v1/slack/interactions`
  **outside** the `authenticated` group (auth = Slack signature, like the
  OAuth callback); pass the signing secret through.
- `internal/api/grant_requests.go` — extract the decide flow (store call +
  audit event + notify) into a helper shared by the HTTP handlers and the
  Slack handler, so the two paths can't drift.
- `internal/store/user_identities.go` (or `users.go`) — add
  `ListAdminSlackUserIDs(ctx) ([]string, error)`: one query joining users
  with `'admin' = ANY(roles)` to `user_identities WHERE provider = 'slack'`.
- `internal/api/openapi.yml` — document the endpoint (form-encoded `payload`,
  `200`/`401`).
- `slack_app_manifest.json` — enable interactivity (below).
- `CLAUDE.md`, `README.md`/docs — env var + setup instructions.

No migration needed: `slack_channel` / `slack_message_ts` already exist on
`grant_requests`, and `user_identities.provider_id` already holds the Slack
user ID (OIDC `sub`) for provider `slack`.

## Message rendering (`internal/notify/slack.go`)

Extend `GrantRequestEvent` with denormalized fields (keeping the notifier
store-free, per its existing design comment):

```go
RequesterSlackID string   // "" if the requester has no linked Slack identity
AdminSlackIDs    []string // admins with linked Slack identities
Interactive      bool     // signing secret configured → render buttons
```

`loadEventContext` in `internal/api/grant_requests.go` fills them
(requester identities via `GetUserIdentities`, admins via
`ListAdminSlackUserIDs`). Same for the decider on decision events, so the
thread reply can mention them (`<@U…>`, falling back to username).

Pending + `Interactive` message shape:

```
Header:   🔐 Grant request — florent
Section:  <@U0REQ> requested access on *prod-pg* with *Read-only 1h*.
          <@U0ADM1>, <@U0ADM2> — please approve or deny.
Section:  *Database* / *Definition* / *Duration* / *Status* / *Justification*  (existing)
Actions:  [✅ Approve (style: primary)]  [❌ Deny (style: danger)]
Context:  Review in dbbat →                                                    (existing)
```

- Mentions use `<@SLACK_ID>` only for users with a linked identity; others
  render as plain usernames (existing `userLabel`). No linked admins → drop
  the second sentence of the mention line.
- Both buttons carry `value` = request UID; `action_id` =
  `grant_request_approve` / `grant_request_deny`; both get a Slack native
  `confirm` dialog to prevent fat-finger decisions.
- The actions block is rendered **only** for `GrantActionCreated` +
  `Interactive` — every `chat.update` (from UI decisions, cancels, or button
  decisions) rebuilds blocks without it, which is how stale buttons disappear.

New notifier method for thread replies, called after decision events when
`(channel, ts)` are known:

```go
func (n *SlackNotifier) PostThreadReply(ctx context.Context, channel, ts, text string)
```

(`PostMessageContext` + `slack.MsgOptionTS(ts)`; best-effort, log-and-swallow
like everything else in the notifier.)

## Inbound endpoint (`internal/api/slack_interactions.go`)

`POST /api/v1/slack/interactions` — Slack sends
`application/x-www-form-urlencoded` with a `payload` field containing the
interaction JSON.

**Security**
1. Cap the body (1 MB) and read it raw.
2. Verify `X-Slack-Signature` / `X-Slack-Request-Timestamp` with
   `slack.NewSecretsVerifier` (HMAC-SHA256 over `v0:{ts}:{body}`, ±5 min
   replay window). Failure → `401`, no body detail.
3. Parse into `slack.InteractionCallback`; only `type = block_actions` with
   our two `action_id`s is processed — anything else gets `200` and is
   ignored (don't give Slack a reason to show the user an error).
4. Trust only IDs from the payload (`user.id`, action `value`), never names.
   Parse `value` as a UUID; garbage → log warn, `200`.

**3-second ack**: Slack requires a response within 3 s. The handler returns
`200` (empty body) immediately after signature verification + parsing, and
processes the decision in a goroutine with its own timeout context — same
pattern as `notifyAsync`. All user-visible feedback flows through
`chat.update`, the thread reply, or ephemeral `response_url` posts
(`slack.PostWebhookContext` with `response_type: "ephemeral"`,
`replace_original: false`).

**Processing flow**
1. `store.GetUserByIdentity(ctx, "slack", payload.User.ID)`
   - not found → ephemeral: _"Your Slack account isn't linked to a dbbat
     user. Sign in to dbbat with Slack first: `<DBB_PUBLIC_URL>`"_
2. `user.IsAdmin()` false → ephemeral: _"Only dbbat admins can decide grant
   requests."_
3. Call the shared decide helper (approve or deny, decider = mapped user,
   deny reason = empty in v1). Audit details gain `"via": "slack"`.
   - `ErrInvalidTransition` (already decided/cancelled/expired — including
     two admins racing; the store's `SELECT … FOR UPDATE` makes the loser
     deterministic) → ephemeral: _"This request is no longer pending."_ and
     re-render the message to current state so stale buttons vanish.
   - `ErrGrantRequestNotFound` / `ErrDefinitionInactive` → ephemeral with
     the matching message.
4. On success the shared helper already fires the notifier (chat.update);
   additionally post the thread reply.

## Slack app manifest

```json
"settings": {
  "interactivity": {
    "is_enabled": true,
    "request_url": "https://<YOUR_DBBAT_HOST>/api/v1/slack/interactions"
  },
  ...
}
```

Document in the README: after editing the manifest, copy the app's *Signing
Secret* (Basic Information page) into `DBB_SLACK_SIGNING_SECRET`.

## Tests

### Unit
- Signature verification: valid → 200; bad signature → 401; stale timestamp
  (> 5 min) → 401; oversized body rejected.
- Handler with stubbed store/notifier (Slack API + `response_url` served by
  `httptest`):
  - admin clicks Approve → store approve called with mapped decider UID,
    audit `via: slack`, message updated, thread reply posted.
  - admin clicks Deny → same for deny.
  - non-admin → ephemeral error, store untouched.
  - unlinked Slack user → ephemeral error, store untouched.
  - already-decided request → ephemeral "no longer pending" + message
    re-rendered.
  - unknown `action_id` / non-UUID `value` → 200, no side effects.
- Notifier: blocks contain mention line + actions block only when
  `Interactive` and action is `created`; decided renders drop the buttons;
  `PostThreadReply` hits `chat.postMessage` with `thread_ts`.

### Manual
- Real workspace: set bot token, channel, public URL, signing secret; expose
  dbbat (e.g. a tunnel) and set the manifest request URL.
- Request access as `connector`; verify mentions and buttons.
- Click Deny as a non-admin Slack user → ephemeral error only.
- Click Approve as a linked admin → confirm dialog → message updates, thread
  reply appears, grant exists in dbbat, audit shows `via: slack`.
- Decide another request from the web UI → Slack message loses its buttons.
- Two browsers / two admins clicking near-simultaneously → one wins, the
  other gets the ephemeral "no longer pending".

## Verification checklist

- [ ] `make lint` clean, `make test` green
- [ ] Signing secret unset → no route registered, messages have no buttons,
      behavior identical to today
- [ ] Signing secret set without bot token → startup error
- [ ] Unsigned/forged POST to the endpoint → 401, nothing happens
- [ ] Admin click approves/denies with correct `decided_by`, audit event
      `via: slack`, message updated + thread reply
- [ ] Non-admin and unlinked users get ephemeral errors, channel unpolluted
- [ ] Already-decided clicks are safely rejected (race included)
- [ ] `CLAUDE.md` env table + README setup updated, manifest committed

## Out of scope (future specs)

- **Socket Mode** for deployments that can't accept inbound Slack traffic.
- **Deny-reason modal** (`views.open` with the interaction `trigger_id`) —
  v1 denies with an empty reason; the UI remains the place for nuance.
- Mentioning a Slack **usergroup** (e.g. `@dbbat-admins`) instead of
  individual admins, via an optional env var.
- DM to the requester on decision (thread reply covers notification).
- Approving from Slack **slash commands** or App Home.

## Implementation Plan

Backend-Go only. Steps land as granular commits.

1. **Config** (`internal/config/config.go`)
   - Add `SigningSecret string` to `SlackNotifyConfig` (koanf `signing_secret`;
     `slack_notify_signing_secret` already maps via the `slack_notify_` prefix).
   - Add `Interactive()` helper returning `SigningSecret != "" && BotToken != ""`.

2. **Startup validation** (`internal/notify/slack.go` `NewSlackNotifier`)
   - New sentinel `ErrSigningSecretWithoutBotToken`: signing secret set while
     bot token unset → fail fast, mirroring `ErrChannelMissing`. Store the
     `interactive` flag + signing secret on the notifier.
   - Expose `SigningSecret()` and `Interactive()` accessors for the API layer.

3. **Notifier rendering** (`internal/notify/slack.go`)
   - Update package doc comment (drop "no interactive buttons").
   - Extend `GrantRequestEvent` with `RequesterSlackID string`,
     `AdminSlackIDs []string`, `Interactive bool`.
   - `buildBlocks`: add a mention line (`<@ID>` for linked users, plain
     username otherwise; second sentence only when admins have linked IDs) and,
     for `GrantActionCreated && Interactive` only, an actions block with
     ✅ Approve (primary) / ❌ Deny (danger) buttons carrying
     `value = request UID`, `action_id = grant_request_approve|deny`, each with
     a native `confirm` dialog. Decider mention line on decided renders.
   - Add `PostThreadReply(ctx, channel, ts, text)` using
     `PostMessageContext` + `MsgOptionTS`; best-effort log-and-swallow.
   - Add mention helper `slackMention(slackID, username)`.

4. **Store** (`internal/store/user_identities.go`)
   - `ListAdminSlackUserIDs(ctx) ([]string, error)` — one query joining users
     with `'admin' = ANY(roles)` to `user_identities WHERE provider = 'slack'`,
     honoring soft-deletes. Store test in `user_identities_test.go`.

5. **Shared decide helper** (`internal/api/grant_requests.go`)
   - Extract approve/deny into a `decideGrantRequest` helper returning a
     structured outcome (updated request, resulting grant, notify action,
     mapped error) plus a `decisionSource` ("web"|"slack") threaded into audit
     `details.via`. HTTP approve/deny handlers call it; Slack handler reuses it.
   - `loadEventContext` fills `RequesterSlackID`, `AdminSlackIDs`, `Interactive`,
     and the decider Slack ID for thread-reply mentions.

6. **Interaction endpoint** (`internal/api/slack_interactions.go`)
   - `POST /api/v1/slack/interactions`: cap body at 1 MB, verify with
     `slack.NewSecretsVerifier` (401 on failure/expiry/oversize, no detail),
     parse `slack.InteractionCallback`, accept only `block_actions` with our two
     `action_id`s (else 200 no-op), parse `value` as UUID (garbage → 200 warn).
   - Ack 200 immediately; process decision in a goroutine with its own timeout
     (mirrors `notifyAsync`). Feedback via `chat.update` (through the shared
     helper's notify), thread reply, and ephemeral `response_url`
     (`PostWebhookContext`, `response_type: ephemeral`, `replace_original:false`).
   - Flow: map Slack user → dbbat user (unlinked → ephemeral); non-admin →
     ephemeral; call shared helper; on `ErrInvalidTransition` ephemeral "no
     longer pending" + re-render; success → thread reply.
   - Define small interfaces (`interactionStore`, `interactionNotifier`) so the
     handler is unit-testable with stubs; wire concrete `*store.Store` /
     `*notify.SlackNotifier` in `server.go`.

7. **Router wiring** (`internal/api/server.go`)
   - Register the endpoint OUTSIDE the `authenticated` group (auth = Slack
     signature), only when the notifier reports `Interactive()`.

8. **OpenAPI** (`internal/api/openapi.yml`) — document the endpoint
   (form-encoded `payload`, `200`/`401`).

9. **Manifest + docs** — `slack_app_manifest.json` interactivity block;
   `CLAUDE.md` env table; `website/docs/configuration/index.md` Slack section.

10. **Tests** — `slack_interactions_test.go` (signature valid/bad/stale/oversize;
    admin approve/deny happy paths incl. `via: slack`, chat.update, thread
    reply; non-admin & unlinked ephemeral + store untouched; already-decided
    ephemeral + re-render; unknown action_id / non-UUID no-op); notifier test
    additions (mention line + actions only when Interactive+created; decided
    drops buttons; `PostThreadReply` posts with `thread_ts`).

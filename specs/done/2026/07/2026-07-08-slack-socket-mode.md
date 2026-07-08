# Slack Socket Mode for interactive grant approval

> Follow-up to `2026-07-07-slack-interactive-grant-approval.md`, which delivered
> the Approve/Deny buttons over an inbound HTTPS interactions endpoint and listed
> **Socket Mode** as out of scope for v1. This spec adds it.

## Goal

Let dbbat receive Approve/Deny button clicks over an **outbound** Slack Socket
Mode WebSocket, so the feature works on deployments that cannot accept inbound
Slack traffic (intranet / IP-allowlisted ingress). No inbound reachability and no
signing secret are required — the connection is authenticated by an app-level
token.

## Why

The inbound endpoint (`POST /api/v1/slack/interactions`) requires Slack's servers
to reach `DBB_PUBLIC_URL`. On gated deployments (e.g. Stonal's shared Istio
gateway, whose NLB security group allowlists source IPs) Slack's webhook IPs are
dropped and every button click times out. Socket Mode sidesteps this entirely:
dbbat dials out to Slack and receives interactions over the socket.

## Configuration

| Var | Description | Required |
|-----|-------------|----------|
| `DBB_SLACK_NOTIFY_APP_TOKEN` | Slack **app-level** token (`xapp-...`, scope `connections:write`). When set together with the bot token, dbbat opens a Socket Mode connection and receives Approve/Deny interactions over it. Empty = no Socket Mode. | No |

- `SlackNotifyConfig.SocketMode()` = `AppToken != "" && BotToken != ""`.
- `SlackNotifyConfig.Interactive()` widens to `BotToken != "" && (SigningSecret != "" || AppToken != "")` — buttons render if **either** inbound transport (HTTP signing secret OR Socket Mode app token) is configured.
- Startup validation: `AppToken` set without a bot token → fail fast
  (`ErrAppTokenWithoutBotToken`), mirroring `ErrSigningSecretWithoutBotToken`.
- HTTP endpoint and Socket Mode may both be configured; they're independent
  transports feeding the same decision pipeline. (At the Slack app level, enabling
  Socket Mode makes Slack deliver over the socket and ignore the request URL.)

## Design

The decision pipeline is already transport-agnostic: `slackDecisionFromCallback`
and `processSlackDecision` (in `slack_interactions.go`) operate on a parsed
`slack.InteractionCallback`. Socket Mode just feeds them from a different source.

New file `internal/api/slack_socketmode.go`:
- `startSocketMode()` — no-op unless `config.SlackNotify.SocketMode()`. Builds
  `slack.New(botToken, slack.OptionAppLevelToken(appToken))` + `socketmode.New(...)`,
  launches the event loop and `client.RunContext(ctx)` (reconnects internally),
  stores a cancel func on the Server.
- `runSocketMode(ctx, client)` — ranges `client.Events`, dispatches interactive
  ones via `handleSocketEvent`.
- `handleSocketEvent` — for `EventTypeInteractive`: extract the callback, **Ack
  the envelope immediately** (Slack's 3s deadline), then `dispatchSlackCallback`.
- `dispatchSlackCallback(decider, cb)` — shared with tests: runs
  `slackDecisionFromCallback` + `processSlackDecision` in a goroutine (own
  timeout context, like the HTTP path).
- `socketInteractionCallback(evt)` — pure helper: returns the callback when the
  event is a block-actions interaction.

Server lifecycle: `Start()` calls `startSocketMode()`; `Shutdown()` cancels it.

Notifier: package doc comment updated to mention Socket Mode; `interactive` is
driven by the widened `config.Interactive()`, so no other notifier change.

## Tests

- config: `DBB_SLACK_NOTIFY_APP_TOKEN` populates `SlackNotify.AppToken`;
  `SocketMode()` true only with app+bot token; `Interactive()` true with app
  token alone (no signing secret), with signing secret alone, and false with only
  a bot token.
- notifier: `AppToken` without bot token → `ErrAppTokenWithoutBotToken`.
- `socketInteractionCallback`: interactive block-actions event → ok; non-interactive
  event → not ok; wrong data payload → not ok.
- `dispatchSlackCallback` (reusing the existing `stubDecider` / `ephemeralRecorder`):
  admin Approve → `approve` + thread reply; non-admin → ephemeral, store untouched;
  unknown action_id → no dispatch. (The decision core itself is already covered by
  the HTTP handler tests — same functions.)

## Docs

- `CLAUDE.md` env table: add `DBB_SLACK_NOTIFY_APP_TOKEN`.
- `website/docs/configuration/index.md`: Socket Mode setup (generate an app-level
  token with `connections:write`, enable Socket Mode on the app).
- `slack_app_manifest.json`: note Socket Mode as an alternative to the request URL.

## Out of scope

- Migrating the HTTP endpoint away (it stays for publicly-reachable deployments).
- Slack Events API / slash commands over the socket (only `block_actions`).

## Implementation Plan

1. config: `AppToken` field, `SocketMode()`, widen `Interactive()`.
2. notify: `ErrAppTokenWithoutBotToken` + validation + package doc.
3. api: `slack_socketmode.go` (runner, dispatch, helper) + Server field + Start/Shutdown wiring.
4. tests: config, notifier, socket helper + dispatch.
5. docs: CLAUDE.md, website, manifest.
6. QA: `make fmt build-binary lint test`.

# Slack notifications for grant requests

> Depends on `03-grant-requests.md`. Implement after Spec 03 ships.

## Goal

When a grant request is created, post a message to a configured Slack channel
(typically `#dbbat`) summarising the request with a deep-link to the dbbat UI.
When the request is decided (approved / denied / cancelled), update that
message in place so the channel stays tidy and reflects the current state.

**Outbound only.** No interactive buttons, no inbound webhook, no signature
verification, no public ingress required. The admin sees the ping in Slack,
clicks through to dbbat, and uses the existing UI to decide.

## Why outbound-only (vs interactive buttons)

An earlier draft considered Approve / Deny buttons in Slack. The cost is
material: bot token + signing secret, a public webhook endpoint with HMAC
verification, sub-3-second ack with async processing, stale-button handling
(button shown after the request is already decided), Slack-user-ID →
dbbat-admin mapping for authorization. The user-confirmed call is to ship the
notification path first; if buttons turn out to matter, they become a Spec 05
that builds on the bot token and message-ts plumbing this spec installs.

## Files added / modified

**Add**
- `internal/notify/slack.go`
- `internal/notify/slack_test.go`

**Modify**
- `go.mod`, `go.sum` — add `github.com/slack-go/slack`.
- `internal/config/config.go` — extend the Slack config block.
- `internal/api/grant_requests.go` (from Spec 03) — fire-and-forget calls to
  the notifier on create / approve / deny / cancel.
- `slack_app_manifest.json` — add `chat:write` to bot scopes; keep the
  existing OIDC redirect intact.
- `README.md` (or `docs/`) — one paragraph + env-var table covering the
  notification setup.

No new migrations: Spec 03 already added `slack_channel` and `slack_message_ts`
columns to `grant_requests`.

## Configuration

New environment variables (read via koanf in `internal/config/config.go`):

| Var                          | Description                                         | Required |
|------------------------------|-----------------------------------------------------|----------|
| `DBB_SLACK_NOTIFY_BOT_TOKEN` | Bot user OAuth token (`xoxb-…`). Empty → feature off| no       |
| `DBB_SLACK_NOTIFY_CHANNEL`   | Channel id or `#name` (e.g. `#dbbat`)               | required if token set |
| `DBB_PUBLIC_URL`             | Public dbbat base URL (used for deep-links)         | required if token set |

If `DBB_SLACK_NOTIFY_BOT_TOKEN` is empty, the feature is a no-op. Log once at
startup: `"slack notifications: disabled (DBB_SLACK_NOTIFY_BOT_TOKEN unset)"`.
If set without `DBB_PUBLIC_URL` or `DBB_SLACK_NOTIFY_CHANNEL`, fail fast at
startup with a clear error.

Add the values to the `# Environment Variables` table in
`/Users/florent/code/fclairamb/dbbat/CLAUDE.md`.

## Notifier package

`internal/notify/slack.go`:

```go
package notify

type SlackNotifier struct {
    client    *slack.Client // nil if disabled
    channel   string
    publicURL string
    log       *slog.Logger
}

func NewSlackNotifier(cfg config.SlackNotifyConfig, log *slog.Logger) (*SlackNotifier, error)

type GrantRequestEvent struct {
    Request    *store.GrantRequest
    Definition *store.GrantDefinition
    Database   *store.Database
    User       *store.User    // requester
    Decider    *store.User    // optional; set on approve/deny
    Action     GrantAction    // "created" | "approved" | "denied" | "cancelled"
}

func (n *SlackNotifier) NotifyGrantRequest(ctx context.Context, ev GrantRequestEvent) error
```

Behavior:
- `Action == "created"`: post a new message; on success, persist
  `(channel, ts)` to the request via `Store.SetGrantRequestSlackMessage`
  (added in Spec 03 store). Use `slack.Client.PostMessageContext`.
- `Action != "created"`: read `(channel, ts)` from the request. If both are
  present, `chat.update` the existing message. If either is missing (token
  was added later, or original post failed), post a fresh message.
- All Slack errors are warnings, never bubble up to the API handler. The
  notifier is best-effort.
- A 5-second hard timeout via `context.WithTimeout` so a slow Slack doesn't
  pile up goroutines.

Block Kit message shape (sketch — implement via `slack.MsgOptionBlocks`):

```
Header:    🔐 Grant request — <user.email>
Section:   *Database*: <db.name>   *Definition*: <def.name>
           *Duration*: 1h   *Controls*: read_only, block_copy
           *Quotas*: 1000 queries · 100 MB
Section:   *Justification*: <justification or "—">
Context:   Status: <status badge>   <Review in dbbat → URL>
```

Status emoji per state: ⏳ pending, ✅ approved, ❌ denied, 🚫 cancelled.

The "Review in dbbat" link points to
`<DBB_PUBLIC_URL>/grant-requests/<request.uid>`.

## Wiring in the API handler

In `internal/api/grant_requests.go` (Spec 03), each lifecycle endpoint fires
the notifier in a goroutine after the DB transaction commits:

```go
go func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := s.notifier.NotifyGrantRequest(ctx, ev); err != nil {
        s.log.Warn("slack notify failed", "uid", req.UID, "err", err)
    }
}()
```

The handler injects the notifier via the existing API server constructor
(append a parameter or extend the `Server` struct). Tests can pass a stub.

## Slack app manifest update

In `slack_app_manifest.json`, under the `bot_user`/`oauth_config` section,
add scopes:

```json
"oauth_config": {
  "scopes": {
    "user": ["openid", "email", "email:read", "profile"],
    "bot":  ["chat:write"]
  }
}
```

Document in `README.md` how to install the bot user to a workspace and
extract the resulting `xoxb-…` token. The OIDC scopes stay user-side so login
still works as before.

## Tests

### Unit
- Fake Slack server (`httptest.NewServer`) returning canned responses for
  `chat.postMessage` and `chat.update`. The `slack-go` client lets you set a
  custom HTTP base URL:
  `slack.New(token, slack.OptionAPIURL(testServer.URL+"/"))`.
- Cases:
  - Disabled (no token) → `NotifyGrantRequest` is a no-op, no HTTP call.
  - Created → `chat.postMessage` called with expected blocks; channel/ts
    persisted.
  - Approved with prior message → `chat.update` called with new blocks.
  - Approved without prior message → falls back to `chat.postMessage`.
  - Slack returns 5xx → warning logged, no error returned.

### Manual
- Set `DBB_SLACK_NOTIFY_BOT_TOKEN`, `DBB_SLACK_NOTIFY_CHANNEL=#dbbat-test`,
  `DBB_PUBLIC_URL=https://dbbat.example.com`.
- Run `make dev`, log in as connector, request access via a "Read-only 1h"
  definition.
- Confirm a message appears in `#dbbat-test` with the expected fields and a
  working "Review in dbbat" link.
- As admin, click the link, approve in the UI; confirm the same Slack
  message updates in place to "✅ Approved by …".
- Repeat for deny and cancel.

## Verification checklist

- [ ] `make lint` clean, `make test` green
- [ ] With token unset, app starts and grant requests work normally with no
      Slack traffic
- [ ] With token + channel + public_url set, all four lifecycle events
      produce / update the Slack message
- [ ] Slack 5xx during a notification does not fail the API request
- [ ] Manifest updated; admin can rotate / install the bot user without
      breaking OIDC login
- [ ] CLAUDE.md env-var table includes the new variables

## Out of scope

- **Interactive buttons** — explicit non-goal. If desired later, becomes
  Spec 05 and reuses the bot token + persisted message-ts.
- Slack signature verification (no inbound traffic in this spec).
- Notifications for grant lifecycle events (`grant.created`, `grant.revoked`)
  — those already audit-log; if Slack visibility is wanted there, separate
  ticket.
- Multi-channel routing (e.g. one channel per database).
- DM to the requester on decision — link in the UI is sufficient.

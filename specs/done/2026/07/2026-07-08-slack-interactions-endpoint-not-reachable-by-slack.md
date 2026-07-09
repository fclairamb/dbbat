# Slack can't reach the interactions endpoint on dbbat.tools.stonal.io (3s timeout)

## Goal

Make `POST https://dbbat.tools.stonal.io/api/v1/slack/interactions` reachable by
Slack's servers so the Approve/Deny buttons actually decide requests. Today Slack's
delivery times out and the interaction never reaches dbbat.

## Why

Verified live on 2026-07-08: created a grant request → the DBBat bot posted to
`#dbbat` with working Approve/Deny buttons → clicking **Deny** → native confirm
dialog → confirmed. Slack then showed **⚠️ "Operation timed out. Apps need to
respond within 3 seconds."** dbbat logged **zero** hits on `/api/v1/slack/interactions`
(single pod `dbbat-...`, `--since=10m`), i.e. the request never reached the app.

Meanwhile my own `curl -X POST` to the same URL returns `401` in ~56 ms. So the
endpoint + signature verification work; the problem is the network boundary:

- `dbbat.tools.stonal.io` is served by the **shared Stonal Istio gateway**
  `http-gateway` (ns `istio-gateway`, HTTPRoute `dbbat` in `tooling`), ELB
  `master-gateway-api-istio-...elb.eu-west-3.amazonaws.com` (internet-facing).
- **Confirmed mechanism (read-only AWS inspect):** the gateway is an internet-facing
  **NLB** `master-gateway-api-istio` whose security group
  `k8s-istiogat-httpgate-1713436ae6` allowlists inbound 80/443 to ~21 source CIDRs
  (e.g. GitHub ranges `140.82.112.0/20`, `192.30.252.0/22`, `185.199.108.0/22`,
  `143.55.64.0/20`, plus a handful of `/32`s). A dev's own IP is in the list (so it
  works from a laptop); Slack's webhook source IPs are not → SYN dropped → timeout.
  This is a deliberate "allow specific webhook sources" control, shared by every
  service on the master gateway.
- Per-service Istio `AuthorizationPolicy` DENY allowlists also exist for some
  services (`tooling-metabase-ip-allowlist`, `tooling-service-db-mgmt-ip-allowlist`),
  but dbbat has none — the block for dbbat is purely the NLB SG allowlist above.

Interactive Slack buttons fundamentally require Slack's servers (not just users'
browsers) to reach the endpoint. This is the exact caveat the original spec called
out for intranet/gated deployments.

## Implementation

This is a platform-infra change on a **shared** gateway — do it deliberately, with
review (blast radius = every service behind `http-gateway`). Options:

1. **Expose only the interactions path publicly.** Add an HTTPRoute / gateway
   listener that serves `POST /api/v1/slack/interactions` (and only that) with no
   IP allowlist, relying on dbbat's HMAC signature verification for security
   (that's what it's designed for). Keep the rest of dbbat gated.
2. **Allowlist Slack egress ranges** on the gateway for the dbbat host. Note Slack
   does NOT publish a small stable set of interactivity source IPs — they span wide,
   changing AWS ranges — so IP-allowlisting Slack is brittle and largely
   impractical. Prefer option 1.
3. **Slack Socket Mode** (outbound WebSocket from dbbat; no ingress needed). The
   original spec listed this as out-of-scope for v1, but it's the clean answer for a
   deployment that can't accept inbound public traffic. Would require implementing
   Socket Mode in dbbat.

Until one of these lands, the feature degrades to link-through-UI (buttons post but
clicking them times out). Consider not advertising buttons on gated deployments.

No GitHub issue filed yet — open one and link it here. Related:
`specs/todos/2026-07-08-slack-signing-secret-env-var-name-mismatch.md`.

## Implementation Plan

Options 1 and 2 are changes to **Stonal's shared Istio gateway / internet-facing
NLB security group** — blast radius is every service behind the shared gateway,
and this spec itself says to do that deliberately, with review. They are out of
scope for an automated in-repo run and are intentionally not implemented here.

Option 3 (**Slack Socket Mode**) has since landed in-repo:
`feat(api): add Slack Socket Mode transport for Approve/Deny interactions`
(#229, commit `d1d33d4`, spec archived at
`specs/done/2026/07/2026-07-08-slack-socket-mode.md`). It is the sanctioned
resolution of this spec. Remaining work in this pass:

1. **Verify** Socket Mode closes the gap: with `DBB_SLACK_NOTIFY_BOT_TOKEN` +
   `DBB_SLACK_NOTIFY_APP_TOKEN` and no reachable inbound endpoint, Approve/Deny
   clicks arrive over the outbound WebSocket and run the *same* decision
   pipeline as the HTTP endpoint (`internal/api/slack_socketmode.go` →
   `dispatchSlackCallback` → `processSlackDecision`, shared with
   `internal/api/slack_interactions.go`). Confirm test coverage.
2. **Degraded-mode guidance** (the "consider not advertising buttons on gated
   deployments" note): add a startup log line when interactivity is enabled via
   signing-secret only (no app token), stating that
   `POST /api/v1/slack/interactions` must be reachable from *Slack's servers*
   — not just users' browsers — and that Socket Mode
   (`DBB_SLACK_NOTIFY_APP_TOKEN`) is the alternative for gated deployments.
   Unit-test the predicate + log emission. No config redesign.
3. **Docs**: strengthen the website configuration page with an explicit
   three-deployment-shapes summary (public endpoint / gated + Socket Mode /
   neither = link-through-UI), add `app_token` to the YAML example, and add a
   note on the Kubernetes page (IP-allowlisted gateways → Socket Mode).
4. **Spec resolution section** (below) with the ops runbook for
   dbbat.tools.stonal.io.

Not in this pass: filing the GitHub issue (still to do, see above), and the
`DBB_SLACK_SIGNING_SECRET` vs `DBB_SLACK_NOTIFY_SIGNING_SECRET` env-var name
mismatch, which is tracked separately in
`specs/todos/2026-07-08-slack-signing-secret-env-var-name-mismatch.md`.

## Resolution

**Option 3 — Slack Socket Mode — is the resolution.** Options 1 and 2 (opening
the interactions path on the shared gateway / allowlisting Slack egress on the
NLB security group) were rejected for this run: both mutate Stonal's shared
internet-facing gateway, whose blast radius is every service behind
`http-gateway`, and option 2 is additionally brittle (Slack publishes no small
stable set of interactivity source IPs). Socket Mode needs no infra change at
all.

### Verified in code (2026-07-09)

- With `DBB_SLACK_NOTIFY_BOT_TOKEN` + `DBB_SLACK_NOTIFY_APP_TOKEN` set,
  `Server.startSocketMode()` (`internal/api/slack_socketmode.go`) opens an
  outbound WebSocket authenticated by the app-level token — no signing secret,
  no inbound reachability. `config.SlackNotifyConfig.Interactive()` returns
  true for bot token + app token alone, so the Approve/Deny buttons render.
- Clicks arrive as `socketmode.EventTypeInteractive` envelopes, are acked
  within Slack's 3s deadline, and dispatch through `dispatchSlackCallback` →
  `processSlackDecision` — the *same* decision pipeline (admin check, decide,
  ephemeral errors, chat.update, thread reply, `via: slack` audit) as the
  inbound HTTP endpoint in `internal/api/slack_interactions.go`.
- Covered by `internal/api/slack_socketmode_test.go` (event extraction,
  admin approve/deny dispatch, non-admin ephemeral, unknown-action no-op) on
  top of the shared-pipeline tests in `slack_interactions_test.go`.

### Degraded-mode guidance (implemented in this pass)

When interactivity is configured with the signing secret as its **only**
transport (no app token), dbbat now logs an info line at startup stating that
`POST /api/v1/slack/interactions` must be reachable from Slack's servers (not
just users' browsers) or clicks will time out after 3s, and pointing at
`DBB_SLACK_NOTIFY_APP_TOKEN` as the alternative
(`Server.logSlackInteractivityTransport`, unit-tested). The website
configuration page now leads with a three-deployment-shapes table (public
endpoint / gated → Socket Mode / neither → link-through-UI) and the Kubernetes
page carries a gated-gateway note.

### Ops runbook — dbbat.tools.stonal.io

The fix for this deployment is **configuration only — no gateway change**:

1. In the Slack app `A0B2P5ZNYQ5` (the notify/interactivity app, bot
   `@dbbat2`): **Settings → Socket Mode → enable**, then **Basic Information →
   App-Level Tokens → generate** a token with the `connections:write` scope.
2. Set `DBB_SLACK_NOTIFY_APP_TOKEN=xapp-...` on the dbbat deployment (tooling
   namespace) alongside the existing bot token, and roll the pod.
3. Verify: startup log shows `slack socket mode enabled`; create a grant
   request and click Approve/Deny — the decision must land (message updates,
   thread reply) with no *"Operation timed out"*.

Once Socket Mode is enabled at the Slack-app level, Slack delivers over the
socket and ignores the request URL, so the (unreachable-from-Slack) inbound
endpoint and its signing secret can stay configured or be removed — either
works.

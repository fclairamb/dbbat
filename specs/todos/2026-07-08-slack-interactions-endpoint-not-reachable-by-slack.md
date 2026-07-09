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

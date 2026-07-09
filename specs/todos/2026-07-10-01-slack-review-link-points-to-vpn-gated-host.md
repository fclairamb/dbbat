# Slack "Review in dbbat →" link points to the VPN-gated DB host instead of the public web UI

## Problem

The grant-request Slack notification ends with a `Review in dbbat →` deep-link
built at [`internal/notify/slack.go:277`](internal/notify/slack.go:277):

```go
link := fmt.Sprintf("%s/app/grant-requests", publicURL)
```

where `publicURL` is `cfg.PublicURL` (env `DBB_PUBLIC_URL`, defined at
[`internal/config/config.go:275`](internal/config/config.go:278)).

In production this link resolves to `https://db.stonal.io/app/grant-requests`,
but `db.stonal.io` is the **VPN-gated API/DB host**. The publicly reachable
DBBat web UI — the one a Slack reviewer can actually open — lives at
`https://dbbat.tools.stonal.io/`. So the reviewer clicks the link and lands on
a host they can't reach without VPN.

The root cause is that `DBB_PUBLIC_URL` is conflated with the DB/API address.
Today `PublicURL` is used in exactly two Slack-facing places, both of which
want the **public web UI** URL, not the DB address:

- [`internal/notify/slack.go:277`](internal/notify/slack.go:277) — the deep-link.
- [`internal/api/slack_interactions.go:305`](internal/api/slack_interactions.go:305)
  — the user-facing fallback hint in `publicURLForMessage()`.

(The Slack SSO OAuth `redirect_uri` is stored per-provider in the DB —
`store.OAuthProvider.RedirectURL`, [`internal/store/models.go:465`](internal/store/models.go:465)
— so it is *not* affected by this value.)

The deployment currently sets `DBB_PUBLIC_URL` to the DB address, so we need a
clean way to keep "where clients connect to the database proxy" and "where
humans open the web UI" as two distinct settings.

## Proposal

Keep `DBB_PUBLIC_URL` semantically as **the public web UI base URL** (used for
Slack deep-links and user-facing copy) and make sure it is deployed as
`https://dbbat.tools.stonal.io`, distinct from the DB proxy address
(`db.stonal.io`).

Concretely:

1. **Doc/semantics** — tighten the `PublicURL` doc comment at
   [`internal/config/config.go:275`](internal/config/config.go:275) and the
   `DBB_PUBLIC_URL` row in `CLAUDE.md` to state explicitly that it is the
   **externally reachable web UI** base URL (the one Slack users click), *not*
   the database connection host.

2. **Deployment** — set `DBB_PUBLIC_URL=https://dbbat.tools.stonal.io` on the
   unified dbbat-proxy deployment (the DB proxy address `db.stonal.io` stays as
   the DB connection host, unrelated to this var).

3. **Guard (optional)** — since the deep-link is only useful when reachable,
   consider warning at startup if `PublicURL` host equals a known internal/DB
   host, or simply document the distinction well enough that ops don't reuse the
   DB address.

## Open questions

- Is a **separate env var** wanted (e.g. keep `DBB_PUBLIC_URL` for the DB
  address and add `DBB_WEB_URL` for the UI), or is renaming/clarifying the
  existing single var + fixing the deployment sufficient? The code only ever
  uses `PublicURL` for web links today, so the latter is the smaller change —
  but the user's phrasing ("differentiate the public URL and the DB address")
  may imply they want both as first-class, independently settable config.
- Should the fallback string in `publicURLForMessage()`
  ([`internal/api/slack_interactions.go:309`](internal/api/slack_interactions.go:309))
  also be revisited so it never suggests a non-clickable host?

## Implementation Plan

### Design decision (open question resolved)

Chose the **minimal single-var clarification** over introducing a second env
var (`DBB_WEB_URL`). Rationale:

- `PublicURL` (`DBB_PUBLIC_URL`) is consumed in exactly two non-test places,
  confirmed by grep across `internal/` (worktrees ignored):
  - `internal/notify/slack.go:277` — the `Review in dbbat →` deep-link.
  - `internal/api/slack_interactions.go:305-306` — the user-facing fallback
    hint in `publicURLForMessage()`.
  Both want the public **web UI** base URL, never the DB proxy host. There is
  no consumer that assumes DB-host semantics, so a second var would add config
  surface with zero functional gain.
- The var is already named/keyed `public_url` / `DBB_PUBLIC_URL`; renaming it
  would break existing deployments. The real defect is **documentation
  ambiguity** ("base URL for this dbbat instance" reads as the DB address),
  which is exactly what a deployment operator mis-set. Fixing the docs so the
  var unambiguously means the reachable web UI is the smaller, correct change.

### Steps

1. Tighten the `PublicURL` doc comment in `internal/config/config.go` (~line
   275) to state it is the externally reachable **web UI** base URL that Slack
   users click (e.g. `https://dbbat.tools.stonal.io`), explicitly **not** the
   database connection/proxy host (e.g. `db.stonal.io`).
2. Update the `DBB_PUBLIC_URL` row in the root `CLAUDE.md` env-var table with
   the same clarified semantics and a concrete example.
3. QA: `make lint` + `make test` (fallback: `go build/vet/test` on the touched
   packages). No behavioral code change, so existing tests remain valid.

Deployment (step 2 of the Proposal — setting
`DBB_PUBLIC_URL=https://dbbat.tools.stonal.io` on the dbbat-proxy deployment)
is an ops action outside this repo and is not part of this code change.

# On-demand website showcase media (screenshots + videos)

## Goal

A `make showcase` target (plus a manual `workflow_dispatch` GitHub Action) that
regenerates the website's marketing screenshots and videos from a live demo-mode
instance, writing them to `website/static/img/showcase/`.

Strictly on-demand — **no** automatic trigger on release. Instead, every generated
set is stamped with the app version it was captured from, via a manifest file saved
next to the assets, so it's always clear how stale the visuals are.

Scenarios:
- Screenshot: adding a server
- Screenshot: a grant request
- Screenshot: query list
- Screenshot: query results
- Video: live UPDATE statement held for approval, approved from the UI

No GitHub issue yet — one should be filed.

## Why

The website has no real product visuals. The repo is already 90% equipped: the
Playwright e2e suite (`front/e2e/`) has auth fixtures and already takes screenshots,
and demo mode (`DBB_RUN_MODE=demo`, `make demo`) seeds realistic data. Regenerating
at each major release keeps visuals honest without manual capture work.

## Implementation

- **New Playwright project** `front/showcase/` (own config, reuses `front/e2e/fixtures.ts`
  auth helpers). Not part of `make test-e2e` — different determinism needs, never gates CI.
- **Environment**: run against demo mode, not test mode, for realistic data. Docker-compose
  provides the upstream PostgreSQL.
- **Determinism**:
  - Freeze time with Playwright's clock API; keep demo data dates absolute — otherwise
    "2 minutes ago" labels churn on every regeneration.
  - Fixed viewport 1280×800. `deviceScaleFactor: 2` for screenshots, `1` for video.
- **Approval video** (the hard one): `front/e2e/approvals.spec.ts` notes Playwright can't
  create a live hold — but the showcase runner can. Spawn a real client (`pg` npm package
  or `psql` subprocess) through the proxy with `DBB_APPROVAL_ENABLED=true` and a grant
  carrying an approval pattern matching `UPDATE`. Render the client side as a fake
  terminal pane (xterm.js or styled `<pre>`) that the script types into while actually
  executing via the pg client — real terminals aren't reproducible in CI.
  Inject a fake cursor overlay (dot following `mousemove`) — Playwright videos render
  no mouse cursor.
- **Encoding**: Playwright records WebM/VP8. Transcode with ffmpeg:
  - AV1 primary: `ffmpeg -i in.webm -c:v libsvtav1 -preset 6 -crf 42 -an out-av1.mp4`
    (SVT-AV1, not libaom — speed).
  - H.264 fallback: `-c:v libx264 -crf 26 -preset slow -pix_fmt yuv420p` — Safari only
    hardware-decodes AV1 (iPhone 15 Pro+/M3+), so `<video>` needs both `<source>`s.
  - Keep 20–24 fps; screencast size is driven by CRF/duration/resolution, not fps, and
    low fps makes typing look janky. Target <1 MB per clip.
- **Version stamp**: after a successful run, write
  `website/static/img/showcase/manifest.json` alongside the assets:
  `{ "version": "0.20.0", "commit": "<sha>", "generatedAt": "<ISO date>" }`.
  Version source: `.release-please-manifest.json` (key `"."`) — same source the
  Makefile's `VERSION`/ldflags pipeline uses via `internal/version`. The website can
  optionally read this manifest to caption visuals ("captured on v0.20.0").
- **Rot guard**: run the showcase suite headless (artifacts discarded) in CI on `front/`
  changes so drift is caught when the UI changes, not at release time.
- **Key files**: `front/e2e/fixtures.ts`, `front/e2e/global-setup.ts`,
  `front/playwright.config.ts` (model for the new config), `Makefile` (`demo` target),
  `website/static/`, `internal/config/config.go` (`RunModeDemo`).

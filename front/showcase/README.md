# Showcase runner

Regenerates the website's marketing media into `website/static/img/showcase/`:

| Asset | Scenario |
|---|---|
| `add-server.png` | The Add Server dialog, filled |
| `grant-request.png` | The Request access dialog, filled |
| `query-list.png` | The query list, populated by real proxy traffic |
| `query-results.png` | A query's captured result rows |
| `approval-hold-poster.png` | The hold itself — the video's poster, and what the site shows under `prefers-reduced-motion` |
| `approval-hold-av1.mp4`, `approval-hold-h264.mp4` | An `UPDATE` held for approval and released from the UI |
| `manifest.json` | `{ version, commit, generatedAt }` for the run |

Run it with `make showcase` from the repo root. Never with a bare
`bunx playwright test` — the suite needs the demo-mode instance that
`scripts/showcase.sh` brings up.

## Why it is not part of `make test-e2e`

Different job. The e2e suite asserts behaviour and must be fast, parallel and
green on every PR. This one produces *artwork*: single-worker, serial, pinned
clock, fixed 1280×800 viewport, and a live database session parked on a human.
It is on-demand tooling, and it must never become a required check.

The `expect`s in the specs are not the point of the suite — they are there so a
UI change that breaks a scenario fails loudly instead of silently emitting a
screenshot of an empty page. That is what makes the suite usable as the CI rot
guard (`.github/workflows/showcase.yml`, `continue-on-error`).

## Isolation

Demo mode **drops every table on startup**. `scripts/showcase.sh` therefore
starts its own throwaway PostgreSQL container (no volume) and its own dbbat, on
ports that collide with neither the documented defaults (4200/5433/5001) nor
the e2e suite's (8080/5433/5001):

| | Port |
|---|---|
| API / UI | 8099 (`SHOWCASE_API_PORT`) |
| PostgreSQL proxy | 5499 (`SHOWCASE_PROXY_PORT`) |
| Throwaway upstream | 5099 (`SHOWCASE_PG_PORT`) |

Nothing in here may call `docker compose`: that would stop a developer's shared
stack and take its database with it.

## What is real and what is drawn

Real: the server row, the grant and its approval pattern, the upstream table,
every query in the query list, the proxy session, the hold, the approve click,
and the rows the `UPDATE` touched. All of it produced by a `pg` client dialling
through the proxy (`lib/traffic.ts`).

Drawn: the terminal pane (`lib/terminal.ts`) and the mouse pointer
(`lib/cursor.ts`). A real terminal emulator is not reproducible in CI, and
Playwright's recorder captures the page rather than the compositor, so a real
cursor never appears in the frame. Neither fakes a *result*: every line the
pane prints came back from the real connection.

Restated afterwards: *when* it all happened, and how long the statements took
(`lib/normalise.ts`), plus the grant's validity window and the "held for"
counter for the one frame of the poster (`approval-poster.spec.ts`). The
measured durations against a container on the same host come back as `0.0ms` —
true, and not worth a screenshot. Those are the only invented values anywhere
in the captures.

## Determinism

Regenerating the stills against an unchanged UI produces byte-identical PNGs.
That is the bar, and everything below exists to hold it.

- **Demo data.** Seeded at absolute dates: `demoEpoch()` in `main.go` dates
  every demo user, server, grant and history row from the start of the current
  UTC day, so those rows never move under a capture.
- **Timeline.** `lib/normalise.ts`, run by `global-setup.ts` once the traffic
  has been generated. The scenario is still produced for real — real API calls,
  real statements through the proxy — and it is exactly that which used to
  churn: a fresh `executed_at`, a freshly measured `duration_ms` and a fresh
  UUIDv7 connection id every run, the last eight hex digits of which the query
  list prints. So afterwards the run's own session and statements are pinned to
  fixed offsets from `SHOWCASE_EPOCH`, the demo history is shifted wholesale
  onto the same epoch (preserving its "4 hours ago" / "3 days ago" spread), and
  every connection uid is reissued as a UUIDv7 derived from its now-fixed
  connect time. `access_grants` is left alone here: every session after this
  point authenticates through the grant, so it has to stay valid at the real
  clock. The poster borrows it for one frame instead — see *The hold* below.
- **Clock.** `page.clock.setFixedTime()` — pins `Date.now()` without stopping
  timers, so React Query and the watch panel's reconnect backoff still work. It
  is a constant (`SHOWCASE_EPOCH` + 30s), which it can be because every row it
  is read against is one too. Override with `SHOWCASE_FIXED_TIME`.
- **Locale and zone.** `en-US`/`UTC`, pinned in `playwright.config.ts` rather
  than inherited: the query-detail page prints an absolute `Executed …` line,
  so a laptop in CEST and a CI runner in UTC would otherwise disagree.
- **Auto-refresh.** Off for the stills (`fixtures.ts` seeds the toggle's
  localStorage key). The countdown badge ticks on a real one-second interval
  that the clock pin deliberately does not stop, so "Next: 9s" was however long
  the page took to load. The video leaves it on.
- **The hold.** `approval-hold-poster.png` is a still, not a frame lifted out of
  the recording (`approval-poster.spec.ts`, its own project). Two things in it
  move with the wall clock and neither can be restated afterwards, because the
  frame is what renders them: the grant's validity window is dated from
  `SHOWCASE_EPOCH` before the page loads it and put back verbatim the moment
  the shutter closes — the grant has to be valid at the real clock again for
  the video's session — and the clock is pinned to the held statement's own
  `executed_at` plus a fixed offset, so "held 12s" is a constant.
- **Geometry.** 1280×800 everywhere; `deviceScaleFactor: 2` for the four
  stills (so the PNGs are 2560×1600), `1` for the video and for its poster,
  which is displayed at the `<video>` element's own size.
- **Ordering.** One worker, serial, no retries. Projects run in the order
  `playwright.config.ts` declares them — `screenshots`, then `poster`, then
  `video` — and that matters: the last two each open a further live session,
  which would otherwise show up in the query list the first one captures.

One thing still moves, on purpose: the version string in the sidebar
(`git describe`, so every commit changes it). The clips do too, but a recording
of a live session re-encoded by ffmpeg was never going to be byte-stable, and
nobody reviews it as a diff.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `SHOWCASE_OUT` | `website/static/img/showcase` | Where finished assets land |
| `SHOWCASE_WORK` | `front/showcase/.artifacts` | Raw WebM, traces, scenario state |
| `SHOWCASE_PROJECT` | *(all)* | `screenshots`, `poster` or `video` |
| `SHOWCASE_SKIP_BUILD` | `0` | Reuse the existing `./dbbat` |
| `SHOWCASE_SKIP_TRANSCODE` | `0` | Copy the raw WebM instead of encoding |
| `SHOWCASE_KEEP` | `0` | Leave the stack running afterwards |
| `SHOWCASE_EPOCH` | `2026-08-06T09:12:00Z` | Instant every rendered row is dated from |
| `SHOWCASE_FIXED_TIME` | `SHOWCASE_EPOCH` + 30s | Force the pinned clock |
| `SHOWCASE_STORAGE_DSN` | the instance's own `dbbat` DB | Where `lib/normalise.ts` restates the timeline |
| `SHOWCASE_FREEZE_CLOCK` | *(auto)* | Force the pin on or off for both projects |
| `SHOWCASE_FPS` | `22` | Transcode frame rate |

# Showcase runner

Regenerates the website's marketing media into `website/static/img/showcase/`:

| Asset | Scenario |
|---|---|
| `add-server.png` | The Add Server dialog, filled |
| `grant-request.png` | The Request access dialog, filled |
| `query-list.png` | The query list, populated by real proxy traffic |
| `query-results.png` | A query's captured result rows |
| `approval-hold-av1.mp4`, `approval-hold-h264.mp4` | An `UPDATE` held for approval and released from the UI |
| `approval-hold-poster.png` | The hold itself — the video's poster, and what the site shows under `prefers-reduced-motion` |
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

## Determinism

- **Demo data.** Seeded at absolute dates: `demoEpoch()` in `main.go` dates
  every demo user, server, grant and history row from the start of the current
  UTC day, so a demo instance renders the same dates on every start and those
  rows never move under a capture.
- **Clock.** `page.clock.setFixedTime()` — pins `Date.now()` without stopping
  timers, so React Query and the watch panel's reconnect backoff still work.
  The pin is read by `global-setup.ts` once the scenario is ready; it is the
  run's own clock, not a constant, because the server, the grant and every
  query in the query list are created live by this suite — a constant pin in
  the past would render them "in 7 months". Override with
  `SHOWCASE_FIXED_TIME`.
- **Geometry.** 1280×800 everywhere; `deviceScaleFactor: 2` for stills (so the
  PNGs are 2560×1600), `1` for video.
- **Ordering.** One worker, serial, no retries.

What is still not reproducible is what this suite creates itself: the absolute
`Executed …` timestamp and the measured duration on `query-results.png` come
from a query that really ran during the capture. See `specs/todos/` for the
follow-up.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `SHOWCASE_OUT` | `website/static/img/showcase` | Where finished assets land |
| `SHOWCASE_WORK` | `front/showcase/.artifacts` | Raw WebM, traces, scenario state |
| `SHOWCASE_PROJECT` | *(all)* | `screenshots` or `video` |
| `SHOWCASE_SKIP_BUILD` | `0` | Reuse the existing `./dbbat` |
| `SHOWCASE_SKIP_TRANSCODE` | `0` | Copy the raw WebM instead of encoding |
| `SHOWCASE_KEEP` | `0` | Leave the stack running afterwards |
| `SHOWCASE_FIXED_TIME` | *(auto)* | Force the pinned clock |
| `SHOWCASE_FREEZE_CLOCK` | *(auto)* | Force the pin on or off for both projects |
| `SHOWCASE_FPS` | `22` | Transcode frame rate |

# Make the showcase's own rows reproducible

## Goal

Make `make showcase` emit byte-identical `query-list.png` / `query-results.png`
when nothing about the UI changed, by removing the last source of churn: the
rows the suite creates *itself*.

## Why

`specs/todos/2026-08-06-demo-data-absolute-dates.md` fixed demo seeding — every
demo user, server, grant and history row is now dated from `demoEpoch()` and
stable for the day. It did **not** fix the showcase's own scenario, and that is
what the two query screenshots actually render:

- `global-setup.ts` creates the server, the grant definition and the grant at
  the real clock;
- `lib/traffic.ts` runs the statements through the proxy for real, so every row
  in the query list carries a wall-clock `executed_at` **and** a measured
  `duration_ms` that differs on every run.

So `query-results.png` still shows a different `Executed …` and a different
duration each time, and the diff is still not reviewable. It is also why the
browser clock pin cannot become a constant instant (`fixedTime()` in
`front/showcase/config.ts`): a constant in the past would render those live
rows "in 7 months".

## Implementation

Two candidate approaches, pick one:

1. **Normalise after the fact.** After `generateTraffic()`, connect to the
   showcase instance's *storage* database — `scripts/showcase.sh` runs it on
   `postgres://postgres:postgres@localhost:${SHOWCASE_PG_PORT}/dbbat`, the same
   throwaway container as the upstream — and rewrite `connections.connected_at`
   / `last_activity_at` / `disconnected_at`, `queries.executed_at` and
   `queries.duration_ms` to fixed values derived from a showcase epoch. Then
   `SHOWCASE_FIXED_TIME` can default to a constant instant just after that
   epoch, and `configuredFixedTime()`'s fallback disappears.
   Cheap, but it reaches behind a running dbbat.

2. **Let the seeding own it.** Extend demo mode (or a dedicated seed command)
   so the showcase's scenario — `analytics-prod`, its grant, its history — is
   seeded at absolute dates like the rest of demo data, and keep live traffic
   only for the approval video, which is not a still. Cleaner, bigger.

Either way, drop the caveat paragraph at the end of the Determinism section of
`front/showcase/README.md` and the matching comments in
`front/showcase/config.ts` and `scripts/showcase.sh`.

No GitHub issue yet — one should be filed.

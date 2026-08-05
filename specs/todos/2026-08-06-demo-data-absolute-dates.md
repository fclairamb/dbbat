# Seed demo mode at absolute dates

## Goal

Make `provisionDemoData` (main.go) create its users, servers, grants and any
sample history at **fixed absolute timestamps** rather than at `time.Now()`
offsets, so a demo-mode instance renders the same dates on every start.

## Why

Discovered while building the showcase runner
(`specs/todos/2026-08-05-website-showcase-media.md`, `front/showcase/`). The
spec asked for reproducible visuals; the runner pins the browser clock with
Playwright's `page.clock.setFixedTime()`, which stabilises the *relative*
labels ("less than a minute ago"). It cannot stabilise the **absolute**
timestamps — "Executed Aug 6, 2026, 12:57:03 AM" on the query-detail page —
because the underlying rows really were created seconds earlier.

Consequences today:

- Every `make showcase` regeneration produces a byte-different
  `query-results.png` even when nothing about the UI changed, so the diff is
  never reviewable.
- The pinned clock has to be chosen *after* seeding (see `resolveFixedTime()`
  in `front/showcase/config.ts`), because a pin chosen before it renders every
  seeded row in the future. That is a workaround for this, not a design.

A demo instance also just looks better with a plausible spread of history
instead of everything created "less than a minute ago".

## Implementation

- In `provisionDemoData` (main.go, ~line 867), replace `time.Now()` /
  `time.Now().AddDate(...)` with constants derived from a single
  `demoEpoch` — e.g. `2026-01-15T09:00:00Z` — so grants, users and servers
  carry stable `created_at` / `starts_at` / `expires_at`.
  - `expires_at` still has to be in the future relative to the *real* clock or
    the grants are dead on arrival. Either keep expiry relative (documenting
    the exception) or make the epoch a rolling but truncated value
    (`time.Now().Truncate(24 * time.Hour)`), which is stable within a day.
- Consider seeding a small spread of demo query history at epoch-relative
  offsets (hours/days back) so the query list shows a realistic timeline
  rather than five rows from the same second.
- Once absolute, `front/showcase/config.ts` can default `SHOWCASE_FIXED_TIME`
  to a constant instant and drop `resolveFixedTime()`'s "now + 5s" heuristic;
  update `front/showcase/README.md`'s Determinism section accordingly.

No GitHub issue yet — one should be filed.

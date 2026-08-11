# Make the generated route tree byte-stable across builds

## Goal

Find out why `front/src/routeTree.gen.ts` is sometimes re-emitted with a
different ordering by an otherwise identical `bun run build`, and pin the
output so it is a pure function of the route files.

## Why

`.github/workflows/ci.yml` now fails the frontend job when `bun run build`
leaves `front/src/routeTree.gen.ts` modified (added in `2e56774`, and the file
had to be re-synced by hand in `7a6bb60`). That check is only sound if the
generator is deterministic.

It is not, at least not reliably. While working on
`2026-08-10-11-expose-row-chain-verification-over-the-api` — a change that
touches no route file — the first `make build-front` of the session rewrote
`routeTree.gen.ts` with its import block and route declarations in a different
(roughly reversed) order, 154 lines each way. Four consecutive rebuilds
immediately afterwards produced the committed content again and left the tree
clean, so the flip was a one-off rather than an alternation.

That reads like the TanStack Router plugin's incremental cache: a cold or
invalidated cache walks the route directory in one order, a warm one replays a
remembered order. Whatever the cause, it means a contributor whose cache is
cold gets a spurious 154-line diff on an unrelated PR, and CI can fail on a
change that has nothing to do with routing.

No GitHub issue yet — file one when picking this up.

## Implementation

- Reproduce first: remove the plugin's cache (`front/node_modules/.cache`,
  `front/.tanstack`, and whatever `@tanstack/router-plugin` writes under
  `node_modules/.vite`) and run `bun run build` twice, comparing
  `front/src/routeTree.gen.ts` after each. A cold-vs-warm difference confirms
  the hypothesis; if the file is stable, look for a filesystem-readdir-order
  dependency instead (macOS vs the Linux CI runner).
- The plugin is configured in `front/vite.config.ts`. Check the installed
  `@tanstack/router-plugin` version for a sorting option, and for an upstream
  issue — an unsorted `readdir` is a known shape of bug in route generators.
- If the plugin cannot be made to sort, the fallback is to stop trusting it for
  ordering: have CI compare a normalized form, or regenerate from a clean cache
  in the check step so both sides are cold. Prefer fixing the generator —
  normalizing in CI leaves every contributor with the noisy diff.
- Whatever lands, add a note next to the CI check in `.github/workflows/ci.yml`
  saying what makes the comparison sound, since that check is the only thing
  keeping the file honest.

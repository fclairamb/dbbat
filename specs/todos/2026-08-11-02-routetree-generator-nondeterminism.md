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

Two candidate causes, in order of likelihood:

1. **A `make dev` Vite server was running throughout.** It watches the route
   files and re-emits `routeTree.gen.ts` itself, and its ordering may differ
   from the one a cold `bun run build` produces — the two generators then take
   turns rewriting the file. Later in the same session `git status` twice
   reported the file modified while `git diff` was empty, which is the dev
   server rewriting it with byte-identical content and only bumping its mtime.
   That half is harmless; a *different* ordering from the same watcher is not.
2. **The plugin's incremental cache**: a cold or invalidated cache walks the
   route directory in one order, a warm one replays a remembered order.

Either way a contributor gets a spurious 154-line diff on an unrelated PR, and
CI can fail on a change that has nothing to do with routing.

No GitHub issue yet — file one when picking this up.

## Implementation

- Reproduce the dev-server case first, since it is the cheaper of the two: with
  `make dev` running, run `bun run build` in `front/` and see whether the file
  changes; then stop the dev server and repeat. If the orderings differ, the
  two generators disagree and the fix belongs in whatever config they do not
  share (`front/vite.config.ts` vs the dev-mode plugin options).
- Otherwise reproduce the cache case: remove the plugin's cache (`front/node_modules/.cache`,
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

## Findings

Candidate cause #1 was right, but the mechanism is sharper than "the two
generators may disagree": they are **different versions of the generator**, and
only one of them sorts.

- `@tanstack/router-generator` **1.166.24** ended its `multiSortBy` accessor
  list with `(d) => d` — the whole route-node *object*. `multiSortBy` compares
  with `>`/`<`, so every pair coerces to `"[object Object]"`, compares equal,
  and the "tiebreaker" resolves nothing. The order that survives is the stable
  fallback: the order `acc.routeNodes` was built in. That order comes from
  `getRouteNodes`, which recurses with
  `await Promise.all(dirList.map(...))` and pushes into a shared array from
  concurrently racing `readdir`s — i.e. it is I/O-timing dependent.
- **1.167.25** replaced that accessor with `(d) => d.routePath` and added
  `sortRouteNodes()`. Every route path is distinct, so the sort is now total
  and the readdir race is neutralised.
- Commit `85209da` (2026-08-07) bumped `@tanstack/router-plugin`
  1.167.12 → 1.168.27, dragging `router-generator` 1.166.24 → 1.167.25. That is
  the "the plugin now sorts its route imports" that `7a6bb60` re-synced against.
- The `make dev` Vite server in the working tree had been running since
  **2026-08-05 19:25** — *before* that bump. Node's ESM module cache means a
  live process keeps the code it imported at start, so that server was still
  emitting `routeTree.gen.ts` with **1.166.24**'s unsorted ordering while every
  cold `bun run build` emitted **1.167.25**'s sorted one. Hence the 154/154
  flip, and hence its one-off shape: the dev server only re-emits when it sees a
  route-file change.

Evidence:

- Replaying both accessor lists over 200 shuffled permutations of the repo's 20
  real route nodes: 1.166.24's yields **200 distinct orderings**, 1.167.25's
  yields **1**.
- Patching the installed `getRouteNodes.js` to shuffle every `readdir` result
  and add random per-directory jitter, then running `bun run build` six times:
  byte-identical output every time. The installed generator is order-insensitive.
- `git show 7a6bb60^:front/src/routeTree.gen.ts` has its imports in a
  roughly reverse-alphabetical traversal order — an unsorted artefact, not any
  total order.

Candidate cause #2 (an incremental disk cache) is ruled out: the generator's
caches (`routeNodeCache`, `routeTreeFileCache`) are in-memory `Map`s on the
`Generator` instance. Nothing is written to `node_modules/.vite`,
`node_modules/.cache` or `.tanstack`.

### What landed

The generator itself is already fixed upstream and is pinned by `front/bun.lock`,
so there is nothing to fix in `front/vite.config.ts`'s plugin options. What is
*not* fixed by a lockfile is a dev server that outlives a dependency bump, so:

- `front/vite.config.ts` gained a dev-only guard that snapshots the
  `@tanstack/router-generator` and `@tanstack/router-plugin` versions this
  process actually imported and re-checks them against `node_modules` every 30s,
  printing a loud "restart `make dev`" warning when they diverge. It never
  restarts or kills anything — `server.restart()` would not help, because Vite
  externalises node_modules when it reloads a config, so the stale plugin stays
  in Node's ESM cache either way. Only a new process picks up the new generator.
- `.github/workflows/ci.yml` documents what makes the committed-route-tree
  comparison sound.
- `front/CLAUDE.md` gained a troubleshooting entry.

Still open: no GitHub issue was filed (creating public content was out of scope
for the automated run) — see
`specs/todos/2026-08-11-15-file-issue-routetree-nondeterminism.md`.

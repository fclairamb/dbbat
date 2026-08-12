# File the GitHub issue for the route-tree nondeterminism

## Goal

Open a GitHub issue on `fclairamb/dbbat` recording the `routeTree.gen.ts`
flip-flop and its root cause, and link it from
`specs/done/2026/08/2026-08-11-02-routetree-generator-nondeterminism.md`.

## Why

`2026-08-11-02-routetree-generator-nondeterminism.md` says "No GitHub issue yet
— file one when picking this up." The investigation ran as an automated session,
which does not create public content on the repository, so the issue is still
missing. The diagnosis is worth a public record: the same trap catches anyone
whose `make dev` server outlives a frontend dependency bump, and the issue is
the natural place to point the `front/CLAUDE.md` troubleshooting entry at.

## Implementation

- `gh issue create --repo fclairamb/dbbat`, title along the lines of
  "`routeTree.gen.ts` is rewritten in a different order by a long-lived dev
  server".
- Body: lift the `## Findings` section of the spec — 1.166.24's `(d) => d`
  sort tiebreaker, the racy `Promise.all(dirList.map(...))` recursion in
  `getRouteNodes`, the 1.167.25 fix, and the Node ESM module cache that keeps a
  running Vite server on the old generator after `bun install`.
- Note the mitigation that landed: the dev-only staleness guard in
  `front/vite.config.ts` and the note above the CI check in
  `.github/workflows/ci.yml`.
- Close it immediately as resolved-by-`<commit>` if the issue tracker is only
  used for open work; the point is the searchable record.

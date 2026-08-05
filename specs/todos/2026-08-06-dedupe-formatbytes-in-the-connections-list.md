# Drop the connections list's private `formatBytes` copy

## Goal

Delete the local `formatBytes` at the bottom of
`front/src/routes/_authenticated/connections/index.tsx` and import the shared
one from `front/src/lib/utils.ts` instead.

## Why

`formatBytes` was hoisted into `front/src/lib/utils.ts` when the capture
download button landed, and the connection *detail* page
(`front/src/routes/_authenticated/connections/$uid.tsx`) already uses the
shared copy. The list page kept its own, and the two do not agree:

- the shared one tops out at `TB` and guards `bytes <= 0`;
- the local one tops out at `GB` and guards `bytes === 0`, so anything past
  1024 GB renders `undefined` (`sizes[i]` is out of range) and a negative
  count produces nonsense.

Neither is reachable with today's data, but the list and the detail page
formatting the same `bytes_transferred` differently is a latent trap.

## Implementation

- `front/src/routes/_authenticated/connections/index.tsx`: remove the
  file-local `formatBytes`, add `formatBytes` to the existing
  `@/lib/utils` import (the file does not import from it yet).
- Grep for other private copies — `grep -rn "function formatBytes" front/src`
  — and fold any others in at the same time.
- No behaviour change worth an e2e assertion; `bun run typecheck` and
  `bun run lint` are the gate.

No GitHub issue filed yet — one should be opened.

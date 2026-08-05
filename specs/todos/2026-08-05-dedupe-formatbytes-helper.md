# Three copies of formatBytes in the frontend

No GitHub issue filed yet — one should be opened when this is picked up.

## Goal

Collapse the frontend's three near-duplicate `formatBytes` implementations
down to the one canonical helper in `front/src/lib/utils.ts`.

## Why

While implementing capture download
(`specs/todos/2026-08-05-03-download-session-capture-from-ui.md`), the local
`formatBytes` in `front/src/routes/_authenticated/connections/$uid.tsx` was
lifted to use the existing `front/src/lib/utils.ts` one (already used by
`grant-definitions/index.tsx` and `grants/index.tsx`) instead of copying it
again. That leaves one more duplicate behind:
`front/src/routes/_authenticated/connections/index.tsx:231` still has its own
local `formatBytes`, and the two do not agree:

- the shared one tops out at `TB` and guards `bytes <= 0`;
- the local one tops out at `GB` and guards `bytes === 0`, so anything past
  1024 GB renders `undefined` (`sizes[i]` is out of range) and a negative
  count produces nonsense.

Neither divergence is reachable with today's data, but the list and the detail
page formatting the same `bytes_transferred` differently is a latent trap —
exactly the kind of thing that produces inconsistent byte labels across pages
for no reason.

## Implementation

- Delete the local `formatBytes` in
  `front/src/routes/_authenticated/connections/index.tsx` (~line 231) and its
  usage at ~line 132, import `formatBytes` from `@/lib/utils` instead (same
  swap already done in `$uid.tsx`).
- Diff the two implementations' edge-case behavior (0 bytes, negative bytes,
  bytes >= 1 TB) before deleting, and confirm the canonical one in
  `lib/utils.ts` doesn't regress any case the local one handled — it already
  looks like a superset (handles `<= 0` and adds a `TB` bucket), but verify.
- Grep for other private copies — `grep -rn "function formatBytes" front/src`
  — and fold any others in at the same time.
- No test changes expected beyond re-running any existing connections-list
  tests that assert on the formatted bytes column; `bun run typecheck` and
  `bun run lint` are the gate.

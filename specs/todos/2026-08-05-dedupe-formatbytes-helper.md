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
local `formatBytes`, slightly different from the canonical one (`bytes === 0`
vs `bytes <= 0`, and it stops at `GB` instead of including `TB`).

Three implementations of the same formatting logic drifting independently is
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
- No test changes expected beyond re-running any existing connections-list
  tests that assert on the formatted bytes column.

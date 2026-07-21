# Fix the deprecated `baseUrl` option in `website/tsconfig.json`

## Goal

Make `cd website && bun run typecheck` pass again. It currently fails with:

```
tsconfig.json(4,5): error TS5101: Option 'baseUrl' is deprecated and will stop
functioning in TypeScript 7.0.
```

## Why

The website pins `typescript: ~6.0.0`, which turned the `baseUrl` deprecation into a hard
error. The failure is pre-existing and unrelated to any content change, but it means the
`typecheck` script is dead weight — nobody can use it as a gate, and it will break CI the
moment typechecking is wired into the website pipeline.

## Implementation

- `website/tsconfig.json` extends `@docusaurus/tsconfig`; the offending `baseUrl` may come
  from either the local file or the base config. Check both.
- Preferred fix: drop `baseUrl` and express any path mapping with `paths` relative to the
  tsconfig location (TS 5.x+ resolves `paths` without `baseUrl`).
- Escape hatch if the base config is the source and cannot be changed yet: set
  `"ignoreDeprecations": "6.0"` and file an upstream Docusaurus issue.
- Verify with `cd website && bun run typecheck` and `bun run build`.

No GitHub issue exists for this yet — one should be filed if it is not picked up soon.

## Implementation Plan

- Confirmed `tsc` emits a single `TS5101` at `website/tsconfig.json(4,5)` — the local
  file's `baseUrl: "."`. The base `@docusaurus/tsconfig` also sets `baseUrl: "."` plus
  `paths: { "@site/*": ["./*"] }`, but its occurrence is not what tsc flags here.
- Fix: remove the redundant `baseUrl` from the local `website/tsconfig.json`. The local
  file defines no `paths`, so nothing local depends on it. `@site/*` / `@theme/*`
  resolution is provided by the base config + `@docusaurus/module-type-aliases`, which
  TS 5.x+/6.x resolves without a `baseUrl`.
- Verify: `cd website && bun run typecheck` (primary gate) and `bun run build`.

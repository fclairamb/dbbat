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

## Implementation Plan (as shipped)

The spec's preferred fix — "just delete the local `baseUrl`" — was tried and does **not**
work. Findings and the actual fix:

- Deleting the local `baseUrl` did not silence `TS5101`: the deprecated `baseUrl` also
  lives in the vendored `@docusaurus/tsconfig` base config (`node_modules`, uneditable),
  which tsc still flags after the local one is gone.
- Worse, the local `baseUrl: "."` is **load-bearing**: it anchors the base config's
  inherited `paths: { "@site/*": ["./*"] }` to this project dir. Removing it re-anchors
  `@site/*` to the base config's own dir, breaking `@site/...` module resolution.
- Fix applied (the spec's documented escape hatch): keep `baseUrl: "."` in
  `website/tsconfig.json` and add `"ignoreDeprecations": "6.0"` (with an explanatory
  comment + TODO to drop both once Docusaurus ships a tsconfig without `baseUrl`).
- Silencing the config error unmasked pre-existing React 19 errors (tsc had been bailing
  at the config error before ever typechecking sources): `Cannot find namespace 'JSX'`.
  Fixed by switching `JSX.Element` → `ReactNode` (imported from `react`) in
  `website/src/pages/index.tsx` and `website/src/components/HomepageFeatures/index.tsx`.
- Verified: `cd website && bun run typecheck` and `bun run build` both pass.
- Follow-up (out of scope here): file the upstream Docusaurus issue about the base
  config's deprecated `baseUrl`, per the note above.

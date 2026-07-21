# Make the frontend build type-checked so `tsc` failures cannot rot silently

## Goal

Ensure a TypeScript type error in `front/` fails the build (locally via
`make build-front` / `make build-app`, and in CI), instead of shipping.

## Why

`front/package.json` has two build targets:

- `build` — `tsc -b && vite build` (type-checked)
- `build:no-check` — `vite build` only

`scripts/build-frontend.sh`, which `make build-front` invokes, runs the
`no-check` variant. As a result the servers page carried three real
`TS2345` errors for an unknown length of time without anything going red
(fixed separately in `2026-07-21-fix-tsc-type-check-servers-page.md`). The
type-checked build only catches regressions if something actually runs it.

## Why it was not done inline

Flipping the build target is a CI/build-pipeline change with its own blast
radius (build time, and any other latent type errors elsewhere in `front/`
that would suddenly turn the build red). It deserves its own change and its
own verification pass rather than riding along with a schema fix.

## Implementation

1. Run `cd front && bun run build` on a clean tree first and fix (or file)
   whatever else it flags — the switch is only safe once it is green.
2. In `scripts/build-frontend.sh`, switch the invocation from
   `build:no-check` to `build`. Keep `build:no-check` available as an escape
   hatch for fast local iteration.
3. Confirm `make build-app` still succeeds and check the added wall-clock
   cost of `tsc -b` is acceptable for the CI pipeline.
4. Consider a dedicated `typecheck` script (`tsc -b --noEmit`) wired into the
   lint/CI stage, so type errors surface separately from bundling failures.

## Related

No GitHub issue exists for this yet — one should be filed, along with one for
the original type-check failure spec.

## Implementation Plan

1. **Baseline** — run `cd front && bun run build` on the clean tree to confirm
   `tsc -b` is already green (it is; ~5s wall clock, of which `tsc` is a small
   fraction). No pre-existing type errors to fix or defer.
2. **Add a dedicated `typecheck` script** to `front/package.json`
   (`tsc -b --noEmit`) so type errors can be surfaced independently of
   bundling. Document it in `front/CLAUDE.md`'s scripts table.
3. **Flip the local build** — `scripts/build-frontend.sh` runs `bun run build`
   instead of `bun run build:no-check`, so `make build-front` / `make build-app`
   are type-checked. `build:no-check` stays in `package.json` as an escape
   hatch for fast local iteration.
4. **Flip CI** — in `.github/workflows/ci.yml`, the `frontend` job gains an
   explicit `Type-check frontend` step (`bun run typecheck`) right after lint,
   and the build step switches to `bun run build`. Type errors then show up as
   their own failing step rather than buried in a bundling failure.
5. **Verify the gate bites** — temporarily introduce a deliberate type error,
   confirm `make build-front` (and `bun run typecheck`) go red, then revert.
   Re-run `make build-front`, `cd front && bun run build`, `bun run lint`.

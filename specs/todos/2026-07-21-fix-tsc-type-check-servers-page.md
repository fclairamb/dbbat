# Fix `bun run build` type-check failures on the servers page

## Goal

Make `cd front && bun run build` (i.e. `tsc -b && vite build`) pass again.

## Why

`front/src/routes/_authenticated/servers/index.tsx` fails type checking in three
places (lines ~474, ~489, ~939): the create/update payloads omit
`test_connection`, which `openapi-typescript` emits as a **required** property
because the OpenAPI schema gives it a `default: false`.

This is pre-existing (it reproduces without any local changes) and is masked in
CI/dev because `make build-front` runs `build:no-check`, which skips `tsc`. So
the app builds and ships, but the type-checked build is red and no longer
catches real regressions.

## Implementation

Two viable fixes — pick one:

1. **Frontend**: pass `test_connection: false` explicitly in the three call
   sites in `front/src/routes/_authenticated/servers/index.tsx`.
2. **Spec**: drop `default: false` from `test_connection` in
   `CreateDatabaseRequest` / `UpdateDatabaseRequest` in
   `internal/api/openapi.yml` (documenting the default in the description
   instead), then re-run `bun run generate-client`. This makes the generated
   type optional, matching the API's actual contract.

Option 2 is closer to the truth (the field really is optional) and prevents the
same trap on future defaulted fields.

Afterwards, consider making `make build-front` run the type-checked `build` so
this cannot silently rot again.

No GitHub issue exists for this yet — one should be filed.

## Implementation Plan

**Chosen fix: option 2 (spec-side).** Rationale: `test_connection` is genuinely
optional on the wire — the Go handlers decode into a struct where the zero value
is `false`, so omitting the field is a valid request. `openapi-typescript` v7
treats any property carrying a `default` as non-optional in the generated type,
so `default: false` was making the generated contract lie about the API. Fixing
the spec fixes every current and future call site at once, and stops the same
trap from recurring on the next defaulted boolean. Option 1 would have papered
over an inaccurate schema at three call sites.

Steps:

1. `internal/api/openapi.yml`: remove `default: false` from `test_connection` in
   `CreateDatabaseRequest` and `UpdateDatabaseRequest`; state the default in the
   property description instead so the documented behaviour is unchanged.
2. Regenerate the client: `cd front && bun run generate-client` (writes
   `front/src/api/schema.ts`). Expect `test_connection?: boolean` in both request
   schemas. No hand-editing of the generated file.
3. Verify `cd front && bun run build` (the `tsc -b && vite build` target) passes —
   this is the acceptance criterion — plus `bun run lint` for no new errors in
   touched files.
4. Since `openapi.yml` changed, run the backend gates: `make build-binary`,
   `make lint`, `make test`.
5. Follow-up (out of scope here, captured as a separate todo): make
   `make build-front` use the type-checked `build` target so this cannot rot
   silently again, and file the missing GitHub issue.

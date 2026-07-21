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

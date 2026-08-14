# `front/src/api/schema.ts` has drifted from `internal/api/openapi.yml`

No GitHub issue yet — file one when picking this up.

## Goal

Bring the committed generated client types back in step with the OpenAPI spec,
and make CI notice when they drift again.

## Why

Running `make build-front` on a clean tree modifies
`front/src/api/schema.ts` — the committed file predates edits to the
`access_grants` descriptions in `internal/api/openapi.yml` (the `database_id`
anchor wording and the `server_group_uid` "replaces the single-database scope"
paragraph). Noticed while regenerating the showcase media, where an unrelated
file turning up modified mid-run is exactly the noise that makes a shared
working tree hard to reason about.

Nothing is broken today — the drift is in doc comments, not in types — which is
precisely why it will keep happening unattended until something type-bearing
drifts and a frontend change silently compiles against a stale contract.

## Implementation

- `cd front && bun run generate-client`, review the diff, commit it.
- Add a CI check that regenerates and fails on a dirty tree. The natural home is
  the frontend workflow: run `bun run generate-client` then
  `git diff --exit-code front/src/api/schema.ts`, with a message pointing at the
  command to run locally.
- Check whether `scripts/build-frontend.sh` regenerates as a side effect of
  `make build-front`; if it does, that is the reason the drift is invisible
  locally and visible only as a dirty tree, and the check above is what turns it
  into a signal.

## Key files

- `front/src/api/schema.ts` — the generated types
- `internal/api/openapi.yml` — the source of truth
- `front/package.json` — the `generate-client` script
- `.github/workflows/` — where the drift check belongs

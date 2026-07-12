# Connection URL placeholder `{API_KEY}` is ambiguous — rename to `{DBBAT_KEY}`

## Problem

The database detail modal (and the API-keys page) shows a ready-to-paste
Connection URL such as:

```
admin/{API_KEY}@db.stonal.io:1522/TEST01
postgresql://admin:{API_KEY}@db.stonal.io:5432/
```

with the helper text *"Use one of your API keys as the password."*
(see https://dbbat.tools.stonal.io/app/databases).

The `{API_KEY}` placeholder is not clear enough. Users read "API key" as
*their target database's* API key, or as some Oracle/PG-side credential, rather
than a **dbbat** API key (the `dbb_…` token minted in dbbat). The word "key" in
a password slot next to a real hostname invites the wrong substitution.

The placeholder is emitted by the backend, not the frontend:

- `internal/api/connection_url.go:22` — `const keyPlaceholder = "{API_KEY}"`
- `internal/api/connection_url.go:25` — doc comment referencing the placeholder
- `internal/api/connection_url_test.go:83,187` — tests asserting `{API_KEY}`
- `internal/api/openapi.yml:1721` — endpoint description referencing `{API_KEY}`
- `front/src/api/schema.ts:843` — generated OpenAPI description (regenerated from `openapi.yml`)
- `front/src/routes/_authenticated/databases/index.tsx:504` and
  `front/src/routes/_authenticated/api-keys/index.tsx` — the "Use one of your API keys as the password." helper text

## Proposal

Rename the placeholder from `{API_KEY}` to `{DBBAT_KEY}` so it unambiguously
names a dbbat-issued API key.

1. `internal/api/connection_url.go:22` — change `keyPlaceholder` to `"{DBBAT_KEY}"`
   and update the doc comment on `BuildConnectionURL` (line 25).
2. `internal/api/connection_url_test.go` — update the two test cases (lines ~83, ~187)
   to assert the new placeholder.
3. `internal/api/openapi.yml:1721` — update the endpoint description; regenerate
   `front/src/api/schema.ts` so line 843 follows.
4. Frontend helper text (databases `index.tsx:504`, api-keys `index.tsx`) — reword
   to reinforce the mapping, e.g. *"Replace `{DBBAT_KEY}` with one of your dbbat API
   keys (the `dbb_…` token)."* Consider surfacing the placeholder visually (e.g. a
   subtle highlight on the `{DBBAT_KEY}` span) so it's obvious what to swap.

### Open questions

- Is `{API_KEY}` referenced anywhere externally (docs on dbbat.com, blog, saved
  user snippets) that would break on rename? Grep `website/` and `docs/` before
  landing.
- Purely cosmetic string change, no version bump semantics — ship as a `feat(api)`
  or `fix(ui)` depending on how the helper-text rewording is scoped.

## Implementation Plan

1. **Backend placeholder** (`internal/api/connection_url.go`): change `keyPlaceholder`
   const from `"{API_KEY}"` to `"{DBBAT_KEY}"`; update the doc comment on
   `BuildConnectionURL` (line 25) to reference `{DBBAT_KEY}`.
2. **Backend tests** (`internal/api/connection_url_test.go`): the two assertions use the
   `keyPlaceholder` constant (not a literal), so only the `t.Run` subtest names mention
   `{API_KEY}` — rename them to `{DBBAT_KEY}` for clarity.
3. **OpenAPI** (`internal/api/openapi.yml:1721`): update the endpoint description to say
   `{DBBAT_KEY}`.
4. **Generated schema** (`front/src/api/schema.ts`): regenerate via
   `cd front && bun run generate-client` (falls back to hand-edit line 843 if the
   generator is unavailable).
5. **Frontend helper text** (`front/src/routes/_authenticated/databases/index.tsx:504`):
   reword "Use one of your API keys as the password." to name the dbbat `dbb_…` token and
   the `{DBBAT_KEY}` placeholder. The api-keys page renders URLs with the *real* key
   (no placeholder), so no rewording is needed there.
6. **Docs/website sweep**: grep `website/` and `docs/` for `{API_KEY}` — none found, so
   nothing external breaks.
7. **QA**: `make build-binary`, `make test`, `make lint`; `make build-front` +
   `cd front && bun run lint`. Add/adjust an e2e assertion under `front/e2e/` if there's a
   natural spot for the connection-URL placeholder.

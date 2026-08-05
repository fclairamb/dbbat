---
model: sonnet
effort: medium
---

# Grant definitions need a slug for human-friendly reference in CLI / agents / API

## Problem

Grant definitions are only addressable by their `uid` (a UUID). Anything that
wants to reference one — a CLI invocation, an agent prompt, a REST call, a
runbook — has to first list definitions and copy a UUID. That is fine for the
web UI (which picks from a list) but hostile everywhere else:

- `POST /api/v1/grant-requests` takes `grant_definition_id` as a UUID
  (`internal/api/grant_requests.go:104`), so a scripted "request the
  read-only-prod definition" needs a lookup round-trip first.
- The API paths `/grant-definitions/{uid}` (`internal/api/openapi.yml:1424`)
  are equally UUID-only.
- Nothing on `GrantDefinition` (`internal/store/models.go:523`) is both
  stable and machine-friendly: `name` is free text, not unique, and not
  URL/CLI-safe.

## Proposal

Add a **mandatory, unique `slug`** column to `grant_definitions`.

### Store + migration

- New field on `GrantDefinition` (`internal/store/models.go:523`):
  `Slug string` with `bun:"slug,notnull"` and a unique index.
- New migration pair in `internal/migrations/sql/`
  (`YYYYMMDDHHMMSS_grant_definition_slug.up.sql` / `.down.sql`):
  add the column, **backfill existing rows with `slug = uid::text`** (trivially
  unique, keeps the migration dumb), then set `NOT NULL` + `UNIQUE`.
- Store helpers in `internal/store/grant_definitions.go`: a
  `GetGrantDefinitionBySlug` lookup alongside the existing uid-based one.

### API

- `CreateGrantDefinitionRequest` gains a `slug` field; validation in
  `validateDefinitionRequest` (`internal/api/grant_definitions.go:79`)
  enforces the format (suggested: `^[a-z0-9]+(-[a-z0-9]+)*$`, reasonable max
  length) and create/update handlers
  (`internal/api/grant_definitions.go:129` / `:301`) surface a clear 409/400
  on uniqueness conflicts.
- Slug is **mandatory at the API level** — the server does not auto-generate
  it; auto-generation is the frontend's job (below). This keeps the API
  contract explicit for CLI/agent callers.
- Wherever a definition is referenced by id, accept the slug as an
  alternative: at minimum `GET/PUT/DELETE /grant-definitions/{uid}` (resolve
  path param as uuid first, else slug) and `grant_definition_id` in
  `POST /grant-requests` (`internal/api/grant_requests.go:104`) — either by
  widening that field to accept a slug or adding a sibling
  `grant_definition_slug` field. Open question: pick whichever matches
  existing API conventions; widening the path param is the lighter touch.
- Update `internal/api/openapi.yml` (`/grant-definitions` at `:1353`,
  `/grant-definitions/{uid}` at `:1424`) accordingly.

### Frontend

- In the create/edit dialog
  (`front/src/routes/_authenticated/grant-definitions/index.tsx`), add a slug
  input that **auto-generates from the name** (lowercase, hyphenate, strip
  punctuation) as the user types, until the user edits the slug field
  manually — the usual "linked until touched" pattern. On edit, show the
  existing slug without re-deriving it.
- Display the slug in the definitions list so operators can copy it.

### Tests

- Store: slug uniqueness + lookup-by-slug (`internal/store/grant_definitions_test.go`).
- API: create with valid/invalid/duplicate slug; fetch by slug; grant-request
  creation referencing a definition by slug.
- Migration backfill: existing definitions end up with `slug = uid`.

# Guard against OpenAPI / route drift with a parity test

## Goal

Add a Go test in `internal/api` that parses the embedded `openapi.yml` and
asserts that every route registered on the gin router under `/api/v1` has a
matching path + method in the spec (and vice versa), so an endpoint can never
again ship undocumented.

## Why

`GET`/`DELETE /connections/{uid}/dump` shipped and stayed completely absent from
`internal/api/openapi.yml` until 2026-08-03 — the word "dump" did not appear in
the spec at all, so the capture-download API was invisible in Swagger UI
(`GET /api/docs`) and missing from any generated client. Nothing in `make test`
or CI parses `openapi.yml`: it is not even checked for being valid YAML, so a
typo could ship a spec that Swagger UI fails to render.

The drift is silent and recurring, and it is cheap to detect: gin exposes the
full route table via `(*gin.Engine).Routes()`, and the spec is already embedded
in the binary via `//go:embed openapi.yml` (`internal/api/server.go:29`).

No GitHub issue filed yet — one should be opened.

## Implementation

New file `internal/api/openapi_parity_test.go`:

1. Unmarshal the embedded spec (the `//go:embed` var in
   `internal/api/server.go:29`) with `gopkg.in/yaml.v3` into a minimal struct —
   `paths: map[string]map[string]any` is enough, no full OpenAPI model needed.
   Failing to unmarshal is itself a useful assertion (spec is valid YAML).
2. Build a server with the existing test helper used by the other
   `internal/api` tests, then walk `engine.Routes()`.
3. Normalise both sides to a comparable key: gin uses `/api/v1/connections/:uid`,
   the spec uses `/connections/{uid}` — strip the `/api/v1` prefix from gin
   routes and rewrite `:param` to `{param}` (or the reverse).
4. Compare the two sets and report both directions with a readable diff:
   routes missing from the spec, and spec paths with no route.
5. Skip non-API routes deliberately: the SPA/static handlers, `/api/docs`,
   `/api/openapi.yml`, `/health` and friends if they are registered outside
   `/api/v1`. Keep the skip list explicit and short so it does not become a
   dumping ground.

Consider also asserting that every operation carries an `operationId` and that
operation ids are unique — both hold today and both matter for client
generation.

## Files

- `internal/api/openapi.yml` — the spec under test.
- `internal/api/server.go:29` — the `//go:embed` of the spec.
- `internal/api/server.go:200-420` — route registration (the other side of the
  comparison).
- `internal/api/*_test.go` — existing test server helpers to reuse.

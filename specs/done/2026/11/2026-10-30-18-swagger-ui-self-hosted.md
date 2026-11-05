---
model: opus
effort: low
---

# Swagger UI loads unpinned scripts from a public CDN on the admin origin

## Problem

Found by the security scan of 2026-10-30 (finding L7).

Severity: Low. Needs a compromise of the CDN or of the `swagger-ui-dist` package.

`GET /api/docs` serves a page that loads `swagger-ui-dist@5` CSS and JavaScript from a public CDN, with a floating major version and no Subresource Integrity (`internal/api/server.go:618`, `server.go:622`). That script runs on dbbat's own origin, where the web UI keeps its session token in `localStorage`. Any new 5.x release, or a tampered CDN response, runs with the ability to read that token and call the admin API as whoever opened the page.

## Proposal

1. Embed a pinned `swagger-ui-dist` build in `internal/api/resources` (like the frontend) and serve it locally. Or, at minimum, pin an exact version and add `integrity` + `crossorigin` attributes.
2. Optionally add a restrictive `Content-Security-Policy` on the docs page.

## Tests

- `internal/api` test: the docs HTML references no external script, or only one with an exact version and an `integrity` attribute. This fails before the fix.
- Positive control: `GET /api/docs` still returns 200 and the assets it references are served.

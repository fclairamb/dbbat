# Document the connection dump endpoints in the OpenAPI spec

## Goal

Add `GET /api/v1/connections/{uid}/dump` and `DELETE /api/v1/connections/{uid}/dump`
to `internal/api/openapi.yml`, including the binary response media type
(`application/x-pcapng`) and the `.pcapng` download filename.

## Why

Both routes are registered in `internal/api/server.go:364-365` and implemented in
`internal/api/observability.go:292-349`, but the word "dump" does not appear
anywhere in `internal/api/openapi.yml`. The capture-download API is therefore
invisible in Swagger UI (`GET /api/docs`) and absent from any client generated
from the spec — users have to read the Go source to discover it, and to learn
that the payload is a pcapng file rather than the bespoke legacy format.

No GitHub issue filed yet — one should be opened.

## Implementation

In `internal/api/openapi.yml`, next to the existing `/connections/{uid}` path:

- `GET /connections/{uid}/dump`
  - Tag: the same tag used by the other connection endpoints.
  - Auth: admin **or** viewer (`s.requireAdminOrViewer()`).
  - `200` response with content `application/x-pcapng`, schema
    `{type: string, format: binary}`; document the
    `Content-Disposition: attachment; filename="<uid>.pcapng"` header.
  - `404` for both "dumps are not enabled" (`DBB_DUMP_DIR` empty) and
    "no dump available for this connection", reusing the standard error schema.
  - `400` for an invalid UID.
- `DELETE /connections/{uid}/dump`
  - Auth: admin only (`s.requireAdmin()`).
  - `204` on success, `404` in the same two cases, `500` on unlink failure.

Keep the constants in sync: `dumpFileContentType` and `dumpFileExt` live in
`internal/api/observability.go:286-289` (`dumpFileExt = dump.FileExt`, itself
`.pcapng` from `internal/dump/dump.go:10`). Reference `docs/dump-format.md` from
the endpoint description so readers know what the bytes are and how to open them
with `tcpdump`/`tshark`.

## Files

- `internal/api/openapi.yml` — the addition.
- `internal/api/server.go:364` — route registration (source of truth for auth).
- `internal/api/observability.go:286` — handlers, content type, extension.
- `docs/dump-format.md` — pcapng format reference to link from the description.

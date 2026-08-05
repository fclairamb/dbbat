---
model: sonnet
effort: high
---

# The raw session capture can only be downloaded by hand-crafting an API call

## Problem

Session captures (the raw client↔proxy↔upstream exchanges, pcapng) are
downloadable over the API but completely absent from the web UI:

- `GET /api/v1/connections/{uid}/dump` streams the file
  (`internal/api/observability.go:292`), `DELETE` removes it (`:321`); both are
  registered in `internal/api/server.go:364-365` and documented in
  `internal/api/openapi.yml:1973+`.
- The connection detail page (`front/src/routes/_authenticated/connections/$uid.tsx`)
  renders identity, timings, counters, the live watch panel and the query list —
  and never mentions the capture. `dump` appears in `front/src/api/schema.ts`
  (generated) and nowhere else in `front/src`.

So the only way to get the bytes today is `curl` with a bearer token or an API
key. That is exactly backwards: the capture is the deepest debugging artefact
dbbat produces, and it is the one thing the UI hides.

Two things block a naive "add an `<a href>`" fix:

1. **The UI cannot know whether a capture exists.** The connection payload
   (`internal/store/models.go:291`) has no capture field, and `useConnection`
   (`front/src/api/queries.ts:791`) returns it verbatim. Captures only exist
   when `DBB_DUMP_DIR` is set, are capped by `DBB_DUMP_MAX_SIZE`, and are swept
   by `DBB_DUMP_RETENTION` (`internal/dump/cleanup.go:20`) — so "does a file
   exist for this connection" is genuinely dynamic. An always-visible button
   would 404 most of the time.
2. **A plain link is unauthenticated.** The API client injects
   `Authorization: Bearer …` through middleware (`front/src/api/client.ts:51`)
   from a token in `localStorage`; a browser-initiated navigation to
   `/api/v1/connections/{uid}/dump` carries no header and gets a 401.

## Proposal

### 1. Backend — a capture-availability signal on the connection detail response

Add capture metadata to `GET /connections/{uid}` only (**not** to the list
endpoint — one `os.Stat` per row is a needless fan-out on a paginated list):

```json
"dump": { "available": true, "size_bytes": 148392 }
```

`available: false` covers both "dumps disabled server-wide" (`Dump.Dir == ""`)
and "no file for this connection". Reporting `size_bytes` lets the UI label the
action ("Download capture (145 KB)") and warns the user before pulling a 10 MB
file.

Do **not** add a column to `connections`. Resolve it in the handler through a
single helper — the natural shape is a small `dumpLocator` on the API server
(`resolve(uid) (path string, size int64, ok bool)`) used by all three call
sites: the new metadata, `handleGetConnectionDump` (`:309`) and
`handleDeleteConnectionDump` (`:338`), which today each re-join the path and
re-stat by hand. That one helper is also the seam
[`2026-08-05-02-s3-dump-storage.md`](2026-08-05-02-s3-dump-storage.md) needs:
when captures move to blob storage, the "local-first, then remote via the stored
key" resolution lands inside it and the UI keeps working unchanged. Adding a
`connections.dump_key` column is that spec's job, not this one's.

Update `internal/api/openapi.yml` (the `Connection` schema) and regenerate
`front/src/api/schema.ts` with `bun run generate-client`.

### 2. Backend — tighten the download to admins

The ask is admin-only, but `GET …/dump` is currently `requireAdminOrViewer()`
(`internal/api/server.go:364`). A UI gate over a viewer-readable endpoint is
cosmetic, so change the route to `s.requireAdmin()` and make the API the
boundary. This is defensible on its own terms: a capture is the raw byte stream,
including the authentication handshake and every result row, at a fidelity well
past the redacted, structured view a viewer gets from the queries pages.

It is a narrowing of a documented endpoint — note it in the OpenAPI description
and flag it in the PR body / changelog so it lands as a deliberate change rather
than a surprise 403. `DELETE` is already admin-only and stays as is.

### 3. Frontend — an authenticated download

Add a `useDownloadConnectionDump(uid)` mutation in `front/src/api/queries.ts`
that fetches with the auth header and hands the browser a blob:

- `apiClient.GET("/connections/{uid}/dump", { parseAs: "blob" })` — `openapi-fetch`
  supports `parseAs`, and it keeps the request on the same middleware, so the
  bearer token and the 401 → login redirect (`front/src/api/client.ts:35`) both
  still apply. Do not hand-roll a `fetch` with a manually read token.
- Then `URL.createObjectURL(blob)` → a synthesized `<a download="<uid>.pcapng">`
  click → `URL.revokeObjectURL` in a `finally`. There is no existing download
  helper in `front/src`; put this one in `front/src/lib/` so the next feature
  that needs it (query result export) doesn't reinvent it.

Surface it on the connection detail page
(`front/src/routes/_authenticated/connections/$uid.tsx`), in the `PageHeader`
`actions` slot next to the Active/Disconnected badge, rendered only when
`isAdmin` (`front/src/contexts/AuthContext.tsx:33`) **and** `connection.dump?.available`.
Requirements:

- Label with the size, e.g. `Download capture (145 KB)` — reuse the local
  `formatBytes` (`$uid.tsx:255`); lift it to `front/src/lib/` rather than
  copying it.
- `data-testid="download-dump-button"` per `front/CLAUDE.md`.
- Pending state while the blob streams (a 10 MB capture over a slow link is not
  instant) and a toast on failure — a capture swept by retention between page
  load and click yields a 404, which must read as "capture no longer available",
  not a silent no-op.
- A short hint that the file is pcapng and opens in Wireshark/tcpdump, linking
  `docs/dump-format.md`. Mention `dbbat dump anonymise` for sharing.

An in-browser capture viewer is explicitly **out of scope** — this is download
only.

### 4. Tests

- Go: extend the API tests around `internal/api/observability.go` to cover the
  new metadata (dumps disabled → `available: false`; file present → `true` with
  the right `size_bytes`) and the admin-only narrowing (viewer token → 403).
- E2E: `front/e2e/observability.spec.ts` — assert the button is absent for a
  connection with no capture, and absent for the `viewer` account. Asserting a
  successful download needs a test-mode connection that actually has a capture
  file on disk; if the test harness can't produce one cheaply, skip the
  happy-path assertion rather than faking it, and say so in the test.

No GitHub issue filed yet — one should be opened when this is picked up.

## Implementation Plan

Starting point note: the sibling S3 spec (`specs/done/2026/08/2026-08-05-02-s3-dump-storage.md`)
already landed local-spool-first/remote-via-`dump_key` logic inline in
`handleGetConnectionDump` and `handleDeleteConnectionDump`. This plan factors
that logic into the shared helper this spec asks for, instead of writing it
from scratch.

1. **`internal/dump/upload.go`** — add `(*Uploader) Stat(ctx, key) (int64, error)`,
   nil-receiver-safe like `Open`/`Delete`, using `bucket.Attributes` and
   returning `os.ErrNotExist` (wrapped) on `gcerrors.NotFound`. Needed so the
   detail-page metadata can report `size_bytes` for a capture that only lives
   remotely (no local `os.Stat` available for it).
2. **`internal/api/observability.go`** — add a `dumpLocator` type + `(*Server)
   resolveDump(c, uid) dumpLocator` that stats the local spool file and looks
   up the remote key once, in one place. Route all three call sites through
   it:
   - `handleGetConnection` (detail only) — build a `connectionDetailResponse{
     *store.Connection; Dump DumpMetadata }` (same embedding pattern as the
     existing `userDetailResponse`), resolving `Dump.Available`/`SizeBytes`
     via `resolveDump` (+ one `Uploader.Stat` round trip when the capture is
     remote-only). List endpoint (`handleListConnections`) is untouched — it
     keeps returning bare `store.Connection` rows.
   - `handleGetConnectionDump` — same 404-vs-404 messages as today ("dumps
     are not enabled" vs "no dump available…"), now sourced from
     `resolveDump`'s local/remote fields instead of re-stating and re-joining
     paths inline.
   - `handleDeleteConnectionDump` — same "delete both, local and remote, if
     present" behavior as today, now driven by the same `dumpLocator`.
3. **`internal/api/server.go`** — narrow `GET /connections/:uid/dump` from
   `s.requireAdminOrViewer()` to `s.requireAdmin()`. `DELETE` is already
   admin-only.
4. **`internal/api/openapi.yml`** — add a `ConnectionDetail` schema (`allOf`
   `Connection` + `dump: {available, size_bytes}` object) used only by `GET
   /connections/{uid}`; the list endpoint keeps referencing plain
   `Connection`. Update the `GET …/dump` description to state the endpoint is
   now admin-only (was admin-or-viewer) — a deliberate, documented narrowing.
5. **`front/`** — regenerate `src/api/schema.ts` via `bun run generate-client`
   after the OpenAPI change lands.
6. **`front/src/lib/`** — new `format.ts` (or extend an existing lib file)
   with `formatBytes`, lifted verbatim from `$uid.tsx:255`, and a new
   `download.ts` with a `downloadBlob(blob, filename)` helper
   (`URL.createObjectURL` → synthesized `<a download>` click →
   `URL.revokeObjectURL` in `finally`).
7. **`front/src/api/queries.ts`** — `useDownloadConnectionDump(uid)`: a
   `useMutation` that calls `apiClient.GET("/connections/{uid}/dump", {
   parseAs: "blob" })` and hands the blob to `downloadBlob`.
8. **`front/src/routes/_authenticated/connections/$uid.tsx`** — swap the
   local `formatBytes` for the lifted one; add a "Download capture (…)"
   button in `PageHeader`'s `actions` slot, gated on `isAdmin &&
   connection.dump?.available`, with `data-testid="download-dump-button"`,
   pending state, a failure toast, and a hint line pointing at
   `docs/dump-format.md` / `dbbat dump anonymise`.
9. **Tests**:
   - Go: extend `internal/api/connection_dump_test.go` (or add a new file)
     for the detail-metadata field (disabled → `available:false`; local file
     → `true` + correct size; remote-only → `true` + correct size via
     `Uploader.Stat`) and a viewer-token 403 on `GET …/dump` in
     `newDumpTestRouter`.
   - E2E: `front/e2e/observability.spec.ts` — button absent for a capture-less
     connection and absent for the `viewer` account; happy-path download
     skipped with a comment if the test harness can't cheaply produce a
     connection with an on-disk capture.

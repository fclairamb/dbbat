# Show whether a session's upstream leg was encrypted in the connections UI

## Goal

Surface `connections.upstream_tls` in the frontend: a badge in the connections
list and a row in the connection detail page.

## Why

The column exists and the API returns it (`Connection.upstream_tls`, documented
in `internal/api/openapi.yml`, typed in `front/src/api/schema.ts`), but nothing
displays it, so the answer to "was this session actually encrypted?" is still
only reachable by querying the database.

That question is the whole point of the column. `ssl_mode` states a policy, not
an outcome: under `prefer` — the default of every unset server row, and what
almost the entire Stonal fleet uses — dbbat offers TLS and falls back to
plaintext when the target refuses. An operator looking at a connection cannot
tell which happened. A silent downgrade is exactly the case worth seeing.

Backend landed with the unified upstream-connect work
(`specs/todos/2026-08-04-unify-proxy-and-probe-upstream-connect.md`); the UI was
deliberately left out of that change because it was a backend-scoped task.

## Implementation

- `front/src/routes/_authenticated/connections/index.tsx` — add a column (or a
  small lock/unlock icon next to the database) driven by `c.upstream_tls`.
  Keep it quiet when encrypted and visible when not: the plaintext case is the
  one an operator needs to notice.
- `front/src/routes/_authenticated/connections/$uid.tsx` — add a `<dt>/<dd>`
  pair near the existing `bytes_transferred` row.
- Consider pairing it with the server's `ssl_mode` in the detail view, so
  "policy said prefer, outcome was plaintext" reads as one statement.
- Oracle sessions are always `false` (the proxy relays the client's own TNS
  Connect descriptor over a plain socket and never upgrades). Either label it
  as "not applicable" for Oracle rows or accept the honest `false` — do not
  render it as a warning for a protocol that cannot do better yet.

No GitHub issue filed yet — one should be opened.

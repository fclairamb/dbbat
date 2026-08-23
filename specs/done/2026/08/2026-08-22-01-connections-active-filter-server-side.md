# The "Active only" toggle on /app/connections still filters client-side, inside the fetched page

## Goal

Move the `active` toggle on `/app/connections` from a client-side `Array.filter`
over the already-fetched page to a server-side `disconnected_at IS NULL`
predicate, so it stops under-reporting the moment the matching sessions are not
in the current 50 rows.

## Why

`/app/connections` fetches a page of connections and then narrows it in the
browser:

```tsx
const filteredConnections = active
  ? connections?.filter((c) => !c.disconnected_at)
  : connections;
```

(`front/src/routes/_authenticated/connections/index.tsx`.)

That reads as "show me the live sessions" but actually means "show me the live
sessions *that happen to be on this page*". On an instance where the most recent
50 connections are all closed, an operator who flips the toggle sees an empty
table and concludes nothing is running — while a long-lived session sits two
pages back. There is no indication that the answer was computed over a window.

This is exactly the trap the observability filters added in
`2026-08-21-01-connection-query-filters-and-user-last-activity.md` were written
to avoid: every filter that spec added is applied in the database. It was
deliberately left **out of scope** there, because moving this one is a
*behaviour change* to a control that already exists — the toggle currently
returns a different set of rows than a server-side version would, and shipping
it inside a "new filters" change would hide that. It gets its own spec so the
change is visible on its own.

The supporting index already exists: `20260803010000_connections_disconnected_at_index.up.sql`
adds a partial index on the live sessions, so no migration is needed.

## Implementation

**Store** (`internal/store/models.go`, `internal/store/connections.go`)

- Add `ActiveOnly bool` to `store.ConnectionFilter` (a plain bool, not a
  `*bool`: "unfiltered" and "false" mean the same thing here, unlike the
  tri-state settings elsewhere in the codebase).
- In `ListConnections`, apply `Where("disconnected_at IS NULL")` when set. It
  composes with AND alongside the other filters, like everything else there.
- Confirm with `EXPLAIN` that the partial index
  `idx_connections_disconnected_at` (or whatever `20260803010000` named it) is
  used, and that the `ORDER BY uid DESC LIMIT n` pagination still does not sort
  the table.

**API** (`internal/api/observability.go`)

- Parse `active=true` on `GET /connections`. Follow the rule the new filters
  set: reject a value that is neither `true` nor `false` with a 400 rather than
  silently ignoring it.
- The connector scoping overwrite must stay the last thing that touches the
  filter, as it already is.
- Document the parameter in `internal/api/openapi.yml` (the parity test
  enforces the path, and the parameter belongs next to the others), then
  regenerate the typed client with `bun run generate-client` in `front/`.

**Frontend** (`front/src/routes/_authenticated/connections/index.tsx`)

- Delete `filteredConnections` and pass `active` through to `useConnections`
  instead. `active` is already a URL search param, so nothing changes about how
  it is carried.
- Route the toggle through the same `applyFilters` helper the filter bar uses,
  so flipping it resets `before` to `undefined` — a cursor from the unfiltered
  list is meaningless against the filtered one. This is the part that is
  currently wrong in a second, subtler way: the toggle presently keeps the
  cursor.
- Consider moving the toggle into `ObservabilityFilterBar` so every server-side
  filter on the page lives in one place. Optional, but it stops the page having
  two visually unrelated filtering mechanisms that now behave identically.

**Tests**

- Store: a fixture with more closed sessions than the page size, asserting the
  filtered list returns the live one that a client-side narrowing would have
  missed. That is the regression this spec exists for, and it must fail against
  the current implementation.
- API: `?active=true` narrows; a malformed value is a 400; a connector still
  sees only their own live sessions.
- E2E: flipping the toggle puts `active=true` in the URL and clears `before`.

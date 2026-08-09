---
model: sonnet
effort: low
---

# /grant-definitions shows deactivated definitions by default instead of behind a toggle

## Problem

For admins, the grant definitions page fetches the full list — active and
deactivated mixed together — and renders every row, relying on a small
"deactivated" badge to tell them apart
([index.tsx:105-110](front/src/routes/_authenticated/grant-definitions/index.tsx:105)):

```tsx
// Admins see all definitions (active+inactive); other roles never reach
// this page in the nav, but the API also enforces the active-only filter
// for non-admins as a defense-in-depth.
const { data: definitions = [], isLoading } = useGrantDefinitions({
  active_only: !isAdmin,
});
```

Deactivated definitions are permanent (hard deletion is refused while
anything references them), so this list only grows: over time the page
becomes dominated by dead rows the admin almost never cares about. The
default view should show only active definitions, with deactivated ones
reachable behind an explicit toggle.

## Proposal

Frontend-only change — the API already supports the filter (`active_only`
query param, [grant_definitions.go:393-395](internal/api/grant_definitions.go:393),
plumbed through `useGrantDefinitions` in
[queries.ts:548](front/src/api/queries.ts:548)).

1. In `GrantDefinitionsPage`, add a `showDeactivated` state (default
   `false`). Fetch with `active_only: !isAdmin || !showDeactivated` so the
   default admin view only loads active rows, and flipping the toggle
   refetches with `active_only: false` (distinct query keys mean TanStack
   Query caches both views).
2. Add the toggle to the page header — a small switch or checkbox labelled
   "Show deactivated" (`data-testid="show-deactivated-toggle"`), visible to
   admins only (non-admins are already forced to active-only server-side).
3. Keep the existing "deactivated" badge on rows so the mixed view stays
   legible when the toggle is on.
4. Update/extend the E2E coverage in `front/e2e/` that touches grant
   definitions: after deactivating a definition it should disappear from
   the default list, then reappear when the toggle is enabled.

### Terminology note

The user-facing wording asked for an "archived" toggle, but in this
codebase **archived** already means something else: a superseded version
row (`archived_at IS NOT NULL`) created when a definition is edited, and
the list endpoint deliberately never returns those
([grant_definitions.go:193-209](internal/store/grant_definitions.go:193)).
What the toggle reveals are **deactivated** definitions (`is_active =
false`) — the label should say "deactivated" (matching the existing badge
and the deactivate action) to avoid colliding with the versioning
vocabulary. No backend change is needed or wanted here.

---
model: sonnet
effort: low
---

# The "Active only" toggle on /app/connections reloads the whole page

No GitHub issue yet — one should be filed.

## Problem

On `/app/connections`, flipping the "Active only" switch causes a full
browser reload instead of an in-app update. The handler bypasses the SPA
router entirely
(`front/src/routes/_authenticated/connections/index.tsx:210-217`):

```tsx
onCheckedChange={(checked) => {
  // Navigate to update the search param
  window.location.search = new URLSearchParams({
    ...(before ? { before } : {}),
    size: String(size),
    ...(checked ? { active: "true" } : {}),
  }).toString();
}}
```

Assigning `window.location.search` is a hard navigation: the browser
re-requests the document, the SPA re-bootstraps, auth context and TanStack
Query cache are rebuilt, and the page flashes. Every other search-param
change on this page (page size, Newer/Older pagination) already goes through
TanStack Router `<Link search={...}>` and stays client-side — the toggle is
the only offender. The filter itself is client-side anyway (line 87 filters
the already-fetched page by `disconnected_at`), so the reload buys nothing.

## Proposal

Replace the raw location assignment with the router's navigate, keeping the
same URL shape so bookmarks and `validateSearch`
(`index.tsx:27-31`) keep working:

```tsx
const navigate = Route.useNavigate();
...
onCheckedChange={(checked) => {
  navigate({
    search: (prev) => ({ ...prev, active: checked ? true : undefined }),
    replace: true,
  });
}}
```

Notes:

- `active: undefined` removes the param from the URL when toggled off,
  matching today's behavior; `validateSearch` already accepts the boolean
  `true` (line 30), so no change needed there.
- `replace: true` keeps flip-flopping the switch from polluting browser
  history; drop it if history entries per toggle are preferred.
- Verify with the dev server that toggling updates the table with no
  document reload (network tab shows no new document request), and that
  pagination + page-size links still carry `active` through.

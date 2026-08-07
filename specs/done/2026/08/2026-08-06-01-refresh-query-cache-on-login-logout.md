---
model: sonnet
effort: medium
---

# Login/logout leaves the TanStack Query cache populated with the previous session's data

## Problem

The frontend's `QueryClient` is created once at module level
(`front/src/main.tsx:12`) and is never cleared when the authenticated user
changes. `AuthContext.login()` and `AuthContext.logout()`
(`front/src/contexts/AuthContext.tsx:111` and `:161`) only manage the token and
the auth state — they never touch the query cache.

Consequences of the in-app login → logout → login-as-someone-else flow (soft
navigation, no page reload):

- Cached lists — users, databases/servers, grants, connections, queries — from
  the previous session are still in the cache. With the 1-minute `staleTime`,
  the next user is served the previous user's data without even a background
  refetch if they navigate there quickly.
- Role differences make it worse: log out of `admin`, log in as `viewer` (or
  vice versa), and pages briefly render data the current user should not see
  (or stale `AccessDenied`/error states the current user should not get).

The session-expiry path is *not* affected: the 401 handler in
`front/src/api/client.ts:35` does a hard `window.location.href` redirect, which
reloads the SPA and discards the in-memory cache. Only the in-app
`login()`/`logout()` transitions leak cache across sessions.

## Proposal

Clear the query cache whenever the authenticated identity changes:

1. In `AuthProvider` (`front/src/contexts/AuthContext.tsx`), grab the client
   with `useQueryClient()` — `AuthProvider` is already mounted inside
   `QueryClientProvider` (`front/src/main.tsx:45-48`), so this works.
2. In `logout()`: after `clearToken()` / state reset, call
   `queryClient.clear()`. Ordering note: reset the auth state first so the
   router unmounts authenticated routes before the cache is emptied —
   otherwise still-mounted queries could refetch without a token and bounce
   through the 401 redirect.
3. In `login()`: call `queryClient.clear()` before setting the new
   authenticated state, so every query mounted after login starts from an
   empty cache and fetches fresh data under the new identity. (Alternative:
   `queryClient.removeQueries()` — `clear()` also drops mutation state, which
   is what we want here.)

Also worth covering:

- The initial `checkStoredAuth` failure path (stored token invalid) already
  leads to the login screen; give it the same `clear()` for consistency.
- An e2e assertion in `front/e2e/` — log in as `admin`, load the users list,
  log out, log in as `viewer`, and assert the users page does not render the
  admin-cached rows (viewer should get its own fetch / access-denied state).

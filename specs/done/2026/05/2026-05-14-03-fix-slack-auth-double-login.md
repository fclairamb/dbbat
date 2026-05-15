# Fix: Slack OAuth Double-Login (React Timing Race)

## Problem

After a successful Slack OAuth callback, the user is redirected back to the login page and must authenticate a second time before landing in the app.

The first authentication **does succeed** on the backend — a valid session is created and the token is passed to the frontend via `/app/login?token=SESSION_TOKEN`. The bug is purely in the frontend.

## Root Cause

In `front/src/routes/login.tsx` (lines 72–86), the OAuth callback handler stores the token and navigates after `refreshUser()` resolves:

```typescript
storeToken(token);
refreshUser().then(() => {
  navigate({ to: "/" });
});
```

`refreshUser()` calls `validateSession()` which calls `/auth/me` and, on success, calls React's `setState({ isAuthenticated: true, ... })`. This state update is **scheduled but not yet committed** when the `.then()` callback runs.

When `navigate({ to: "/" })` fires, TanStack Router evaluates `beforeLoad` in `_authenticated.tsx` with the router context snapshot from `InnerApp`:

```typescript
// main.tsx — context reflects the *last committed* React render
function InnerApp() {
  const auth = useAuth();
  return <RouterProvider router={router} context={{ auth, queryClient }} />;
}
```

Because the React state update hasn't been committed yet, `context.auth.isAuthenticated` is still `false`. The guard throws a redirect to `/login`:

```typescript
// _authenticated.tsx
beforeLoad: ({ context }) => {
  if (!context.auth.isAuthenticated && !context.auth.isLoading) {
    throw redirect({ to: "/login" });
  }
},
```

The user ends up at `/login` even though the session is valid and the token is in localStorage. On the second visit to `/login`, there is no token in the URL, so the OAuth handler doesn't run. But the token IS in localStorage — a prior render already set `isAuthenticated: true` — and now the router context is up-to-date, so manually navigating to `/` works. This forces the user to click "Sign in with Slack" a second time.

## Fix

Move the navigation out of the async `.then()` and into a `useEffect` that reacts to `isAuthenticated` becoming `true`. React `useEffect` runs **after** the state update has been committed to the DOM and to the router context — so `navigate` will see the correct `isAuthenticated: true`.

### `front/src/routes/login.tsx`

Two changes:

1. **Add a reactive redirect effect** (placed near the top of `LoginPage`):

```typescript
// Redirect authenticated users away from the login page
useEffect(() => {
  if (!isLoading && isAuthenticated) {
    navigate({ to: "/" });
  }
}, [isAuthenticated, isLoading, navigate]);
```

2. **Simplify the OAuth callback handler** — just store the token and call `refreshUser()`. Drop the `.then(() => navigate(...))`:

```typescript
useEffect(() => {
  const params = new URLSearchParams(window.location.search);
  const token = params.get("token");
  if (token) {
    window.history.replaceState({}, "", window.location.pathname);
    storeToken(token);
    refreshUser();
  }
}, []); // eslint-disable-line react-hooks/exhaustive-deps
```

The reactive effect handles the redirect once React commits `isAuthenticated: true`.

### Why this works

`useEffect` fires **after** React flushes state updates to the DOM. By the time the effect runs:
- `isAuthenticated: true` is the committed state
- `InnerApp` has re-rendered and passed the updated `auth` to `RouterProvider`
- TanStack Router's `context.auth` snapshot reflects the new state
- `beforeLoad` sees `isAuthenticated: true` → no redirect

## Secondary benefit

The reactive redirect also handles the case where an authenticated user navigates directly to `/login` (e.g., via browser history). They'll be redirected to `/` immediately instead of seeing the login form.

## Files to Change

| File | Change |
|------|--------|
| `front/src/routes/login.tsx` | Add `isAuthenticated` to `useAuth()` destructuring; add redirect `useEffect`; remove `.then(() => navigate(...))` from OAuth handler |

No backend changes needed. No other frontend files need modification.

## Acceptance Criteria

1. Clicking "Sign in with Slack" completes in a single round-trip: user lands in the app after one OAuth flow
2. No second login required after a successful Slack callback
3. An already-authenticated user visiting `/login` is redirected to `/` immediately
4. Password login and password-change flows are unaffected
5. OAuth error codes (e.g., `slack_denied`) still display correctly on the login page

## Estimated Size

~5 lines changed in one file.

## Implementation Plan

1. **Read `front/src/routes/login.tsx`** to identify exact lines to change.
2. **Add `isAuthenticated` to `useAuth()` destructuring** in `LoginPage`.
3. **Add reactive redirect `useEffect`** that fires when `isAuthenticated` becomes true.
4. **Simplify OAuth callback handler** — drop `.then(() => navigate(...))`, keep just `storeToken(token); refreshUser();`.
5. **Build** (`bun run build`) to confirm no TypeScript errors.
6. **Commit** the fix.

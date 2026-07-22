# Preserve intended destination through the login redirect

## Goal

When an unauthenticated visitor opens any authenticated route directly (e.g. a
deep link, or the new `/cli-auth/$uid` CLI-authorization approval page from
[#todo](2026-07-22-cli-authorization-flow.md)), the `_authenticated` guard
redirects to `/login` without recording where they were headed. After
logging in, they land on `/` instead of continuing to the original page.

## Why

Discovered while manually testing the CLI-authorization approval page
(`front/src/routes/_authenticated/cli-auth/$uid.tsx`): pasting the
`authorize_url` while logged out drops the user on the dashboard after login,
not back on the approval screen, forcing them to re-paste the URL. This is a
generic limitation of `front/src/routes/_authenticated.tsx` — it affects every
protected route, not just this one, so it's worth fixing centrally rather than
special-casing the CLI-auth page.

## Implementation

- In `_authenticated.tsx`'s `beforeLoad`, capture `location.href` and pass it
  through the `redirect({ to: "/login", search: { redirect: ... } })` call
  (TanStack Router supports `search` params on `redirect()`).
- In `login.tsx`, after a successful login (both password and OAuth-callback
  paths), read the `redirect` search param and `navigate({ to: redirect })`
  instead of always `navigate({ to: "/" })`. Validate it's a same-origin,
  in-app path before using it (avoid open-redirect).
- Cover with a Playwright test: visit a protected deep link while logged out,
  log in, assert the final URL matches the original deep link.

## Status

Implemented in `front/src/routes/_authenticated.tsx` and
`front/src/routes/login.tsx`.

- `_authenticated.tsx`'s `beforeLoad` now redirects to
  `/login?redirect=<location.href>`; its component-level fallback effect
  (auth fails after the route already mounted) does the same via
  `useLocation()`.
- `login.tsx` captures the target ONCE at mount via
  `useState(() => getSafeRedirectTarget())` and reuses that same value in all
  three post-login `navigate()` calls (password login, password-change
  auto-login, and the reactive "already authenticated" effect).
- `getSafeRedirectTarget()` only accepts values starting with `/` and not
  `//`, so an external or protocol-relative `redirect` param is ignored in
  favor of `/` — verified manually with `?redirect=https://evil.example.com`.

**Two real bugs found and fixed while building this, both worth knowing about
if this code is touched again:**

1. First attempt included `location.href` as a dependency of the
   `_authenticated.tsx` fallback `useEffect`. Since the effect's own
   `navigate()` call changes `location.href`, this created a feedback loop:
   navigate → href changes → effect re-fires → navigate to a target now
   wrapping the previous (already-encoded) href → href changes again →
   ad infinitum. Observed as the `redirect` query param growing by one layer
   of percent-encoding per iteration. Fixed by dropping `location.href` from
   the dependency array (still read fresh inside the effect body) with an
   `eslint-disable-next-line` and a comment explaining why.
2. Second attempt called `getSafeRedirectTarget()` fresh at each of the three
   `navigate()` call sites in `login.tsx`. Because two different
   effects/handlers can each trigger a navigate for the same login (the
   explicit post-`login()` call and the reactive "already authenticated"
   effect), the second one to fire would read `window.location.search`
   *after* the first had already navigated away from `/login?redirect=...`,
   find no `redirect` param anymore, and silently clobber the correct
   destination with the `/` fallback. Fixed by computing the target once
   (lazy `useState` initializer) and reusing that single value everywhere.

Verified end-to-end in the browser (`make dev`): cleared the stored session,
opened a fresh CLI-authorization `authorize_url`, got redirected to
`/login?redirect=/cli-auth/<uid>`, signed in, and landed back on the exact
approval page (with the app shell, correct request name and code) rather than
the dashboard. Confirmed no console errors in a clean tab.

Still deferred: the Playwright regression test described above.

# Adopt eslint-plugin-react-hooks 7.1

## Goal

Unpin `eslint-plugin-react-hooks` in `front/package.json` (currently held at
`~7.0.1`) and fix the four violations its 7.1 rules report, so the frontend
lints clean on the current plugin.

## Why

The 2026-08-07 security dependency sweep bumped every frontend dependency. The
jump from `eslint-plugin-react-hooks` 7.0.1 to 7.1.1 turns on two new rules —
`react-hooks/set-state-in-effect` and the refs-during-render check — which flag
four pre-existing components. None of them is a security issue and none is
related to that sweep, so the plugin was pinned back to `~7.0.1` rather than
refactoring shared UI under a security change. That pin is the debt.

No GitHub issue exists for this; file one if it is picked up as its own piece
of work.

## Implementation

1. `cd front && bun add -d eslint-plugin-react-hooks@^7.1.1`.
2. `bun run lint` — expect four errors:
   - `src/contexts/BreadcrumbContext.tsx:139` — "Cannot access refs during
     render". The ref read needs to move into an effect or a callback.
   - `src/hooks/use-mobile.tsx:16` — `setState` called synchronously inside the
     media-query effect. Seed the state from a lazy `useState` initializer and
     leave the effect for subsequent changes.
   - `src/routes/login.tsx:146` — the OAuth-error effect calls `setError`
     synchronously. Derive the message during render from the URL instead.
   - `src/routes/login.tsx:153` — the demo-mode credential prefill calls
     `setUsername`/`setPassword` in an effect. Seed both `useState` calls from
     the run mode instead, once `versionInfo` is known.
3. Behaviour must not change: `make test-e2e` covers the login page, and the
   demo prefill is asserted by the `demo-credentials-hint` path.

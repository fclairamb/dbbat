# Guard E2E specs against absolute `page.goto()` paths

## Goal

Make it impossible for an E2E spec to silently navigate outside the app's
`/app/` base and assert against the Go server's bare 404 page.

## Why

`playwright.config.ts` sets `baseURL: "http://localhost:8080/app/"`. Playwright
resolves a **relative** path (`page.goto("api-keys")`) under that base, but an
**absolute** one (`page.goto("/api-keys")`) drops the `/app/` segment and lands
on `http://localhost:8080/api-keys` — which the Go router answers with a plain
`404 page not found`.

The page then contains none of the app, so every locator simply times out. The
failure reads as "the feature is broken" (a missing badge, a missing button),
which is the most expensive possible way to be told you typed a slash: the
`api-keys.spec.ts` added with the Oracle-capability column burned a full CI
round-trip on exactly this, and the symptom pointed at the React page rather
than at the URL.

Every other spec already writes it relatively (`goto("grants")`,
`goto("users")`) or spells the base out (`goto("/app/users")`), so the
convention exists — it is just unenforced.

## Implementation

Cheapest first; one of these is enough.

- An ESLint rule scoped to `front/e2e/**` and `front/showcase/**` — a
  `no-restricted-syntax` selector matching `page.goto("/…")` where the argument
  is a string literal starting with `/` and not with `/app/`, with a message
  naming the base-URL trap. `front/eslint.config.js` already has per-directory
  overrides to hang it off.
- Or, at runtime, a fixture-level guard in `front/e2e/fixtures.ts`: wrap
  `page.goto` and throw on such a path, so the spec fails with the real reason
  instead of a locator timeout.
- Optionally assert in the fixture that the landed page is the SPA (e.g. the
  app shell / nav is present) after `goto`, which catches the same class of
  mistake for paths that are typo'd rather than absolute.

Key files: `front/playwright.config.ts` (baseURL), `front/e2e/fixtures.ts`,
`front/eslint.config.js`.

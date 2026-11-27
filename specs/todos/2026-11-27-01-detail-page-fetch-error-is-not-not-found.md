# A failed detail fetch reads as "not found"

## Goal

When a detail or edit page cannot load its record because the request failed (429, 500,
network error), say so and offer a retry. Keep "not found" for a 404.

## Why

`connections/$uid` and `queries/$uid` render `EntityNotFound` whenever `useConnection` /
`useQueryDetails` return no data. Those hooks throw on any error, so a rate-limited
request (seen on 2026-11-27 while screenshotting every page in a row: `GET
/connections/{uid}` answered 429) shows "Connection not found" for a connection that
exists. The edit pages (`users/$uid`, `user-groups/$uid`, `server-groups/$uid`,
`grant-definitions/$uid`, `settings/parameters/$key`) find their record in a list query and
have the same blind spot when the list fails. Telling someone a record does not exist when it
does sends them looking for a deletion that never happened.

## Implementation

- Have the detail hooks keep the HTTP status on the thrown error (or return `null` on 404
  and throw otherwise), so a page can tell "absent" from "failed".
- Add a `LoadError` state next to `EntityNotFound` in `src/components/shared/FormPage.tsx`:
  the error message, a Retry button calling the query's `refetch`, and the same
  `← Parent` link. Document it in the Feedback section of `/design-reference`.
- Use it in every page that renders `EntityNotFound` today (grep for `EntityNotFound`).
- E2E: route a detail fetch to a 500 with `page.route` and assert the error state, not
  `entity-not-found`.

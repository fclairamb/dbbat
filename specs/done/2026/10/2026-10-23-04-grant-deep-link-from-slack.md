# Slack notices about one grant link to the whole grants list

## Goal

The byte soft-limit notice (`internal/notify/grant_bytes.go`) should link to the
grant it is about, not to `/app/grants`.

## Why

There is no page for a single grant in the front (`front/src/routes/_authenticated/grants/`
only has `index.tsx`), so the notice names the grant uid in its text and links to
the list. A reader then has to find the row by hand. Spec
`2026-10-23-02-grant-byte-soft-limit-slack-notifications` asked for "a link to the
grant in the UI"; this is the gap left.

## Implementation

- Either a `grants/$uid.tsx` detail route (usage meters, definition, connections
  under the grant), or a `?uid=` search param on the grants list that filters or
  highlights the row (`validateSearch` on the route, `data-testid` on the row).
- Point `grantBytesText` at it: `<publicURL>/app/grants/<uid>` or
  `<publicURL>/app/grants?uid=<uid>`. Update `TestNotifyGrantBytes_RepliesInRequestThread`.
- Extend `front/e2e/grants.spec.ts` to open the link and see the grant.

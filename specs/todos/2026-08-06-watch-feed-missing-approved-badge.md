# The live watch feed loses the approval outcome once a hold resolves

## Goal

After an operator approves (or denies) a held statement, the corresponding row
in the connection watch panel's **Live queries** feed should carry its outcome
badge — "Approved" / "Denied" / "Abandoned" — and the resolver's name.
Currently it renders with no badge at all.

## Why

Found while recording the approval video (`front/showcase/approval-video.spec.ts`).
The natural assertion for "the hold was released" was
`getByTestId("approval-status-approved")`; it never appears, and the spec had
to fall back on asserting that the *held* card disappeared.

From an operator's point of view this is a real gap, not just a test
inconvenience: the panel is the four-eyes surface, and once a hold resolves the
feed silently forgets that the statement was ever gated. Someone scrolling the
feed cannot tell an approved `UPDATE` from one that was never held. The
`ApprovalStatusBadge` component already handles all four states, and the feed
already renders "by <resolver>" when `resolvedBy` is set — the data just is not
reaching it.

## Implementation

- `front/src/components/shared/ConnectionWatchPanel.tsx`:
  - `toFeedItem` (~line 100) maps `d.approval_status` and `d.resolved_by` off
    the stream event. The feed renders the badge at ~line 312 only when
    `item.approvalStatus` is set.
  - Check whether the resolve event published on `connection/<uid>/queries`
    actually carries `approval_status` (and `resolved_by`). Likely it does not,
    and the second event for the same query UID overwrites the first feed entry
    with a status-less one.
- Backend side: `internal/approval/` + wherever the query-resolved event is
  published (`internal/events/`). Either include `approval_status`/`resolved_by`
  on the resolve event, or have the panel merge events by query UID instead of
  replacing, keeping the last non-empty status.
- Merging by UID in the panel is probably the better fix regardless: the feed
  currently treats each event as an independent row, which is also why a query
  can appear twice.
- Add e2e coverage in `front/e2e/approvals.spec.ts` if the fix is client-side
  and can be exercised with a synthetic stream event; otherwise cover it in the
  Go approval tests and restore the badge assertion in
  `front/showcase/approval-video.spec.ts`.

No GitHub issue yet — one should be filed.

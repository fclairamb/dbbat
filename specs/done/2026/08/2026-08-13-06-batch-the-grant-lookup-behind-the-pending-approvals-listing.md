# Batch the grant lookup behind the pending-approvals listing

## Goal

Stop `GET /api/v1/queries/pending` from doing one connection lookup plus one
grant lookup **per held query** when the caller is a non-admin.

## Why

The server-approver half of that listing is now batched: it resolves the
caller's user groups once and every distinct database's approver groups in two
queries (`store.ResolveServerApproverGroupsByServers`), then decides every row
and computes every hat against that one map.

What is still per-row is step 2 of the hold chain — the *definition's*
`approver_user_group_uids`, which wins outright when non-empty.
`mayApproveQueryWith` reaches it through `Server.resolveApprovalGrant`, which
per query does `GetConnectionByUID` and then either `GetGrantByUID` or
`GetActiveGrant`. So a page of N holds still costs ~2N round trips, and the
visibility gate and the hat share only the prefetch, not that lookup.

It is much less of a fan-out than what was removed (the same *connection* often
recurs across a page — one session parking several statements — and so does the
grant behind it), and it is on the same page a delegated approver loads often.

## Implementation

- Add a batched `Store.GetConnectionsByUIDs(ctx, uids) map[uuid.UUID]*Connection`
  and a batched `Store.GetGrantsByUIDs(ctx, uids) map[uuid.UUID]*Grant`, both in
  the one-query-per-call shape `ListServerGroupMemberUIDsByGroups` and
  `ResolveServerApproverGroupsByServers` already use.
- Extend `listingApprovers` (`internal/api/approvals.go`) with the resolved
  grant per query uid, filled by `prefetchApprovers`' caller in
  `handleListPendingApprovals`.
- Keep `resolveApprovalGrant` as the single implementation of *which* grant
  governs a hold — the batched path must feed it the rows it would have fetched,
  not re-derive the choice. The stamped `connections.grant_uid` beating the
  legacy `GetActiveGrant` fallback is an authorization rule, and a second
  implementation of it is exactly the drift the single function exists to
  prevent (same reasoning as the approver chain).
- The legacy `GetActiveGrant(user, database)` branch is per (user, database) and
  cannot be keyed off a uid list; either batch it by pair or leave that branch
  per-row, since it only fires for connections predating `grant_uid`.
- Cover it with a query-count assertion, like
  `TestResolveServerApproverGroupsByServers` does with `queryCountHook`, plus a
  batched-vs-per-row equivalence check in the shape of
  `TestListPendingApprovals_BatchedMatchesPerRow`.

No GitHub issue filed yet — one should be.

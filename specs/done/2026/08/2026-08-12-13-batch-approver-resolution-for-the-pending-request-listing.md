# Batch approver resolution for the non-admin grant-request listing

## Goal

Stop `GET /api/v1/grant-requests` from doing one server lookup plus one
server-group lookup **per pending request** when the caller is a non-admin
access approver.

## Why

`Server.listGrantRequestsForNonAdmin` (`internal/api/grant_requests.go`) walks
the whole pending set and calls `mayDecideGrantRequest` on each row, which calls
`store.MayApproveForServer` → `store.ResolveServerApproverGroups`, which reads
the server row *and* its server groups. So a listing costs O(pending) round
trips, and every one of them re-reads the same handful of servers.

It was written that way on purpose — the chain lives in exactly one function so
the two decision paths cannot drift, and a SQL-shaped second implementation is
precisely the drift that would be an authorization bug. The pending set is also
small by nature. But it is still a per-row fan-out on a page a delegated
approver loads often, and `approverHatForRequest` repeats it a second time for
the same rows to render the "you decide as" badge.

Same shape exists on `handleListPendingApprovals`, which calls
`mayViewQuery` **and** `approverHatForQuery` per held query.

## Implementation

- Add a batched resolver next to `ResolveServerApproverGroups` in
  `internal/store/server_approvers.go`, e.g.
  `ResolveServerApproverGroupsByServers(ctx, serverUIDs []uuid.UUID, kind)`
  returning `map[uuid.UUID][]uuid.UUID`. One query for the servers, one for the
  group memberships + group lists — the shape
  `ListServerGroupMemberUIDsByGroups` already uses for the definition listing.
- Keep the single-server function as the thin wrapper over it so there is still
  one chain, not two.
- Have the two listing handlers resolve once for the distinct set of
  `database_id`s, then decide each row against the map. That also lets the hat
  computation reuse the same map instead of re-resolving.
- Cover it with a query-count assertion, like
  `TestListServerGroupMemberUIDsByGroups` does with `queryCountHook`: the point
  of the change is the count, so the test has to be about the count.

No GitHub issue filed yet — one should be.

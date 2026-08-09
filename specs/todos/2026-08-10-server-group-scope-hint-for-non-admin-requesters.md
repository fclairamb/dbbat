---
model: sonnet
effort: medium
---

# Non-admin requesters get no server-group narrowing in the request dialog

**No GitHub issue filed yet — one should be.**

## Goal

Let a non-admin filing a grant request see which databases a definition
actually covers, instead of being offered the whole fleet and finding out via
a 403.

## Why

Grant definitions now scope on **server groups**
(`GrantDefinition.server_group_uids`), and resolving that scope to a list of
databases requires the group's membership. `/api/v1/server-groups` is
admin-gated — membership is access-relevant, so that gate is right — which
leaves the request dialog in
[front/src/routes/\_authenticated/grant-requests/index.tsx](front/src/routes/_authenticated/grant-requests/index.tsx)
unable to narrow its database picker for exactly the people who use it most.

Today it degrades honestly: `useServerGroups({ enabled: canManageServerGroups(...) })`
returns nothing for a non-admin, so the picker offers every database and the
server answers 403 on an out-of-scope pick. Correct, but a worse experience
than the pre-server-groups behaviour, which narrowed for everyone because the
scope was a plain list of database uids on the definition.

## Implementation

Two candidate approaches; pick one at review time.

1. **Resolve the scope server-side on the definition response.** Add a
   read-only `scoped_database_uids` to the `GrantDefinition` schema, filled by
   `handleListGrantDefinitions` / `handleGetGrantDefinition` from
   `ListServerGroupMemberUIDs` for the definition's groups. It leaks no group
   names or memberships — only "these databases are in scope", which the
   requester can already discover by trying. One batched query per listing,
   not one per definition.

2. **A narrow, non-admin-readable endpoint**, e.g.
   `GET /api/v1/grant-definitions/{uid}/databases`, returning the same list.
   More round-trips, but keeps the definition schema unchanged.

Option 1 is probably right: the request dialog already fetches definitions,
and the field is exactly what it needs.

Whichever lands, restore the narrowing in the request dialog and drop the
`enabled:` gate comment there. `internal/api/grant_requests.go`'s
`enforceRequestScope` stays the control — narrowing is a convenience, never
the gate.

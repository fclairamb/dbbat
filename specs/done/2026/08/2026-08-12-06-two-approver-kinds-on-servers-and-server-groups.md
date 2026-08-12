# Two approver kinds on servers and server groups (access vs query), as a fallback chain

Idea credit: Mohammed.

## Goal

Attach two distinct approver lists (user groups) to each database server, with the
server group as fallback:

- **Access approver** ("connection approver" in the original phrasing — but nobody
  approves a *connection*; what gets approved is a grant request): the ops group
  allowed to approve grant requests targeting this server. Applies to every
  non-`auto_approve` definition.
- **Query approver**: the lead-ops group allowed to release approval holds
  (pattern-triggered four-eyes) on statements against this server.

## Why

Two real gaps today:

1. **Grant-request approval is admin-only** (`handleApproveGrantRequest`,
   `internal/api/grant_requests.go`). There is no way to delegate "ops may approve
   access requests to their own servers" without handing out full admin. This half
   of the idea is needed regardless of where the lists live.
2. **Query-hold approvers live only on the grant definition**
   (`approver_user_group_uids`). Who guards a database is a property of the
   database, not of the policy shape — so today, giving prod-finance and staging
   different approvers forces cloning definitions per server group. Approver data
   leaks into policy shape and multiplies definitions.

The two-role split itself matches reality: a grant request is an async policy
decision; a hold blocks a live wire-protocol connection and needs the fastest
competent responder. Different audiences (ops vs lead ops), different latency.

## Design decisions (proposed)

- **Layer, don't replace.** The definition's `approver_user_group_uids` stays and
  keeps winning when set — it is an explicit policy choice, and its stamped-grant
  resolution (`resolveApprovalGrant`) was hard-won. Resolution chain for a query
  hold: definition approvers (if non-empty) → server's query approvers → union of
  the server's groups' query approvers → admins (fail closed, as today). For a
  grant request: server's access approvers → server groups' → admins. Empty
  everywhere = admin-only, i.e. exactly today's behaviour — fully backward
  compatible.
- **Live, not snapshotted.** Approver lists on servers/groups are operational
  data, resolved at decision time — the second deliberate exception to the
  immutable-versioning rule, same rationale as live server-group membership
  (a departed lead's replacement must be effective immediately). Document it as
  such next to the existing exception.
- **Union across multiple server groups**, matching how multiple approver groups
  on a definition already union. Self-approval stays rejected everywhere.
- **No hierarchy between the two roles.** Query approvers do not implicitly
  inherit access-approval rights or vice versa; an org wanting overlap lists the
  same user group in both.

## Implementation

- Migration: `access_approver_user_group_uids` + `query_approver_user_group_uids`
  (uuid arrays, default `{}`) on `database_servers` and `server_groups`.
- `internal/api/grant_requests.go`: replace the admin-only gate on
  approve/deny with admin-or-resolved-access-approver; Slack notification fan-out
  (`AdminSlackIDs`) and the deep-link routing must follow, as must
  `isApproverSomewhere` (`internal/api/stream.go`) so the pending-requests badge
  reaches the new approvers.
- `internal/api/approvals.go` `mayApproveQuery`: on empty
  `grant.ApproverUserGroupUIDs()`, fall through to the server/group chain instead
  of returning false.
- Slack interactions (`slack_interactions.go`, socket mode): the Approve/Deny
  click authorization must use the same resolution, not `IsAdmin`.
- UI: server + server-group edit forms grow the two pickers; grant-request and
  hold views show *why* the current user may resolve (which hat they wear).
- Tests: fallback precedence, multi-group union, fail-closed empty case,
  self-approval still refused when the requester is also an approver.

## Documentation

Ship the doc updates in the same PR:

- `docs/approvals.md`: the hold-resolution chain gains the two server-level
  fallback steps — spell out the full precedence (definition → server → server
  groups → admins) and that resolution is live, at decision time.
- `website/docs/features/grant-requests.md`: approval is no longer admin-only;
  describe access approvers and who gets the Slack notification.
- `website/docs/features/access-control.md`: introduce the two approver kinds on
  servers and server groups, the fallback chain, and the union rule across
  groups.
- `CLAUDE.md` (Access Control section): document the two lists, and name the
  live-resolution behaviour as the second deliberate exception to immutable
  versioning, next to the existing server-group-membership one.
- `internal/api/openapi.yml`: the new fields on the server and server-group
  endpoints, with the fallback semantics in their descriptions.

No GitHub issue filed yet — one should be.

# MCP `revoke_grant` tool

## Goal

Let an AI agent drop its owner's grant over MCP once its work is done, without
falling back to the REST call.

## Why

#436 made `DELETE /api/v1/grants/{uid}` callable by a grant's own holder,
including through an API key, so an agent can already revoke over REST. The
spec listed an MCP tool as optional and in scope only if small. It was not
done there: the revoke path (ownership check, live-session signal,
`grant.revoked` audit entry with `self`, the Slack thread reply) lives in
`internal/api/grants.go`, and duplicating it in `internal/mcp` would let the
two drift. `list_databases` also does not expose a grant uid today, so the
agent has nothing to pass.

## Implementation

- Move the body of `handleRevokeGrant` after the authorization check into a
  reusable function (store-level or a small service injected into
  `mcp.Deps`), so REST and MCP share one path.
- Add `grant_uid` to `DatabaseInfo` in `internal/mcp/tools.go`.
- Register `revoke_grant` (input: `grant_uid`), allowed only on a grant owned
  by the caller (an admin key may still be restricted to its own, to keep the
  agent surface small). Destructive annotation.
- Tests in `internal/mcp`: own grant revoked, another user's refused, unknown
  uid refused. Document in `docs/mcp.md`.

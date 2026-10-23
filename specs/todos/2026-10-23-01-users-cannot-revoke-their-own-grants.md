---
model: opus
effort: medium
---

# Users cannot revoke their own grants

GitHub Issue: https://github.com/fclairamb/dbbat/issues/436

## Source

Filed by stonalbot on behalf of Florent (Stonal, from Slack), 2026-10-23. A user
finished a measurement holding four `all-ro-30d` prod grants and had to ask an
admin in chat to revoke them one by one; otherwise read access to prod would have
stayed open for ~30 more days. The ask: let a user revoke **their own** grants,
the way they can already revoke their own API keys, with a "Revoke" button in the
UI and optionally a CLI/MCP path for agents.

The report is right on every point it makes about the code (checked below). It
misses one existing defect on the same handler: revoking an already-revoked grant
answers 500.

## Problem

- `internal/api/server.go:418` mounts `grants.DELETE("/:uid", s.requireAdmin(),
  s.handleRevokeGrant)`, so a connector gets 403 before the handler runs.
- `handleRevokeGrant` (`internal/api/grants.go:210`) revokes first and loads the
  grant *afterwards*, only to fill the audit event's `UserID`
  (`grants.go:232`). There is no ownership check to relax, because there is no
  pre-load.
- `Store.RevokeGrant` (`internal/store/grants.go:547`) returns
  `ErrGrantAlreadyRevoked` both when the grant is already revoked **and** when the
  uid does not exist (0 rows affected), and the handler maps every error to
  `writeInternalError` (`grants.go:219`), so a double-click or an unknown uid is a
  500.
- The API-key precedent to copy is `handleRevokeAPIKey` (`internal/api/keys.go:182`):
  load, 404 if absent, `!IsAdmin() && owner != caller` → 403, then act.
- UI: `front/src/routes/_authenticated/grants/index.tsx:320` disables the revoke
  action through `getDisabledReason("revoke-grant", user?.roles)`, whose message is
  "Only administrators can revoke grants" (`front/src/lib/permissions.ts:77`). The
  button already exists; it is gated on role, not on ownership.
- OpenAPI: `internal/api/openapi.yml:1329` documents the DELETE as "Requires admin
  role."
- Live sessions are already handled on every replica: the in-process
  `Revocations().Revoke(uid)` signal plus the `revoked_at` arm of the termination
  poll (CLAUDE.md, "The poll has a second arm"). Nothing to add there.

## Why a spec

The classification row that fired: *correctness hinges on authorization scope*.
It widens who may call a mutating endpoint, so the ownership check, the 403/404
split and the role matrix (admin, viewer, connector, API key) deserve the
`/implement-todos` audit rather than an on-the-spot fix. It also touches the
OpenAPI description and the front.

## Proposal

1. **API** (`internal/api/server.go`, `internal/api/grants.go`):
   - Drop `s.requireAdmin()` from `DELETE /grants/:uid`.
   - In `handleRevokeGrant`: `GetGrantByUID` first → 404 if absent; then
     `!currentUser.IsAdmin() && grant.UserID != currentUser.UID` → 403
     (`ErrCodeForbidden`). A **viewer** is not an admin: it may revoke its own
     grants only, like a connector.
   - Map `store.ErrGrantAlreadyRevoked` to **409** (already revoked), not 500.
     Since the grant was loaded first, the "uid does not exist" branch is now the
     404 above.
   - Keep `Revocations().Revoke(uid)` and the `grant.revoked` audit entry,
     reusing the loaded grant for `UserID`. Add `"self": true/false` to the
     details so the audit page can tell the two apart without comparing uids.
   - API-key callers go through the same check: a key acts as its owner, so an
     agent can drop its owner's grant. That is the point of the CLI/MCP item.
2. **OpenAPI** (`internal/api/openapi.yml` around line 1325): description "Admins
   may revoke any grant; other users only their own", plus `403`, `404`, `409`
   responses.
3. **Front**:
   - `front/src/lib/permissions.ts`: the revoke action is enabled for an admin or
     for the grant's owner. This needs the grant in the check, not only the
     roles; add a small helper rather than overloading `getDisabledReason`.
   - `grants/index.tsx`: show the enabled button on the caller's own **active**
     grants, with the same confirmation dialog admins get. Keep the
     `data-testid` the button already has (add one if missing).
4. **MCP** (optional, in scope only if it stays small): a `revoke_grant` tool on
   `internal/mcp/tools.go` that revokes a grant owned by the key's user, through
   the same store call and audit path. Skip the CLI: dbbat has no client CLI for
   grants today.
5. **Tests** (`internal/api/grants_test.go`): connector revokes own grant → 200 and
   `revoked_at` set; connector revokes another user's → 403 and grant untouched;
   viewer on another's → 403; admin on any → 200; unknown uid → 404; second revoke
   → 409; API-key-authenticated connector on own grant → 200. One Playwright case
   in the grants e2e spec: a connector sees an enabled Revoke button on its own
   grant and revokes it.
6. **Docs**: `website/docs/features/access-control.md` gets one sentence saying a
   user can revoke their own grant.

## Open questions

- **Should a self-revocation post in the grant request's Slack thread?**
  Recommended: yes, a one-line reply ("@user revoked this grant themselves") via
  the existing `PostThreadReply`, only when the grant came from a request with a
  thread. It closes the loop for the approver who granted it. Trade-off: one more
  message per revoke; skip it if the channel is already noisy.

## Closing the issue

This spec closes #436. The implementing PR body **must** carry a `Closes #436`
line so the squash-merge closes it. When the spec is archived to `specs/done/`,
verify the issue is closed and, if the merge did not close it, close it by hand:

    gh issue close 436 --comment "Implemented by <PR or commit>; spec: <archived spec path>"

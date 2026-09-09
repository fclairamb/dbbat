# Report the blast radius when a server joins a server group

## Goal

Make `PUT /api/v1/server-groups/{uid}/members/{server_uid}` tell the caller what it
just widened: how many **live** grants are bound to the group, and therefore how
many people gained access to the newly added database the instant the call
returned.

Today the response is `{"message":"member added"}` and nothing else.

## Why

Server-group membership is live and never snapshotted — one of the two deliberate
exceptions to the immutable-versioning rule. Adding a server to a group extends
every grant bound to that group, sessions already running included.

The admin UI warns about this at the point of edit. The REST API does not, and the
API is what automation and CLI callers use. Encountered live on 2026-09-09:
adding `abyla_vendee` to `Abyla R/W 3d servers` (17 → 18 members) simultaneously
extended a live `Abyla R/W 3d` grant to cover the new schema, in R/W, with no
separate approval. The operator had to reconstruct that consequence by hand —
list the group, list grants, filter to the ones bound to the group and still
unexpired — before deciding whether the call was safe.

An authorization side effect that only one of two front doors announces is a
place where the safe path depends on which door you used.

## Implementation

- `internal/api/server_groups.go`, the `addServerGroupMember` handler: after the
  membership write, count the grants bound to this group that are live at
  `time.Now()` (not revoked, `starts_at <= now < expires_at`) and return them in
  the response body — a count plus the distinct usernames is enough. A new
  `store` helper next to the existing group queries (`internal/store/`) keeps the
  predicate in one place; reuse whatever `GetActiveGrant` already uses for
  liveness rather than re-spelling it in SQL, so the two never drift.
- Extend `MessageResponse` for this route, or introduce a small
  `ServerGroupMemberAddedResponse` in `internal/api/openapi.yml` (`/server-groups/{uid}/members/{server_uid}` put)
  with `message`, `live_grants_widened`, `users`. The description there already
  says "Takes effect immediately for every live grant bound to the group" — the
  response should now quantify it.
- Same treatment for the DELETE half is *not* symmetric and should be skipped:
  removing a member narrows, which is not a surprise worth a payload.
- Write an `audit_log` entry for the membership change if one is not already
  emitted, carrying the group uid, server uid and the widened-grant count —
  the current trail records the server creation but not the group edit that gave
  it its blast radius.
- Test in `internal/api/server_groups_test.go`: a group with one live grant and
  one expired grant reports exactly 1.

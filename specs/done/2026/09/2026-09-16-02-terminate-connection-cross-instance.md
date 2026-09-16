---
model: opus
effort: high
---

# An admin cannot end a live proxied session, and a grant revoked on one replica does not reach sessions on another

## Problem

"Tu as la main pour tuer ces requêtes ?" was asked in the incident thread of
2026-09-15. Through dbbat the honest answer is no. The connections API is
read-only apart from the capture (`internal/api/server.go:457-463`: `GET`,
`GET /:uid`, `GET|DELETE /:uid/dump`). The only lever is revoking the whole
grant (`DELETE /grants/:uid`, `internal/api/grants.go:210`), which is both too
wide (every session of that user on that database) and, it turns out, only
local.

The revocation path signals sessions through `cache.RevocationRegistry`
(`internal/store/store.go:283`), an in-process map from grant uid to the
sessions that registered on this process (`internal/cache/revocation.go:66`).
Nothing store-backed carries the signal to another replica: `GetActiveGrant`
runs only at connect, and `LimitGuard.Check` compares `expires_at`, never
`revoked_at`. In a deployment with two dbbat replicas behind one load balancer
(the Stonal shape), revoking a grant from the UI ends the sessions that happen
to live on the replica that served the API call and leaves the others running
until they expire. That is a correctness bug in shipped code, and the same
mechanism this feature needs.

## Proposal

### Endpoint

```
POST /api/v1/connections/{uid}/terminate     admin only
body: { "reason": "optional free text, shown to nobody but the audit log" }
202  the session is live and the termination was requested
409  the connection is already closed
404  unknown uid
```

`POST .../terminate` rather than `DELETE /connections/{uid}`: the latter would
read as deleting the ledger row, which retention owns and which the audit
chain protects.

### One shared live-session registry

Every protocol keeps its own registry keyed by its own protocol handle (PG by
cancel key, `internal/proxy/postgresql/approval.go:35`; MySQL by connection id,
`internal/proxy/mysql/kill.go:20`). None is keyed by the connection uid, which
is the only identifier an admin has. Add one, in `internal/cache` next to the
revocation registry:

```go
type SessionRegistry struct{ ... }
type SessionHandle struct{ terminated atomic.Bool; reason atomic.Pointer[string] }

func (r *SessionRegistry) Register(connUID uuid.UUID) *SessionHandle
func (r *SessionRegistry) Deregister(connUID uuid.UUID, h *SessionHandle)
func (r *SessionRegistry) Terminate(connUID uuid.UUID, reason string) bool // false when not on this process
```

Each protocol registers right after `CreateConnection` and deregisters in its
cleanup, at the five places `Revocations().Register` is called today
(`internal/proxy/postgresql/session.go:398`, `oracle/session.go:1514`,
`mongodb/auth.go:128`, `mysql/auth.go:223`, `mssql/session.go:365`). The
handle's flag is attached to the session's `LimitGuard` the way the revocation
flag is (`WithRevocation`, `internal/proxy/shared/limits.go:111`), so
`Check()` returns a new `ErrAdminTerminated` and the existing
`onLimitViolation` teardown runs: upstream cancel first, then both sockets
(the cancel helper is introduced by the statement-timeout spec, which is why
this one is queued after it).

### Crossing the instance boundary

The API replica that receives the request does not necessarily own the
session. `connections.run_id` says who does. Two additions:

1. **A request row.** `connections.terminate_requested_at timestamptz`,
   `terminate_requested_by uuid REFERENCES users`, `terminate_reason text`,
   written by the handler. Partial index on `(run_id) WHERE
   terminate_requested_at IS NOT NULL AND disconnected_at IS NULL`, so the
   poll below touches almost nothing.
2. **A per-process poller**, not per session. One goroutine per dbbat
   process, started next to the instance heartbeat (`store.HeartbeatInstance`,
   `internal/store/instances.go:146`), every 2s runs one query:

   ```sql
   SELECT uid, terminate_reason FROM connections
    WHERE run_id = $1 AND terminate_requested_at IS NOT NULL AND disconnected_at IS NULL
   UNION ALL
   SELECT c.uid, 'grant_revoked' FROM connections c JOIN grants g ON g.uid = c.grant_uid
    WHERE c.run_id = $1 AND c.disconnected_at IS NULL AND g.revoked_at IS NOT NULL
   ```

   and calls `SessionRegistry.Terminate` for each hit. The second arm is what
   fixes the revocation gap: it makes the in-process `RevocationRegistry`
   signal a fast path and the store the source of truth. When the API replica
   does own the session it also signals the registry directly, so the local
   case stays instant.

   `LISTEN/NOTIFY` would make the remote case instant too, at the cost of a
   dedicated connection per replica and reconnect handling; the 2s poll is the
   first version, and the query is cheap enough to leave running forever.

### Recording

The termination is recorded exactly like a statement-timeout kill (that spec
introduces the columns and events): `connections.termination_reason =
'admin_terminated'`, a `connection.terminated` audit entry carrying the
requesting user and their reason, the in-flight query row completed with
`session terminated by <admin>`, and a `terminated` connection stream event.
`GET /connections/{uid}` exposes `terminated_by` (user) alongside the reason.

### UI

On the connection detail page (`front/src/routes/_authenticated/connections/$uid.tsx`),
for a live connection and an admin viewer, a **Terminate session** button with
a confirmation dialog on the revoke-grant model
(`front/src/routes/_authenticated/grants/index.tsx:113`), an optional reason
field, and a note that the running statement is cancelled upstream. After the
call the page follows the stream and flips to closed with the reason and the
admin's name. Nothing on the list page in this version.

### Tests

- `cache.SessionRegistry` unit tests mirroring the revocation ones
  (`internal/cache/revocation_test.go`).
- API: 202 / 409 / 404 and the admin-only guard; the request columns are
  written.
- Store: the poller query returns a requested connection for its run only, and
  a connection whose grant was revoked.
- Integration (PostgreSQL suite): open a session, run `pg_sleep(30)`, call the
  endpoint from a *second* store handle with a different `run_id` so the
  in-process fast path cannot be what worked; the session drops within ~3s,
  `pg_stat_activity` no longer shows the backend, the reason and audit entry
  are there. Same shape for revocation: revoke the grant through the store
  only, observe the drop.
- Frontend e2e: the button is absent for viewers and for closed connections.

### Notes

- Terminating is not the same as revoking: the user can reconnect
  immediately under the same grant. The UI copy says so, and the dialog offers
  a link to revoke the grant instead.
- The reconcile that closes crash-orphaned connections
  (`internal/store/connections.go:268`) already writes `disconnected_at`; make
  it also stamp `termination_reason = 'instance_lost'` so the column has no
  unexplained NULLs on closed rows.

## Implementation Plan

Ordered, each step a commit. The statement-timeout spec landed first and already
built `store.Termination`, `CloseConnectionWithReason`, the
`connection.terminated` audit event, the `terminated` stream state and the
per-protocol `onLimitViolation` cancel-then-close teardown — every step below
reuses those rather than adding a parallel path.

1. **`cache.SessionRegistry`** (`internal/cache/session.go`) — keyed by
   connection uid, next to `RevocationRegistry`. `SessionHandle` carries
   `terminated atomic.Bool` + `atomic.Pointer[TerminationRequest]`
   (`{Reason, By, Detail}`), so the poller's two arms can signal different
   reasons through one mechanism. Unit tests mirroring `revocation_test.go`.
2. **`Store.Sessions()`** — expose the registry the way `Revocations()` is
   exposed, built in `New`.
3. **`ErrAdminTerminated` on `LimitGuard`** — a sentinel in
   `internal/proxy/shared/limits.go`, `WithTermination(*cache.SessionHandle)`
   following `WithRevocation`'s shape, checked first in `Check()`;
   `TerminationReasonFor`/`TerminationFor` map it, with the handle's request
   overriding the default reason and filling `By`/`Detail`.
   `store.Termination` gains `By`, and `Message()` renders
   `session terminated by <admin>`.
4. **Per-protocol registration** — at the five `Revocations().Register` sites
   (postgresql/session.go, oracle/session.go, mongodb/auth.go, mysql/auth.go,
   mssql/session.go) also `Sessions().Register(connectionUID)`, attach the
   handle to the guard, and `Deregister` next to the existing
   `Revocations().Deregister`. No new teardown: `onLimitViolation` already
   cancels upstream then closes both sockets.
5. **Migration + model** — `connections.terminate_requested_at`,
   `terminate_requested_by uuid REFERENCES users`, `terminate_reason text`,
   plus the partial index on `(run_id) WHERE terminate_requested_at IS NOT NULL
   AND disconnected_at IS NULL`. Model fields + projections.
6. **Store API** — `RequestConnectionTermination(ctx, uid, by, reason)`
   (guards on `disconnected_at IS NULL`, reports not-found / already-closed),
   and `PendingTerminations(ctx)` running the UNION ALL of the spec's two arms
   scoped to this run.
7. **The endpoint** — `POST /api/v1/connections/:uid/terminate`, admin only,
   202/404/409, writes the request row, signals the local registry immediately
   for the same-replica case, audits the request. OpenAPI + generated client.
8. **The poller** — a 2s tick in `heartbeat.go`'s existing loop (same goroutine,
   same panic guards), calling `PendingTerminations` and
   `Sessions().Terminate` per hit.
9. **Reconcile stamp** — `orphanCloseQuery` also sets
   `termination_reason = 'instance_lost'`; new
   `store.TerminationInstanceLost` constant added to the vocabulary.
10. **`terminated_by` on the detail response** — resolve
    `terminate_requested_by` to a username in `GET /connections/{uid}`.
11. **Frontend** — a "Terminate session" button + confirmation dialog on the
    connection detail page, admin-only and live-only, optional reason, a note
    that the running statement is cancelled upstream and that terminating is not
    revoking (with a link to the grant).
12. **Tests** — registry unit tests, API 202/409/404 + guard, store poller-query
    tests, the PostgreSQL integration test driven from a second store handle
    with a different run id (both arms), frontend e2e for button absence.

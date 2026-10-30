---
model: opus
effort: low
---

# MySQL: any session can cancel another user's statement parked on an approval hold

## Problem

Found by the security scan of 2026-10-30 (finding L11).

Severity: Low. Triggered by any authenticated MySQL user when approval holds are enabled.

A `KILL [QUERY|CONNECTION] <id>` is matched against dbbat's own session registry. When the id names a session with a statement parked on an approval hold, `releaseHoldForKill` resolves that hold as abandoned (`internal/proxy/mysql/kill.go:80-96`, `KillHeldQuery` at `internal/proxy/mysql/intercept.go:516-528`, called at `intercept.go:283`). Nobody checks that the session sending the `KILL` belongs to the same dbbat user as the session it names. The ids are small connection counters, so a user can cancel other users' pending statements before an approver sees them. This is the approval flow's equivalent of a denial of service. PostgreSQL's equivalent, `CancelRequest`, needs the target's secret key.

## Proposal

1. In `releaseHoldForKill`, release the hold only when the target session's user is the caller's user (`s.user.UID == target.user.UID`). Otherwise leave the hold alone (and still forward the `KILL` upstream, as today).
2. Log a refused cross-user release at WARN.

## Tests

- `internal/proxy/mysql/kill_test.go`: a `KILL QUERY <id>` from user A naming user B's held session leaves B's hold pending. This fails before the fix.
- Positive control: the same `KILL` from a second session of user B releases the hold as abandoned.

# SQL Server: `attentionAckDue` can go stale when a cancel loses inside the arm window

## Goal

Close a narrow window in the mssql proxy where an ATTENTION is swallowed but the
`DONE_ATTN` it owes is never consumed, leaving `attentionAckDue` set on a session
that is going to keep running.

## Why

Found by the completeness audit of
[2026-08-07-mssql-attention-during-approval-hold.md](../done/2026/08/2026-08-07-mssql-attention-during-approval-hold.md),
as an observation outside that spec's scope. The ATTENTION-cancels-a-hold feature
itself is correct; this is a leftover edge in its race handling.

`internal/proxy/mssql/intercept.go` has two paths that swallow an ATTENTION:

- **The normal path** sets `attentionAckDue = true` only *after* a successful
  `Registry.Resolve` (`intercept.go:462`). If the resolve loses to a human
  approver, the flag is never set and the ATTENTION travels upstream. Symmetric
  and correct.
- **The arm window** — the few instructions between `Register` and `OnPending` in
  `internal/proxy/shared/approval.go:416-421`, where the gate has parked the
  statement but not yet published its uid — sets `attentionAckDue = true`
  *before* it knows whether the deferred catch-up `Resolve` will win
  (`intercept.go:424-426`). If that catch-up loses (`session.go:183` →
  `resolveHoldCanceled` returns `false`), the ATTENTION has already been consumed
  and the flag stays `true` with nothing on the approved path to take it.

Consequence: the statement runs (correctly — a human approved it), but the
session carries a spurious owed-`DONE_ATTN`. The next refused statement on that
connection would be answered with a bare `DONE_ATTN` instead of its real error,
so the client sees "cancelled" where it should see the actual failure. It is a
wrong-error bug, not a security hole — nothing extra reaches the upstream.

The window is a few instructions wide and needs a human approving in exactly that
sliver, so this is very unlikely in practice. There is no test for "lose the race
*inside* the arm window" — the existing coverage
(`TestAttentionLosingToAnApproverTravelsUpstream`, `intercept_test.go:730`) only
exercises losing on the normal path.

## Implementation

Make the arm-window path set `attentionAckDue` on the same condition the normal
path uses — i.e. only once the catch-up `Resolve` has actually won. That means
deferring the flag rather than setting it optimistically at `intercept.go:424`,
and having the catch-up in `session.go:179-184` set it when `resolveHoldCanceled`
returns `true` and forward the ATTENTION upstream when it returns `false`.

Care is needed on the forwarding half: by the time the catch-up runs, the reader
goroutine has already consumed the ATTENTION packet, so "forward it upstream"
means re-emitting it rather than letting it flow.

A regression test wants a way to make the approver win deterministically inside
the arm window — probably a test hook on the gate that blocks between `Register`
and `OnPending` while the test resolves the hold, rather than trying to hit the
window by timing.

Files: `internal/proxy/mssql/intercept.go` (`cancelHeldStatement`,
`resolveHoldCanceled`, `takeAttentionAck`), `internal/proxy/mssql/session.go`
(`setHeldQuery` catch-up), `internal/proxy/shared/approval.go` (test hook),
`internal/proxy/mssql/intercept_test.go`.

No GitHub issue exists yet; per the batch decision of 2026-08-07 none is to be
filed automatically.

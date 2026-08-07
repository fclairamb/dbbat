# SQL Server: release an approval hold on an ATTENTION

## Goal

Let a SQL Server client cancel a statement that is parked on an approval hold,
the way a PostgreSQL `CancelRequest`, a MySQL `KILL QUERY` and a MongoDB
`killOperations` already do.

## Why

Stage 3 of the SQL Server proxy wired approval holds through the shared gate.
While a statement is parked, the session goroutine is blocked inside
`ApprovalGate.Hold`, and the client conn is parked
(`shared.WatchedConn.Park`) so a disconnect still ends the hold.

The gap: a parked conn *buffers* whatever the client sends and replays it in
stream order once the session resumes. A TDS `ATTENTION` (`0x06`) — the cancel a
driver sends when its `context.Context` is cancelled or its query timeout
fires — is therefore not seen until the hold has already resolved. What the
client experiences is:

- it sends ATTENTION and waits for the `DONE` with `DONE_ATTN` that never comes;
- once (if) a human approves, the statement runs upstream and *then* the queued
  ATTENTION cancels it there.

So a cancel is honoured eventually and in the right order — nothing is
corrupted — but the client hangs until it gives up and closes the socket, which
is the path that finally abandons the hold. Every other protocol ends the hold
immediately on its own cancel signal, and the approval UI shows "canceled by the
client" rather than "client is gone".

`docs/mssql.md` documents the behaviour today (section "Approval holds").

## Implementation

The obstacle is that `WatchedConn` deals in raw bytes and knows nothing about
TDS framing, deliberately: it sits *below* TLS so it never has to decrypt.

Two workable shapes:

1. **A TDS-aware peek in the mssql session.** Give `WatchedConn` an optional
   callback invoked on each captured chunk (still without consuming it), and
   have `internal/proxy/mssql` feed those bytes to a tiny header-only scanner
   that looks for a packet whose type byte is `packetTypeAttention`. On a match,
   resolve the held query as `store.ApprovalAbandoned` with reason "canceled by
   the client (ATTENTION)" — the same call `mysql.KillHeldQuery` makes via
   `s.server.approvalDeps.Registry.Resolve`. The session already keeps the held
   uid in `session.heldQueryUID` (`setHeldQuery`, wired as the gate's
   `OnPending`), and a `heldQuery()` accessor was removed in stage 3 for lack of
   a caller — restore it.
   - **Caveat**: on an encrypted client leg the watcher sees TLS records, not
     TDS packets, so the scan finds nothing. That is most sessions. This shape
     only helps plaintext links unless the park point moves above TLS, which
     would defeat the reason `WatchedConn` sits where it does.

2. **Park above TLS instead, for this protocol.** Wrap the session's
   `revertibleConn` rather than the raw socket, so the watcher reads decrypted
   TDS. This needs care: `revertibleConn` is an `io.ReadWriter`, not a
   `net.Conn`, and `Unpark` interrupts its blocked read with a read deadline —
   which a `tls.Conn` does support, but a deadline that fires mid-record leaves
   the TLS session unusable. Prototype before committing to it.

Either way, add an integration case: park a statement with an approval pattern,
cancel the `context.Context` the driver is using, and assert the hold resolves
as abandoned promptly rather than on disconnect.

Files: `internal/proxy/mssql/intercept.go` (`holdIfNeeded`),
`internal/proxy/mssql/session.go` (`setHeldQuery`),
`internal/proxy/shared/watch.go`, `internal/proxy/mssql/integration_test.go`.

No GitHub issue exists yet — one should be filed.

## Resolved open questions

**Should a GitHub issue be filed for this spec?**

Decision (2026-08-07, repository owner): **no.** Do not run `gh issue create`.
The spec file is the record.

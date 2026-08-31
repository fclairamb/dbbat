# Oracle: answer an OER instead of ending the session when a fragmented statement ends on a full-sized packet

## Goal

Remove the last case in which refusing an SDU-fragmented statement still costs
the client its connection: a message whose statement text is covered exactly by
a *full-sized* fragment, where dbbat cannot tell whether the client still owes
bytes of that same message.

## Why

`internal/proxy/oracle/reassembly.go` stops reading the moment the declared
statement length is covered, and never a byte past it — over-reading would
swallow the next message. Completeness is therefore inferred from the last
fragment being shorter than the first (the client fills each packet to the SDU
and the tail is what is left), which is the shape every recorded client
produces.

When the statement instead ends on a fragment that is itself SDU-sized, the
collector cannot rule out one more continuation. `statementFragments.complete`
is false, and `session.refusalWouldStrandFragments` then ends the session rather
than answering an OER, because answering one would let the residual continuation
reach the upstream alone — the desync the 2026-08-31 fix removed.

That is strictly better than the pre-fix behaviour (which tore the session down
for *every* >8KB refusal) and it is fail-closed, but it is still a session lost
on a knife-edge alignment, and the client sees an I/O error rather than the ORA
refusal.

## Implementation

Sketch: once the declared length is covered and the last fragment was full-sized,
do a bounded **peek** for one more packet instead of guessing — a short read
deadline (tens of milliseconds) on the client socket, since a client writes all
the fragments of one message back to back and is then parked waiting for the
reply. A timeout means the message really did end on the boundary, so
`complete` can be set and the ordinary OER path used; a packet that does arrive
is appended as a continuation.

Key files:

- `internal/proxy/oracle/reassembly.go` — `readStatementFragments`,
  `statementFragments.complete`
- `internal/proxy/oracle/session.go` — `refusalWouldStrandFragments`,
  `gateStatement`
- `internal/proxy/oracle/fragment_reassembly_test.go` — add a probe that writes a
  message whose statement ends exactly on a full-sized fragment, and assert the
  refusal comes back as an OER on a session that then answers the next call.

Non-goals: reading past the declared length unconditionally (it would swallow
the following message), and any change to the allow path, which already forwards
the residual continuation in order on the next relay iteration.

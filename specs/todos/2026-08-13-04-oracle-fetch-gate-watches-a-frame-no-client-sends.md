# The Oracle fetch gate watches a frame no client sends

**No GitHub issue filed yet — one should be.** (Automation must not run
`gh issue create`; see `specs/todos/2026-08-11-06-*.md`.)

## Goal

Either gate the fetch op Oracle clients actually send, or delete the gate that
watches the one they don't — so `handleOFETCH` stops being enforcement that
cannot fire.

## Why

Measured while fixing
`specs/done/.../2026-08-12-12-bundled-oci-client-refused-and-hung-under-a-restrictive-grant.md`.
A histogram of every client-side TTC op in `testdata/*.pcapng` says:

- every message-type `0x11` frame on the wire is `11/69` (close-cursors), plus
  the single `11/6b` that spec was about;
- every real fetch is message type `0x03`, function `0x05` (`TNS_FUNC_FETCH`) —
  15 of them in `dbeaver.pcapng` alone.

`TTCFuncOFETCH = 0x11` is the *piggyback message type*, not a fetch, and
`decodeOFETCH`'s layout (a big-endian cursor id at bytes 1..3) is a fiction for
every frame Oracle sends: bytes 1..3 are (function, sequence). The only frames
that ever reached `handleOFETCH` were piggybacks being misread — which is how
the bundled OCI client's first message became "a re-execution of cursor 27396".

That path is now closed (the dispatcher refuses nothing it cannot name), so
`handleOFETCH` is unreachable from `interceptClientMessage`. Its unit tests
still pass, and they document a real intent — "a fetch arriving with no query in
flight is a re-execution and is gated like a statement" — but nothing exercises
it on real traffic, and `docs/approvals.md` describes it as one of the three
frames the re-execution gate covers. Two of the three are real (the SQL-less
`OALL8`, the `03/0x4e|0x04` piggyback); this one is not.

## Implementation

Two honest options, and the choice needs a measurement first:

1. **Wire the gate to `03/05`.** Decode the real fetch frame (cursor id and
   fetch count are compressed ints after the header — pin the layout against
   `dbeaver.pcapng`, `python_thin.pcapng` and `sqlplus_cursor_reexec.pcapng`,
   all of which carry them), and route it through `handleOFETCH`. The existing
   `hasPendingQuery` early return is what keeps a continuation fetch off the
   gate, so the blast radius is "a fetch with nothing in flight" — which needs
   measuring on a live suite before it can be turned on, because refusing there
   on a false positive breaks ordinary read-only work.
2. **Delete it.** Drop `decodeOFETCH`, `handleOFETCH`, `buildOFETCH` and the
   tests resting on the synthetic frame, and correct `docs/approvals.md` and
   `docs/oracle.md` to name the two re-execution frames that are real.

Option 1 is the better outcome and option 2 is better than the status quo;
what must not survive is a documented gate that no traffic can reach.

Key files: `internal/proxy/oracle/session.go` (`interceptClientMessage`),
`internal/proxy/oracle/intercept.go` (`handleOFETCH`),
`internal/proxy/oracle/ttc_decode.go` (`decodeOFETCH`),
`internal/proxy/oracle/cursor_reexec_gate_test.go`, `docs/approvals.md`.

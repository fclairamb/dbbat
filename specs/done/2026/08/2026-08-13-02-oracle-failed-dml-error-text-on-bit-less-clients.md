# A failing statement on a client with no end-of-call bit still records no ORA error

## Goal

A statement that *fails* on a python-oracledb thin session (or any client whose
OERs carry `CallStatus 1–2` rather than the `0x010000` end-of-call bit) should
record its `ORA-NNNNN` text in `queries.error`, the way it already does for
go-ora sessions.

## Why

`2026-08-12-10` fixed the *success* half: `handleResponse` now completes a
pending query from a bit-less OER via `findPlausibleOERInResponse`, which is
what restored `rows_affected` and a truthful `duration_ms` for DML on those
clients.

The failure half is untouched, deliberately and for a reason worth keeping in
mind — the relaxed scan's anchors include "error code is 0 or ORA-01403",
inherited from cursor-id learning, where they mean *an OER reporting a real
failure assigns no cursor*. Loosening that anchor is not free: it is one of the
few things keeping a run of arbitrary bytes from being read as a diagnostic and
persisted as `Query.Error`, which is exactly the defect
`TestDecodeTTCResponse_LegacyErrorGate` and `shared.SanitizeQueryError` exist to
stop.

So today, on a bit-less client:

- the failing statement is closed by the *next* statement's `flushPendingQuery`
  → `completeQuery(nil, nil)`, i.e. logged as a success with no error text;
- `handleOERStatus` (standalone func `0x04`) would carry the error, but it still
  requires the bit, and correctly so — it is routed on byte 0 alone.

This has **not** been observed live; it is reasoned from the code and from the
fixtures. The first task is therefore to measure it, not to fix it.

## Implementation

1. **Measure first.** Drive a deliberately failing statement (`SELECT * FROM
   nope`, an `INSERT` violating a constraint) through the proxy from
   python-oracledb thin, with a session dump on, and look at what actually
   arrives: an embedded OER inside a Response, a standalone func `0x04` after a
   marker exchange, or the legacy layout. `make test-e2e-oracle` already has the
   harness; add a capture next to `testdata/python_thin_cursor_reexec.pcapng`.
   If the error arrives as a standalone `0x04` *with* the bit, there is nothing
   to fix and this todo closes with a test.
2. If it does arrive as a bit-less embedded OER, the anchor to relax is the
   error-code one — but only for the completion caller, never for cursor-id
   learning, and only outside a row stream. The safe shape is a second predicate
   on top of `findPlausibleOERInResponse` in `internal/proxy/oracle/ttc_oer.go`
   that accepts an error code **and** requires `extractORAMessage` to find a
   printable `ORA-`/`PLS-`/`TNS-` diagnostic in the tail — proof, the same
   standard `decodeTTCResponse` was held to.
3. Regression test alongside `oer_no_end_of_call_test.go`, and extend the
   which-path-requires-the-bit table in `docs/oracle.md`.

No GitHub issue filed (automation does not run `gh issue create` — see
`specs/todos/2026-08-11-06-*.md`); one should be filed by hand if this is
confirmed live.

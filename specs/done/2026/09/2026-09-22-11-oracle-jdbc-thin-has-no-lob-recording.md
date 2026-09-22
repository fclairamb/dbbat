---
model: opus
effort: low
---

# JDBC thin has no LOB recording, so its LOB fetches are unmeasured

## Goal

Record a LOB fetch from the JDBC thin driver and find out which of the thin
dialect's two readings it asks for — and whether its define block is one dbbat's
walk can read.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-09-oracle-a-thin-client-that-asks-for-lob-locators-loses-its-rows.md`.

That spec made the LOB reading a per-session decision taken off the client's own
define block, and pinned it against three recordings: go-ora with nothing
configured (inlines the bodies), go-ora with `lob fetch=post` (locators), and
python-oracledb thin with nothing configured (locators). JDBC thin is the third
major thin client and it has **no LOB recording at all** — the corpus has
`jdbc_thin_cursor_reexec.pcapng`, `jdbc_thin_midfetch_fail.pcapng` and
`jdbc_thin_refcursor.pcapng`, none of which selects a LOB.

That matters in one direction only, and it is the cheap one to be wrong in: a
session whose define block dbbat cannot walk reads **locators**, which is what
the server sends unless the client asked otherwise, so a JDBC thin session that
fetches locators (its documented default) is already right by construction.
The case that would be silently wrong is JDBC thin *prefetching* LOB data
(`oracle.jdbc.defaultLobPrefetchSize`, which modern drivers enable by default
with a non-zero size) in a define block shaped differently enough that
`execDefineLOBShape` does not read it. Those rows would be refused — the safe
failure, but a silent one.

## Implementation

1. Record it. `internal/proxy/oracle/capture_lob_test.go` is the harness and
   `goOraLOBQuery` the statement; the JDBC recordings in `testdata/` were made
   through the same relay, so follow whichever of `capture_reexec_test.go` /
   `capture_refcursor_test.go` drives the Java client.
2. Read the recording's client frames with `execDefineLOBShape` against
   `goOraLOBColumns` and say what it learns — inline, locator, or nothing.
3. Replay it with `replayCapturedRows` and assert the row, the way
   `TestPythonThinLOBFetchCapturesItsLocators` does.
4. If nothing is learned but the rows *are* inlined, the define walk in
   `internal/proxy/oracle/ttc_define.go` needs JDBC's entry layout — read it off
   the recording rather than off the driver's source, and keep the exact-fill and
   type-agreement validation.
5. Add the recording to the census in
   `TestDefineBlockIsNotFoundInFramesThatAreNotOne` and, if it carries a define,
   to the one in `TestOJDBC6ReexecDoesNotDisturbTheParsePath`.

## Files

- `internal/proxy/oracle/capture_lob_test.go`
- `internal/proxy/oracle/ttc_define.go`
- `internal/proxy/oracle/lob_dialects_test.go`, `ttc_define_test.go`
- `internal/proxy/oracle/testdata/jdbc_thin_lob.pcapng` (new)

---
model: opus
effort: medium
---

# A thin client that asks for LOB locators loses every row of the fetch

## Goal

Read the LOB policy a thin session negotiated — off the client's own execute,
which dbbat already sees — so a compressed-dialect row can be walked under the
shape the server actually sent, instead of under the one a thin client happens
to default to.

## Why

Found on 2026-09-22 while implementing
`2026-09-22-07-oracle-lob-framing-is-measured-on-one-dialect-only.md`.

That spec turned the LOB framing from two package constants into a per-dialect
reading, and recorded the same query on all three dialects to do it. The thin
recording came in **two** halves, and they do not have the same shape:

| go-ora DSN | What a LOB column carries | Recording |
|---|---|---|
| default (`lob fetch=inline`) | the LOB's own bytes as a CLR, then two compressed integers | `testdata/go_ora_lob.pcapng` |
| `lob fetch=post` | two CLRs — a one-byte length, then the 40-byte locator | `testdata/go_ora_lob_stream.pcapng` |

`readCompressedLOBColumn` reads the first, which is what a thin client defaults
to and therefore the overwhelmingly common case. The second walks off the end of
the column, `rowEndsAtMarker` refuses the row, and the fetch captures nothing —
which is exactly what it captured before any of the LOB work, so this is an
unfixed case rather than a regression. `TestThinStreamedLOBFetchIsRefusedRatherThanGuessed`
pins that.

The reason it was not simply implemented alongside the other is that **the two
are indistinguishable in the server's own frames**: the column records of the
two recordings are byte-identical, because the difference was asked for in the
*client's* execute options, not answered in the describe. Reading them both out
of the same bytes would mean offering the row two readings and keeping whichever
produced something — which is the failure mode every row walk in this package is
bounded against.

## Implementation

1. Find where the policy is asked for. `go_ora_lob_stream.pcapng` and
   `go_ora_lob.pcapng` are the same session but for the DSN, so diffing their
   **client** frames — the execute that carries `ociLOBQuery`'s text, and
   whatever option word sits beside its bind/define block — is where the bit
   lives. go-ora's `configurations.LobFetch` (`INLINE` vs `STREAM`) is what sets
   it, and `parameter_coder/lob.go` is the reader that keys on it, so the
   driver's own source says what to look for.
2. Learn it on the session, the way `oerShape` is learned: once, off the client's
   own frame, never sniffed out of the row bytes it would then be used to read.
   A session where nothing was learned keeps today's reading.
3. Branch `readCompressedLOBColumn` on it — CLR + two compressed integers for
   inline, CLR + CLR for streamed — and pin each against its recording, mirroring
   `TestThinLOBFetchCarriesTheContentInsteadOfALocator`.
4. Check what the streamed shape should *capture*. Inline captures the value
   because the value is in the packet; streamed carries only a handle, so it is
   `lobLocatorPlaceholder`'s case, the same as both OCI dialects.

If step 1 finds no per-session signal dbbat can see, say so and stop: refusing
the row is the correct answer to a shape that cannot be told apart, and this
spec is then closed by that finding rather than by code.

## Files

- `internal/proxy/oracle/ttc_decode.go` — `readCompressedLOBColumn`, `readRowColumn`
- `internal/proxy/oracle/lob_dialects_test.go` — `TestThinStreamedLOBFetchIsRefusedRatherThanGuessed`
- `internal/proxy/oracle/testdata/go_ora_lob.pcapng`, `testdata/go_ora_lob_stream.pcapng`
- `internal/proxy/oracle/capture_lob_test.go` — the harness that recorded both

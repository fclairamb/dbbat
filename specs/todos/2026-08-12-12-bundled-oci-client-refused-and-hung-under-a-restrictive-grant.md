# The DB-bundled OCI client is refused on its first call under a restrictive grant, and then hangs

**No GitHub issue filed yet — one should be.**

## Goal

Make a `read_only` (or any restrictive) grant usable from the Oracle-bundled
sqlplus: its first statement must execute, and a refusal must reach it as an ORA
error rather than hanging the session.

## Why

Measured, not suspected. Wiring the OCI client into CI
(`specs/done/.../2026-08-12-05-oci-client-coverage-runs-nowhere-in-ci.md`) put
a *second* OCI flavor in front of the proxy for the first time: the client
bundled in `gvenzl/oracle-free:23-slim` (23.26), alongside the Instant Client
23.3 the fixtures were captured from. It authenticates fine — the login test
passes on it — but under a restrictive grant it dies immediately:

```
level=INFO  msg="Oracle session established, entering proxy mode"
level=DEBUG msg="TTC message" func=OFETCH
level=WARN  msg="refused a re-execution of an untracked cursor under a restrictive grant" cursor_id=27396
```

Three things are wrong there, in order of how much they should worry us:

1. **The first client message in proxy mode decodes as `OFETCH`**, on a session
   that has not executed anything. A fresh session has no open cursor to fetch
   from.
2. **`cursor_id=27396`** is not a plausible cursor id — the ids this suite
   learns from the same upstream are single digits (`cursor_id=3`,
   `cursor_id=6`). It reads like a misaligned parse of the client's call, which
   would point at the wide/OCI framing rather than at the gate.
3. **The refusal then hangs the client.** sqlplus never comes back and never
   prints a line — not even the output of the statement it was refused on. The
   whole point of the refusal path is that the client gets ORA-01031 and keeps
   going; the refusal test (`TestIntegration_BlockedStatementRefusesSQLPlus`)
   exists because a frame the client cannot consume parks it forever.

The blast radius is real: **any** dbbat user connecting with an Oracle-bundled
OCI client under a `read_only` / `block_ddl` / `block_copy` grant would be
refused on their first statement and then hang. Restrictive grants are the
normal case for a proxy that exists to constrain access.

The Instant Client 23.3 on PATH passes the same test against the same upstream
image, so this is a property of that client flavor (or of how dbbat frames for
it), not of the gate in general.

## Repro

```bash
# fails: hangs, then context deadline exceeded with no output
ORACLE_TEST_REQUIRE_OCI_CLIENT=1 ORACLE_TEST_OCI_CLIENT=container \
  go test -tags integration -timeout 40m -count=1 -v \
  -run TestIntegration_BlockedStatementRefusesSQLPlus ./internal/proxy/oracle/

# passes on a host with an Instant Client on PATH
ORACLE_TEST_OCI_CLIENT=path ... same -run
```

`TestIntegration_BlockedStatementRefusesSQLPlus` currently **skips** on the
container flavor, naming this file
(`knownBadBundledRefusal` in `internal/proxy/oracle/oci_client_integration_test.go`).
Deleting that skip is how the fix is verified — it is the last step of this
task, not an optional one.

## Implementation

- Start at the decode, not the gate: dump the first client packet in proxy mode
  for both flavors and diff them (`DBB_DUMP_DIR`, or the `ORACLE_TEST_LOG`-style
  tee used to capture the trace above). If the 23.26 client's first call is
  genuinely an `OFETCH`, item 1 is a protocol fact dbbat has to handle; if the
  packet is really an exec and dbbat reads `OFETCH` out of it, the bug is in the
  wide framing walk and item 2 is the same bug's second symptom.
- Suspect the interaction flagged in
  `specs/todos/2026-08-12-08-oracle-wide-leg-bigclrchunks-unverified.md`: the
  wide/OCI leg ignores `UseBigClrChunks`, which the upstream advertises on
  every 23ai session. Two clients that differ in how much they lean on chunked
  CLR would then diverge exactly here. Check that first — it is cheap and the
  two specs may be one bug.
- Whatever the cause of the misread, the hang is a separate defect and deserves
  its own fix: a refusal written for a call the client is not parked on must
  still end *some* call, or the gate must decline to refuse a message it could
  not confidently decode. Failing open on an undecodable message is the safer
  default for a call dbbat cannot even name.
- Add the bundled client's first-call bytes to `testdata/` while the repro is
  in hand, so the unit half of this can be pinned without a container.

Key files: `internal/proxy/oracle/session.go` (`nextOERFrame`,
`observeClientCallNumber`), `internal/proxy/oracle/intercept.go` (the untracked
re-execution gate), `internal/proxy/oracle/cursor_reexec_gate_test.go`,
`internal/proxy/oracle/oci_client_integration_test.go`.

## Implementation Plan

Written after measuring both flavors against a live 23ai upstream with the
proxy's own log teed to stderr (`ORACLE_TEST_LOG=1`, added to the fixture's
capturing handler) and a temporary hex dump of every client packet.

### What was measured

The first client message in proxy mode, byte for byte:

	bundled 23.26   00 00 | 11 6b 04 00 | 00 00 00 00 00 05 00 … | 03 3b 05 00 …
	Instant 23.3    20 00 | 11 6b 04 01 | 05 2f 00 00 00 9d 2d … | 03 3b 05 01 …

Three findings, and the first two overturn the write-up above:

1. **Both** flavors send that frame, and **both** are refused on it —
   `cursor_id=27396` on the Instant Client too. The PATH leg was never clean;
   it passes because that client shrugs the bogus refusal off and carries on,
   while the bundled one parks forever. So symptoms 1 and 2 are not
   flavor-specific at all: only the hang is.
2. `0x11` is **not** a fetch. It is the TTC *piggyback message type*, and byte
   1 is a TTC **function code** — `0x69` close-cursors, `0x6b` (this one),
   `0x87` set-end-to-end-attrs, `0x98` set-schema. A real fetch is message type
   `0x03`, function `0x05` (`TNS_FUNC_FETCH`), which the histogram over
   `testdata/*.pcapng` confirms: every recorded `0x11` frame is `11/69`, plus
   the single `11/6b` already sitting in `sqlplus_cursor_reexec.pcapng`. So
   `decodeOFETCH`, which reads a big-endian cursor id out of bytes 1..3, was
   reading (function, sequence) — 27396 is exactly `0x6b04`.
3. The refusal ends the wrong call. `clientCallNumber` takes the sequence at
   offset 2, which for this packet is the *piggyback's* (4), while the client
   is parked on the call stapled behind it (`03 3b`, sequence 5). dbbat cannot
   walk an unrecognized piggyback body, so it cannot see that call at all.

### The fix, one invariant

**dbbat intercepts — and therefore refuses — only a message whose call it can
name.**

- `clientCallNumber` reports `ok=false` for a message-type-`0x11` piggyback it
  cannot walk (i.e. anything but a decodable close-cursors list). The sequence
  at offset 2 belongs to the piggyback, and any real call is stapled behind
  bytes dbbat cannot parse.
- `observeClientCallNumber` passes that through, and leaves the last
  known-good call number alone instead of overwriting it with the piggyback's
  — which also protects the *response*-leg refusal that fires while the client
  is parked on a call.
- `interceptClientMessage` forwards an unnamed message untouched: no cursor id
  read out of it, no gate, no refusal. This is the spec's "failing open on an
  undecodable message is the safer default", and it is what stops the next
  client whose framing dbbat misreads from hanging too.
- The `TTC message` DEBUG line carries the function byte, so `func=OFETCH` on a
  `11/6b` frame stops being a lie in the log.

Consequence, stated plainly: the `0x11` fetch reading is now unreachable for
real Oracle traffic, because no client sends a fetch that way. Wiring the gate
to the frames Oracle *does* fetch with (`03/05`) is a behaviour change on the
hot path and is filed as its own todo rather than smuggled in here.

### Tests

- `testdata/oci_bundled_first_call.hex` — the bundled client's first proxy-mode
  packet, so the unit half pins this without a container.
- The dispatcher forwards it under a `read_only` grant and writes nothing back
  to the client (symptoms 1, 2 and 3 in one assertion).
- `clientCallNumber` on it reports "cannot name", and the session's previous
  call number survives it (the hang's own half).
- `TestIntegration_BlockedStatementRefusesSQLPlus` loses its
  `knownBadBundledRefusal` skip and must pass on the container flavor.

### What it actually took

The invariant above was necessary and not sufficient. Removing the false
refusal let the session get as far as the *real* one — the `INSERT` a
`read_only` grant has to refuse — and it hung there too, twice more, for
reasons fail-open cannot cover: dbbat must answer that statement, and answer it
correctly. Two further layers, each measured against the live client:

- **The close-cursors list has a 64-bit header.** The bundled client writes
  `11 69 <seq> 00 00 <ub4> <sb8 seq+1>` where the Instant Client writes
  `11 69 <seq> 01 <seq+1>`, and its count is 8 bytes ahead of the same 4-byte
  ids. Until the walk understood it, the *execute stapled behind the list* was
  invisible and the refusal carried the last sequence dbbat had seen (12)
  instead of the call's (14). `isCloseCursorsWide8Header` /
  `decodeCloseCursorsWide8`, pinned on `testdata/oci_bundled_close_cursors.hex`.
- **The summary object has a 64-bit layout.** Call status `u32@1`, ECID
  `u16@5`, error number `u16@12`, cursor id `u16@18`, call number `u32@49`,
  RetCode `u32@132`, a 136-byte prefix, then an 8-byte row count. `learnOERShape`
  recognized neither of the two real ORA-01403 summaries the upstream sent this
  client, so the session stayed on the unlearned default and dbbat answered a
  64-bit client with a 32-bit frame. `oerFixed64Layout`, pinned on
  `testdata/oci_bundled_oer.hex`; the learner tries both layouts, widest first,
  each validated by the invariant it already used (the trailing RetCode repeats
  the leading error number *at that layout's offsets*).

`TestIntegration_BlockedStatementRefusesSQLPlus` now passes on the container
flavor, the skip is gone, and the whole `./internal/proxy/oracle/...`
integration suite is green under `-race` on that flavor.

### Two more, from the completeness audit

- **The fail-open was a smuggling channel, and nothing said so.** A piggyback is
  by construction a frame with another call stapled behind it — dbbat's own
  recordings show `11 69 … 03 5e <exec>` — so forwarding an unwalkable one
  unread let a client put an `INSERT` behind it and have it travel ungated under
  a `read_only` grant. `gateUnnameableFrame` now scans such a frame with the
  same extractor the JDBC exec path gates on, runs the statement through the
  grant's static controls and approval patterns, and refuses by **ending the
  session** rather than by answering a call it cannot name (the same answer
  `onLimitViolation` gives, for the same reason). A statement the grant permits
  still travels; both unnameable frames a live bundled-client session emits
  carry none.
- **The nameability check ran after the exec reading**, so a `11 69` execute
  whose close list did not walk was still answered with an OER — stamped with a
  stale call number, which is the ORA-18745/hang mode
  `specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md` is
  about. The check moved ahead of it. Not live-visible (every recorded `11 69`
  walks), so `buildJDBCExec` was corrected to the recorded shape and a second
  builder covers the unwalkable one.
- **The unlearned OER fallback** (originally deferred as
  `2026-08-13-03`) is implemented rather than deferred: it is this spec's own
  stated goal, since a session refused before any upstream OER has been seen
  still hung. `nextOERFrame` seeds `fixedWidth64` from the client's AUTH Phase 1,
  which carries the same 64-bit op header
  (`usesWide64OpHeader`, `testdata/oci_bundled_auth_phase1.hex`). No integration
  test can reach that window — sqlplus issues its own login SELECTs first — so
  both dialects are pinned by unit tests.
- `logMsgLearnedOERTail` now reports `fixed_width_64`, and the 32-bit learn
  tests assert it stays false.

One follow-up remains filed rather than smuggled in:
`specs/todos/2026-08-13-04-oracle-fetch-gate-watches-a-frame-no-client-sends.md`
(the `0x11` fetch reading is unreachable now that it is no longer misfed
piggybacks).

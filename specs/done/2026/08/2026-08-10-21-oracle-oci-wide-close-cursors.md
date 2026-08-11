---
model: sonnet
effort: medium
---

# Oracle: decode the OCI thick client's wide-encoded close-cursors list

**No GitHub issue filed yet — one should be.**

## Goal

Drop the cursors an OCI thick client (sqlplus, SQL*Developer via the OCI
driver, anything on Instant Client) closes, so `s.tracker.cursors` is emptied
for those sessions the same way it now is for thin clients.

## Why

`decodeCloseCursors` (`internal/proxy/oracle/ttc_decode.go`) reads the
close-cursors piggyback in the **compressed-int** framing every thin client
uses — `0x11 0x69 <seq> [0x00] 0x01 <count> <ids…>`. The OCI thick client sends
the same op in the **wide** framing dbbat already knows from the auth path
(`payloadUsesWideKVEncoding`, `replaceAuthKVValueWide`): an 8-byte pointer
sentinel followed by little-endian 32-bit fields. From
`testdata/sqlplus_cursor_reexec.pcapng`, client frame 9:

```
11 69 08 01 09 fe ff ff ff ff ff ff ff 01 00 00 00 02 00 00 00 11 87 …
      ^seq  ^?  ^pointer sentinel      ^count=1    ^cursor id=2
```

Frames 12, 14 and 16 of the same capture are the same shape, closing cursors 4,
3 and 2. The decoder rejects all of them (the count field decodes as a
compressed int of size 9, which is invalid) and deletes nothing, which is the
safe answer but not the right one.

The exposure is small and that is why it was left out of
`specs/done/…-oracle-stale-cursor-resolves-to-the-wrong-statement.md`: sqlplus
never re-executes by cursor id — it resends the full statement text on every
run (see the client table in `docs/oracle.md`) — so a stale entry it leaves
behind cannot mis-resolve a re-execution. What remains is a tracker that grows
for the life of a long OCI session, and a mixed deployment where some *other*
OCI-based client does re-execute by id would re-arm the wrong-SQL gate.

## Implementation

- Add a wide branch to `decodeCloseCursors`: after the function header, if the
  next eight bytes are the `fe ff ff ff ff ff ff ff` pointer sentinel, read the
  count and the ids as little-endian `uint32`s instead of compressed ints. Keep
  the same guards — bounded count, every id inside 16 bits, enough bytes for the
  whole list — and keep returning `ErrNotCloseCursors` (deleting nothing) on
  anything that does not fit.
- Work out what the byte between the sequence number and the sentinel is
  (`01 09` at frames 9, `01 0e` at 12 — it tracks the *next* sequence number,
  so it is probably the wide framing's own header padding). Pin whatever it
  turns out to be rather than skipping a fixed two bytes on faith.
- Extend `TestDecodeCloseCursors` with the sqlplus frames above, and move the
  "the OCI wide encoding" case out of
  `TestDecodeCloseCursorsRejectsWhatItCannotRead` — it is currently pinned as a
  *rejection*, so that test has to change in the same commit or the two will
  contradict each other.
- Add `sqlplus_cursor_reexec.pcapng` to `TestDumpReplay_CloseListsAreRealAndBatched`.

Key files: `internal/proxy/oracle/ttc_decode.go`,
`internal/proxy/oracle/cursor_close_test.go`,
`internal/proxy/oracle/testdata/sqlplus_cursor_reexec.pcapng`,
`docs/oracle.md` ("Closing cursors").

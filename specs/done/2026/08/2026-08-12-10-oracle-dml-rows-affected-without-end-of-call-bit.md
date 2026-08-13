# Oracle DML loses rows_affected (and inflates duration) when the OER lacks the end-of-call bit

## Goal

A successful INSERT/UPDATE/DELETE through the Oracle proxy should record its
`rows_affected` and a truthful `duration_ms` for every client, including the
ones whose sessions carry OERs without the `0x010000` end-of-call bit
(python-oracledb thin; go-ora sessions carry the bit and are unaffected).

## Why

Observed live on dbbat.tools.stonal.io, connection
`019ff66d-d9c3-7f29-8685-abd2373d59b1` (2026-08-12, python-oracledb-style
client against a customer Abyla schema): every UPDATE in the session shows
`rows_affected: null`, and its `duration_ms` equals the gap to the *next*
statement, not the execution time (one UPDATE recorded 74 091 ms because the
client sat idle afterwards).

The session dump proves the data was on the wire. The server's Response
(func=0x08) to the OALL8 execute embeds a perfectly decodable OER at payload
offset 19:

```
04 | 01 02 | 02 df 62 | 01 01 | 00 | 00 | 00 | 01 04
     status=2  seq=57186  rows=1  err=0            cursor=4
```

`CurRowNumber=1` is the affected-row count. But `decodeOERAt`
(`internal/proxy/oracle/ttc_oer.go`) rejects any OER whose CallStatus lacks
`oerEndOfCallBit` (0x010000), and this session's OERs come with CallStatus 1–2
— exactly the shape the comment on `decodeOERFieldsAt` documents for
python-oracledb (`testdata/python_thin_cursor_reexec.pcapng`). So
`findOERInResponse` returns nil, `completeQueryFromOER` never runs, and the
pending DML is only closed by `flushPendingQuery` when the next statement
arrives → `completeQuery(nil, nil)`: no row count, duration = time-to-next-
statement. SELECTs are unaffected because they complete via the ORA-01403
end-of-data marker and fall back to the captured-row count.

Note the inconsistency: `findCursorIDInResponse` deliberately does *not*
require the bit (it uses anchored plausibility bounds instead) and happily
learned cursor 4 from this very OER — dbbat already trusted the message for
cursor learning while refusing it for completion.

## Implementation

- In `handleResponse` (`internal/proxy/oracle/session.go`), when no
  bit-carrying OER is found and the session is *not* in a row stream
  (`rowStreamActive()` false — the payload is a return-parameter block, not row
  bytes, so the false-positive risk that motivated the bit check is much
  lower), fall back to a bounded scan: accept the first OER candidate that
  decodes as seven compressed ints with `ErrorCode` 0 or 1403,
  `SeqNumber <= oerMaxSeqNumber` and a plausible cursor id — i.e. the exact
  bounds `findCursorIDInResponse` already uses — and complete the pending query
  from it (`CurRowNumber` as rows affected).
- Alternatively (smaller): have cursor-id learning and completion share one
  scan, so an OER good enough to learn a cursor from is good enough to
  complete on, outside a row stream.
- Keep the strict bit check for `handleOERStatus` (standalone func=0x04) and
  for anything mid-row-stream — those are the paths where row bytes can fake a
  0x04 marker.
- Duration comes right with the same fix: the query completes when the
  response arrives instead of when the next statement shows up.
- Also check the session-close path: a session whose *last* statement is DML
  under this OER shape closes with the query still pending — verify what seals
  it and what duration it gets.
- Regression material is already in hand: the 6 KB session dump above
  (downloadable via `GET /api/v1/connections/<uid>/dump`) can join
  `internal/proxy/oracle/testdata/` next to
  `python_thin_cursor_reexec.pcapng`; a unit test can replay the 91-byte
  Response payload through `handleResponse` and assert rows_affected=1.

No GitHub issue exists yet — one should be filed.

# Oracle: legacy TTC Response decoder invents errors from row data

## Goal

Stop `handleResponse` from marking a healthy Oracle query as failed (and
truncating its captured rows) when a mid-fetch TNS packet happens to start with
byte `0x08`.

Observed on production query
[`019fcf4e-6774-7293-9f3f-ac023d2a6f4f`](https://dbbat.tools.stonal.io/app/queries/019fcf4e-6774-7293-9f3f-ac023d2a6f4f):
a `SELECT ac.NUM_ATTR, ac.NUM_COMP, TRIM(ac.VALEUR) FROM I3F.ATTRIB_COMPOSANT`
that ran 57.9 s, streamed fine to the client, and was recorded with
`rows_affected = 5316` **and** a 772-byte "error" whose content is verbatim
column-compressed row data (`0x15` bitmask descriptors, `1-Habitation`,
`Non défini`, `PVC`, `NR`, …).

No issue filed yet — one should be.

## Why

`interceptUpstreamMessage` treats the first byte after the TTC data flags as a
function code. During a long fetch, a packet whose leading byte is `0x08`
(`TTCFuncResponse`) is routed to `handleResponse`
([session.go:1457](internal/proxy/oracle/session.go:1457)). When
`findOERInResponse` finds no valid OER — which is the normal case for row data —
the code falls through to `decodeTTCResponse`, a fixed-offset *legacy* layout
whose own comment already admits it "misreads v315+ responses":

- `payload[2:6]` read as an error code → nonzero (row bytes)
- `payload[12:14]` read as an error flag → nonzero (row bytes)
- `payload[14:16]` read as a message length → `0x0304` = 772
- `payload[16:16+772]` copied verbatim into `Query.Error`
  ([ttc_decode.go:99](internal/proxy/oracle/ttc_decode.go:99))

Reproduced locally: a synthetic 16-byte prefix plus 772 bytes of row-shaped data
yields `IsError=true`, `msgLen=772`, and the same garbage message.

Consequences beyond the cosmetic error text — `completeQuery` clears
`tracker.pendingQuery`, so for the rest of that fetch:

1. Row capture stops. The stored `rows_affected` (5316, taken from
   `pending.rowNumber` since the OER path passed `nil`) and the "Result Rows"
   table are both truncated at the misparse point, not the true result size —
   and `results_truncated` is not set, so nothing signals it.
2. Mid-stream quota/expiry enforcement stops: the `s.guard.Check()` kill switch
   at [session.go:1327](internal/proxy/oracle/session.go:1327) is gated on
   `pendingQuery != nil`, so a grant's byte cap can no longer terminate the
   remainder of a huge result set.
3. Byte accounting for the rest of the stream is charged to the next query on
   the connection (or dropped if there is none).

The client is unaffected — packets are forwarded verbatim, which is why the
result still came back.

## Implementation

Key file: `internal/proxy/oracle/session.go`, `internal/proxy/oracle/ttc_decode.go`.

1. **Don't route mid-fetch packets to `handleResponse`.** In
   `interceptUpstreamMessage`, when a pending query has an open cursor with
   known columns (i.e. we are streaming rows), treat `0x08` as row-stream
   continuation rather than a fresh Response. A leading `0x08` should only be
   honoured as `TTCFuncResponse` at a call boundary.
2. **Make the legacy error path prove itself.** In `decodeTTCResponse`, only set
   `IsError` when the extracted text actually parses as an Oracle diagnostic
   (`ORA-`/`PLS-`/`TNS-` prefix) and the error code is a plausible ORA code
   (< 100000). Otherwise return no error rather than raw bytes. Nothing should
   ever write non-diagnostic bytes into `Query.Error`.
3. **Belt and braces at the sink.** In `completeQuery` /
   `shared`, reject an error string that isn't valid UTF-8 or that contains
   control bytes — log it at debug and drop it instead of persisting it. This
   also protects the MySQL/Mongo paths.
4. Regression test: a `handleResponse` table test feeding a row-data payload
   with a `0x08` lead byte must complete the query **without** an error and
   **without** clearing `pendingQuery`. Add a capture-based case under
   `internal/proxy/oracle/testdata` if a dump of a >5k-row Oracle fetch can be
   produced (`capture_largeresult_test.go` is the closest existing harness).
5. Consider a data fix-up: `UPDATE queries SET error = NULL WHERE error !~ '^[[:print:][:space:]]*$'` —
   low volume (1 in 500 recent queries), so optional.

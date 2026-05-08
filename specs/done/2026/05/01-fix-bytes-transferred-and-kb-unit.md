# Fix bytes_transferred undercounting + add KB unit

## Goal

Two related fixes to the `bytes_transferred` quota:

1. **Bug**: today the counter only accumulates result-row payload bytes
   (PostgreSQL `DataRow`, MySQL JSON-encoded rows). A `SELECT 2;` registers
   ~2 bytes instead of the ~100+ that actually crossed the wire. Switch to a
   wire-level counter that adds **every byte the proxy reads or writes** on the
   client and upstream sockets during the session.
2. **UX**: the unit selector in the create-grant dialog only offers MB / GB.
   Add KB so admins can configure tight quotas (and so test environments can
   exercise the limit easily).

This spec ships independently — no schema changes, no new entities.

## Why wire-level

Asked offline: the user wants `bytes_transferred` to reflect what actually
flows through the proxy, not a curated subset. Wire-level is the simplest
mental model and matches what users intuitively pay for. Counting at
`net.Conn.Read` / `Write` automatically picks up:

- query text bytes (Parse/Bind/Execute on PG, COM_QUERY on MySQL, TTC on Oracle)
- protocol framing (1-byte type + 4-byte length per PG message, etc.)
- response metadata (`RowDescription`, `CommandComplete`, `ReadyForQuery`)
- error packets, notifications, keepalives — all of it

Auth handshake bytes are also counted, which is correct for a quota that
represents "data this connector caused to move".

## Files modified / added

**Add**
- `internal/proxy/shared/byte_counter.go` — `net.Conn` wrapper with atomic
  per-direction counters.
- `internal/proxy/shared/byte_counter_test.go` — unit tests.

**Modify**
- `internal/proxy/postgresql/session.go` — wrap client + upstream conns at
  session bootstrap; remove the DataRow byte additions at lines 385–391 and
  the CopyData addition at line 444.
- `internal/proxy/mysql/result.go` — drop the JSON-encoded row sizing in
  `captureRows()`; rely on the wrapped conns.
- `internal/proxy/mysql/session.go` (or wherever the MySQL proxy creates
  client/upstream conns) — apply the wrapper.
- `internal/proxy/oracle/intercept.go` — drop the response-size addition;
  apply the wrapper at `internal/proxy/oracle/session.go` (or equivalent).
- `internal/store/connections.go` — `IncrementConnectionStats()` already exists
  (~line 313). No signature change; it now receives wire-level totals.
- `front/src/routes/_authenticated/grants/index.tsx` — add `KB` option to the
  unit `<select>` (~line 456) and to the unit→bytes conversion (~line 337).

## Approach

### Wire-level counter

Create a small wrapper that satisfies `net.Conn`:

```go
// internal/proxy/shared/byte_counter.go
package shared

import (
    "net"
    "sync/atomic"
)

type CountingConn struct {
    net.Conn
    BytesIn  *atomic.Int64 // bytes read FROM this conn (i.e. received)
    BytesOut *atomic.Int64 // bytes written TO this conn (i.e. sent)
}

func (c *CountingConn) Read(p []byte) (int, error) {
    n, err := c.Conn.Read(p)
    if n > 0 {
        c.BytesIn.Add(int64(n))
    }
    return n, err
}

func (c *CountingConn) Write(p []byte) (int, error) {
    n, err := c.Conn.Write(p)
    if n > 0 {
        c.BytesOut.Add(int64(n))
    }
    return n, err
}
```

Each session owns **one pair of counters** (`bytesFromClient`,
`bytesToClient`). Wrap:

- the client conn with `BytesIn = bytesFromClient`, `BytesOut = bytesToClient`
- the upstream conn with `BytesIn = bytesToClient`, `BytesOut = bytesFromClient`

Both directions are now double-counted? No — each byte is read on one side and
written on the other, but we only count one side per direction. Pick a
convention: count on the **read** side only (i.e. set the `Write` hook to a
no-op for the upstream wrapper, or use two separate wrappers per role). The
simplest correct setup:

```go
clientReadCounter := &atomic.Int64{} // bytes received from client
clientWriteCounter := &atomic.Int64{} // bytes sent to client
upstreamReadCounter := &atomic.Int64{} // bytes received from upstream
upstreamWriteCounter := &atomic.Int64{} // bytes sent to upstream
```

Total `bytes_transferred = clientRead + clientWrite + upstreamRead + upstreamWrite`
**halved** because each byte traverses the proxy twice (once as ingress, once
as egress). Or, simpler: track only the two halves of the conversation —
`clientRead + clientWrite` — and call those total bytes, since that's the
"client side" view. **Pick the simpler one** and document the choice in a
one-line comment on the counter struct. Recommend: `clientRead + clientWrite`
(== "bytes the proxy exchanged with the client").

### Per-query attribution

Today there is a per-query `bytes_transferred` field in the queries table.
Snapshot the counter at query start and diff at query end:

```go
startBytes := clientRead.Load() + clientWrite.Load()
// ... run query ...
endBytes := clientRead.Load() + clientWrite.Load()
queryBytes := endBytes - startBytes
```

For PostgreSQL the natural snapshot points are around the simple/extended
query handlers. For MySQL it's `recordQuery` (line 171 of `result.go` today).
For Oracle it's the TTC OAL8 boundaries in `intercept.go`. Keep the existing
field; just feed it the diff.

### Connection totals

Connection `bytes_transferred` becomes `clientRead + clientWrite` at the time
of stat increment. The existing `IncrementConnectionStats` keeps its
signature; the call sites change.

### Frontend KB

In `front/src/routes/_authenticated/grants/index.tsx`:

```tsx
// before
<select value={dataUnit} onChange={...}>
  <option value="MB">MB</option>
  <option value="GB">GB</option>
</select>

// after
<select value={dataUnit} onChange={...}>
  <option value="KB">KB</option>
  <option value="MB">MB</option>
  <option value="GB">GB</option>
</select>
```

Conversion map (line ~337):

```ts
const UNIT_MULT: Record<string, number> = {
  KB: 1024,
  MB: 1024 * 1024,
  GB: 1024 * 1024 * 1024,
};
```

Default unit stays MB. Display when reading a grant back: pick the largest
unit that yields an integer ≥ 1 (so `1024` shows as `1 KB`, `1048576` as
`1 MB`).

## Tests

### Unit
- `byte_counter_test.go`: write 100 bytes, read 50 bytes via the wrapped conn,
  assert counters. Concurrent goroutines hammering Read/Write — no race.

### Integration (PostgreSQL)
- testcontainers PG, dbbat proxy in front, `psql` client running `SELECT 2;`,
  assert the row in `connection_queries.bytes_transferred` is materially > 2
  (expected ≥ ~50 bytes — the query text alone is 9 bytes, plus framing and
  response).

### Manual
- `make dev`, log in as `admin`, open create-grant dialog, confirm KB appears
  in the unit dropdown and conversion is correct (e.g. `512 KB` → 524288 in
  the request payload visible in DevTools).

## Verification checklist

- [ ] `make lint` clean
- [ ] `make test` green (with the new wire-level test)
- [ ] `SELECT 2;` via the PG proxy produces `bytes_transferred ≥ 50` on the
      query row
- [ ] Same for MySQL proxy (`SELECT 2`) and Oracle (`SELECT 2 FROM DUAL`)
- [ ] Existing quota enforcement still triggers `ErrDataLimitExceeded` past
      the configured limit (no regression)
- [ ] Create-grant UI offers KB / MB / GB; default is MB; round-trip works

## Out of scope

- Changing the quota enforcement check (it stays `>= max_bytes_transferred`).
- Schema changes — none needed.
- Per-query bytes split (in vs out) on the API — the existing single field is
  fine.
- Backfilling historical rows — the old undercount stays as historical truth.

## Implementation Plan

Concrete steps grounded in the actual codebase. Each phase is a separate
commit.

### Phase 1 — `internal/proxy/shared/byte_counter.go`

Add a `net.Conn` wrapper with two `*atomic.Int64` counters (one per
direction). Provide a constructor `NewCountingConn(conn, in, out)` and a
`Total()` helper that returns `in + out`. Use atomics so per-query snapshots
are safe to take while the conn is being read concurrently in another
goroutine (PG proxies the upstream→client direction in a separate goroutine).

Tests in `byte_counter_test.go`: a pipe-pair proves Read and Write update the
right counter; concurrent goroutines hammering Read+Write cause no race
(`go test -race`).

### Phase 2 — PostgreSQL proxy

`internal/proxy/postgresql/session.go`

- In `NewSession`, allocate two atomic counters on the `Session`
  (`bytesFromClient`, `bytesToClient`). Wrap `clientConn` with
  `NewCountingConn` before constructing the buffered reader. The TLS upgrade
  path at line 601 already does `tls.Server(s.clientConn, ...)` — that now
  wraps our wrapper, so encrypted bytes still flow through it.
- After `connectUpstream`, wrap `s.upstreamConn` with the same counters
  (swapped: writes to upstream are bytes-from-client, reads from upstream are
  bytes-to-client). Then pass the wrapped conn to `pgproto3.NewFrontend`.
- Add `startBytes int64` to `pendingQuery`. When a pending query is created
  (look up the call sites that set `s.currentQuery`), snapshot
  `bytesFromClient + bytesToClient`.
- In `proxyUpstreamToClient`, replace the per-`DataRow` and per-`CopyData`
  byte additions with a single diff at `ReadyForQuery`:
  `bytesTransferred = (bytesFromClient + bytesToClient) - query.startBytes`.
- `logQuery` keeps its signature; the value passed in is the diff.

### Phase 3 — Oracle proxy

`internal/proxy/oracle/session.go`

- Add atomic counters on `session`. Wrap `clientConn` at the top of
  `newSession`. Wrap `upstreamConn` at the point it's first assigned (search
  for `s.upstreamConn = `).
- Rip out the existing `bytesTransferred := int64(len(pkt.Payload))` at line
  740 and the per-call propagation through `interceptUpstreamMessage`,
  `handleQueryResultV2`, `handleResponse`, `handleContinuation`,
  `completeQuery`. Replace with a `startBytes` field on the per-query
  tracker and a diff at `completeQuery`.
- `internal/proxy/oracle/intercept.go` `completeQuery` accepts the diff
  instead of computing it from packet length.

### Phase 4 — MySQL proxy

`internal/proxy/mysql/session.go` and `upstream.go`

- Add atomic counters on `Session`. Wrap `clientConn` in `newSession` so
  `gomysqlServer.NewCustomizedConn(s.clientConn, ...)` reads through the
  wrapper.
- `connectUpstream`: switch from `gomysqlclient.Connect` to
  `gomysqlclient.ConnectWithDialer` (the upstream library exposes a `Dialer`
  type that returns a `net.Conn`, see vendor inspection). The custom dialer
  dials normally and wraps the resulting conn with `NewCountingConn` before
  returning.
- In `internal/proxy/mysql/result.go` `recordQuery` (line 171): replace the
  JSON-encoded sizing with the snapshot diff at the per-query boundary.
  The session needs a `startBytes` snapshot taken when each command starts;
  hook this into `Handler.HandleQuery`/`HandleStmtExecute` (via the wrapping
  `handler` struct) or compute against the previous total in
  `IncrementConnectionStats` at query end.
- The per-query bytes column on the queries row now reflects the diff;
  connection totals naturally reflect the cumulative counter.

### Phase 5 — Connection-total reporting

No store changes (`IncrementConnectionStats` keeps its signature). Audit
that connection-total bytes across all three protocols is `Total()` of the
wrapper at session close, fed via the same `IncrementConnectionStats` call
path.

### Phase 6 — Frontend KB unit

`front/src/routes/_authenticated/grants/index.tsx`

- Add `KB` to the unit `<select>` (~line 456) ahead of MB.
- Extend the unit→bytes map (~line 337) with `KB: 1024`.
- Display logic when reading a grant back: pick the largest unit that yields
  an integer ≥ 1 (so `512 KB` doesn't show as `0.5 MB`). Default new grants
  to MB.

### Phase 7 — Tests

- Unit: `byte_counter_test.go` (Phase 1).
- Integration (PostgreSQL): existing integration test in
  `internal/proxy/postgresql/` runs `SELECT 1` through the proxy; add an
  assertion that the resulting connection's `bytes_transferred` is materially
  > the value bytes alone (≥ 50).
- Frontend: Playwright e2e on the create-grant dialog asserting KB option
  exists and round-trips. (May lift to a follow-up if the e2e harness is
  flaky.)

### Phase 8 — Build + lint

`make build-binary build-front lint test` green. Fix issues until clean.

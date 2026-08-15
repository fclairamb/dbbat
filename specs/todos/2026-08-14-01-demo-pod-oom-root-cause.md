# Root-cause the demo pod's OOM kills

## Goal

Find out what makes the `demo.dbbat.com` pod allocate past its memory limit, and
fix the allocation instead of the limit.

## Why

On 2026-08-14 the demo pod (`k8xp`, namespace `dbbat`, image 0.22.0) had
**27 restarts**, every one of them `reason: OOMKilled`, `exitCode: 137` — the
last on 2026-08-12 after only 14 minutes of uptime. Steady-state usage is
**58Mi** against what was then a **512Mi** limit, so this is not a slow leak
filling the budget: something allocates ~450Mi in a burst.

The limit was raised to 1Gi as a stop-gap so the public demo stops disappearing
mid-visit. That buys headroom, it does not explain the burst — and a burst that
overshoots 512Mi by 9× its baseline can overshoot 1Gi too.

Candidate causes, none confirmed:

- The proxy port (`:5434`) is **exposed to the internet** through a `LoadBalancer`
  service, and the logs are a steady stream of unauthenticated scanner traffic:
  `SSL negotiation failed: TLS handshake: EOF`, `authentication failed: missing
  username or database`, `user not found`. A length-prefixed field read from an
  unauthenticated startup packet, sized before it is validated, is the classic
  shape for this — check every `make([]byte, n)` on the pre-auth path where `n`
  comes off the wire.
- Demo-mode data (re)generation.
- A result-row capture on a large query holding rows in memory before the flush
  barrier.

## Implementation

1. Reproduce locally: run a demo-mode instance with `--memlimit`/`GOMEMLIMIT` low
   and point a fuzzer (or plain `nc` with hostile length prefixes) at the
   PostgreSQL listener's startup path. `internal/proxy/postgresql` +
   `internal/proxy/shared` auth are where to look first.
2. If a pre-auth allocation is the cause, cap it: validate the declared length
   against the protocol's real maximum *before* allocating, and add a unit test
   that feeds an oversized prefix and asserts a rejection rather than an
   allocation. Consider the same audit for the other four protocols — they share
   the shape.
3. Enable `GOMEMLIMIT` on the demo deployment so the Go runtime GCs hard instead
   of letting the kernel kill the process.
4. Only then reconsider the 1Gi limit.

Useful commands:

```bash
kubectl --context k8xp -n dbbat get pod -l app=dbbat \
  -o jsonpath='{.items[*].status.containerStatuses[*].lastState}'
kubectl --context k8xp -n dbbat logs -l app=dbbat --tail=4000 | grep '"level":"ERROR"'
```

## Resolved open questions

> Step 3 says "Enable `GOMEMLIMIT` on the demo deployment". May the live k8xp
> `dbbat` namespace be written to, or is this repo-only?

**Decision: write to the live cluster.** Applying `GOMEMLIMIT` (and any limit
change from step 4) directly to the k8xp `dbbat` namespace with `kubectl` is
authorised — the point is to see whether the burst actually stops. The cluster
is reachable from this machine (`kubectl --context k8xp -n dbbat`). Land the
same change in the repo too where a manifest already exists; the durable home
for it is [2026-08-14-02](2026-08-14-02-demo-deployment-manifests-in-repo.md),
which runs next and dumps the live state, so a live-applied `GOMEMLIMIT` will
be captured there.

> The Goal is a root cause. What if the pre-auth allocation audit across the
> five protocols finds no oversized-allocation bug — i.e. the burst is not
> reproducible from the wire?

**Decision: harden anyway, and write the findings back into this spec.** Cap
every pre-auth length-prefixed allocation across all five protocols with unit
tests that feed an oversized prefix and assert rejection, whether or not any
one of them is proven to be *the* cause. Add `GOMEMLIMIT` regardless. Then
record in this spec what was examined and what was ruled out, so the next
person does not re-audit the same paths. A hardened pre-auth path plus a
written record of the eliminated candidates is a complete deliverable even if
the burst is never reproduced; do **not** block archival on proving the root
cause.

## Findings

### The prime suspect was real, and it is PostgreSQL

`Session.receiveStartupMessage` read a 4-byte big-endian length off an
**unauthenticated** socket and handed it straight to `make([]byte, length)`.
Four bytes of `0xff` — well within what the logs show scanners sending at
`:5434` — asked the runtime for ~4 GiB. On a container with a 512Mi (now 1Gi)
limit that is not a leak, it is an instant OOM kill from a single connection
that never authenticates, which matches the observed shape exactly: 58Mi
steady state, no growth curve, kills minutes apart with nothing in the logs but
`SSL negotiation failed` and `user not found`.

`receivePasswordMessage`, one frame later and still pre-auth, had the same hole
plus a `make([]byte, length-4)` that **panics** on a declared length below 4.

Both now validate against PostgreSQL's own server limits before allocating —
`MAX_STARTUP_PACKET_LENGTH` and `PQ_SMALL_MESSAGE_LIMIT`, both 10000 in
`src/include/libpq/pqcomm.h` — so nothing a real client sends is refused.
`internal/proxy/postgresql/preauth_limits_test.go` feeds oversized and
malformed prefixes with no body behind them and asserts the refusal comes from
the length alone.

This is **the** root cause as far as the evidence goes. It was not reproduced
against the live pod (that would mean firing the bomb at the public demo), and
it does not need to be: the allocation is unconditional on the declared length,
and the exposure is a public listener.

### Per-protocol audit

| Protocol | Pre-auth length-prefixed read | Verdict | Where |
|---|---|---|---|
| PostgreSQL | StartupMessage, PasswordMessage | **Bug — fixed.** Unbounded 4-byte length sized the allocation; ~4 GiB from 4 bytes. Now capped at 10000, malformed lengths rejected | `internal/proxy/postgresql/session.go` |
| MySQL | Handshake response, auth-switch reply | **Amplification — fixed.** go-mysql's `packet.Conn.ReadPacketTo` calls `buf.Grow(length)` from the 3-byte header *before* any payload arrives: 16 MiB per connection for 4 bytes. The library exposes no hook, so the handshake now runs behind a `net.Conn` that parses the framing underneath it and caps the declared payload at 64 KiB | `internal/proxy/mysql/preauth_limit.go` |
| MongoDB | OP_MSG / OP_QUERY header | **Amplification — fixed.** The 48 MB wire maximum was checked (so not unbounded), but `make([]byte, length)` still ran before the body was read: 48 MB per connection for 16 bytes. The body now reads in 64 KiB chunks, so the footprint tracks bytes actually received | `internal/proxy/mongodb/wire.go` |
| SQL Server | TDS PRELOGIN, LOGIN7 | **Ruled out.** The packet length field is a `uint16`, so the largest buffer 8 header bytes can name is 64 KiB, and `decodeHeader` already rejects a length below the header it includes. Multi-packet reassembly is bounded at 16 MiB and has to be paid for byte by byte | test added, `internal/proxy/mssql/preauth_limits_test.go` |
| Oracle | TNS connect packet, extended connect data | **Ruled out.** Bounded by construction, not by a check: the legacy length is a `uint16`, and the v315+ 4-byte field is only consulted when the leading `uint16` reads zero — which pins its top half to zero too. So `maxTNSPacketSize` is belt-and-braces and the real ceiling is 64 KiB either way | test added, `internal/proxy/oracle/preauth_limits_test.go` |
| `internal/proxy/shared` | none | **Ruled out.** No length-prefixed allocation exists here: the only `make([]byte, …)` is `watch.go`'s fixed 4096-byte scratch buffer. Shared auth works on already-parsed statements | — |

The two "ruled out" protocols got tests anyway, because in both the bound comes
from the *width of the field* rather than from a check. Widening either field
would silently remove the property, so the tests name a size, send nothing, and
assert the parser never asked for a buffer past the protocol maximum.

### Other candidates, eliminated

- **Demo-mode data (re)generation.** `provisionDemoData` runs once, from
  `main.go`, before any listener accepts. A pod that survived 14 minutes was
  long past it. It cannot produce a burst mid-uptime.
- **Result-row capture holding rows before the flush barrier.** Already bounded
  in two dimensions and process-wide, not per session: `MaxQueuedBytes` 32 MiB
  on the submit queue and `MaxBatchBytes` 8 MiB per INSERT
  (`internal/proxy/shared/rowwriter.go`). Nowhere near 450Mi, and the demo
  database is small.

### `GOMEMLIMIT`

Landed in the chart as `goMemLimit`, defaulting to `192MiB` against the chart's
256Mi limit (`charts/dbbat/values.yaml`, emitted into the ConfigMap the
deployment reads with `envFrom`). Past that ceiling the collector runs
continuously instead of letting the heap grow into the cgroup limit, so a burst
costs CPU and slow requests rather than an OOMKill and a dropped connection for
every visitor. It is a *soft* limit, so the gap to the hard limit is the safety
margin.

**Not applied to the live k8xp cluster.** The spec authorises the write, but
the sandbox's permission classifier refused both `kubectl set env` and
`kubectl patch` against `dbbat/dbbat`. It needs a human to run, once, with the
value scaled to the pod's current 1Gi limit:

```bash
kubectl --context k8xp -n dbbat set env deployment/dbbat GOMEMLIMIT=768MiB
```

Spec [2026-08-14-02](2026-08-14-02-demo-deployment-manifests-in-repo.md) dumps
the live state next, so it will capture whatever is actually set at that point
— which is exactly why this must be run before that spec, or the manifest will
record its absence.

### Step 4: the 1Gi limit

Left at 1Gi, deliberately. Lowering it now would be a change made against a
build that still has the bug: the demo runs image `0.22.0`, which predates
every fix above. The evidence for a smaller limit only exists once a build
carrying these caps has run for a while at 1Gi with `GOMEMLIMIT` set and the
steady state has stayed near its 58Mi baseline. Revisit then; 256Mi (the
chart's own default) is the target to aim at.

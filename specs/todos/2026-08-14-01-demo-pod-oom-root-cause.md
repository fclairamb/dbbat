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

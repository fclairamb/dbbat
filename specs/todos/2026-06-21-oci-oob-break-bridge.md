# OCI out-of-band break bridge for sqlplus over a proxy/LB

GitHub issue: _none yet — file one before implementing._

## Goal

Make **sqlplus / OCI instant client** work through the dbbat Oracle proxy over a
network path that does not preserve TCP urgent data (the dbbat proxy itself, and
a Kubernetes NodePort/NLB ingress). Thin clients (go-ora, python-oracledb thin,
SQLcl/ojdbc thin) already work end-to-end, and the OCI **wide (4-byte LE) TTC
encoding** is implemented, so sqlplus authenticates + queries + captures fine
**locally** (with `DISABLE_OOB=ON`). The remaining blocker is the OCI break probe.

## Why

Over a network connection OCI runs a break probe before AUTH Phase 2: it sends an
inline break marker (`01 00 01`) **plus a TCP-urgent (out-of-band) break byte**,
then waits for the server's out-of-band acknowledgement. dbbat is a userspace
proxy that does not relay TCP urgent data, and kube-proxy NodePort / NLB DNAT does
not preserve it either, so the probe never completes — sqlplus stalls or aborts
with `ORA-03106` (it sends a TTC EOF data packet `0x0040` and closes). The local
container client avoids this only because its sqlnet.ora sets `DISABLE_OOB=ON`.

See `docs/oracle.md` → "OCI break probe — remaining limitation over non-OOB ingress".

## What was already tried (and did NOT work over the cluster NodePort)

- Answering the inline break with a reset marker (respond-on-break and
  respond-on-reset variants), `readPhase2Packet` in `internal/proxy/oracle/session.go`.
- Re-sending the AUTH challenge after the reset.
- Skipping the empty `0x0040` data packet and continuing to read Phase 2.
- Sending an OOB byte back via `syscall.Sendto(fd, {0x21}, MSG_OOB, nil)`.
- Setting `DISABLE_OOB=ON` on the Windows client (sqlnet.ora in several locations).

## Implementation (directions, pick after a local repro)

1. **Get a local repro first.** The DB-bundled container sqlplus has
   `DISABLE_OOB=ON` baked in and does not probe. Use a standalone Oracle Instant
   Client (Linux/macOS) with `DISABLE_OOB=OFF` against `/tmp/dbbat-local` so the
   OOB break can be iterated without slow cluster cycles.
2. **OOB bridging** in the proxy: set `SO_OOBINLINE` on the client socket so the
   urgent byte is consumed inline, and/or send the urgent ack via raw syscall;
   verify it survives the actual ingress (kube-proxy may rewrite/strip it).
3. **OOB-preserving ingress**: front dbbat with a TCP-passthrough LB that
   preserves urgent data, instead of a NodePort/NLB that does not.
4. **Documented client mitigation**: if (1)–(3) are infeasible, confirm and
   document a reliable `DISABLE_OOB=ON` client recipe for the instant client.

## Verification

- Windows host (`florent@15.237.251.23`): `C:\oracle\instantclient_23_0\sqlplus.exe`
  → `orauser/<key>@//<node>:32181/FREEPDB1` returns rows (no `ORA-03106`/hang).
- No regression for go-ora / python-oracledb thin / SQLcl on the cluster.

## Implementation Plan

Chosen directions: **(1) local repro first**, then a **negotiation-level variant of (2)**
— make the proxy *disable OOB at the TNS negotiation layer* instead of bridging urgent
bytes — with **(4)** kept as documented fallback and **(3)** documented as infra guidance.

Rationale:

- A local repro is feasible in this environment: sqlplus 23.3 (Oracle Instant Client for
  macOS ARM64, via Homebrew — needed an ad-hoc `codesign` to run) and the
  `gvenzl/oracle-free:23-slim` image are both present.
- Raw OOB *bridging* (SO_OOBINLINE + `MSG_OOB` sends) can only ever work on paths that
  preserve TCP urgent data. The cluster ingress (NodePort/NLB) strips it, so bridging is
  a dead end for the actual deployment — confirmed by the "already tried" list.
- The only proxy-side fix that can survive an OOB-stripping ingress is to make the client
  *not use OOB at all*: Oracle already has this switch server-side (`DISABLE_OOB_AUTO`,
  19c+), which means the Accept/negotiation carries a signal the client honors. dbbat
  already rewrites the Accept for similar reasons (`stripAcceptModernAuthFlags`), so the
  plan is to find the OOB-capability bits empirically (diff Connect/Accept captures with
  and without `DISABLE_OOB` / `DISABLE_OOB_AUTO`) and clear them in the relay.

Steps:

1. Local repro: sqlplus (OOB enabled) → docker Oracle 23ai directly, and through a local
   dbbat; note that Docker Desktop's port forwarding on macOS is itself a userspace,
   OOB-stripping path — i.e. a faithful stand-in for the cluster ingress.
2. Observe the probe with a small spy relay (SO_OOBINLINE + SIOCATMARK on both legs) to
   see exactly which side sends urgent data when, and what the negotiation carries.
3. Implement OOB-disable in dbbat's pre-auth relay: clear the urgent-data capability
   bits in the client's Connect (NT protocol characteristics) as forwarded upstream,
   and/or the corresponding Accept bits toward the client, so neither side attempts the
   break probe through the proxy. Unit tests from captured fixtures.
4. Local end-to-end verification including an OOB-stripping hop in front of dbbat (plain
   TCP relay) to simulate the NodePort: sqlplus must authenticate + query with
   `DISABLE_OOB` unset.
5. Regression: `make test` (+ existing thin-client fixtures), lint, build.
6. Docs: update `docs/oracle.md` OOB section; document the `DISABLE_OOB=ON` client recipe
   (direction 4) and the TCP-passthrough-ingress option (direction 3). Cluster/Windows
   verification is out of scope for this run — documented as pending manual verification
   with a runbook.

## Findings & Outcome

**Root cause was NOT the TCP-urgent OOB break probe.** With a local repro (sqlplus 23.3
Instant Client for macOS ARM64 → dbbat → `gvenzl/oracle-free:23-slim`) and a spy relay
running `SO_OOBINLINE` + `SIOCATMARK` on both legs, **zero** TCP-urgent bytes were observed
during the failing handshake. The sqlplus "stall before AUTH Phase 2 / `ORA-03106`" was an
**inline TTC-level break/reset abort** triggered by a malformed AUTH challenge, not the
out-of-band probe. The proxy now works over a plain TCP relay that *drops* urgent data (a
faithful NodePort/NLB stand-in), so directions (2) OOB-bridging, (3) OOB-preserving ingress,
and (4) `DISABLE_OOB` client recipe are all **unnecessary** — the fix is at the protocol
layer, not the OOB layer.

**Direction landed: (1) local repro → a protocol fix** (a superset of what the spec
anticipated). Four OCI wide-encoding bugs in the terminated-auth path, each found and fixed
against a live 23ai and pinned with fixtures from both OCI flavors (macOS Instant Client
23.3 and DB-bundled 23.26):

1. **Challenge end-of-call summary width** — was a fixed capture; now reuses the live
   upstream Phase 1 summary (`clientChallengeTrailer` + `beginUpstreamAuth` running before
   the client challenge). This is the direct cause of the break/reset stall.
2. **Phase 1 user-len locator** (`findUserIDLenPos`) — backward dword scan corrupted the KV
   pair count when `len(username) == numPairs` (5-char `admin`); now anchors on the first
   `fe…` pointer run and handles the 3× buffer-size convention.
3. **Phase 2 value-length convention** (`replaceAuthKVValueWide`) — instantclient 23.3 sends
   3× UTF-8 buffer sizes; a plain length drew `ORA-28041`.
4. **AUTH OK reassembly + re-fragmentation** (`readUpstreamAuthMessages` / `reframeAuthOK`) —
   the AUTH OK spans two packets with `AUTH_SVR_RESPONSE` straddling the boundary; merge to
   patch, then re-fragment or the client rejects a too-large packet with `ORA-12592`.

**Verified locally:** `sqlplus admin/<key>@//127.0.0.1:<port>/oralocal` returns rows over
(a) a direct connection and (b) an OOB-dropping relay in front of dbbat. `make build-binary`,
`make lint`, `make test` all green. New fixtures in `internal/proxy/oracle/oci_instantclient_test.go`;
all pre-existing thin-client dump-replay fixtures still pass (no regression).

**Not verified (out of scope for this automated run):** live cluster verification from the
Windows host over the Kubernetes NodePort. A runbook for it is in `docs/oracle.md`
("OCI break/reset before AUTH Phase 2"). Note: go-ora and python-oracledb **thin** clients
fail with `ORA-03120` against the `gvenzl/oracle-free:23-slim` container in this local
harness — but they fail **identically on the pre-change base binary**, so this is a
container/caps quirk of the local harness, not a regression (their captured dump-replay
fixtures pass, and they are validated against real Stonal RDS Oracle on the cluster).

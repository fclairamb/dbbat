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

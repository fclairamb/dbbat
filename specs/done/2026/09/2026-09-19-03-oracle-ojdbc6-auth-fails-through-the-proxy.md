---
model: opus
effort: medium
---

# ojdbc6 cannot authenticate through the proxy: `Invalid Packet Lenght` at OSESSKEY

## Goal

Let a pre-v315 JDBC client (ojdbc6 11.2.0.4, TNS version 310) complete the
O5LOGON exchange through dbbat, or establish and document that it cannot and
why. Today it dies during login, so the one client population dbbat has a
recording of is the one it cannot actually serve.

## Why

Found on 2026-09-19 while trying to verify
`2026-09-19-02-oracle-piggyback-exec-5e-reexec-ungated.md` **live**. The gating
fix there is pinned by replaying `testdata/ojdbc6_legacy.pcapng` through the
real intercept pipeline, which is as far as the recording can go: the recording
was taken through a dumb byte relay (`capture_legacy_oall8_test.go`), not
through the proxy.

Driving the same driver through a real proxy — the `startOracleThroughProxy`
fixture, Oracle 23ai Free upstream, fixture user plus `dbb_` API key, exactly
the shape every other client integration test uses — fails at login:

```
java.sql.SQLRecoverableException: IO Error: Invalid Packet Lenght
  Caused by: oracle.net.ns.NetException: Invalid Packet Lenght
    at oracle.net.ns.Packet.processHeader(Packet.java:387)
    at oracle.jdbc.driver.T4CTTIoauthenticate.doOSESSKEY(T4CTTIoauthenticate.java:407)
    at oracle.jdbc.driver.T4CConnection.logon(T4CConnection.java:416)
```

So the client cannot read the packet dbbat writes in answer to OSESSKEY. It is
**not** a regression from the gating fix: the failure is in the AUTH phase,
before any statement op, and it reproduces identically with that fix's only
pre-auth change (the `acceptNegotiatedSDU` length guard) reverted. ojdbc6 talks
to the same 23ai container perfectly well through a byte relay, so the
difference is dbbat's own AUTH leg.

Worth fixing on its own terms — it is a client dbbat silently does not support —
and worth fixing for the leverage: with login working, the three ojdbc6 findings
(the `03 5e` re-execution gate, the `03 5e` statement tag, the pre-v315 Accept)
all become live end-to-end tests instead of replays.

## Implementation

1. **Reproduce.** The probe is small; it was written and then removed rather
   than committed as a knowingly-failing test. Stand it up again:
   `startOracleThroughProxy(t, nil)`, compile a JDBC client against
   `OJDBC6_JAR`, connect to `env.host:env.port/env.service` as
   `env.username`/`env.apiKey`. `docker run -d -p 51521:1521 -e
   ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim` plus the Maven Central jar
   (`com.oracle.database.jdbc:ojdbc6:11.2.0.4`) is the whole dependency list.
2. **Capture both legs.** Record the client↔dbbat and dbbat↔upstream halves of
   the AUTH exchange (the capture relay already does one; `DBB_DUMP_DIR` gives
   the other) and diff dbbat's answer against the upstream's own, byte for byte.
   `Invalid Packet Lenght` is the client rejecting a TNS header, so suspect the
   packet-length field form first: a v310 client expects the legacy 2-byte
   length where v315+ writes a 4-byte one (`encodeDataPacketLike` already models
   exactly this distinction for statement frames — see the `model[0:2] != 0`
   test — so the reading exists, it may simply not be applied on this leg).
3. **Check what else the AUTH leg assumes about version.** `stripAcceptModernAuthFlags`,
   the O5LOGON rewrite and the synthetic AUTH builders were all measured against
   v317+ clients and the OCI wide encoding; a v310 session is a third shape none
   of them was written for.
4. **Pin it live** with the integration test from step 1, skipped without the
   jar and a JDK (the `OJDBC6_JAR` convention `capture_legacy_oall8_test.go`
   already uses), and then convert the replay assertions in
   `exec_no_statement_reexec_test.go` into a live counterpart: the probe's
   second execution produces its own `queries` row, and a byte quota exhausted
   by the first refuses it.
5. **Update `docs/oracle.md`** — the client-compatibility tables say nothing
   about pre-v315 clients failing to log in, which is the thing an operator
   would want to know before pointing one at dbbat.

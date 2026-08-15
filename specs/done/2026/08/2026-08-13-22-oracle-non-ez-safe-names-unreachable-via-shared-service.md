# Oracle: candidate upstreams are compared as text, so two spellings of one host read as two hosts

## Goal

When several dbbat databases share one upstream `oracle_service_name`, deciding
whether they sit on the *same* upstream must not hinge on how the host was
spelled in each row.

## Why

Original incident (Nicolas Heinrich, Slack #C0B2P27DLQ5, p1786649003353289):
`abyla_abymutualise02 (R/O)` was unreachable — `DPY-6001 Service "MUTU02" is
not registered with the listener` — through a catch-22:

1. The name contained ` (R/O)` → `isEZConnectSafeName` false → the connection
   endpoint (`internal/api/connection_url.go:128`) fell back to the raw upstream
   service name `MUTU02`.
2. Three rows carried `oracle_service_name = MUTU02` under **two spellings of
   the same machine** (`oracle-abymutualise02.db.stonal.io` vs
   `abymutualise02.cusruf0cguz3.eu-west-3.rds.amazonaws.com`). `resolveDatabase`
   (`internal/proxy/oracle/session.go:602`) compares `host:port` **textually**,
   concluded "different upstreams", and refused ORA-12514.
3. The refusal's advice — "connect using the dbbat database name" — was
   impossible: that name is exactly what EZ-Connect cannot carry.

**The catch-22 half is fixed.** The servers were renamed to slugs
(`abyla_abymutualise02_ro`, `abyla_abymutualise_ro`, …) on 2026-08-13 and
verified end to end the same evening: the DSN now carries the unambiguous name,
`GetServerByName` matches exactly, no candidate list is consulted, and all
three shared entries connect and read across their instance. The naming rules
that keep it fixed are filed separately —
`2026-08-13-23-server-names-must-be-slugs.md` and
`2026-08-13-23-servers-cannot-be-renamed.md`.

What remains is item 2 on its own. It is dormant rather than gone: any future
entry that falls back to a shared service name (an unsafe name, a tnsnames
descriptor, a client that sends the raw service) still hits the textual compare,
and a CNAME vs A-record spelling of one host still reads as two upstreams.

## Implementation

- Compare candidate upstreams after resolution — on `(resolved IPs, port)`
  rather than on the literal `host:port` string — in `resolveDatabase`
  (`internal/proxy/oracle/session.go:602`). Weigh it against determinism: the
  textual compare never surprises, a DNS-dependent one can change answer between
  two connects, and the lookup sits on the connect path.
- Alternatively, keep the textual compare and make the misconfiguration visible:
  flag, in the admin UI and/or the connectivity check, Oracle rows that share an
  `oracle_service_name` but disagree on host spelling. That is the cheap half
  and it addresses what actually happened.
- Check whether `parseConnectDescriptor` accepts a quoted `(SERVICE_NAME=...)`
  carrying spaces/parens; if so, document the full descriptor as the escape
  hatch for names EZ-Connect cannot express.

## Resolved open questions

> Compare candidate upstreams on `(resolved IPs, port)` rather than on the
> literal `host:port` string — **or** keep the textual compare and make the
> misconfiguration visible?

**Decision: keep the textual compare; make the misconfiguration visible.** Do
*not* add DNS resolution to the connect path — the textual compare never
surprises, and a resolved compare can answer differently between two connects
of the same DSN. Implement the cheap half, which is also the half that addresses
what actually happened:

- Flag Oracle rows that share an `oracle_service_name` but disagree on host
  spelling — surface it in the connectivity check and on the server rows in the
  admin UI, so an operator sees the ambiguity before a client hits ORA-12514.
- `resolveDatabase` (`internal/proxy/oracle/session.go:602`) keeps comparing
  `host:port` textually. Its ORA-12514 refusal stays; it is correct to refuse an
  ambiguous service name. Improve the message if it does not already point at
  the conflicting rows.
- Still do the third bullet above: check `parseConnectDescriptor`'s handling of a
  quoted `(SERVICE_NAME=...)` and document the full descriptor as the escape
  hatch if it works.

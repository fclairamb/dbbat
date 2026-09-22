---
model: opus
effort: medium
---

# `dbbat dump decode`: read a capture as protocol messages without Wireshark

## Goal

`dbbat dump decode <in.pcapng>` prints a capture as one line per protocol
message, both directions, with the relative timestamp:

```
234ms C> Execute portal="" maxRows=501
251ms <S RowDescription(8 fields)
329ms <S PortalSuspended
329ms <S ReadyForQuery I
```

PostgreSQL first (`pgproto3` is already a dependency); the other four
protocols have frame parsers in their proxies and follow.

## Why

Diagnosing `2026-09-17-01-pg-extended-protocol-statement-terminators` meant
writing a throwaway decoder. The capture is Wireshark-readable by design, but
Wireshark is not on a build host, not on a laptop without a GUI session, and
not in a terminal-driven investigation. The `.pcapng` is the primary evidence
behind every statement-timeout termination and every "why did dbbat do that",
and today reading it needs a desktop tool. A CLI view also makes captures
usable in tests (assert a message sequence against a fixture) and in bug
reports (paste the decoded trace, not the file).

## Implementation

- `internal/dump/reader.go` already yields `Packet{RelativeNs, Direction,
  Data}`. Add `internal/dump/decode/` with a per-protocol splitter: buffer each
  direction, cut messages at the protocol's framing (PostgreSQL: one type byte
  plus an int32 length, with the untyped `StartupMessage` / `SSLRequest` and
  the one-byte SSL answer handled before the typed stream), decode each cut
  with `pgproto3.NewBackend` / `NewFrontend`, print one line per message.
- `main.go`: `dump decode <in> [--rows]` beside `dump anonymise`. Protocol
  from the capture header (`Header.Protocol`).
- Redact by default what a pasted trace would leak: `DataRow` values collapse
  to `DataRow(n cols)`, bind parameters to their count, auth messages are
  named but never dumped. `--rows` opts into values.
- MySQL, MongoDB, SQL Server and Oracle reuse the frame readers in
  `internal/proxy/<protocol>/`, one follow-up commit each.

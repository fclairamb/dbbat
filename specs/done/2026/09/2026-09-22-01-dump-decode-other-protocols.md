---
model: opus
effort: medium
---

# `dbbat dump decode` for MySQL, MongoDB, SQL Server and Oracle

## Goal

`dbbat dump decode` reads a PostgreSQL capture today and refuses the other four
by name. Extend it so every protocol dbbat proxies can be read as a message
trace from a terminal, with the same line shape and the same redaction default:

```
12ms  C> COM_QUERY "SELECT id FROM orders WHERE status = ?"
31ms  <S ResultSet(3 cols)
31ms  <S Row(3 cols)
```

## Why

`2026-09-17-03-dump-decode-cli` shipped the command and the PostgreSQL splitter
and deliberately deferred the rest: "MySQL, MongoDB, SQL Server and Oracle reuse
the frame readers in `internal/proxy/<protocol>/`, one follow-up commit each."
The argument for the command is protocol-independent — the `.pcapng` is the
primary evidence behind every statement-timeout termination, and Wireshark is
not on a build host — so a capture of an Oracle session is exactly as hard to
read today as a PostgreSQL one was before.

## Implementation

- `internal/dump/decode` already has the shape: `splitter` is the per-protocol
  interface, `newSplitter` dispatches on `Header.Protocol`, and `Supported`
  reports what is implemented. Each protocol adds one file implementing `Feed`,
  one line in `newSplitter`, one in `Supported`.
- The framing and the message names come from the proxies, which already parse
  every one of these on the wire:
  - MySQL: `internal/proxy/mysql/` — 3-byte length + sequence id, then the
    command byte; `go-mysql`'s packet helpers.
  - MongoDB: `internal/proxy/mongodb/` — `OP_MSG` header, then BSON; the command
    name is the first key of section 0.
  - SQL Server: `internal/proxy/mssql/` — the 8-byte TDS header, packet type and
    the `EOM` bit for multi-packet messages.
  - Oracle: `internal/proxy/oracle/` — TNS packet header then TTC function code;
    `internal/proxy/oracle/testdata/*.pcapng` is 186 recorded frames of ready-made
    fixtures to assert a golden trace against.
- Keep the redaction contract identical, since it is the reason a trace is
  pasteable: values collapse to counts by default, `--rows` opts in, and
  authentication payloads are never printed either way. On MongoDB that means
  documents are summarised (`find orders (filter: 2 keys)`), not dumped.
- Each protocol gets a golden test the way PostgreSQL has one: synthesized
  packets through the splitter for exact offsets and framing (one packet
  carrying several messages, one message spanning two), plus one end-to-end
  `File` test over a real capture written by `dump.Writer`.
- Drop each protocol out of `TestFile_UnsupportedProtocol` as it lands, and
  update `docs/dump-format.md` ("PostgreSQL is the only one decoded so far") and
  the CLI usage string.

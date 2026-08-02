---
model: opus
effort: high
---

# Replace the custom `.dbbat-dump` capture format with a tcpdump-compatible (pcapng) format

## Problem

Session captures currently use a bespoke binary format (`.dbbat-dump` v2: magic +
JSON header + 13-byte packet frames, spec in `docs/dump-format.md`, implementation
in `internal/dump/`). It works, but it is a format only dbbat can read: no
Wireshark/tcpdump/tshark tooling, no existing dissectors for the wrapped
protocols (PostgreSQL, MySQL, Oracle TNS, MongoDB — all of which Wireshark can
dissect natively), and every inspection or debugging task goes through our own
reader code.

Decision: switch the capture format to a tcpdump-compatible one so standard
tooling works on dumps out of the box, and convert the existing files with a
**one-off conversion tool that is never committed**.

## Proposal

### Format

- Use **pcapng** (not classic pcap): it supports per-packet direction flags
  (`epb_flags`), timestamps with nanosecond resolution, and option/comment
  fields where the current JSON session header (session_id, protocol,
  connection object) can be carried — e.g. as a Section Header Block comment or
  custom option. Classic pcap has none of that.
- Since we capture application-layer bytes only, **synthesize Ethernet/IP/TCP
  headers** around each captured payload (stable fake endpoints, real upstream
  port so dissector heuristics fire, monotonically advancing TCP seq numbers)
  so Wireshark can reassemble the stream and apply its protocol dissectors.
  Direction maps to client→server / server→client addressing.
- Pure-Go writing/reading via `gopacket/pcapgo` (no cgo/libpcap dependency).
- New file extension (`.pcapng`); `DBB_DUMP_*` env vars keep their names.

### Code touched

- `internal/dump/writer.go`, `reader.go`, `tapconn.go` — write/read pcapng
  instead of the v2 framing. The reader API (relative time, direction, payload)
  should stay stable so proxy replay tests don't change shape.
- `internal/dump/anonymise.go` + CLI `dbbat dump anonymise` — re-express
  "strip session metadata" as stripping the pcapng option/comment metadata.
- `internal/dump/cleanup.go` — retention scan must match the new extension.
- `internal/api/observability.go` — dump download endpoint: filename,
  content type.
- `docs/dump-format.md` — rewrite to describe the pcapng mapping (metadata
  placement, header synthesis rules) instead of the v2 binary layout.

### Conversion of existing files

- Write a small standalone converter (`.dbbat-dump` v1/v2 → pcapng) in the
  scratchpad or a gitignored path — **it must never be committed**. Run it once
  over:
  - the test fixtures `internal/proxy/oracle/testdata/*.dbbat-dump`
    (~15 files, consumed by `capture_*_test.go` and `dump_replay_test.go`),
    committing the converted `.pcapng` fixtures and deleting the old ones;
  - any operational dump directories (`DBB_DUMP_DIR`) as needed.
- After conversion, drop the v1/v2 read path entirely — no dual-format support
  in the committed code.

### Decisions

- Session metadata lives in the **Section Header Block comment** (JSON blob in
  the SHB `opt_comment` option). Verify `pcapgo` round-trips it; Wireshark
  displays SHB comments in capture file properties.
- Anonymise **also rewrites the synthesized IP addresses** (they may encode the
  real upstream). This rewrite is optional via a flag, **enabled by default**.
- No GitHub issue needed for this task.

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

## Implementation Plan

Verified up front (prototype, scratchpad): `github.com/gopacket/gopacket v1.7.0`
`pcapgo.NgWriter`/`NgReader` **does** round-trip the SHB `opt_comment`
(`NgWriterOptions.SectionInfo.Comment` → `NgReader.SectionInfo().Comment`) and the
EPB `opt_flags` direction bits (`NgPacketOptions.Flags`). `capinfos` shows the blob
as "Capture comment"; `tshark` dissects the synthesized frames as PGSQL. One gotcha:
`SnapLength: 0` (unlimited) makes Apple's libpcap `tcpdump` bail with
"invalid packet capture length … bigger than snaplen", so the interface must declare
an explicit snap length.

1. **`go.mod`** — add `github.com/gopacket/gopacket` (pure Go, no cgo/libpcap).
2. **`internal/dump/dump.go`** — `FileExt = ".pcapng"`; drop the v1/v2 magic /
   version / frame-size constants; keep `Header`, `Packet`, `Dir*`, `Protocol*`.
3. **`internal/dump/synth.go`** (new) — header synthesis:
   - LinkType Ethernet; snaplen 262144; ts resolution 9 (ns).
   - Client `10.77.0.1:54321`, MAC `02:db:ba:70:00:01`; server IP = upstream host
     when `connection.upstream_addr` is a literal IP, else `10.77.0.2`, MAC
     `02:db:ba:70:00:02`; server port = upstream port, else the protocol default
     (oracle 1521 / postgresql 5432 / mysql 3306 / mongodb 27017).
   - TCP PSH|ACK, window 65535, per-direction seq starting at 1 and advancing by
     payload length, ack = peer's next seq. Payloads over 65495 bytes are split
     into consecutive segments.
4. **`internal/dump/writer.go`** — `NewWriter` writes SHB (`opt_comment` = JSON
   `Header`, `Application = dbbat`) + IDB; `WritePacket` serializes eth/ip/tcp +
   payload into an EPB with `epb_flags` direction (client→server = inbound,
   server→client = outbound) and `StartTime + elapsed` as the timestamp. Keep the
   `maxSize` skip-silently behaviour and the mutex. `Close` flushes + closes (no
   EOF marker — pcapng ends at EOF).
5. **`internal/dump/reader.go`** — `OpenReader` parses the SHB comment into
   `Header`; `ReadPacket` decodes each EPB, drops non-TCP / zero-payload frames,
   derives `Direction` from `epb_flags` (fallback: source port), and computes
   `RelativeNs = ts - Header.StartTime`. API shape unchanged.
6. **`internal/dump/tapconn.go`** — unchanged API; only follows the writer.
7. **`internal/dump/anonymise.go`** — `Anonymise(in, out string, rewriteAddrs bool)`:
   strips the SHB comment down to `session_id` + `protocol`, and (when
   `rewriteAddrs`, the **default**) re-synthesizes every frame onto the fixed fake
   endpoints with the protocol default port, so the real upstream IP/port encoded in
   the headers is gone. Preserves relative timestamps and direction.
8. **`main.go`** — `dbbat dump anonymise` gains `--keep-addresses` (default false =
   addresses are rewritten); output default extension follows `.pcapng`.
9. **`internal/dump/cleanup.go`** — retention scan matches `.pcapng`.
10. **`internal/api/observability.go`** — `dumpFileExt = ".pcapng"`,
    `Content-Type: application/x-pcapng`.
11. **Fixtures** — standalone converter in the scratchpad (never committed) rewrites
    `internal/proxy/oracle/testdata/*.dbbat-dump` → `*.pcapng`; old files deleted.
    The unreferenced, zero-packet `dbeaver.dbbat-dump.v2` migration artifact is
    dropped. All `capture_*_test.go` / `dump_replay_test.go` / `anonymise_test.go`
    filenames updated.
12. **Docs sweep** — rewrite `docs/dump-format.md` around the pcapng mapping; update
    `README.md`, `CLAUDE.md`, `docs/{oracle,mysql,postgresql}.md`,
    `website/docs/**`, `website/src/components/HomepageFeatures`,
    `scripts/replay_dump.py`, `internal/proxy/*/integration_test.go`.
13. **QA** — unit tests (round-trip, direction, metadata, anonymise, cleanup, size
    cap), `go build ./...`, `make lint`, `make test`, `website` build, plus
    `capinfos`/`tshark`/`tcpdump` run against a generated file.

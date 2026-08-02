#!/usr/bin/env python3
"""Replay and analyze a dbbat .pcapng session capture.

Captures are plain pcapng, so `tshark -r <file>` works too; this script is the
dependency-free shortcut that also prints the dbbat session metadata stored in
the Section Header Block comment and strips the synthesized Ethernet/IP/TCP
headers back down to the raw protocol payloads.
"""

import json
import struct
import sys

BLOCK_SECTION_HEADER = 0x0A0D0D0A
BLOCK_INTERFACE_DESC = 0x00000001
BLOCK_ENHANCED_PACKET = 0x00000006

OPT_END = 0
OPT_COMMENT = 1
OPT_EPB_FLAGS = 2

ETHERNET_HEADER_LEN = 14


def parse_options(buf, endian):
    """Yield (code, value_bytes) pairs from a pcapng options blob."""
    off = 0
    while off + 4 <= len(buf):
        code, length = struct.unpack(endian + "HH", buf[off : off + 4])
        off += 4
        if code == OPT_END:
            return
        yield code, buf[off : off + length]
        off += length + ((4 - length % 4) % 4)


def strip_headers(frame):
    """Return the TCP payload of a synthesized Ethernet/IPv4/TCP frame."""
    if len(frame) < ETHERNET_HEADER_LEN + 20:
        return b""
    ethertype = struct.unpack(">H", frame[12:14])[0]
    if ethertype != 0x0800:
        return b""
    ip = frame[ETHERNET_HEADER_LEN:]
    ihl = (ip[0] & 0x0F) * 4
    total_len = struct.unpack(">H", ip[2:4])[0]
    if ip[9] != 6:  # not TCP
        return b""
    tcp = ip[ihl:total_len]
    data_offset = (tcp[12] >> 4) * 4
    return tcp[data_offset:]


def read_capture(path):
    with open(path, "rb") as f:
        blob = f.read()

    endian = "<"
    header = {}
    packets = []
    off = 0

    while off + 12 <= len(blob):
        block_type = struct.unpack(endian + "I", blob[off : off + 4])[0]

        if block_type == BLOCK_SECTION_HEADER:
            # Byte-order magic tells us how to read the rest of the section.
            endian = "<" if blob[off + 8 : off + 12] == b"\x4d\x3c\x2b\x1a" else ">"

        block_len = struct.unpack(endian + "I", blob[off + 4 : off + 8])[0]
        body = blob[off + 8 : off + block_len - 4]

        if block_type == BLOCK_SECTION_HEADER:
            for code, value in parse_options(body[16:], endian):
                if code == OPT_COMMENT:
                    try:
                        header = json.loads(value.decode("utf-8"))
                    except ValueError:
                        pass
        elif block_type == BLOCK_ENHANCED_PACKET:
            ts_hi, ts_lo, cap_len, _orig_len = struct.unpack(
                endian + "IIII", body[4:20]
            )
            timestamp = (ts_hi << 32) | ts_lo
            frame = body[20 : 20 + cap_len]
            options = body[20 + cap_len + ((4 - cap_len % 4) % 4) :]
            direction = 0
            for code, value in parse_options(options, endian):
                if code == OPT_EPB_FLAGS:
                    # bits 0-1: 1 = inbound (client->server), 2 = outbound
                    direction = 0 if (struct.unpack(endian + "I", value)[0] & 0b11) == 1 else 1
            packets.append((timestamp, direction, strip_headers(frame)))

        off += block_len

    return header, packets


def main(path):
    header, packets = read_capture(path)

    print(f"Session:  {header.get('session_id')}")
    print(f"Protocol: {header.get('protocol')}")
    print(f"Started:  {header.get('start_time')}")
    for k, v in (header.get("connection") or {}).items():
        print(f"  {k}: {v}")
    print()

    if not packets:
        print("No packets.")
        return

    start = packets[0][0]
    total_c2s = 0
    total_s2c = 0

    for n, (timestamp, direction, data) in enumerate(packets, start=1):
        dir_str = "C→S" if direction == 0 else "S→C"
        if direction == 0:
            total_c2s += len(data)
        else:
            total_s2c += len(data)

        # TNS packet type is at byte 4 of the raw TNS packet
        pkt_type = data[4] if len(data) > 4 else 0
        ms = (timestamp - start) / 1_000_000
        print(f"  #{n:3d} [{ms:8.1f}ms] {dir_str} type={pkt_type:2d} {len(data)} bytes")

    print(f"\nTotal: {len(packets)} packets")
    print(f"  Client→Server: {total_c2s:,} bytes")
    print(f"  Server→Client: {total_s2c:,} bytes")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"Usage: {sys.argv[0]} <capture.pcapng>")
        sys.exit(1)
    main(sys.argv[1])

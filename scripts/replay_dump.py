#!/usr/bin/env python3
"""Replay and analyze a .dbbat-dump file."""

import json
import struct
import sys


def read_dump(path):
    with open(path, "rb") as f:
        # File header: 16 magic + 2 version
        magic = f.read(16)
        assert magic[:10] == b"DBBAT_DUMP", f"Invalid magic: {magic[:10]}"
        version = struct.unpack(">H", f.read(2))[0]
        if version != 2:
            raise SystemExit(f"unsupported dump format version {version} (expected 2)")

        # JSON header: 4-byte BE length + JSON bytes
        header_len = struct.unpack(">I", f.read(4))[0]
        header = json.loads(f.read(header_len))

        print(f"Version:  {version}")
        print(f"Session:  {header.get('session_id')}")
        print(f"Protocol: {header.get('protocol')}")
        for k, v in (header.get("connection") or {}).items():
            print(f"  {k}: {v}")
        print()

        # Read packets: 8 relativeNs + 1 direction + 4 length + length bytes
        n = 0
        total_c2s = 0
        total_s2c = 0
        while True:
            rel_ns = struct.unpack(">q", f.read(8))[0]
            direction = f.read(1)[0]
            length = struct.unpack(">I", f.read(4))[0]
            if direction == 0xFF:
                print(f"\nEOF marker at {rel_ns / 1_000_000:.2f}ms")
                break
            data = f.read(length)
            n += 1

            dir_str = "C→S" if direction == 0 else "S→C"
            if direction == 0:
                total_c2s += length
            else:
                total_s2c += length

            # TNS packet type is at byte 4 of the raw TNS packet
            pkt_type = data[4] if len(data) > 4 else 0
            ms = rel_ns / 1_000_000
            print(
                f"  #{n:3d} [{ms:8.1f}ms] {dir_str} type={pkt_type:2d} {len(data)} bytes"
            )

        print(f"\nTotal: {n} packets")
        print(f"  Client→Server: {total_c2s:,} bytes")
        print(f"  Server→Client: {total_s2c:,} bytes")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"Usage: {sys.argv[0]} <dump-file>")
        sys.exit(1)
    read_dump(sys.argv[1])

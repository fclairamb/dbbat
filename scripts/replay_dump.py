#!/usr/bin/env python3
"""Replay and analyze a .dbbat-dump file."""

import struct
import sys
import uuid


def read_dump(path):
    with open(path, "rb") as f:
        # Read header
        magic = f.read(16)
        assert magic[:10] == b"DBBAT_DUMP", f"Invalid magic: {magic[:10]}"
        version = struct.unpack(">H", f.read(2))[0]
        session_uid = uuid.UUID(bytes=f.read(16))
        svc_len = f.read(1)[0]
        service = f.read(svc_len).decode()
        up_len = f.read(1)[0]
        upstream = f.read(up_len).decode()
        start_ns = struct.unpack(">q", f.read(8))[0]

        print(f"Version:  {version}")
        print(f"Session:  {session_uid}")
        print(f"Service:  {service}")
        print(f"Upstream: {upstream}")
        print()

        # Read packets
        n = 0
        total_c2s = 0
        total_s2c = 0
        while True:
            rel_ns = struct.unpack(">q", f.read(8))[0]
            direction = f.read(1)[0]
            if direction == 0xFF:
                break
            length = struct.unpack(">I", f.read(4))[0]
            data = f.read(length)
            n += 1

            dir_str = "C\u2192S" if direction == 0 else "S\u2192C"
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
        print(f"  Client\u2192Server: {total_c2s:,} bytes")
        print(f"  Server\u2192Client: {total_s2c:,} bytes")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print(f"Usage: {sys.argv[0]} <dump-file>")
        sys.exit(1)
    read_dump(sys.argv[1])

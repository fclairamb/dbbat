# Oracle TNS v315+ Packet Format Fix

## Goal

Fix TNS packet reading for Oracle TNS protocol version >= 315, where packets after the Connect/Accept exchange use a 4-byte length field instead of 2-byte. This enables the TNS-parsed relay (`proxyMessages`) to work correctly, which is required for query interception and logging.

## Root Cause

In TNS v315+ (used by modern Oracle clients like oracledb Python, JDBC thin):
- The Connect packet uses 2-byte length at bytes 0-1 (legacy format)
- ALL packets after Accept (Control, Data, Marker) use **4-byte length at bytes 0-3**
- The 2-byte length field reads as `0x0000` for these packets

Current `readTNSPacket` sees `Length=0 < 8` and returns a header-only packet with no payload, losing all the actual data. This corrupts the TCP stream and breaks the connection.

## Evidence

Raw packet capture of a successful direct Oracle connection:

| # | Dir | Type | Bytes 0-3 (hex) | 4-byte len | Actual size |
|---|-----|------|-----------------|-----------|-------------|
| 5 | C→S | Control | 0000000b | 11 | 11 |
| 6 | C→S | Data | 0000001d | 29 | 29 |
| 9 | C→S | Data | 00000a4c | 2636 | 2636 |
| 12 | S→C | Data | 00000173 | 371 | 371 |

## Fix

In `readTNSPacket`, when the 2-byte length is 0, read it as 4-byte big-endian:

```go
pkt.Length = binary.BigEndian.Uint16(raw[0:2])
if pkt.Length == 0 && len(raw) >= 4 {
    // TNS v315+: 4-byte length at bytes 0-3
    pkt.Length32 = binary.BigEndian.Uint32(raw[0:4])
}
```

Then switch from `rawRelay` to `proxyMessages` for query interception.

## Changes

1. `internal/proxy/oracle/tns.go` — Handle 4-byte length in readTNSPacket/writeTNSPacket
2. `internal/proxy/oracle/session.go` — Switch back to proxyMessages
3. `internal/proxy/oracle/tns_test.go` — Add tests for v315+ packet format

## Acceptance Criteria

1. Oracle proxy connects and queries work through proxyMessages
2. Queries are logged in the dbbat database
3. Existing unit tests pass

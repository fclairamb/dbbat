# Oracle SQL Text Extraction from TTC Protocol

## Goal

Extract SQL text from Oracle TTC protocol messages flowing through the proxy, enabling query logging for Oracle connections.

## Root Cause of Current Failure

The existing `decodeOALL8` assumes TTC function code `0x0E` for OALL8. In reality, modern Oracle 19c uses TTC function code `0x03` as a generic "piggyback" function, with a sub-operation code at byte 1:

| Sub-op (byte 1) | Purpose |
|------------------|---------|
| `0x5e` (94) | Execute with SQL (OALL8) |
| `0x76` (118) | AUTH Phase 1 |
| `0x73` (115) | AUTH Phase 2 |
| `0x09` | Close cursor (OCLOSE) |

For sub-op `0x5e`, the SQL is at a fixed offset:
- Byte 50: SQL length (1 byte for len < 254, or varlen encoding)
- Byte 51+: SQL text (UTF-8)

## Wire Format Evidence

Captured from `oracledb` Python client → Oracle 19c:

```
Offset  Value     Meaning
[0]     0x03      TTC function code (generic piggyback)
[1]     0x5e      Sub-operation: execute with SQL
[2-49]  ...       Cursor options, flags, parameters
[50]    0x12      SQL length = 18
[51-68] SELECT... SQL text "SELECT 1 FROM DUAL"
```

## Changes

1. `internal/proxy/oracle/session.go` — Update `interceptClientMessage` to detect func=0x03 sub=0x5e
2. `internal/proxy/oracle/ttc_decode.go` — Add `decodeOALL8v2` for the v315+ format
3. Tests with real captured payloads

## Acceptance Criteria

1. SQL text extracted from `SELECT 1 FROM DUAL` through proxy
2. SQL text extracted from `SELECT COUNT(*) FROM all_users` through proxy
3. Queries appear in `/api/v1/queries` endpoint
4. Connection record created in `/api/v1/connections`

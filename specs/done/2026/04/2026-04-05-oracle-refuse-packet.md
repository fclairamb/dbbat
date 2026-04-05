# Oracle: Proper TNS Refuse Packet Format

## Problem

When connecting to an undeclared database through the Oracle proxy, DBeaver shows:
```
SQL Error [17904] [08006]: ORA-17904: Bad packet type
```

The `sendRefuse()` function sends a raw text string as the TNS Refuse payload, but JDBC clients expect a structured format with a 4-byte header + Oracle descriptor string.

## Current State

- `sendRefuse(reason string)` in `session.go:506-518` sends `[]byte(reason)` directly as the TNS Refuse payload
- The comment acknowledges: "Real Oracle Refuse has a more structured format"
- 8 call sites across `session.go` and `auth.go`
- Upstream refuse forwarding (line 153-154) correctly passes through real Oracle packets untouched

## Goals

- Oracle clients (DBeaver, SQLDeveloper, sqlplus) receive a parseable error with a meaningful ORA code
- Map each refusal reason to an appropriate Oracle error code

## Design

### TNS Refuse Payload Format

Real Oracle TNS Refuse packets have this structure:
```
Bytes 0-1: User reason (uint16 BE) - 0x0004 = user error
Bytes 2-3: System reason (uint16 BE) - 0x0000
Bytes 4+:  Oracle descriptor string, e.g.:
  (DESCRIPTION=(ERR=12514)(VSNNUM=0)(ERROR_STACK=(ERROR=(CODE=12514)(EMFI=4)(ARGS='(database not found)'))))
```

### Error Code Constants

Add to `internal/proxy/oracle/errors.go`:

```go
const (
    ORA12505 uint16 = 12505 // TNS:listener does not currently know of SID
    ORA12514 uint16 = 12514 // TNS:listener does not currently know of service
    ORA12520 uint16 = 12520 // TNS:listener could not find available handler
    ORA12535 uint16 = 12535 // TNS:operation timed out
    ORA12541 uint16 = 12541 // TNS:no listener
)
```

### Updated `sendRefuse` Signature

```go
func (s *session) sendRefuse(oraCode uint16, reason string)
```

### Call Site Mapping

| File:Line | Error code | Reason |
|-----------|-----------|--------|
| `session.go:83` | ORA12520 | expected TNS Connect packet |
| `session.go:108` | ORA12505 | missing SERVICE_NAME in connect descriptor |
| `session.go:120` | ORA12514 | database not found |
| `session.go:133` | ORA12541 | cannot reach upstream database |
| `session.go:140` | ORA12541 | upstream connection failed |
| `session.go:231` | ORA12535 | upstream did not respond |
| `session.go:249` | ORA12535 | too many resend attempts |
| `auth.go:61` | ORA12514 | access denied (from `checkAccess()`) |

### Files to Modify

- `internal/proxy/oracle/errors.go` - add ORA code constants
- `internal/proxy/oracle/session.go` - update `sendRefuse` + 7 call sites
- `internal/proxy/oracle/auth.go` - update 1 call site
- `internal/proxy/oracle/session_test.go` - update `TestSession_SendRefuse` to verify structured payload

### What NOT to Touch

- Upstream refuse forwarding (`session.go:153-154`) - those are real Oracle packets passed through as-is

## Verification

```bash
make test
```

Then manually test with DBeaver connecting to an undeclared Oracle service name through the proxy - should now show `ORA-12514` instead of `ORA-17904`.

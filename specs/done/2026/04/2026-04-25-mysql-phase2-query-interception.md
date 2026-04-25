# MySQL Proxy — Phase 2: Query Interception

> Parent spec: `2026-04-25-mysql-proxy.md`
> Depends on: `2026-04-25-mysql-phase1-connection-auth.md`

## Goal

Replace Phase 1's raw `io.Copy` relay with a command-aware loop that:
- Logs every `COM_QUERY`, `COM_STMT_PREPARE`, `COM_STMT_EXECUTE`, and `COM_INIT_DB`
- Applies grant controls (read-only, block_ddl, block_copy)
- Blocks MySQL-specific dangerous operations (`LOAD DATA INFILE`, `SELECT INTO OUTFILE`, replication commands)
- Refuses `LOAD DATA LOCAL INFILE` at the protocol level (the `0xFB` server-to-client packet)
- Updates `connections.queries` and `connections.bytes_transferred`

No result row capture yet — Phase 3.

## Outcome

### Files added
```
internal/proxy/mysql/intercept.go       # command dispatcher + handlers
internal/proxy/mysql/intercept_test.go
internal/proxy/mysql/prepared.go        # prepared statement registry per session
internal/proxy/mysql/blocked.go         # MySQL-specific blocked patterns
internal/proxy/mysql/blocked_test.go
```

### Files modified
```
internal/proxy/mysql/session.go         # replace proxyLoop with intercept loop
internal/proxy/shared/validation.go     # add ValidateMySQLQuery + REPLACE keyword
```

## Command Dispatch

go-mysql's `server.Conn` exposes a `HandleCommand` method that returns the next command from the client. We loop over commands and dispatch:

```go
func (s *Session) interceptLoop() error {
    for {
        data, err := s.serverConn.ReadPacket()
        if err != nil { return err }

        cmd := data[0]
        payload := data[1:]

        switch cmd {
        case mysql.COM_QUERY:
            err = s.handleQuery(string(payload))
        case mysql.COM_STMT_PREPARE:
            err = s.handlePrepare(string(payload))
        case mysql.COM_STMT_EXECUTE:
            err = s.handleStmtExecute(payload)
        case mysql.COM_STMT_CLOSE:
            s.prepared.Delete(binary.LittleEndian.Uint32(payload))
            err = s.forwardCommand(data)
        case mysql.COM_STMT_RESET:
            err = s.forwardCommand(data)
        case mysql.COM_INIT_DB:
            err = s.handleInitDB(string(payload))
        case mysql.COM_PING:
            err = s.forwardCommand(data)
        case mysql.COM_QUIT:
            return nil
        case mysql.COM_BINLOG_DUMP, mysql.COM_BINLOG_DUMP_GTID,
             mysql.COM_REGISTER_SLAVE, mysql.COM_TABLE_DUMP,
             mysql.COM_SHUTDOWN, mysql.COM_PROCESS_KILL, mysql.COM_DEBUG:
            err = s.refuseCommand(cmd, "command not permitted through dbbat")
        default:
            err = s.forwardCommand(data)
        }
        if err != nil { return err }
    }
}
```

## Query handler

```go
func (s *Session) handleQuery(sql string) error {
    if err := s.checkQuotas(); err != nil {
        return s.sendErrorPacket(ER_QUERY_INTERRUPTED, err.Error())
    }

    if err := shared.ValidateMySQLQuery(sql, s.grant); err != nil {
        s.recordBlockedQuery(sql, err)
        return s.sendErrorPacket(ER_SPECIFIC_ACCESS_DENIED_ERROR, err.Error())
    }

    s.currentQuery = &pendingQuery{
        sql:       sql,
        startTime: time.Now(),
    }

    // Forward COM_QUERY to upstream
    if err := s.upstreamConn.WriteCommand(mysql.COM_QUERY, []byte(sql)); err != nil {
        return err
    }

    // Relay response packets back to client (Phase 3 will also capture rows here)
    return s.relayResponse()
}
```

## Prepared statement registry

`COM_STMT_PREPARE` returns a statement ID + column/parameter metadata. We store `(stmt_id → sql, param_count)` per session. `COM_STMT_EXECUTE` references the statement ID; we look up the SQL for logging.

```go
// internal/proxy/mysql/prepared.go
type preparedStmt struct {
    sql        string
    paramCount int
}
type preparedRegistry struct {
    mu sync.Mutex
    stmts map[uint32]*preparedStmt
}
```

`COM_STMT_EXECUTE` payload encodes parameters using a type-tagged binary format. For Phase 2 we log the SQL + a base64 of the parameter blob; Phase 3 (binary row capture) is when we'd parse parameters per type. Alternatively, we can parse parameters in Phase 2 — but parameter parsing is independent of result row parsing, so it's a judgment call. **Decision: Phase 2 logs parameters as base64 only, with a `format: "binary-raw"` marker. Real parameter decoding lands with binary row capture in v2.**

## MySQL blocked patterns

```go
// internal/proxy/mysql/blocked.go (used via shared.ValidateMySQLQuery)
var mysqlBlockedPatterns = []*regexp.Regexp{
    regexp.MustCompile(`(?i)\bLOAD\s+DATA\s+(?:LOCAL\s+)?INFILE\b`),
    regexp.MustCompile(`(?i)\bINTO\s+(?:OUT|DUMP)FILE\b`),
    // SET GLOBAL — privilege escalation
    regexp.MustCompile(`(?i)\bSET\s+GLOBAL\s+`),
    // SET PASSWORD — covered by IsPasswordChangeQuery? Add explicit pattern.
    regexp.MustCompile(`(?i)\bSET\s+PASSWORD\b`),
}
```

Then in `internal/proxy/shared/validation.go`:
```go
func ValidateMySQLQuery(sql string, grant *store.Grant) error {
    if err := ValidateQuery(sql, grant); err != nil { return err }
    for _, p := range mysqlBlockedPatterns { if p.MatchString(sql) { return ErrMySQLPatternBlocked } }
    return nil
}
var ErrMySQLPatternBlocked = errors.New("blocked: this MySQL operation is not permitted through the proxy")
```

Add `REPLACE` to `writeKeywords` (it's a MySQL upsert that writes data).

## LOCAL INFILE refusal at protocol level

Even if the client sends `LOAD DATA LOCAL INFILE` and we miss it via regex, MySQL's protocol response from the server includes a `0xFB` packet asking the *client* to upload a file. **The proxy must refuse this packet** if it ever arrives. In `relayResponse()`:

```go
if response[0] == 0xFB {
    // Server is asking client to upload file. Block.
    s.logger.WarnContext(s.ctx, "LOCAL INFILE protocol refused", ...)
    // Send empty file packet back to upstream to gracefully close the request
    if err := s.upstreamConn.WritePacket([]byte{}); err != nil { return err }
    // Then send ER to client
    return s.sendErrorPacket(ER_NOT_ALLOWED_COMMAND, "LOCAL INFILE not permitted")
}
```

## INIT_DB handling

`COM_INIT_DB` (`USE database`) changes the session's current database. The grant is database-scoped, so switching to a different database mid-session is suspicious. **Decision: refuse `COM_INIT_DB` if it would change to a different database than the one in the original handshake.** This matches how the PG proxy doesn't allow `\c` to switch databases mid-session.

```go
func (s *Session) handleInitDB(dbName string) error {
    if dbName != s.database.DatabaseName {
        return s.sendErrorPacket(ER_DBACCESS_DENIED_ERROR,
            "switching database not permitted through dbbat")
    }
    // No-op: already on this database. Send OK without forwarding.
    return s.sendOKPacket()
}
```

## Logging

Each successfully forwarded query inserts a row into `queries`. After upstream returns `OK` (with `affected_rows`) or `ERR`, update the row with `duration_ms`, `rows_affected`, and `error`. Use the same `pendingQuery` pattern as PG.

## Tests

### Unit
- `intercept_test.go`:
  - `TestQuery_AllowedSelect_Forwarded`
  - `TestQuery_ReadOnlyBlocksInsert`
  - `TestQuery_BlockDDLBlocksCreate`
  - `TestQuery_LoadDataInfile_Refused`
  - `TestQuery_SelectIntoOutfile_Refused`
  - `TestQuery_SetGlobal_Refused`
  - `TestStmt_PrepareExecute_LoggedWithSQL`
  - `TestInitDB_DifferentDB_Refused`
  - `TestInitDB_SameDB_OK`
  - `TestProtocolCommands_BinlogDump_Refused`
  - `TestProtocolCommands_Shutdown_Refused`
  - `TestLocalInfile_ProtocolRefusal`
- `blocked_test.go`:
  - Pattern coverage for each blocked pattern + false-positive checks

### Integration
- `TestQuery_Logged` — execute `SELECT 1`, verify `queries` row appears with correct SQL/timing
- `TestQuery_QuotaExceeded` — set max_query_count=1, verify second query refused
- `TestPrepare_LoggedAsPrepare` — execute `?`-parametrized query, verify SQL + parameters captured

## Verification checklist

- [ ] `make lint` clean, `make test` passes
- [ ] Manual: `mysql -h 127.0.0.1 -P 3307 ... -e "SELECT 1"` succeeds, `queries` row appears
- [ ] Manual: `mysql ... -e "INSERT INTO ..."` against a read-only grant → ERROR 1227 with our message
- [ ] Manual: `mysql ... -e "LOAD DATA LOCAL INFILE 'x' INTO TABLE t"` → blocked
- [ ] Manual: `mysqldump` (which issues `SHOW`/`SELECT` only) succeeds; against read-only grant the actual dump still works for SELECT-based dumps

## Out of scope

- Result row capture (Phase 3)
- Binary parameter decoding (v2)
- Stored procedure routine introspection (v2)

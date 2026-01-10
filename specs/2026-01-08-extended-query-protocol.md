# Spec: Support Extended Query Protocol for Query Logging

**Date**: 2026-01-08
**Status**: Draft
**Author**: Claude

## Summary

PgLens currently only logs queries sent via the **Simple Query Protocol** (`Query` message), but does not log queries sent via the **Extended Query Protocol** (`Parse`, `Bind`, `Execute` messages). This causes queries from JDBC clients (DBeaver, pgJDBC, etc.) to be untracked.

This specification outlines the changes needed to support query logging for the Extended Query Protocol.

## Problem Statement

### Observed Behavior

When connecting to PgLens via DBeaver (which uses the PostgreSQL JDBC driver), queries are executed but not stored in the `queries` table. The logs show:

```json
{"msg":"received message from client","message":{"Type":"Parse","Query":"SELECT td.* FROM public.test_data AS td"}}
{"msg":"received message from client","message":{"Type":"Bind"}}
{"msg":"received message from client","message":{"Type":"Execute","MaxRows":200}}
{"msg":"received message from client","message":{"Type":"Sync"}}
```

These are **Extended Query Protocol** messages, not the `Query` message that PgLens currently intercepts.

### Root Cause

In `internal/proxy/session.go:130-139`, only `*pgproto3.Query` messages are intercepted:

```go
// Handle query interception
if query, ok := msg.(*pgproto3.Query); ok {
    if err := s.handleQuery(query); err != nil {
        s.sendQueryError(err)
        continue
    }
}
```

All other message types (Parse, Bind, Execute, Describe, Sync) pass through untracked.

### PostgreSQL Protocol Background

PostgreSQL supports two query protocols:

1. **Simple Query Protocol**: A single `Query` message containing the SQL text. Response includes row data and `ReadyForQuery`.

2. **Extended Query Protocol**: Multiple messages for parameterized/prepared statements:
   - `Parse`: Prepares a statement (contains SQL text, optionally a statement name)
   - `Bind`: Binds parameters to a prepared statement, creates a portal
   - `Describe`: Requests row/parameter descriptions
   - `Execute`: Executes a portal
   - `Sync`: Ends the extended query sequence

JDBC drivers (and many other clients) use the Extended Query Protocol because:
- It supports parameterized queries (prevents SQL injection)
- It allows statement reuse (prepare once, execute many times)
- It provides detailed type information via Describe

## Design

### Tracking State

The Extended Query Protocol requires tracking state between messages:

```go
// extendedQueryState tracks state for Extended Query Protocol
type extendedQueryState struct {
    // Parsed statements: name -> SQL text
    // Empty name "" is the unnamed statement
    preparedStatements map[string]string

    // Current portal being executed
    // Tracks which statement a portal refers to
    portals map[string]string // portal name -> statement name

    // Active query being executed (set when Execute is received)
    activeQuery *pendingQuery
}
```

### Message Handling Flow

```
Client                         PgLens                              Upstream
  |                              |                                     |
  |-- Parse(sql, stmt="") ------>| store stmt[""] = sql                |
  |                              |-- forward Parse ------------------>|
  |-- Bind(stmt="", portal="") ->| store portal[""] = stmt[""]        |
  |                              |-- forward Bind ------------------->|
  |-- Execute(portal="") ------->| start tracking query from stmt     |
  |                              |-- forward Execute ---------------->|
  |                              |<-- DataRow ------------------------|
  |<-- DataRow ------------------|  (track bytes)                     |
  |                              |<-- CommandComplete ----------------|
  |<-- CommandComplete ----------|  (capture rows affected)           |
  |                              |<-- ReadyForQuery ------------------|
  |<-- ReadyForQuery ------------|  log query to DB                   |
```

### Named Prepared Statements

Named prepared statements are prepared once and can be executed multiple times:

```
-- First execution
Parse("SELECT * FROM t WHERE id = $1", stmt="s1")  -> store preparedStatements["s1"]
Bind(stmt="s1", params=[1])                        -> create portal
Execute()                                          -> lookup preparedStatements["s1"], log query

-- Second execution (no Parse needed)
Bind(stmt="s1", params=[2])                        -> reuse preparedStatements["s1"]
Execute()                                          -> log query again with same SQL
```

### Read-Only Enforcement

Read-only enforcement must be applied at `Parse` time (before the statement is prepared):

```go
func (s *Session) handleParse(msg *pgproto3.Parse) error {
    if s.grant.AccessLevel == "read" && isWriteQuery(msg.Query) {
        return ErrWriteNotPermitted
    }
    // Store prepared statement
    s.extendedState.preparedStatements[msg.Name] = msg.Query
    return nil
}
```

### Quota Enforcement

Quota checks should happen at `Execute` time (when the query is actually executed):

```go
func (s *Session) handleExecute(msg *pgproto3.Execute) error {
    if err := s.checkQuotas(); err != nil {
        return err
    }
    // Start tracking the query
    portalName := msg.Portal
    stmtName := s.extendedState.portals[portalName]
    sql := s.extendedState.preparedStatements[stmtName]
    s.currentQuery = &pendingQuery{
        sql:       sql,
        startTime: time.Now(),
    }
    return nil
}
```

## Implementation

### Changes to `internal/proxy/session.go`

#### 1. Add Extended Query State

```go
// extendedQueryState tracks state for Extended Query Protocol
type extendedQueryState struct {
    preparedStatements map[string]string // stmt name -> SQL
    portals            map[string]string // portal name -> stmt name
}

type Session struct {
    // ... existing fields ...
    extendedState *extendedQueryState
}

func NewSession(...) *Session {
    return &Session{
        // ... existing initialization ...
        extendedState: &extendedQueryState{
            preparedStatements: make(map[string]string),
            portals:            make(map[string]string),
        },
    }
}
```

#### 2. Update `proxyClientToUpstream()`

```go
func (s *Session) proxyClientToUpstream() error {
    for {
        msg, err := s.clientBackend.Receive()
        if err != nil {
            // ... existing error handling ...
        }

        s.logger.Info("received message from client", "message", msg)

        // Handle query interception
        var interceptErr error
        switch m := msg.(type) {
        case *pgproto3.Query:
            // Simple Query Protocol (existing)
            interceptErr = s.handleQuery(m)

        case *pgproto3.Parse:
            // Extended Query Protocol - prepare statement
            interceptErr = s.handleParse(m)

        case *pgproto3.Bind:
            // Extended Query Protocol - bind parameters
            s.handleBind(m)

        case *pgproto3.Execute:
            // Extended Query Protocol - execute
            interceptErr = s.handleExecute(m)

        case *pgproto3.Close:
            // Extended Query Protocol - close statement/portal
            s.handleClose(m)
        }

        if interceptErr != nil {
            s.sendQueryError(interceptErr)
            continue
        }

        // Forward message to upstream
        s.upstreamFrontend.Send(msg)
        if err := s.upstreamFrontend.Flush(); err != nil {
            return fmt.Errorf("failed to send to upstream: %w", err)
        }
    }
}
```

### Changes to `internal/proxy/intercept.go`

Add new handler functions:

```go
// handleParse handles Parse messages (prepared statement creation)
func (s *Session) handleParse(msg *pgproto3.Parse) error {
    sqlText := msg.Query

    // Check for read-only enforcement
    if s.grant.AccessLevel == "read" && isWriteQuery(sqlText) {
        return ErrWriteNotPermitted
    }

    // Store the prepared statement
    s.extendedState.preparedStatements[msg.Name] = sqlText

    return nil
}

// handleBind handles Bind messages (portal creation)
func (s *Session) handleBind(msg *pgproto3.Bind) {
    // Map portal to prepared statement
    s.extendedState.portals[msg.DestinationPortal] = msg.PreparedStatement
}

// handleExecute handles Execute messages (query execution)
func (s *Session) handleExecute(msg *pgproto3.Execute) error {
    // Check quotas before executing
    if err := s.checkQuotas(); err != nil {
        return err
    }

    // Look up the SQL for this portal
    stmtName := s.extendedState.portals[msg.Portal]
    sqlText, ok := s.extendedState.preparedStatements[stmtName]
    if !ok {
        // Statement not found - might be using unnamed statement without Parse
        // This shouldn't happen in normal usage
        s.logger.Warn("execute for unknown statement", "portal", msg.Portal, "stmt", stmtName)
        return nil
    }

    // Start tracking query for logging
    s.currentQuery = &pendingQuery{
        sql:       sqlText,
        startTime: time.Now(),
    }

    return nil
}

// handleClose handles Close messages (cleanup)
func (s *Session) handleClose(msg *pgproto3.Close) {
    switch msg.ObjectType {
    case 'S': // Statement
        delete(s.extendedState.preparedStatements, msg.Name)
    case 'P': // Portal
        delete(s.extendedState.portals, msg.Name)
    }
}
```

### Edge Cases

#### 1. Multiple Execute Messages per Sync

A client might send multiple Execute messages before a Sync:

```
Parse(sql1) -> Bind -> Execute -> Parse(sql2) -> Bind -> Execute -> Sync
```

This creates two `CommandComplete` responses. We need to track queries in order:

```go
type extendedQueryState struct {
    preparedStatements map[string]string
    portals            map[string]string
    pendingQueries     []*pendingQuery // Queue of pending queries
}

func (s *Session) handleExecute(msg *pgproto3.Execute) error {
    // ... validation ...
    query := &pendingQuery{sql: sqlText, startTime: time.Now()}
    s.extendedState.pendingQueries = append(s.extendedState.pendingQueries, query)
    return nil
}
```

Then in `proxyUpstreamToClient()`, pop from the queue on each `CommandComplete`:

```go
case *pgproto3.CommandComplete:
    if len(s.extendedState.pendingQueries) > 0 {
        s.currentQuery = s.extendedState.pendingQueries[0]
        s.extendedState.pendingQueries = s.extendedState.pendingQueries[1:]
    }
    rowsAffected = parseRowsAffected(string(m.CommandTag))
```

#### 2. ErrorResponse Handling

If a query fails, the server sends an `ErrorResponse` instead of `CommandComplete`. We need to handle this:

```go
case *pgproto3.ErrorResponse:
    errMsg := m.Message
    queryError = &errMsg
    // Still need to consume a pending query
    if len(s.extendedState.pendingQueries) > 0 {
        s.currentQuery = s.extendedState.pendingQueries[0]
        s.extendedState.pendingQueries = s.extendedState.pendingQueries[1:]
    }
```

#### 3. DEALLOCATE Statement

Clients might use `DEALLOCATE statement_name` via Simple Query Protocol to close prepared statements:

```go
func (s *Session) handleQuery(query *pgproto3.Query) error {
    sqlText := query.String

    // Check for DEALLOCATE
    if isDeallocate(sqlText) {
        stmtName := extractDeallocateName(sqlText)
        delete(s.extendedState.preparedStatements, stmtName)
    }

    // ... existing handling ...
}
```

### Testing

#### Unit Tests

Add tests for the new handlers in `intercept_test.go`:

```go
func TestHandleParse_ReadOnly(t *testing.T) {
    s := &Session{
        grant:         &store.Grant{AccessLevel: "read"},
        extendedState: newExtendedQueryState(),
    }

    // Write query should fail
    err := s.handleParse(&pgproto3.Parse{Query: "INSERT INTO t VALUES (1)"})
    if err != ErrWriteNotPermitted {
        t.Errorf("expected ErrWriteNotPermitted, got %v", err)
    }

    // Read query should succeed
    err = s.handleParse(&pgproto3.Parse{Query: "SELECT * FROM t"})
    if err != nil {
        t.Errorf("expected nil, got %v", err)
    }
}

func TestExtendedQueryFlow(t *testing.T) {
    s := &Session{
        grant:         &store.Grant{AccessLevel: "write"},
        extendedState: newExtendedQueryState(),
    }

    // Parse
    s.handleParse(&pgproto3.Parse{Name: "", Query: "SELECT * FROM t"})
    if s.extendedState.preparedStatements[""] != "SELECT * FROM t" {
        t.Error("statement not stored")
    }

    // Bind
    s.handleBind(&pgproto3.Bind{DestinationPortal: "", PreparedStatement: ""})
    if s.extendedState.portals[""] != "" {
        t.Error("portal not mapped")
    }

    // Execute
    s.handleExecute(&pgproto3.Execute{Portal: ""})
    if s.currentQuery == nil || s.currentQuery.sql != "SELECT * FROM t" {
        t.Error("query not tracked")
    }
}
```

#### Integration Tests

Test with actual DBeaver/JDBC connection to verify queries are logged.

### Verification

After implementation, verify by:

1. Connect to PgLens with DBeaver
2. Run queries: `SELECT * FROM test_data`
3. Check that queries appear in `GET /api/queries`
4. Verify `sql_text`, `duration_ms`, `rows_affected` are populated correctly

## Files to Modify

| File | Changes |
|------|---------|
| `internal/proxy/session.go` | Add `extendedQueryState`, update `proxyClientToUpstream()` |
| `internal/proxy/intercept.go` | Add `handleParse()`, `handleBind()`, `handleExecute()`, `handleClose()` |
| `internal/proxy/intercept_test.go` | Add tests for new handlers |

## Risks and Mitigations

### Risk: Memory Usage for Long Sessions

Prepared statements accumulate in memory for the session lifetime.

**Mitigation**:
- Handle `Close` messages to clean up
- Consider adding a max statement limit
- Statements are cleaned up when the connection closes

### Risk: Complex Multi-Statement Batches

Some clients send complex batches with multiple statements.

**Mitigation**:
- Use a queue (`pendingQueries`) instead of single `currentQuery`
- Test with various JDBC driver configurations

### Risk: Partial Query Logging

If PgLens crashes between Execute and logging, queries may be lost.

**Mitigation**:
- This is existing behavior with Simple Query Protocol too
- Asynchronous logging is intentional for performance
- Consider optional synchronous logging mode for audit-critical deployments

## Success Criteria

1. Queries from DBeaver/JDBC clients appear in `queries` table
2. All existing Simple Query Protocol functionality continues to work
3. Read-only enforcement works for Extended Query Protocol
4. Quota enforcement works for Extended Query Protocol
5. All existing tests pass
6. New tests cover Extended Query Protocol flows

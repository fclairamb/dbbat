package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"

	goora "github.com/sijms/go-ora/v3"
)

// executeOracle runs one statement against the Oracle proxy listener.
//
// The connect string is the EZ-Connect shape dbbat hands a human
// (`user/key@host:port/<dbbat database name>`): what travels in the TNS connect
// descriptor's SERVICE_NAME is the **dbbat entry's name**, never the upstream
// SERVICE_NAME. The proxy's session resolver does an exact GetServerByName
// lookup on it, and several dbbat entries may legitimately proxy one mutualized
// upstream service — a raw service name would resolve to an arbitrary one of
// them.
//
// The leg is plaintext: the Oracle listener speaks plain TNS (dbbat terminates
// no TLS on it), and the socket never leaves loopback.
func executeOracle(ctx context.Context, addr string, req ExecRequest) (*QueryResult, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrListenerDisabled, addr)
	}

	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrListenerDisabled, addr)
	}

	// PROGRAM is what the proxy folds into the upstream login, so a DBA reading
	// v$session can tell an agent's session from a human's.
	//
	// Deliberately no TIMEOUT / READ TIMEOUT option: go-ora would arm real
	// socket deadlines, and an approval hold parks the statement on a human for
	// as long as it takes. Cancellation is the execution context's job
	// (ExecutionMaxLifetime), which closes the socket — the proxy's ordinary
	// client-disconnect path, ending the hold as `abandoned`.
	dsn := goora.BuildUrl(host, port, req.Database, req.Username, req.APIKey, map[string]string{
		"PROGRAM": mcpApplicationName,
	})

	db := sql.OpenDB(goora.NewConnector(dsn))

	// One statement, one loopback session. Pooling would keep a proxy session —
	// and the connection row that goes with it — alive past the execution that
	// opened it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)

	defer func() { _ = db.Close() }()

	return executeSQLDB(ctx, db, req, oracleIsExecStatement(req.SQL))
}

// oracleIsExecStatement reports whether an Oracle statement should go through
// ExecContext.
//
// Oracle is the easy dialect for this: DML never hands a client a result set —
// `RETURNING` writes into bind outputs rather than opening a cursor — so the
// leading keyword decides it outright. Anything unrecognized (including a
// statement that opens with a comment) goes down the query path.
func oracleIsExecStatement(sqlText string) bool {
	switch leadingKeyword(sqlText) {
	case "insert", "update", "delete", "merge",
		"create", "alter", "drop", "truncate", "grant", "revoke", "comment",
		"begin", "declare", "call", "commit", "rollback", "savepoint", "set":
		return true
	default:
		return false
	}
}

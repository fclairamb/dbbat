package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// executeMSSQL runs one statement against the SQL Server (TDS) proxy listener.
//
// The connection asks for `encrypt=disable`, which puts ENCRYPT_NOT_SUP in
// PRELOGIN. The proxy never *requires* encryption on its client leg — a client
// that cannot do TLS is always let through in cleartext, whether or not
// DBB_MSSQL_TLS_DISABLE is set (see mssql.negotiateEncryption) — so the
// loopback leg needs no certificate decision at all. It also sidesteps
// DBB_MSSQL_TLS_MAX_VERSION entirely: there is no handshake to cap. The socket
// never leaves loopback, so nothing is given up.
//
// The database in the connection string is the **dbbat entry's name**: the
// proxy maps the LOGIN7 database field onto a dbbat server row, exactly as it
// does for a human's `database=` parameter.
func executeMSSQL(ctx context.Context, addr string, req ExecRequest) (*QueryResult, error) {
	query := url.Values{
		"database": {req.Database},
		"encrypt":  {"disable"},
		"app name": {mcpApplicationName},
		// Bounds the handshake with our own listener: reaching loopback is
		// either immediate or broken.
		"dial timeout": {"10"},
		// Explicitly no statement timeout. An approval hold parks the statement
		// on a human for as long as it takes, and a driver-side clock would
		// cancel it; the execution context (ExecutionMaxLifetime) is the bound,
		// and canceling it closes the socket — the proxy's ordinary
		// client-disconnect path.
		"connection timeout": {"0"},
	}

	dsn := (&url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(req.Username, req.APIKey),
		Host:     addr,
		RawQuery: query.Encode(),
	}).String()

	connector, err := mssql.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("building the loopback SQL Server config: %w", err)
	}

	db := sql.OpenDB(connector)

	// One statement, one loopback session: pooling would keep a proxy session —
	// and its connection row — alive past the execution that opened it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)

	defer func() { _ = db.Close() }()

	return executeSQLDB(ctx, db, req, mssqlIsExecStatement(req.SQL))
}

// mssqlIsExecStatement reports whether a T-SQL statement should go through
// ExecContext.
//
// Unlike Oracle, a SQL Server write can return rows: `DELETE … OUTPUT
// deleted.*` hands the client a result set. A statement mentioning OUTPUT as a
// standalone word therefore falls back to the query path, which keeps the rows
// and merely omits `rows_affected` — the safe direction. A false positive (a
// column genuinely named `output` elsewhere in the statement) costs nothing
// more than that.
func mssqlIsExecStatement(sqlText string) bool {
	switch leadingKeyword(sqlText) {
	case "insert", "update", "delete", "merge",
		"create", "alter", "drop", "truncate", "grant", "revoke",
		"commit", "rollback", "use", "set":
	default:
		return false
	}

	return !containsIdentifier(sqlText, "output")
}

// containsIdentifier reports whether text contains word as a standalone
// identifier, case-insensitively. word must already be lower-case.
// Only "output" needs it today; the word stays a parameter so the boundary
// rule reads as the general thing it is.
//
//nolint:unparam // see above
func containsIdentifier(text, word string) bool {
	lower := strings.ToLower(text)

	for offset := 0; offset < len(lower); {
		idx := strings.Index(lower[offset:], word)
		if idx < 0 {
			return false
		}

		start := offset + idx
		end := start + len(word)

		beforeOK := start == 0 || !isIdentByte(lower[start-1])
		afterOK := end == len(lower) || !isIdentByte(lower[end])

		if beforeOK && afterOK {
			return true
		}

		offset = start + 1
	}

	return false
}

func isIdentByte(c byte) bool {
	return isLetter(c) || (c >= '0' && c <= '9') || c == '_' || c == '$' || c == '#'
}

package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/fclairamb/dbbat/internal/store"
)

// Execution errors surfaced to the agent as tool errors.
var (
	// ErrProtocolUnsupported means the database speaks a protocol the MCP
	// server cannot drive a loopback client for yet. Phase 1 is PostgreSQL
	// and MySQL/MariaDB; Oracle, MongoDB and SQL Server are Phase 2.
	ErrProtocolUnsupported = errors.New("protocol not supported by the MCP server yet")
	// ErrListenerDisabled means the proxy listener for that protocol is not
	// running in this process, so there is nothing to dial. The MCP server
	// refuses rather than reaching around the proxy.
	ErrListenerDisabled = errors.New("the proxy listener for this protocol is disabled on this instance")
)

// SupportedProtocol reports whether the MCP server can execute statements
// against a database speaking this protocol.
//
// The dispatch is one switch on purpose: adding Oracle, MongoDB or SQL Server
// (Phase 2) means adding a case here and a loopback client next to the two
// that exist, and nothing else — no new enforcement, no new auth path.
func SupportedProtocol(protocol string) bool {
	switch protocol {
	case store.ProtocolPostgreSQL, store.ProtocolMySQL, store.ProtocolMariaDB:
		return true
	default:
		return false
	}
}

// ExecRequest is one statement to run through the loopback proxy.
type ExecRequest struct {
	// Protocol is the target's wire protocol; it selects the loopback client.
	Protocol string
	// Database is the **dbbat server name**, which is what both proxies
	// resolve the target from (PostgreSQL's startup `database` parameter and
	// MySQL's schema name are both looked up with GetServerByName).
	Database string
	// Username is the API key owner's username. The proxies refuse a key
	// whose owner does not match the username on the wire.
	Username string
	// APIKey is the caller's `dbb_` key, used verbatim as the connection
	// password — the documented way a key holder authenticates to a proxy.
	APIKey string
	// SQL is the statement, passed through untouched. Rewriting it would
	// falsify what /queries records and what approval patterns match.
	SQL string
	// Params are bind parameters. Non-empty forces the prepared-statement
	// path on both protocols, which is also what an operator wants for
	// untrusted values.
	Params []any
	// MaxRows caps the rows returned to the agent. Already clamped by the
	// caller; the executor treats it as authoritative.
	MaxRows int
}

// QueryResult is one statement's outcome, protocol-independent.
type QueryResult struct {
	// Columns are the result columns in wire order, de-duplicated so Rows can
	// key by name.
	Columns []string
	// Rows are the (capped) result rows as column-name → value maps.
	Rows []map[string]any
	// Truncated reports that the statement produced more rows than MaxRows.
	Truncated bool
	// RowsAffected is set for statements that report it (INSERT/UPDATE/…).
	RowsAffected *int64
}

// Executor runs one statement through a dbbat proxy listener.
//
// It is an interface for exactly one reason: tests need to drive the
// approval-pending / await_approval machinery without standing up a proxy and
// an upstream database. It is **not** an extension point for a second
// execution strategy — see the package doc.
type Executor interface {
	Execute(ctx context.Context, req ExecRequest) (*QueryResult, error)
}

// LoopbackExecutor dials this process's own proxy listeners.
type LoopbackExecutor struct {
	// pgListen and mysqlListen are the configured listen addresses
	// (config.Config.ListenPG / ListenMySQL). Empty means disabled.
	pgListen    string
	mysqlListen string
}

// NewLoopbackExecutor builds the real executor from the proxy listen addresses.
func NewLoopbackExecutor(pgListen, mysqlListen string) *LoopbackExecutor {
	return &LoopbackExecutor{pgListen: pgListen, mysqlListen: mysqlListen}
}

// Execute dispatches to the loopback client for the request's protocol.
func (e *LoopbackExecutor) Execute(ctx context.Context, req ExecRequest) (*QueryResult, error) {
	switch req.Protocol {
	case store.ProtocolPostgreSQL:
		addr, err := loopbackAddr(e.pgListen)
		if err != nil {
			return nil, err
		}

		return executePostgreSQL(ctx, addr, req)
	case store.ProtocolMySQL, store.ProtocolMariaDB:
		addr, err := loopbackAddr(e.mysqlListen)
		if err != nil {
			return nil, err
		}

		return executeMySQL(ctx, addr, req)
	default:
		return nil, fmt.Errorf("%w: %s", ErrProtocolUnsupported, req.Protocol)
	}
}

// loopbackAddr turns a listen address into something dialable from this
// process. A wildcard bind ("", "0.0.0.0", "::") becomes loopback: the point
// is to reach *our own* listener, never to leave the host.
func loopbackAddr(listen string) (string, error) {
	if listen == "" {
		return "", ErrListenerDisabled
	}

	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("%w: unparsable listen address %q", ErrListenerDisabled, listen)
	}

	if n, convErr := strconv.Atoi(port); convErr != nil || n <= 0 {
		return "", ErrListenerDisabled
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port), nil
}

// dedupeColumns makes column names usable as map keys. A result set may
// legitimately repeat a name (`SELECT a.id, b.id FROM …`); silently dropping
// one of them would hand the agent a row that is missing a column it can see
// in Columns.
func dedupeColumns(names []string) []string {
	seen := make(map[string]int, len(names))
	out := make([]string, len(names))

	for i, name := range names {
		if name == "" {
			name = "column_" + strconv.Itoa(i+1)
		}

		n := seen[name]
		seen[name] = n + 1

		if n == 0 {
			out[i] = name

			continue
		}

		out[i] = name + "_" + strconv.Itoa(n+1)
	}

	return out
}

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
	// server cannot drive a loopback client for. All five database protocols
	// dbbat proxies are covered; an SSH-only entry is not a database and never
	// will be.
	ErrProtocolUnsupported = errors.New("protocol not supported by the MCP server yet")
	// ErrListenerDisabled means the proxy listener for that protocol is not
	// running in this process, so there is nothing to dial. The MCP server
	// refuses rather than reaching around the proxy.
	ErrListenerDisabled = errors.New("the proxy listener for this protocol is disabled on this instance")
)

// SupportedProtocol reports whether the MCP server can execute statements
// against a database speaking this protocol.
//
// The dispatch is one switch on purpose: adding a protocol means adding a case
// here and a loopback client next to the ones that exist, and nothing else —
// no new enforcement, no new auth path.
func SupportedProtocol(protocol string) bool {
	switch protocol {
	case store.ProtocolPostgreSQL, store.ProtocolMySQL, store.ProtocolMariaDB,
		store.ProtocolOracle:
		return true
	default:
		return false
	}
}

// ExecRequest is one statement to run through the loopback proxy.
type ExecRequest struct {
	// Protocol is the target's wire protocol; it selects the loopback client.
	Protocol string
	// Database is the **dbbat server name**, which is what every proxy resolves
	// the target from (PostgreSQL's startup `database` parameter, MySQL's
	// schema name, Oracle's SERVICE_NAME, SQL Server's LOGIN7 database and
	// MongoDB's authSource are all looked up with GetServerByName).
	Database string
	// UpstreamDatabase is the database name *on the target server* — the
	// `database_name` column of the dbbat row.
	//
	// Only the MongoDB client needs it, and only because a MongoDB command
	// carries its own `$db` inside the message, which the proxy forwards
	// verbatim: a command addressed to the dbbat entry's name would reach the
	// upstream naming a database that does not exist there. Every other
	// protocol carries the database once, at login, where the dbbat name is the
	// right one.
	UpstreamDatabase string
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

// LoopbackListeners are this process's proxy listen addresses, one per
// protocol (config.Config.ListenPG, ListenMySQL, ListenOracle, ListenMongo,
// ListenMSSQL). An empty address means the listener is not running here, and
// the executor refuses rather than finding another way to the database.
type LoopbackListeners struct {
	PostgreSQL string
	MySQL      string
	Oracle     string
	MongoDB    string
	MSSQL      string
}

// LoopbackExecutor dials this process's own proxy listeners.
type LoopbackExecutor struct {
	listeners LoopbackListeners
}

// NewLoopbackExecutor builds the real executor from the proxy listen addresses.
func NewLoopbackExecutor(listeners LoopbackListeners) *LoopbackExecutor {
	return &LoopbackExecutor{listeners: listeners}
}

// Execute dispatches to the loopback client for the request's protocol.
func (e *LoopbackExecutor) Execute(ctx context.Context, req ExecRequest) (*QueryResult, error) {
	switch req.Protocol {
	case store.ProtocolPostgreSQL:
		addr, err := loopbackAddr(e.listeners.PostgreSQL)
		if err != nil {
			return nil, err
		}

		return executePostgreSQL(ctx, addr, req)
	case store.ProtocolMySQL, store.ProtocolMariaDB:
		addr, err := loopbackAddr(e.listeners.MySQL)
		if err != nil {
			return nil, err
		}

		return executeMySQL(ctx, addr, req)
	case store.ProtocolOracle:
		addr, err := loopbackAddr(e.listeners.Oracle)
		if err != nil {
			return nil, err
		}

		return executeOracle(ctx, addr, req)
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

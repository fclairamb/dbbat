package mysql

import (
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// handler implements go-mysql-org/go-mysql server.Handler for a single Session.
//
// Phase 1: pass-through to the upstream client connection. No logging,
// validation, or row capture happens yet — that lands in Phase 2 (intercept)
// and Phase 3 (result capture). The forwarding here is the minimum needed for
// "mysql -h proxy ... -e 'SELECT 1'" to round-trip end-to-end.
type handler struct {
	session *Session
}

// UseDB is called both during the MySQL handshake (when the client includes a
// default database name in HandshakeResponse41) and at command time (on
// COM_INIT_DB).
//
// During handshake (s.authComplete == false) we just stash the requested
// database name; the auth handler validates it in OnAuthSuccess. Command-time
// switches are refused — staying on a single grant-bound database matches the
// PostgreSQL proxy's behavior of disallowing mid-session \c.
func (h *handler) UseDB(dbName string) error {
	if !h.session.authComplete {
		h.session.requestedDB = dbName

		return nil
	}

	if dbName == h.session.database.Name || dbName == h.session.database.DatabaseName {
		return nil
	}

	return ErrSwitchDatabaseDenied
}

// HandleQuery executes COM_QUERY against the upstream and returns the result
// for the framework to write back to the client.
func (h *handler) HandleQuery(query string) (*gomysql.Result, error) {
	return h.session.upstreamConn.Execute(query)
}

// HandleFieldList implements COM_FIELD_LIST. Deprecated since MySQL 5.7 but
// still issued by some legacy clients.
func (h *handler) HandleFieldList(table string, wildcard string) ([]*gomysql.Field, error) {
	return h.session.upstreamConn.FieldList(table, wildcard)
}

// HandleStmtPrepare prepares a statement against the upstream and returns the
// param/column counts. The upstream Stmt is stashed in the context any so
// subsequent Execute/Close calls can reach it without a separate registry.
func (h *handler) HandleStmtPrepare(query string) (int, int, any, error) {
	stmt, err := h.session.upstreamConn.Prepare(query)
	if err != nil {
		return 0, 0, nil, err
	}

	return stmt.ParamNum(), stmt.ColumnNum(), stmt, nil
}

// HandleStmtExecute runs a prepared statement against the upstream.
func (h *handler) HandleStmtExecute(ctx any, _ string, args []any) (*gomysql.Result, error) {
	stmt, ok := upstreamStmt(ctx)
	if !ok {
		return nil, ErrCommandNotPermitted
	}

	return stmt.Execute(args...)
}

// HandleStmtClose releases the upstream prepared statement.
func (h *handler) HandleStmtClose(ctx any) error {
	stmt, ok := upstreamStmt(ctx)
	if !ok {
		return nil
	}

	return stmt.Close()
}

// HandleOtherCommand catches commands the framework doesn't dispatch on its
// own (e.g., COM_RESET_CONNECTION, COM_BINLOG_DUMP). For Phase 1 we refuse
// everything we don't explicitly forward — Phase 2 will whitelist a few and
// continue to refuse replication/admin commands explicitly.
func (h *handler) HandleOtherCommand(byte, []byte) error {
	return ErrCommandNotPermitted
}

// upstreamStmt unboxes the *client.Stmt that HandleStmtPrepare stashed.
func upstreamStmt(ctx any) (interface {
	Execute(args ...any) (*gomysql.Result, error)
	Close() error
}, bool,
) {
	stmt, ok := ctx.(interface {
		Execute(args ...any) (*gomysql.Result, error)
		Close() error
	})

	return stmt, ok
}

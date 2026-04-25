package mysql

import (
	"fmt"
	"log/slog"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// handler implements go-mysql-org/go-mysql server.Handler for a single Session.
//
// Phase 2 wraps every command with:
//   - Quota checks against the session's grant
//   - SQL validation via shared.ValidateMySQLQuery
//   - A query log row (insert + completion fields in a single async write)
//   - Refusal of dangerous protocol commands (replication, shutdown, etc.)
//
// Result row capture is deferred to Phase 3 (text protocol) and a v2 follow-up
// (binary protocol).
type handler struct {
	session *Session
}

// UseDB is called both during the MySQL handshake (when the client includes a
// default database name in HandshakeResponse41) and at command time on
// COM_INIT_DB.
//
// During handshake (s.authComplete == false) we just stash the requested
// database name; the auth handler validates it in OnAuthSuccess. Command-time
// switches are refused — staying on a single grant-bound database matches the
// PostgreSQL proxy's behavior of disallowing mid-session \c. The synthetic
// "USE <db>" SQL is logged for visibility either way.
func (h *handler) UseDB(dbName string) error {
	if !h.session.authComplete {
		h.session.requestedDB = dbName

		return nil
	}

	syntheticSQL := "USE " + dbName

	if dbName == h.session.database.Name || dbName == h.session.database.DatabaseName {
		h.recordQuery(syntheticSQL, nil, time.Now(), nil, nil, 0, nil)

		return nil
	}

	h.recordQuery(syntheticSQL, nil, time.Now(), nil, nil, 0, ptrErrString(ErrSwitchDatabaseDenied))

	return ErrSwitchDatabaseDenied
}

// HandleQuery handles COM_QUERY (text protocol).
func (h *handler) HandleQuery(query string) (*gomysql.Result, error) {
	return h.runIntercepted(query, nil, func() (*gomysql.Result, error) {
		return h.session.upstreamConn.Execute(query)
	})
}

// HandleFieldList implements COM_FIELD_LIST. Deprecated since MySQL 5.7 but
// still issued by some legacy clients. Forwarded without validation — it's a
// metadata read, not a query execution.
func (h *handler) HandleFieldList(table string, wildcard string) ([]*gomysql.Field, error) {
	return h.session.upstreamConn.FieldList(table, wildcard)
}

// HandleStmtPrepare prepares a statement against the upstream and logs the
// prepare itself (separate from each subsequent EXECUTE) so audit shows the
// full lifecycle.
func (h *handler) HandleStmtPrepare(query string) (int, int, any, error) {
	start := time.Now()
	syntheticSQL := "PREPARE: " + query

	stmt, err := h.session.upstreamConn.Prepare(query)
	if err != nil {
		errStr := err.Error()
		h.recordQuery(syntheticSQL, nil, start, nil, nil, 0, &errStr)

		return 0, 0, nil, err
	}

	h.recordQuery(syntheticSQL, nil, start, nil, nil, 0, nil)

	return stmt.ParamNum(), stmt.ColumnNum(), stmt, nil
}

// HandleStmtExecute runs a prepared statement against the upstream.
// Phase 2 logs SQL + parameter values; Phase 3/v2 will capture binary rows.
func (h *handler) HandleStmtExecute(ctx any, query string, args []any) (*gomysql.Result, error) {
	stmt, ok := upstreamStmt(ctx)
	if !ok {
		return nil, ErrCommandNotPermitted
	}

	params := stringifyArgs(args)

	return h.runIntercepted(query, params, func() (*gomysql.Result, error) {
		return stmt.Execute(args...)
	})
}

// HandleStmtClose releases the upstream prepared statement.
func (h *handler) HandleStmtClose(ctx any) error {
	stmt, ok := upstreamStmt(ctx)
	if !ok {
		return nil
	}

	return stmt.Close()
}

// HandleOtherCommand explicitly refuses commands that would let a client
// step outside the proxy's audit/security model: replication protocol commands
// (binlog dump, register slave, table dump) and admin commands (shutdown,
// process kill, debug). Any other unhandled command is also refused — better
// to err on the side of denial.
func (h *handler) HandleOtherCommand(cmd byte, _ []byte) error {
	h.session.logger.WarnContext(h.session.ctx, "MySQL command refused",
		slog.String("command", commandName(cmd)),
		slog.Any("byte", cmd))

	return ErrCommandNotPermitted
}

// runIntercepted is the common path for COM_QUERY and COM_STMT_EXECUTE.
// It checks quotas, validates the SQL, executes, captures result rows, then
// records the outcome (single async query log row + StoreQueryRows).
func (h *handler) runIntercepted(
	sql string,
	params *store.QueryParameters,
	exec func() (*gomysql.Result, error),
) (*gomysql.Result, error) {
	s := h.session

	if err := checkQuotas(s.grant); err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, time.Now(), nil, nil, 0, &errStr)

		return nil, err
	}

	if err := shared.ValidateMySQLQuery(sql, s.grant); err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, time.Now(), nil, nil, 0, &errStr)

		return nil, err
	}

	start := time.Now()
	result, err := exec()
	if err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, start, nil, nil, 0, &errStr)

		return result, err
	}

	var rowsAffected *int64

	if result != nil && result.AffectedRows > 0 {
		ra := int64(result.AffectedRows)
		rowsAffected = &ra
	}

	capturedRows, totalBytes, _ := h.captureRows(result)

	h.recordQuery(sql, params, start, capturedRows, rowsAffected, totalBytes, nil)

	return result, nil
}

// recordQuery inserts a single query log row (asynchronously) with all
// completion fields populated, then stores any captured result rows and
// bumps connection stats. Updates the session's grant counters so in-session
// quota checks reflect work just done.
//
// capturedRows / bytesTransferred are 0/nil for non-SELECT queries, validation
// failures, and synthetic USE/PREPARE entries — captureRows returns nil for
// any result without a Resultset.
func (h *handler) recordQuery(
	sql string,
	params *store.QueryParameters,
	start time.Time,
	capturedRows []store.QueryRow,
	rowsAffected *int64,
	bytesTransferred int64,
	queryError *string,
) {
	s := h.session

	if s.connection == nil {
		return // pre-handshake or pre-connection-record; nothing to log against
	}

	durationMs := float64(time.Since(start).Microseconds()) / 1000.0

	record := &store.Query{
		ConnectionID: s.connection.UID,
		SQLText:      sql,
		Parameters:   params,
		ExecutedAt:   start,
		DurationMs:   &durationMs,
		RowsAffected: rowsAffected,
		Error:        queryError,
	}

	go func() {
		created, err := s.server.store.CreateQuery(s.ctx, record)
		if err != nil {
			s.logger.ErrorContext(s.ctx, "create query log failed", slog.Any("error", err))

			return
		}

		if len(capturedRows) > 0 {
			if err := s.server.store.StoreQueryRows(s.ctx, created.UID, capturedRows); err != nil {
				s.logger.ErrorContext(s.ctx, "store query rows failed", slog.Any("error", err))
			}
		}

		if bytesTransferred > 0 {
			if err := s.server.store.IncrementConnectionStats(s.ctx, s.connection.UID, bytesTransferred); err != nil {
				s.logger.DebugContext(s.ctx, "increment connection stats failed", slog.Any("error", err))
			}
		}
	}()

	// In-session quota counters so the next checkQuotas() reflects this query.
	s.grant.QueryCount++
	s.grant.BytesTransferred += bytesTransferred
}

// stringifyArgs converts MySQL prepared-statement args into store.QueryParameters.
// For Phase 2 we serialize each value as its Go %v repr; binary values that
// don't render meaningfully will look like "[]byte{...}". A future spec may
// switch to type-aware decoding alongside binary-protocol row capture.
func stringifyArgs(args []any) *store.QueryParameters {
	if len(args) == 0 {
		return nil
	}

	values := make([]string, len(args))
	for i, a := range args {
		if a == nil {
			values[i] = "NULL"

			continue
		}

		values[i] = fmt.Sprintf("%v", a)
	}

	return &store.QueryParameters{Values: values}
}

// ptrErrString returns a *string pointing to err.Error(). Convenience for
// inline pointer-to-error-string in record calls.
func ptrErrString(err error) *string {
	if err == nil {
		return nil
	}

	s := err.Error()

	return &s
}

// commandName maps a MySQL command byte to its symbolic name for logging.
// Only includes commands we explicitly track or refuse; unknown commands
// fall through to a hex repr.
func commandName(cmd byte) string {
	switch cmd {
	case gomysql.COM_BINLOG_DUMP:
		return "COM_BINLOG_DUMP"
	case gomysql.COM_BINLOG_DUMP_GTID:
		return "COM_BINLOG_DUMP_GTID"
	case gomysql.COM_REGISTER_SLAVE:
		return "COM_REGISTER_SLAVE"
	case gomysql.COM_TABLE_DUMP:
		return "COM_TABLE_DUMP"
	case gomysql.COM_SHUTDOWN:
		return "COM_SHUTDOWN"
	case gomysql.COM_PROCESS_KILL:
		return "COM_PROCESS_KILL"
	case gomysql.COM_DEBUG:
		return "COM_DEBUG"
	case gomysql.COM_RESET_CONNECTION:
		return "COM_RESET_CONNECTION"
	default:
		return fmt.Sprintf("0x%02x", cmd)
	}
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

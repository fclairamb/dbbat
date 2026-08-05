package mysql

import (
	"fmt"
	"log/slog"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
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
		h.recordQuery(syntheticSQL, nil, time.Now(), nil)

		return nil
	}

	h.recordQuery(syntheticSQL, nil, time.Now(), ptrErrString(ErrSwitchDatabaseDenied))

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
		h.recordQuery(syntheticSQL, nil, start, &errStr)

		return 0, 0, nil, err
	}

	h.recordQuery(syntheticSQL, nil, start, nil)

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

	// A grant revoked mid-session invalidates every subsequent command, even
	// before the watchdog force-closes the connection.
	if s.revocation.Revoked() {
		errStr := shared.ErrGrantRevoked.Error()
		h.recordQuery(sql, params, time.Now(), &errStr)

		return nil, shared.ErrGrantRevoked
	}

	if err := checkQuotas(s.grant); err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, time.Now(), &errStr)

		return nil, err
	}

	if err := shared.ValidateMySQLQuery(sql, s.grant); err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, time.Now(), &errStr)

		return nil, err
	}

	// A KILL aimed at another connection may be releasing a statement parked
	// on a human. Handle that before running the statement upstream; the KILL
	// itself still goes through, so a genuinely running query is killed too.
	if s.releaseHoldForKill(sql) {
		s.logger.InfoContext(s.ctx, "KILL released an approval hold", slog.String("sql", sql))
	}

	// Approval hold — after the static validators, before the statement is
	// executed upstream. The row it creates is completed in place afterwards.
	approvalUID, herr := s.holdIfNeeded(sql, params)
	if herr != nil {
		if approvalUID == uuid.Nil {
			errStr := herr.Error()
			h.recordQuery(sql, params, time.Now(), &errStr)
		} else {
			// The hold already persisted (and resolved) the row; recording a
			// second one would duplicate the statement in /queries.
			h.completeHeldQuery(approvalUID, herr)
		}

		return nil, herr
	}

	start := time.Now()
	result, err := exec()
	if err != nil {
		errStr := err.Error()
		h.recordQuery(sql, params, start, &errStr)

		return result, err
	}

	var rowsAffected *int64

	if result != nil && result.AffectedRows > 0 {
		ra := int64(result.AffectedRows)
		rowsAffected = &ra
	}

	capturedRows, _, truncated := h.captureRows(result)

	h.recordQueryWithUID(approvalUID, sql, params, start, capturedRows, truncated, rowsAffected, nil)

	return result, nil
}

// holdIfNeeded runs the approval gate for one MySQL statement.
func (s *Session) holdIfNeeded(sql string, params *store.QueryParameters) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(sql)
	if !matched {
		return uuid.Nil, nil
	}

	var gone <-chan struct{}

	if s.watched != nil {
		gone = s.watched.Park()
		defer s.watched.Unpark()
	}

	return s.approvalGate.Hold(s.ctx, shared.HoldRequest{
		SQL:        sql,
		Params:     params,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// completeHeldQuery records the outcome on the row an approval hold already
// created, rather than inserting a duplicate.
func (h *handler) completeHeldQuery(queryUID uuid.UUID, cause error) {
	s := h.session

	if s.server == nil || s.server.store == nil {
		return
	}

	errStr := cause.Error()

	go func() {
		if err := s.server.store.UpdateQueryCompletion(s.ctx, queryUID, nil, nil, &errStr, false, false); err != nil {
			s.logger.DebugContext(s.ctx, "failed to complete held query", slog.Any("error", err))
		}
	}()
}

// KillHeldQuery ends a statement parked on a human in response to a
// MySQL KILL QUERY. Reports whether anything was parked.
func (s *Session) KillHeldQuery() bool {
	uid := s.heldQuery()
	if uid == uuid.Nil || s.server == nil || s.server.approvalDeps.Registry == nil {
		return false
	}

	return s.server.approvalDeps.Registry.Resolve(approval.Decision{
		QueryUID: uid,
		Status:   store.ApprovalAbandoned,
		Reason:   "canceled by the client (KILL QUERY)",
		At:       time.Now(),
	})
}

// recordQuery inserts a single query log row (asynchronously) with all
// completion fields populated, then stores any captured result rows and
// bumps connection stats. Updates the session's grant counters so in-session
// quota checks reflect work just done.
//
// capturedRows is nil for non-SELECT queries, validation failures, and
// synthetic USE/PREPARE entries.
//
// bytes_transferred is computed from the wire-level CountingConn around the
// client socket — it captures the COM_QUERY/COM_STMT_EXECUTE request, the
// full result-set response (header packets, row packets, EOF/OK), and any
// error packets. Replaces the previous JSON-encoded row size which only
// counted the captured-row payload.
func (h *handler) recordQuery(sql string, params *store.QueryParameters, start time.Time, queryError *string) {
	h.recordQueryWithUID(uuid.Nil, sql, params, start, nil, false, nil, queryError)
}

// recordQueryWithUID is recordQuery with an optional pre-existing row uid: when
// an approval hold already inserted the statement, the completion is an UPDATE
// on that row instead of a second INSERT.
//
// resultsTruncated reports that capture stopped on a storage limit, so the
// stored rows are a prefix of what the client actually received.
//
// Captured rows go through the process-wide batching writer rather than a
// dedicated INSERT, so a busy proxy amortizes the round-trip across queries and
// sessions. The completion write happens after the writer's flush barrier,
// which is also when results_dropped is known.
func (h *handler) recordQueryWithUID(
	queryUID uuid.UUID,
	sql string,
	params *store.QueryParameters,
	start time.Time,
	capturedRows []store.QueryRow,
	resultsTruncated bool,
	rowsAffected *int64,
	queryError *string,
) {
	s := h.session

	if s.connection == nil {
		return // pre-handshake or pre-connection-record; nothing to log against
	}

	// Last gate before the store: never persist an "error" that is not readable
	// text. See shared.SanitizeQueryError.
	queryError = shared.SanitizeQueryError(s.ctx, s.logger, queryError)

	total := s.cumulativeClientBytes()
	bytesTransferred := total - s.lastBytesSnapshot
	s.lastBytesSnapshot = total

	durationMs := float64(time.Since(start).Microseconds()) / 1000.0

	record := &store.Query{
		ConnectionID:     s.connection.UID,
		SQLText:          sql,
		Parameters:       params,
		ExecutedAt:       start,
		DurationMs:       &durationMs,
		RowsAffected:     rowsAffected,
		Error:            queryError,
		ResultsTruncated: resultsTruncated,
	}

	// Held statements already have a row; everything else is inserted here.
	// When there are rows to store, the record goes in without its completion
	// fields and is completed after the rows land — a query must never read as
	// finished while its rows are still arriving.
	hasRows := len(capturedRows) > 0

	go func() {
		sink := s.server.rowWriter.NewSink()
		held := queryUID != uuid.Nil

		if !held {
			insert := record

			if hasRows {
				bare := *record
				bare.DurationMs = nil
				bare.RowsAffected = nil
				bare.Error = nil
				bare.ResultsTruncated = false
				insert = &bare
			}

			created, err := s.server.store.CreateQuery(s.ctx, insert)
			if err != nil {
				s.logger.ErrorContext(s.ctx, "create query log failed", slog.Any("error", err))
				sink.Fail()

				return
			}

			queryUID = created.UID
		}

		sink.Resolve(queryUID)

		// Blocking hand-off: the rows are already resident, so queueing them
		// costs no extra memory and dropping them would lose a capture that
		// used to be stored whole.
		sink.AddAll(s.ctx, capturedRows)
		sink.Flush(s.ctx)

		record.ResultsDropped = sink.Dropped()

		// A held row always needs its completion write. A row inserted here
		// needs one only when it went in bare (rows to store) or when the
		// writer turned out to have dropped some.
		if held || hasRows || record.ResultsDropped {
			if err := s.server.store.UpdateQueryCompletion(
				s.ctx, queryUID, &durationMs, rowsAffected, queryError, resultsTruncated, record.ResultsDropped,
			); err != nil {
				s.logger.ErrorContext(s.ctx, "complete query log failed", slog.Any("error", err))
			}
		}

		s.stream.Query(queryUID, record)

		if bytesTransferred > 0 {
			if err := s.server.store.IncrementConnectionStats(s.ctx, s.connection.UID, bytesTransferred); err != nil {
				s.logger.DebugContext(s.ctx, "increment connection stats failed", slog.Any("error", err))
			}
		}
	}()

	// In-session quota counters so the next checkQuotas() reflects this query.
	if s.grant != nil {
		s.grant.QueryCount++
		s.grant.BytesTransferred += bytesTransferred
	}
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

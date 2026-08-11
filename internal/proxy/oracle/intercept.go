package oracle

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The log messages the cursor-tracking path emits, named because the
// cursor-id learning measurement counts them
// (cursor_learning_integration_test.go) and a measurement that silently counts
// a string nothing emits any more reports a perfect score forever. Naming them
// makes the compiler the guard: rewording one here moves the counter with it.
//
// logMsgQueryIntercepted is deliberately shared by every statement-carrying op
// — each one is a chance to learn a cursor id, which is what the measurement
// counts it for.
const (
	logMsgQueryIntercepted         = "query intercepted"
	logMsgLearnedCursorID          = "learned server-assigned cursor id"
	logMsgReexecGated              = "intercepted cursor re-execution"
	logMsgUntrackedCursorForwarded = "forwarding a re-execution of an untracked cursor: " +
		"the grant carries no statement-shaped control"
	logMsgUntrackedCursorRefused = "refused a re-execution of an untracked cursor under a restrictive grant"
	logMsgCursorsClosed          = "client closed cursors"
	logMsgRecycledCursorID       = "cursor id recycled onto a different statement"
)

// trackedCursor tracks a parsed cursor and its SQL.
type trackedCursor struct {
	cursorID   uint16
	sql        string
	bindValues []string
	parsedAt   time.Time
	columns    []columnDef // Column definitions from first response (for multi-fetch)
}

// oracleQueryTracker manages per-session cursor state and pending queries.
type oracleQueryTracker struct {
	cursors      map[uint16]*trackedCursor
	pendingQuery *pendingOracleQuery
}

// pendingOracleQuery tracks a query in progress (between OALL8/OFETCH and response).
type pendingOracleQuery struct {
	cursor         *trackedCursor
	startTime      time.Time
	capturedBytes  int64
	rowNumber      int
	truncated      bool
	queryUID       uuid.UUID // Set after query record is created in DB
	queryPersisted bool      // True after query record is created
	lastRow        []string  // Last captured row values (for continuation packet duplicate tracking)

	// rowSink batches this query's captured rows through the shared writer.
	// Created by persistQueryRecord, i.e. only once the parent queries row
	// exists — query_rows.query_id is a foreign key.
	rowSink *shared.QuerySink
}

// newOracleQueryTracker creates a new query tracker.
func newOracleQueryTracker() *oracleQueryTracker {
	return &oracleQueryTracker{
		cursors: make(map[uint16]*trackedCursor),
	}
}

// handleOALL8 intercepts an OALL8 message: decodes SQL, checks access controls,
// and begins tracking the query. Returns an error if the query should be blocked.
func (s *session) handleOALL8(ttcPayload []byte) error {
	result, err := decodeOALL8(ttcPayload)
	if err != nil {
		// A well-formed OALL8 with no SQL text is a re-execution of a cursor
		// the client already parsed, not a broken frame: it is gated against
		// the SQL that cursor holds rather than forwarded.
		var noSQL *OALL8NoSQLError
		if errors.As(err, &noSQL) {
			return s.handleCursorReexec(noSQL.CursorID)
		}

		s.logger.WarnContext(s.ctx, "failed to decode OALL8", slog.Any("error", err))
		// Don't block on decode failure — let it pass through
		return nil
	}

	s.logger.DebugContext(s.ctx, "intercepted OALL8",
		slog.String("sql", truncateSQL(result.SQL, 200)),
		slog.Uint64("cursor_id", uint64(result.CursorID)),
		slog.Int("bind_count", len(result.BindValues)),
	)

	// Quota, expiry and revocation — ahead of the static controls and therefore
	// ahead of the hold, so an exhausted grant is refused outright and never
	// parks a statement on a human. Recorded like any other refusal now that the
	// decode has produced the SQL to record it against.
	if err := s.checkQuotas(); err != nil {
		return s.refuseStatement(result.SQL, result.BindValues, err)
	}

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(result.SQL, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(result.SQL, 200)),
				slog.Any("error", err),
			)

			return s.refuseStatement(result.SQL, result.BindValues, err)
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(result.SQL)
	if herr != nil {
		return herr
	}

	// Track the cursor
	cursor := &trackedCursor{
		cursorID:   result.CursorID,
		sql:        result.SQL,
		bindValues: result.BindValues,
		parsedAt:   time.Now(),
	}
	s.rememberCursor(result.CursorID, cursor)

	// Start pending query and persist immediately
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleCursorReexec gates an execution that carries no statement of its own —
// a SQL-less OALL8, or an OFETCH that starts a fresh pending query — against
// the SQL the named cursor was parsed with.
//
// Without this, an approval decision and every statement-shaped control applied
// to the *parse* only: a client that parsed once and re-executed the same
// cursor got one hold and then a free run for the rest of the session.
func (s *session) handleCursorReexec(cursorID uint16) error {
	cursor, ok := s.tracker.cursors[cursorID]
	if !ok {
		return s.refuseUnknownCursor(cursorID)
	}

	return s.regateCursor(cursor)
}

// refuseUnknownCursor decides what to do with a re-execution naming a cursor
// dbbat never saw parsed. All three frames that can only be identified by
// cursor id route through here — the SQL-less OALL8, an OFETCH that starts a
// fresh pending query, and the piggyback re-execution every modern thin client
// sends — so the wire op a client picks cannot change the answer. The statement
// it would run is unknown, so:
//
//   - under a grant carrying statement-shaped controls, it fails closed — a
//     restrictive grant must not be bypassable by an execution the proxy cannot
//     identify (the shape the SQL Server proxy already chose, see
//     ErrUnknownPreparedStatement);
//   - under a grant carrying none of them, it is forwarded with a WARN. An
//     untracked cursor is not by itself an attack — dbbat may have attached
//     mid-session, or the entry may be gone — and refusing there would break
//     permissive sessions for no security gain.
//
// This conditional refusal is documented in docs/approvals.md.
//
// It runs *before* the quota check, which lives one call further down in
// regateCursor: an unknown cursor keeps answering here even when the grant is
// also exhausted, because "dbbat cannot identify this statement" is the more
// specific and more security-relevant answer, and burying it under a quota
// error would make it harder to diagnose. The quota is only consulted on the
// branch that decides *not* to refuse — an execution dbbat cannot identify is
// still an execution, and the quota is the one gate that needs no SQL, so a
// permissive grant that has run out must not start forwarding again just
// because the cursor is untracked. That refusal carries no queries row, for
// want of any statement text to put in it.
func (s *session) refuseUnknownCursor(cursorID uint16) error {
	if !s.hasStatementControls() {
		if err := s.checkQuotas(); err != nil {
			return err
		}

		s.logger.WarnContext(s.ctx, logMsgUntrackedCursorForwarded,
			slog.Uint64("cursor_id", uint64(cursorID)),
		)

		return nil
	}

	s.logger.WarnContext(s.ctx, logMsgUntrackedCursorRefused,
		slog.Uint64("cursor_id", uint64(cursorID)),
	)

	return ErrUnknownCursor
}

// hasStatementControls reports whether the session's grant carries controls
// that are evaluated per statement — the ones a re-execution could otherwise
// evade. Deliberately three: approval patterns, read_only, block_ddl.
// block_copy is left out because an Oracle COPY is a client-side SQL*Plus
// command, never a server statement, so it cannot ride a re-executed cursor.
//
// Note the pattern set is read off the grant regardless of
// DBB_APPROVAL_ENABLED: a grant carrying patterns while approvals are globally
// off still counts as restrictive here. Intentional — it errs fail-closed, and
// the alternative would make an untracked cursor's fate depend on a global
// switch the grant's author cannot see.
func (s *session) hasStatementControls() bool {
	if s.grant == nil {
		return false
	}

	return len(s.grant.ApprovalPatterns()) > 0 || s.grant.IsReadOnly() || s.grant.ShouldBlockDDL()
}

// regateCursor re-runs the statement gate — quota, static controls, then the
// approval hold — against the SQL a tracked cursor holds, and starts a fresh
// pending query for this execution. Same order as the SQL-carrying path, so a
// re-execution is enforced exactly like the parse that created the cursor.
//
// This is the single quota-check insertion point for all three re-execution
// frames: the SQL-less OALL8 (handleCursorReexec), the piggyback re-execution
// (handlePiggybackReexec) and the OFETCH that starts a fresh query. Each
// resolves its cursor before delegating here, which is deliberate — a cursor
// dbbat never saw parsed keeps answering refuseUnknownCursor even when the
// grant is also exhausted. That refusal is the more specific and the more
// security-relevant one, and burying it under a quota error would make it
// harder to diagnose.
func (s *session) regateCursor(cursor *trackedCursor) error {
	sql := cursor.sql

	s.logger.DebugContext(s.ctx, logMsgReexecGated,
		slog.String("sql", truncateSQL(sql, 200)),
		slog.Uint64("cursor_id", uint64(cursor.cursorID)),
	)

	// Quota, expiry and revocation — recorded against the SQL the cursor holds,
	// which is the only place this execution's statement text exists.
	if err := s.checkQuotas(); err != nil {
		return s.refuseStatement(sql, cursor.bindValues, err)
	}

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(sql, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "cursor re-execution blocked by access control",
				slog.String("sql", truncateSQL(sql, 200)),
				slog.Any("error", err),
			)

			return s.refuseStatement(sql, cursor.bindValues, err)
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(sql)
	if herr != nil {
		return herr
	}

	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handlePiggybackReexec gates a piggyback cursor re-execution — func 0x03,
// sub-op 0x4e (SELECT) or 0x04 (everything else). This is the frame every
// modern thin client actually sends to re-run a statement it already parsed;
// the SQL-less OALL8 handleCursorReexec was written for is its legacy
// equivalent, and no tested client emits that one (see docs/oracle.md).
//
// A cursor dbbat knows is re-gated exactly like the parse that created it. One
// it does not know goes through refuseUnknownCursor, exactly like the other two
// re-execution frames: refused under a grant carrying statement-shaped
// controls, forwarded with a WARN otherwise. The wire op a client picks cannot
// change the answer.
//
// That symmetry was not always safe, and the difference is measured rather than
// assumed. dbbat learns these cursor ids by reading them off the server's
// response (learnCursorID), and while that scan bounded the OER sequence number
// at 255 it stopped learning a few dozen statements into every session — under
// which failing closed here would have refused the second execution of ordinary
// read-only work. With that bound corrected, two real thin clients (go-ora v3
// and python-oracledb thin) drove 124 re-executions through this path across
// prepared loops, bind-heavy statements, interleaved cursors, DML, PL/SQL, a
// REF cursor and a churned statement cache, and every single one resolved. See
// docs/oracle.md, "Cursor-id learning", and
// TestIntegration_CursorIDLearningMissRate.
func (s *session) handlePiggybackReexec(ttcPayload []byte) error {
	cursorID, err := decodeCursorReexec(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode piggyback cursor re-execution", slog.Any("error", err))

		return nil
	}

	cursor, ok := s.tracker.cursors[cursorID]
	if !ok {
		return s.refuseUnknownCursor(cursorID)
	}

	return s.regateCursor(cursor)
}

// learnCursorID records the cursor id the server assigned to the statement
// currently in flight, so a later re-execution naming that id resolves to its
// SQL.
//
// Only the legacy OALL8 carries the cursor id on the request; the piggyback and
// JDBC exec paths leave dbbat to read it off the response. Learning is a
// one-shot per statement — a cursor that already has an id is never re-read —
// which is what keeps the anchored scan in findCursorIDInResponse from being
// re-run against row-stream bytes for the rest of a fetch.
func (s *session) learnCursorID(ttcPayload []byte) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.cursor == nil || pending.cursor.cursorID != 0 {
		return
	}

	cursorID, ok := findCursorIDInResponse(ttcPayload)
	if !ok {
		return
	}

	pending.cursor.cursorID = cursorID
	s.rememberCursor(cursorID, pending.cursor)

	s.logger.DebugContext(s.ctx, logMsgLearnedCursorID,
		slog.Uint64("cursor_id", uint64(cursorID)),
		slog.String("sql", truncateSQL(pending.cursor.sql, 200)),
	)
}

// flushPendingQuery completes any outstanding query that hasn't been finalized.
// Called before starting a new query to ensure the previous one is persisted.
func (s *session) flushPendingQuery() {
	if s.tracker.pendingQuery != nil {
		s.completeQuery(nil, nil)
	}
}

// handlePiggybackExec intercepts a v315+ piggyback execute-with-SQL message.
func (s *session) handlePiggybackExec(ttcPayload []byte) error {
	result, err := decodePiggybackExecSQL(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode piggyback exec", slog.Any("error", err))
		return nil // Don't block on decode failure
	}

	s.logger.InfoContext(s.ctx, logMsgQueryIntercepted,
		slog.String("sql", truncateSQL(result.SQL, 200)),
	)

	// Quota, expiry and revocation first — see handleOALL8.
	if err := s.checkQuotas(); err != nil {
		return s.refuseStatement(result.SQL, result.BindValues, err)
	}

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(result.SQL, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(result.SQL, 200)),
				slog.Any("error", err),
			)

			return s.refuseStatement(result.SQL, result.BindValues, err)
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	approvalUID, herr := s.holdIfNeeded(result.SQL)
	if herr != nil {
		return herr
	}

	// Track as pending query and persist immediately
	cursor := &trackedCursor{
		sql:        result.SQL,
		bindValues: result.BindValues,
		parsedAt:   time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleJDBCExec intercepts a JDBC execute-with-SQL message (func=0x11, sub=0x69).
//
// This is a statement-carrying op like OALL8 and the v315+ piggyback exec, so it
// runs the same gate in the same order: static validators, then the approval
// hold, and only then is the row recorded and the packet allowed upstream. It
// used to record the query and nothing else, which made the JDBC thin driver's
// exec path a way around `read_only`, `block_ddl` and every approval pattern.
//
// Returns a non-nil error when the statement must not be forwarded; the caller
// answers the client with a TTC error instead.
func (s *session) handleJDBCExec(ttcPayload []byte) error {
	result, err := decodeExecSQL(ttcPayload)
	if err != nil {
		s.logger.DebugContext(s.ctx, "failed to decode JDBC exec", slog.Any("error", err))
		// Don't block on decode failure — let it pass through, as OALL8 does.
		// See the Oracle caveat in docs/approvals.md.
		return nil
	}

	// The gate matches on the normalized text, so the text recorded and shown in
	// /queries is the same one the patterns ran against.
	sql := shared.NormalizeSQL(result.SQL)

	s.logger.InfoContext(s.ctx, logMsgQueryIntercepted,
		slog.String("sql", truncateSQL(sql, 200)),
		slog.String("source", "jdbc"),
	)

	// Quota, expiry and revocation first — see handleOALL8. Recorded against the
	// normalized text, like the control refusal below it.
	if err := s.checkQuotas(); err != nil {
		return s.refuseStatement(sql, result.BindValues, err)
	}

	// Access control check
	if s.grant != nil {
		if err := shared.ValidateOracleQuery(sql, s.grant); err != nil {
			s.logger.WarnContext(s.ctx, "query blocked by access control",
				slog.String("sql", truncateSQL(sql, 200)),
				slog.Any("error", err),
			)

			return s.refuseStatement(sql, result.BindValues, err)
		}
	}

	// Complete previous query if still pending (sets duration)
	s.flushPendingQuery()

	// Approval hold — after the static validators, before anything reaches
	// upstream.
	approvalUID, herr := s.holdIfNeeded(sql)
	if herr != nil {
		return herr
	}

	// Track as pending query and persist immediately
	cursor := &trackedCursor{
		sql:      sql,
		parsedAt: time.Now(),
	}
	s.tracker.pendingQuery = &pendingOracleQuery{
		cursor:    cursor,
		startTime: time.Now(),
	}

	// An approval hold already inserted the row; reuse it rather than writing
	// a second one for the same statement.
	if approvalUID != uuid.Nil {
		s.tracker.pendingQuery.queryUID = approvalUID
		s.tracker.pendingQuery.queryPersisted = true
	} else {
		s.persistQueryRecord()
	}

	return nil
}

// handleQueryResultV2 processes a v315+ QueryResult (func=0x10) response.
// Extracts column names and row values, stores them as query results.
// For large result sets, rows arrive across multiple response packets.
// We accumulate rows until we see ORA-01403 (no data found) which signals end of data.
func (s *session) handleQueryResultV2(ttcPayload []byte) {
	result := decodeQueryResultV2(ttcPayload)
	if result == nil {
		return
	}

	// Store column definitions from the first response (they only appear once).
	// The type code (when known) feeds type-aware value decoding of continuation
	// packets, which carry no describe of their own.
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil && len(result.Columns) > 0 {
		columns := make([]columnDef, len(result.Columns))
		for i, name := range result.Columns {
			columns[i] = columnDef{Name: name}
			if i < len(result.ColumnTypes) {
				columns[i].TypeCode = uint8(result.ColumnTypes[i])
			}
		}

		s.tracker.pendingQuery.cursor.columns = columns
	}

	// Capture rows from the QueryResult response.
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil {
		columns := s.tracker.pendingQuery.cursor.columns
		for _, row := range result.Rows {
			values := make([]interface{}, len(row))
			for i, v := range row {
				values[i] = v
			}

			s.captureRow(columns, values)
		}

		// Store last row for continuation packet duplicate tracking
		if len(result.Rows) > 0 {
			s.tracker.pendingQuery.lastRow = result.Rows[len(result.Rows)-1]
		}
	}

	// Only complete the query when we see the end-of-data marker (ORA-01403)
	if result.NoData {
		s.completeQuery(nil, nil)
	}
}

// handleOFETCH intercepts an OFETCH message: links the fetch to its cursor.
//
// A fetch arriving with no query in flight is a re-execution, so it carries the
// full pre-flight of a statement — quota, static controls, approval hold — and
// a cursor dbbat never saw parsed goes through refuseUnknownCursor first,
// exactly like the SQL-less OALL8, before the quota is ever consulted. A fetch
// continuing a query already in flight carries none of it.
//
// Returns a non-nil error when the fetch must not be forwarded; the caller
// answers the client with a TTC error instead.
func (s *session) handleOFETCH(ttcPayload []byte) error {
	result, err := decodeOFETCH(ttcPayload)
	if err != nil {
		s.logger.WarnContext(s.ctx, "failed to decode OFETCH", slog.Any("error", err))

		return nil
	}

	// A fetch that CONTINUES a query already in flight is simply more rows of a
	// statement that has already been through the gate. It is deliberately left
	// alone: re-validating here would risk parking a client mid-result-set,
	// which the hold must never do.
	if s.tracker.pendingQuery != nil {
		return nil
	}

	// No query in flight: this fetch re-executes the cursor and starts its own
	// pending query, which is persisted as a distinct row in /queries. Anything
	// recorded as its own query is gated as its own query, or the audit trail
	// and the enforcement disagree — and that includes the quota, which
	// regateCursor checks below once the cursor has resolved. The early return
	// above is what keeps every one of those refusals off the continuation
	// path, where cutting a client off mid-result-set is exactly what must
	// never happen.
	cursor, ok := s.tracker.cursors[result.CursorID]
	if !ok {
		return s.refuseUnknownCursor(result.CursorID)
	}

	return s.regateCursor(cursor)
}

// handleOCLOSE cleans up cursor tracking when a cursor is closed.
func (s *session) handleOCLOSE(cursorID uint16) {
	delete(s.tracker.cursors, cursorID)
}

// handleCloseCursors drops every cursor a close-cursors piggyback named.
//
// This is the whole reason s.tracker.cursors stays bounded on a long session,
// and the reason a recycled id resolves to the statement that owns it now
// rather than the one that used to: clients batch their closes, so removing one
// per frame left the rest behind as stale entries. The `tracked` attribute is
// the tracker's size afterwards — TestIntegration_CursorIDLearningMissRate
// asserts on it, which is what makes "no cap needed" a measured claim rather
// than an assumption.
func (s *session) handleCloseCursors(cursorIDs []uint16) {
	for _, cursorID := range cursorIDs {
		s.handleOCLOSE(cursorID)
	}

	s.logger.DebugContext(s.ctx, logMsgCursorsClosed,
		slog.Int("closed", len(cursorIDs)),
		slog.Int("tracked", len(s.tracker.cursors)),
	)
}

// rememberCursor records the statement a cursor id now stands for.
//
// Overwriting an existing entry is correct — Oracle really does recycle ids
// from a small per-session pool — but it is also the exact moment at which a
// re-execution could start resolving to the wrong statement, so an overwrite
// that changes the SQL is logged at WARN. It stays a signal and never a
// refusal: the original bug went unnoticed for so long precisely because this
// window was silent, and a WARN naming the id and both statements is what makes
// a future one visible.
func (s *session) rememberCursor(cursorID uint16, cursor *trackedCursor) {
	if prev, ok := s.tracker.cursors[cursorID]; ok && prev != cursor && prev.sql != cursor.sql {
		s.logger.WarnContext(s.ctx, logMsgRecycledCursorID,
			slog.Uint64("cursor_id", uint64(cursorID)),
			slog.String("previous_sql", truncateSQL(prev.sql, 200)),
			slog.String("sql", truncateSQL(cursor.sql, 200)),
		)
	}

	s.tracker.cursors[cursorID] = cursor
}

// captureRow hands a single captured row to the shared batching writer.
//
// The submit is non-blocking: if the writer's queue is full the row is dropped
// and the capture is marked degraded, because this runs inline with forwarding
// rows to the client and a slow moment in dbbat's own storage must never
// become a stall on the customer's query. Rows are never accumulated in
// memory here — the writer's bounded queue is the only buffer.
func (s *session) captureRow(columns []columnDef, values []interface{}) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.truncated {
		return
	}

	if !s.queryStorage.StoreResults {
		return
	}

	// Ensure the query record exists in the database: query_rows.query_id is
	// a foreign key, so the parent has to be there before any row flushes.
	if !pending.queryPersisted {
		s.persistQueryRecord()
	}

	if pending.rowSink == nil {
		return // No query record — nowhere to hang the rows
	}

	rowData := make(map[string]interface{})
	rowSize := int64(0)

	for i, col := range columns {
		var val interface{}
		if i < len(values) {
			val = values[i]
		}

		if val != nil {
			valStr := fmt.Sprintf("%v", val)
			rowSize += int64(len(valStr))
		}

		rowData[col.Name] = val
	}

	// Check limits
	if pending.rowNumber >= s.queryStorage.MaxResultRows ||
		pending.capturedBytes+rowSize > s.queryStorage.MaxResultBytes {
		pending.truncated = true

		return
	}

	jsonData, err := json.Marshal(rowData)
	if err != nil {
		return
	}

	pending.rowNumber++
	pending.capturedBytes += rowSize

	// Queue the row for the next batch. Row numbering is a producer-side
	// running counter (pending.rowNumber), so it stays correct across flush
	// boundaries.
	pending.rowSink.Add(store.QueryRow{
		RowNumber:    pending.rowNumber,
		RowData:      jsonData,
		RowSizeBytes: rowSize,
	})
}

// persistQueryRecord creates the query record in the database before streaming rows.
func (s *session) persistQueryRecord() {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.queryPersisted || pending.cursor == nil || s.store == nil {
		return
	}

	pending.queryPersisted = true

	query := &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      pending.cursor.sql,
		ExecutedAt:   pending.startTime,
		Parameters:   formatOracleBinds(pending.cursor.bindValues),
	}

	created, err := s.store.CreateQuery(s.ctx, query)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to create query record", slog.Any("error", err))
		return
	}

	pending.queryUID = created.UID
	pending.rowSink = s.rowWriter.NewSinkFor(created.UID)

	s.stream.Query(created.UID, query)
}

// refuseStatement records a statement that access control declined and returns
// the refusal unchanged, so a caller reads as `return s.refuseStatement(...)`.
func (s *session) refuseStatement(sql string, binds []string, refusal error) error {
	s.recordBlockedQuery(sql, binds, refusal)

	return refusal
}

// recordBlockedQuery persists a queries row for a statement refused by
// read_only, block_copy or block_ddl before a single byte of it reached
// upstream.
//
// Until this existed the refusal lived only in the process log (the WARN just
// above every call site) and in the pcapng capture when capture happened to be
// on — an evidence gap against PCI DSS 10.2.1.4, and an inconsistency with
// MySQL/MariaDB, MongoDB and SQL Server, which have always recorded one. The
// row carries the same error-string shape as those three so the UI badge and
// any log-based alerting treat all five protocols identically.
//
// duration_ms and rows_affected are 0 — the statement never ran — and the row
// is attributed to the session's connection like any other, so it joins to the
// user and the database. It deliberately does not touch pendingQuery: a
// refused statement is not in flight, and the previous query (if any) is still
// owed its own completion.
func (s *session) recordBlockedQuery(sql string, binds []string, refusal error) {
	if refusal == nil || s.completionStore == nil || s.connectionUID == uuid.Nil {
		return
	}

	errText := refusal.Error()
	duration := float64(0)
	rowsAffected := int64(0)

	query := &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      sql,
		ExecutedAt:   time.Now(),
		DurationMs:   &duration,
		RowsAffected: &rowsAffected,
		Parameters:   formatOracleBinds(binds),
		// Last gate before the store: never persist an "error" that is not
		// readable text. See shared.SanitizeQueryError.
		Error: shared.SanitizeQueryError(s.ctx, s.logger, &errText),
	}

	go func() {
		created, err := s.completionStore.CreateQuery(s.ctx, query)
		if err != nil {
			s.logger.ErrorContext(s.ctx, "failed to log blocked query", slog.Any("error", err))

			return
		}

		s.stream.Query(created.UID, query)

		// Bytes are left to the TTC error frame's own accounting; this only
		// bumps the connection's query counter so a refused attempt shows up
		// there too.
		if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, 0); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
		}
	}()

	// The attempt counts against the in-session query quota, exactly as it
	// does on the three protocols that already recorded refusals.
	if s.grant != nil {
		s.grant.QueryCount++
	}
}

// queryCompletionStore is the slice of the store that query completion writes
// through: the create-on-completion path, the update-after-streaming path, and
// the connection byte counter. It exists so the completion path can be driven
// by a unit test — the error text a completed query records is the property
// this package's mid-fetch handling hinges on.
type queryCompletionStore interface {
	CreateQuery(ctx context.Context, query *store.Query) (*store.Query, error)
	UpdateQueryCompletion(
		ctx context.Context,
		uid uuid.UUID,
		durationMs *float64,
		rowsAffected *int64,
		queryError *string,
		resultsTruncated bool,
		resultsDropped bool,
	) error
	IncrementConnectionStats(ctx context.Context, uid uuid.UUID, bytes int64) error
}

// completeQuery finalizes a query record with duration and updates connection stats.
// Rows have already been streamed to the database during capture.
//
// bytes_transferred is computed from the wire-level CountingConn around the
// client socket — this captures TNS framing, AUTH/OALL8/OFETCH payloads and
// error packets, fixing the previous undercount that summed only response
// payload lengths.
func (s *session) completeQuery(rowsAffected *int64, queryError *string) {
	pending := s.tracker.pendingQuery
	if pending == nil || pending.cursor == nil {
		return
	}

	s.tracker.pendingQuery = nil

	// Last gate before the store: never persist an "error" that is not readable
	// text. See shared.SanitizeQueryError.
	queryError = shared.SanitizeQueryError(s.ctx, s.logger, queryError)

	total := s.cumulativeClientBytes()
	bytesTransferred := total - s.lastBytesSnapshot
	s.lastBytesSnapshot = total

	duration := float64(time.Since(pending.startTime).Milliseconds())

	// Use captured row count as rows_affected if not provided by the caller.
	if rowsAffected == nil && pending.rowNumber > 0 {
		rc := int64(pending.rowNumber)
		rowsAffected = &rc
	}

	// If the query record was already created (rows were streamed), update it.
	// Otherwise, create it now (no-result queries like DML).
	if pending.queryPersisted && pending.queryUID != uuid.Nil {
		// Update with duration, error, rows affected. results_truncated is only
		// known now: the row was inserted before any row was streamed.
		if s.completionStore != nil {
			go s.finalizeQuery(pending.queryUID, pending.rowSink, &duration, rowsAffected, queryError, pending.truncated, bytesTransferred)
		}
	} else if s.completionStore != nil {
		// Create the query record (no rows to stream)
		query := &store.Query{
			ConnectionID:     s.connectionUID,
			SQLText:          pending.cursor.sql,
			ExecutedAt:       pending.startTime,
			DurationMs:       &duration,
			RowsAffected:     rowsAffected,
			Error:            queryError,
			Parameters:       formatOracleBinds(pending.cursor.bindValues),
			ResultsTruncated: pending.truncated,
		}

		go func() {
			if _, err := s.completionStore.CreateQuery(s.ctx, query); err != nil {
				s.logger.ErrorContext(s.ctx, "failed to log query", slog.Any("error", err))
			}

			if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
				s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
			}
		}()
	}

	// Update local grant state for in-session quota checks
	if s.grant != nil {
		s.grant.QueryCount++
		s.grant.BytesTransferred += bytesTransferred
	}
}

// finalizeQuery updates a query record with completion data (duration, error,
// whether row capture stopped on a storage limit, and whether the row writer
// had to drop rows).
//
// It runs in its own goroutine, which is what makes the flush barrier
// affordable: the tail of the capture must land before the query reads as
// complete, or the UI shows a finished query with rows still arriving.
func (s *session) finalizeQuery(
	queryUID uuid.UUID,
	rowSink *shared.QuerySink,
	duration *float64,
	rowsAffected *int64,
	queryError *string,
	resultsTruncated bool,
	bytesTransferred int64,
) {
	rowSink.Flush(s.ctx)

	if err := s.completionStore.UpdateQueryCompletion(
		s.ctx, queryUID, duration, rowsAffected, queryError, resultsTruncated, rowSink.Dropped(),
	); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to finalize query", slog.Any("error", err))
	}

	if err := s.completionStore.IncrementConnectionStats(s.ctx, s.connectionUID, bytesTransferred); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to increment connection stats", slog.Any("error", err))
	}
}

// writeTTCError ends the client's call with an ORA error.
//
// This is the frame every Oracle refusal rides on — `read_only`, `block_ddl`,
// `block_copy`, a quota, an expired or revoked grant — so what it emits has to
// be something a real client accepts as the end of a call, not merely something
// that looks like an error. It used to emit a TTC Response (0x08) with
// fixed-width big-endian fields, which no client parses: measured against
// Oracle 23ai Free, go-ora's ExecContext never returned from a refused
// statement at all.
//
// A server ends a call with an OER (message type 0x04) whose body is the TTC
// summary object, and that is what this builds — see encodeOER for the layout
// and for how the two negotiated variations in it are resolved. Nothing else in
// the message matters to a client: go-ora leaves its read loop on the message
// type alone (`msg == 4`), and python-oracledb treats a processed error info as
// the end of the response, so both come back with the ORA code and text and the
// connection stays usable for the next statement.
//
// The truncation is deliberate: a message long enough to need the chunked CLR
// form would have to match the session's negotiated UseBigClrChunks, and no
// refusal reason needs the room.
func (s *session) writeTTCError(oraErrorCode int, message string) error {
	errMsg := fmt.Sprintf("ORA-%05d: %s", oraErrorCode, message)
	if len(errMsg) > oerMaxMessageLen {
		errMsg = errMsg[:oerMaxMessageLen]
	}

	body := encodeOER(s.oer.orDefault(), oerSummary{
		CallStatus:   1,
		SeqNumber:    s.nextOERSeq(),
		ErrorCode:    oraErrorCode,
		ErrorMessage: errMsg,
	})

	payload := make([]byte, 0, ttcDataFlagsSize+len(body))
	payload = append(payload, 0x00, 0x00) // data flags
	payload = append(payload, body...)

	// v315+ framing, for the same reason sendAuthFailed spells out: after the
	// Accept a modern client reads the packet length as a 4-byte field, and the
	// legacy 2-byte framing writeTNSPacket emits leaves the length bytes reading
	// as a multi-megabyte packet. That was the *other* half of this frame's
	// hang, and the half a correct OER body alone does not fix — measured
	// against Oracle 23ai Free, go-ora parked on a byte-perfect OER until it was
	// framed this way.
	if _, err := s.clientConn.Write(encodeV315DataPacket(payload)); err != nil {
		return fmt.Errorf("write TTC error frame: %w", err)
	}

	return nil
}

// oerMaxMessageLen keeps a synthesized error text inside the short (single
// length byte) CLR form, whose encoding is the same whatever the session
// negotiated.
const oerMaxMessageLen = 240

// sendOracleError sends an ORA-01031 (insufficient privileges) error to the client.
func (s *session) sendOracleError(queryErr error) error {
	return s.writeTTCError(1031, queryErr.Error())
}

// formatOracleBinds formats bind values for storage.
func formatOracleBinds(values []string) *store.QueryParameters {
	if len(values) == 0 {
		return nil
	}

	return &store.QueryParameters{
		Values: values,
	}
}

// truncateSQL truncates SQL for logging purposes.
func truncateSQL(sql string, maxLen int) string {
	if len(sql) <= maxLen {
		return sql
	}

	return sql[:maxLen] + "..."
}

// parseResponseRowsAffected attempts to extract rows affected from a TTC Response.
func parseResponseRowsAffected(ttcPayload []byte) *int64 {
	if len(ttcPayload) < 10 {
		return nil
	}

	// Row count at offset 8 (func + seq + errcode + cursor)
	rowCount := int64(binary.BigEndian.Uint32(ttcPayload[8:12]))
	if rowCount > 0 {
		return &rowCount
	}

	return nil
}

// decodeCursorIDFromOCLOSE extracts cursor ID from an OCLOSE payload.
// OCLOSE layout: func(1) + cursorID(2)
func decodeCursorIDFromOCLOSE(ttcPayload []byte) (uint16, error) {
	if len(ttcPayload) < 3 {
		return 0, fmt.Errorf("%w: OCLOSE needs at least 3 bytes", ErrTTCPayloadTooShort)
	}

	return binary.BigEndian.Uint16(ttcPayload[1:3]), nil
}

// checkQuotas checks whether quota limits have been reached. It also enforces
// the grant's expiry between commands: expiry is otherwise only validated at
// connect time, so without this a session opened just before expiry could keep
// issuing queries indefinitely.
func (s *session) checkQuotas() error {
	// A grant revoked mid-session invalidates every subsequent command, even
	// before the watchdog force-closes the connection.
	if s.revocation.Revoked() {
		return shared.ErrGrantRevoked
	}

	if s.grant == nil {
		return nil
	}

	if !s.grant.ExpiresAt.IsZero() && !time.Now().Before(s.grant.ExpiresAt) {
		return shared.ErrGrantExpired
	}

	if maxQueries := s.grant.MaxQueryCounts(); maxQueries != nil && s.grant.QueryCount >= *maxQueries {
		return ErrQueryLimitExceed
	}

	if maxBytes := s.grant.MaxBytesTransferred(); maxBytes != nil && s.grant.BytesTransferred >= *maxBytes {
		return ErrDataLimitExceed
	}

	return nil
}

// holdIfNeeded runs the approval gate for one statement, after the static
// validators and before anything is forwarded upstream. Returns the uid of the
// pending row the gate created (uuid.Nil when no hold happened).
func (s *session) holdIfNeeded(sql string) (uuid.UUID, error) {
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
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// setHeldQuery records the currently parked statement so an out-of-band
// cancellation path can end it.
func (s *session) setHeldQuery(uid uuid.UUID) {
	s.heldMu.Lock()
	s.heldQueryUID = uid
	s.heldMu.Unlock()
}

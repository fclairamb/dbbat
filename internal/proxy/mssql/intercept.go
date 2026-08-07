package mssql

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// maxSQLTextLen bounds the statement text stored on a query row, so a
// multi-megabyte batch does not bloat the queries table.
const maxSQLTextLen = 8 * 1024

// bulkCopyPattern matches the statements that move rows in bulk, which is what
// the grant's block_copy control is about on SQL Server. `INSERT BULK` is the
// statement a bulk-copy client sends before streaming a BulkLoadBCP message;
// `BULK INSERT` and `OPENROWSET(BULK …)` are the server-side file forms.
var bulkCopyPattern = regexp.MustCompile(`(?i)\b(?:BULK\s+INSERT|INSERT\s+BULK|OPENROWSET\s*\(\s*BULK)\b`)

// pendingQuery is the statement whose response is currently in flight.
type pendingQuery struct {
	sqlText string
	params  *store.QueryParameters
	start   time.Time
	// approvalUID is the row an approval hold already inserted for this
	// statement (uuid.Nil when no hold happened), so the completion is an
	// UPDATE on that row rather than a second INSERT.
	approvalUID uuid.UUID
	// prepareFor, when set, is the statement the upstream is about to hand
	// back a prepared-statement handle for.
	prepareFor string
}

// statement is what one client message turned out to be asking for.
type statement struct {
	// text is what is logged and shown, and what approval patterns match.
	text string
	// enforce holds every statement the grant controls run against. Usually
	// one; an RPC batch has several, and an sp_execute contributes the SQL its
	// handle was prepared with — which is what actually runs.
	enforce []string
	// params are the bind values, captured for logging only. Enforcement is
	// on the statement template, which is the right granularity for read-only
	// and DDL blocking and is not something a parameter value can change.
	params *store.QueryParameters
	// refusal is a decision already reached while extracting the statement:
	// an RPC form dbbat cannot reason about under this grant.
	refusal error
	// prepareFor is the statement a handle is about to be returned for.
	prepareFor string
}

// interceptClientMessage is the clientMessageHook: every complete message the
// client sends passes through here before anything reaches the upstream.
//
// The relay hands it whole logical messages rather than packets, so a statement
// split across a packet boundary cannot slip past.
func (s *session) interceptClientMessage(ctx context.Context, msgType byte, payload []byte) ([]byte, error) {
	switch msgType {
	case packetTypeSQLBatch:
		return s.interceptSQLBatch(ctx, payload)

	case packetTypeRPC:
		return s.interceptRPC(ctx, payload)

	case packetTypeBulkLoad:
		return s.interceptBulkLoad(ctx, payload)

	default:
		// Attention, Transaction Manager and anything else carry no statement
		// dbbat can enforce a grant on — see the interception table in
		// docs/mssql.md. They are relayed untouched.
		return forwarded(payload), nil
	}
}

// interceptSQLBatch handles a SQLBatch (0x01): one or more statements as
// UCS-2LE text.
func (s *session) interceptSQLBatch(ctx context.Context, payload []byte) ([]byte, error) {
	sql, err := parseSQLBatch(payload)
	if err != nil {
		return s.runStatement(ctx, payload, statement{text: "SQLBatch", refusal: err})
	}

	return s.runStatement(ctx, payload, statement{text: sql, enforce: []string{sql}})
}

// interceptRPC handles an RPC (0x03).
//
// RPC is **enforced**, not merely logged. read_only and block_ddl are access
// controls, and a log-only RPC path would let any client bypass them by
// wrapping a write in sp_executesql.
func (s *session) interceptRPC(ctx context.Context, payload []byte) ([]byte, error) {
	requests, err := parseRPC(payload)
	if err != nil {
		s.logger.WarnContext(ctx, "MSSQL RPC could not be parsed", slog.Any("error", err))

		return s.runStatement(ctx, payload, statement{text: "RPC", refusal: ErrMalformedRequest})
	}

	st := s.describeRPC(requests)

	return s.runStatement(ctx, payload, st)
}

// describeRPC turns the requests of one RPC message into a statement: what to
// log, what to enforce on, and whether the form is one dbbat can reason about
// at all.
func (s *session) describeRPC(requests []rpcRequest) statement {
	st := statement{}
	texts := make([]string, 0, len(requests))

	for _, req := range requests {
		texts = append(texts, req.logText())
		st.params = appendParams(st.params, req.parameterValues())

		switch {
		case describedInline(req):
			// Every candidate, not just the logged one: dbbat must not be able
			// to validate one parameter while the upstream runs another.
			st.enforce = append(st.enforce, req.statementTexts()...)

			if preparingProcs[req.procID] {
				stmt, _ := req.statementText()
				st.prepareFor = stmt
			}

		case handleOnly(req):
			sql, known := s.preparedStatement(req)

			switch {
			case known:
				st.enforce = append(st.enforce, sql)
			case harmlessProcs[req.procID]:
				// Releasing or repositioning something already checked.
			case grantRestricts(s.grant) && st.refusal == nil:
				st.refusal = ErrUnknownPreparedStatement
			}

		case req.isWellKnown() && harmlessProcs[req.procID]:
			// Cursor bookkeeping: no statement, nothing a control is about.

		default:
			// A stored procedure by name (or a well-known form with no
			// statement and no handle). dbbat cannot see the body, so under a
			// grant that restricts anything it fails closed rather than
			// forwarding something it cannot vouch for.
			if grantRestricts(s.grant) && st.refusal == nil {
				st.refusal = ErrOpaqueProcedureBlocked
			}
		}
	}

	st.text = truncateSQL(strings.Join(texts, "; "))

	return st
}

// describedInline reports whether the request carries its statement inline.
func describedInline(req rpcRequest) bool {
	_, ok := req.statementText()

	return ok
}

// handleOnly reports whether the request executes something prepared earlier.
func handleOnly(req rpcRequest) bool {
	_, ok := req.handle()

	return ok
}

// preparedStatement looks up the statement a handle was prepared with on this
// session.
func (s *session) preparedStatement(req rpcRequest) (string, bool) {
	handle, ok := req.handle()
	if !ok {
		return "", false
	}

	s.preparedMu.Lock()
	defer s.preparedMu.Unlock()

	sql, known := s.prepared[handle]

	return sql, known
}

// rememberPrepared records the statement a handle stands for, so a later
// sp_execute is enforced on the SQL it actually runs.
func (s *session) rememberPrepared(handle int64, sql string) {
	if handle == 0 || sql == "" {
		return
	}

	s.preparedMu.Lock()
	defer s.preparedMu.Unlock()

	// Bounded: a pathological client preparing without ever unpreparing must
	// not grow this without limit. Losing the oldest entries only costs a
	// fail-closed refusal on a stale handle.
	const maxPreparedStatements = 4096
	if len(s.prepared) >= maxPreparedStatements {
		clear(s.prepared)
	}

	s.prepared[handle] = sql
}

// interceptBulkLoad handles a BulkLoadBCP (0x07) message: the rows of a bulk
// copy the client announced with an `INSERT BULK` statement.
//
// The statement is the primary gate — it goes through interceptSQLBatch like
// any other write. This is the belt to that braces: a grant that forbids
// writing or bulk copying refuses the data too, so no ordering trick reaches
// the upstream with rows.
func (s *session) interceptBulkLoad(ctx context.Context, payload []byte) ([]byte, error) {
	st := statement{text: "BULK LOAD DATA"}

	switch {
	case s.grant.IsReadOnly():
		st.refusal = shared.ErrReadOnlyViolation
	case s.grant.ShouldBlockCopy():
		st.refusal = ErrBulkCopyBlocked
	}

	// Allowed or not, it goes through the pipeline: the rows carry no statement
	// to check, but they do consume the grant's quota and belong in the audit.
	return s.runStatement(ctx, payload, st)
}

// runStatement is the enforcement pipeline, in the same order every other
// proxy runs it: revocation, quotas, the static grant controls, then the
// approval gate. Only a statement that clears all four is forwarded.
//
// A nil payload return tells the relay the hook answered the client itself.
func (s *session) runStatement(ctx context.Context, payload []byte, st statement) ([]byte, error) {
	start := time.Now()

	if st.refusal != nil {
		return s.refuse(ctx, st, start, st.refusal)
	}

	// A grant revoked mid-session invalidates every subsequent statement, even
	// before the watchdog force-closes the connection.
	if s.revocation != nil && s.revocation.Revoked() {
		return s.refuse(ctx, st, start, shared.ErrGrantRevoked)
	}

	if err := s.checkGrantQuotas(); err != nil {
		return s.refuse(ctx, st, start, err)
	}

	if err := s.validate(st); err != nil {
		return s.refuse(ctx, st, start, err)
	}

	approvalUID, err := s.holdIfNeeded(ctx, st)
	if err != nil {
		return s.refuseHeld(ctx, st, start, approvalUID, err)
	}

	s.setPending(&pendingQuery{
		sqlText:     truncateSQL(st.text),
		params:      st.params,
		start:       start,
		approvalUID: approvalUID,
		prepareFor:  st.prepareFor,
	})

	return forwarded(payload), nil
}

// forwarded normalizes a payload for the hook contract in relay.go, where a nil
// return means "the hook answered the client itself and nothing must reach the
// upstream".
//
// ATTENTION is a message with no payload at all, so a hook that handed its own
// argument straight back would silently swallow every client cancellation.
func forwarded(payload []byte) []byte {
	if payload == nil {
		return []byte{}
	}

	return payload
}

// validate runs the grant's static controls over every statement the message
// would execute.
func (s *session) validate(st statement) error {
	for _, sql := range st.enforce {
		if sql == "" {
			continue
		}

		if err := shared.ValidateQuery(sql, s.grant); err != nil {
			return err
		}

		if s.grant.ShouldBlockCopy() && bulkCopyPattern.MatchString(sql) {
			return ErrBulkCopyBlocked
		}
	}

	return nil
}

// grantRestricts reports whether the grant limits what a statement may do. It
// is the switch behind failing closed: with nothing to enforce there is nothing
// to fail closed about, so an unrestricted grant still runs opaque procedures.
func grantRestricts(grant *store.Grant) bool {
	return grant != nil && (grant.IsReadOnly() || grant.ShouldBlockDDL() || grant.ShouldBlockCopy())
}

// holdIfNeeded runs the approval gate for one statement.
//
// The session goroutine blocks here for as long as the hold lasts. Parking the
// client conn is what keeps that safe: the watcher goes on reading the socket,
// so a client that gives up and disconnects ends the hold instead of leaving it
// parked forever, and anything the client sent meanwhile is replayed in order
// once the session resumes.
func (s *session) holdIfNeeded(ctx context.Context, st statement) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(st.text)
	if !matched {
		return uuid.Nil, nil
	}

	var gone <-chan struct{}

	if s.watched != nil {
		gone = s.watched.Park()
		defer s.watched.Unpark()
	}

	return s.approvalGate.Hold(ctx, shared.HoldRequest{
		SQL:        truncateSQL(st.text),
		Params:     st.params,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// refuse answers a blocked statement on the client leg and logs it, without
// anything reaching the upstream.
func (s *session) refuse(ctx context.Context, st statement, start time.Time, cause error) ([]byte, error) {
	s.logger.InfoContext(ctx, "MSSQL statement blocked",
		slog.String("user", usernameOf(s.user)),
		slog.String("sql", truncateSQL(st.text)),
		slog.Any("error", cause))

	errText := cause.Error()
	s.recordQuery(ctx, &pendingQuery{
		sqlText: truncateSQL(st.text),
		params:  st.params,
		start:   start,
	}, queryOutcome{queryError: &errText})

	return nil, s.writeRefusal(cause)
}

// refuseHeld reports the outcome of an approval hold. The gate already
// persisted the statement, so the completion is written onto that row rather
// than inserting a duplicate.
func (s *session) refuseHeld(
	ctx context.Context, st statement, start time.Time, approvalUID uuid.UUID, cause error,
) ([]byte, error) {
	if approvalUID == uuid.Nil {
		return s.refuse(ctx, st, start, cause)
	}

	s.logger.InfoContext(ctx, "MSSQL statement blocked by an approval hold",
		slog.String("user", usernameOf(s.user)),
		slog.Any("error", cause))

	errText := cause.Error()
	s.completeHeldQuery(ctx, approvalUID, errText)

	return nil, s.writeRefusal(cause)
}

// writeRefusal sends the ERROR + DONE token stream a refusal takes. It is the
// same shape a real SQL Server error has, so the driver raises it as an
// ordinary SQL error and keeps the connection usable for the next statement.
func (s *session) writeRefusal(cause error) error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()

	return s.pkt.WriteMessage(packetTypeReply, buildStatementRefusal(cause))
}

// appendParams accumulates rendered parameter values across the requests of
// one RPC message.
func appendParams(params *store.QueryParameters, values []string) *store.QueryParameters {
	if len(values) == 0 {
		return params
	}

	if params == nil {
		return &store.QueryParameters{Values: values}
	}

	params.Values = append(params.Values, values...)

	return params
}

// truncateSQL bounds the stored statement text.
func truncateSQL(sql string) string {
	if len(sql) <= maxSQLTextLen {
		return sql
	}

	return sql[:maxSQLTextLen]
}

// usernameOf renders a user for logs without dereferencing a nil.
func usernameOf(user *store.User) string {
	if user == nil {
		return ""
	}

	return user.Username
}

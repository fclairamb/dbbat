package mssql

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/approval"
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
		st.params = appendParams(st.params, req.parameterValues())
		texts = append(texts, s.describeRPCRequest(req, &st))
	}

	st.text = truncateSQL(strings.Join(texts, "; "))

	return st
}

// describeRPCRequest classifies one request, folds its enforcement into st, and
// returns the text the statement is logged and pattern-matched as.
func (s *session) describeRPCRequest(req rpcRequest, st *statement) string {
	switch {
	case describedInline(req):
		// Every candidate, not just the logged one: dbbat must not be able to
		// validate one parameter while the upstream runs another.
		st.enforce = append(st.enforce, req.statementTexts()...)

		stmt, _ := req.statementText()
		if preparingProcs[req.procID] {
			st.prepareFor = stmt
		}

		return stmt

	case handleOnly(req):
		return s.describeHandleRequest(req, st)

	case req.isWellKnown() && harmlessProcs[req.procID]:
		// Cursor bookkeeping: no statement, nothing a control is about.
		return req.logText()

	default:
		// A stored procedure by name (or a well-known form with no statement
		// and no handle). dbbat cannot see the body, so under a grant that
		// restricts anything it fails closed rather than forwarding something
		// it cannot vouch for.
		if grantRestricts(s.grant) && st.refusal == nil {
			st.refusal = ErrOpaqueProcedureBlocked
		}

		return req.logText()
	}
}

// describeHandleRequest classifies a form that executes something prepared
// earlier.
//
// A resolved handle is logged *as the statement it runs*, not as
// "EXEC sp_execute 41". That is what the MySQL proxy does for COM_STMT_EXECUTE,
// and it is load-bearing rather than cosmetic: approval patterns are matched
// against this text, so logging the handle instead would make the four-eyes rule
// silently stop applying the moment a client used prepared statements.
func (s *session) describeHandleRequest(req rpcRequest, st *statement) string {
	// sp_unprepare and friends release a handle; the statement behind it was
	// checked when it was prepared, and logging its SQL here would read as if it
	// ran again.
	if harmlessProcs[req.procID] {
		return req.logText()
	}

	sql, known := s.preparedStatement(req)
	if known {
		st.enforce = append(st.enforce, sql)

		return sql
	}

	if grantRestricts(s.grant) && st.refusal == nil {
		st.refusal = ErrUnknownPreparedStatement
	}

	return req.logText()
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

// attentionCancelReason is what a hold released by a client cancel is recorded
// and announced as. It reads as an outcome in the approvals UI, next to
// "client disconnected while awaiting approval".
const attentionCancelReason = "canceled by the client (ATTENTION)"

// holdIfNeeded runs the approval gate for one statement.
//
// The session goroutine blocks here for as long as the hold lasts. The client
// leg keeps being read by readClientMessages throughout, which is what keeps
// that safe: a client that gives up and disconnects ends the hold instead of
// leaving it parked forever, one that cancels ends it through
// cancelHeldStatement, and anything else it sent meanwhile is forwarded in
// order once the session resumes.
func (s *session) holdIfNeeded(ctx context.Context, st statement) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(st.text)
	if !matched {
		return uuid.Nil, nil
	}

	s.armHoldCancel()
	defer s.disarmHoldCancel()

	return s.approvalGate.Hold(ctx, shared.HoldRequest{
		SQL:        truncateSQL(st.text),
		Params:     st.params,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: s.clientGone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// armHoldCancel opens the window in which an ATTENTION cancels the statement
// about to be parked, rather than traveling upstream. It has to open *before*
// the gate is asked for a hold: the uid only exists once the gate has persisted
// the pending row, and a driver whose timeout has already fired can send its
// cancel inside that gap.
func (s *session) armHoldCancel() {
	s.heldMu.Lock()
	s.holdArmed = true
	s.heldMu.Unlock()
}

// disarmHoldCancel closes that window once the hold is over. In the ordinary
// case setHeldQuery has already closed it; this is the backstop for the paths
// where the gate never published a uid at all, such as a failed pending insert.
//
// A cancel recorded in the arm window can still be undecided here, and that is
// the same case: no uid ever appeared, so nothing arbitrated it and nothing
// ran. The ATTENTION was consumed, the hold is over, and the client is owed its
// acknowledgement — which the refusal path immediately takes.
func (s *session) disarmHoldCancel() {
	s.heldMu.Lock()
	s.holdArmed = false

	if s.armedCancel {
		s.armedCancel = false
		s.attentionAckDue = true
	}

	s.heldMu.Unlock()
}

// cancelHeldStatement releases a statement parked on a human because the client
// sent an ATTENTION, the TDS cancel. It reports whether the ATTENTION was
// consumed here — false means nothing was parked, or a human resolved the hold
// in the same instant, and the cancel must travel upstream as it always has.
//
// Called from the reader goroutine, never from the session goroutine.
func (s *session) cancelHeldStatement() bool {
	s.heldMu.Lock()

	uid, armed := s.heldQueryUID, s.holdArmed

	// A cancel already consumed for the statement in flight — answered, or
	// still waiting for the uid that will arbitrate it — swallows any repeat,
	// so a second ATTENTION cannot slip upstream and cancel whatever runs next.
	consumed := s.attentionAckDue || s.armedCancel

	// The gate is registering the hold right now, so there is no uid to cancel
	// yet. Record the cancel *without* deciding what it earns: whether the
	// client is owed an acknowledgement depends on the resolve winning, which
	// only the OnPending callback can find out, the instant the uid exists.
	if uid == uuid.Nil && armed && !consumed {
		s.armedCancel = true
	}

	s.heldMu.Unlock()

	if consumed {
		return true
	}

	if uid == uuid.Nil {
		return armed
	}

	return s.resolveHoldCanceled(uid)
}

// resolveHoldCanceled hands the parked session an abandoned decision and marks
// the acknowledgement the client is owed. It reports whether the hold was still
// there to be canceled: the registry's Resolve is what arbitrates a cancel
// racing an approver, and the loser must not pretend it won.
func (s *session) resolveHoldCanceled(uid uuid.UUID) bool {
	if uid == uuid.Nil || s.server == nil || s.server.approvalDeps.Registry == nil {
		return false
	}

	if !s.server.approvalDeps.Registry.Resolve(approval.Decision{
		QueryUID: uid,
		Status:   store.ApprovalAbandoned,
		Reason:   attentionCancelReason,
		At:       time.Now(),
	}) {
		return false
	}

	s.heldMu.Lock()
	s.attentionAckDue = true
	s.heldMu.Unlock()

	return true
}

// takeAttentionAck reports whether the statement being answered owes the client
// a cancel acknowledgement, consuming the mark.
func (s *session) takeAttentionAck() bool {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	due := s.attentionAckDue
	s.attentionAckDue = false

	return due
}

// deferAttentionUpstream hands the forward pump an ATTENTION the reader already
// took off the wire and that turned out to belong upstream after all.
//
// It cannot simply be written here: this runs while the gate is still inside
// Hold, so the statement the cancel is meant to interrupt has not been
// forwarded yet. Emitting it now would cancel nothing and let the statement run
// on unbothered — the pump re-emits it *behind* the statement instead, which is
// exactly where the reader would have queued it had it not consumed it.
func (s *session) deferAttentionUpstream() {
	s.heldMu.Lock()
	s.attentionUpstreamDue = true
	s.heldMu.Unlock()
}

// takeAttentionUpstream reports whether a consumed ATTENTION still has to be
// put back on the upstream leg, consuming the mark.
func (s *session) takeAttentionUpstream() bool {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	due := s.attentionUpstreamDue
	s.attentionUpstreamDue = false

	return due
}

// refuse answers a blocked statement on the client leg and logs it, without
// anything reaching the upstream.
func (s *session) refuse(ctx context.Context, st statement, start time.Time, cause error) ([]byte, error) {
	// A statement the client canceled is answered as a cancel, whatever else
	// went on to decide against it: the driver is reading for its cancel
	// acknowledgement and nothing but a DONE_ATTN ends that wait.
	if s.takeAttentionAck() {
		return s.acknowledgeAttention(ctx, st, start)
	}

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

// acknowledgeAttention answers a statement the client canceled with the TDS
// cancel acknowledgement — a DONE carrying DONE_ATTN — rather than an error or
// a dropped socket, and logs the query row as canceled.
//
// It answers for a statement that never reached the upstream, which is the
// whole point: an ATTENTION during an approval hold cancels the statement, it
// does not release it for execution.
func (s *session) acknowledgeAttention(ctx context.Context, st statement, start time.Time) ([]byte, error) {
	s.logger.InfoContext(ctx, "MSSQL statement canceled by the client while awaiting approval",
		slog.String("user", usernameOf(s.user)),
		slog.String("sql", truncateSQL(st.text)))

	errText := attentionCancelReason
	s.recordQuery(ctx, &pendingQuery{
		sqlText: truncateSQL(st.text),
		params:  st.params,
		start:   start,
	}, queryOutcome{queryError: &errText})

	return nil, s.writeAttentionAck()
}

// writeAttentionAck sends the cancel acknowledgement on the client leg, under
// the lock both relay pumps write through.
func (s *session) writeAttentionAck() error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()

	return s.pkt.WriteMessage(packetTypeReply, buildAttentionAck())
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

	// The client canceled it while it hung. The hold is already recorded as
	// abandoned by the gate; what is left is to answer the cancel and complete
	// the row the gate created.
	if s.takeAttentionAck() {
		s.logger.InfoContext(ctx, "MSSQL approval hold canceled by the client",
			slog.String("user", usernameOf(s.user)),
			slog.String("sql", truncateSQL(st.text)))

		s.completeHeldQuery(ctx, approvalUID, attentionCancelReason)

		return nil, s.writeAttentionAck()
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

// truncateSQL bounds the stored statement text, cutting on a rune boundary.
//
// The limit is in bytes, but the text is UTF-8 decoded from UCS-2: a blind cut
// can land inside a multi-byte rune, and the invalid string that results fails
// the query insert outright. Losing the audit row for an oversize statement is
// exactly the wrong failure.
func truncateSQL(sql string) string {
	if len(sql) <= maxSQLTextLen {
		return sql
	}

	cut := maxSQLTextLen
	for cut > 0 && !utf8.RuneStart(sql[cut]) {
		cut--
	}

	return sql[:cut]
}

// usernameOf renders a user for logs without dereferencing a nil.
func usernameOf(user *store.User) string {
	if user == nil {
		return ""
	}

	return user.Username
}

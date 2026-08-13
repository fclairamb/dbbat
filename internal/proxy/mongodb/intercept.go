package mongodb

import (
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/safe"
	"github.com/fclairamb/dbbat/internal/store"
)

// pendingQuery correlates an in-flight command to the upstream reply that
// answers it (phase 3), keyed by the client requestID (== the reply's
// responseTo).
type pendingQuery struct {
	command    string
	sqlText    string
	params     *store.QueryParameters
	start      time.Time
	moreToCome bool
	// cursorID is the server cursor a getMore iterates (item 6); 0 for other
	// commands. Used to drop the cursor→origin link once the cursor is drained.
	cursorID int64
	// approvalUID is the row an approval hold already inserted for this
	// command (uuid.Nil when no hold happened). Non-nil means the completion
	// path must UPDATE that row rather than INSERT a second one.
	approvalUID uuid.UUID
	// resultsTruncated reports that cursor-row capture stopped on a storage
	// limit, so the rows stored are a prefix of the batch the client received.
	resultsTruncated bool
}

// pumpClientToUpstream reads each client message, classifies + validates it
// (phase 2), forwards it verbatim when allowed, and registers it for result
// capture (phase 3).
func (s *Session) pumpClientToUpstream() error {
	for {
		m, err := s.readClientMessage()
		if err != nil {
			return err
		}

		if m.opCode != opCodeMsg {
			if err := s.refuseLegacyOpCode(m); err != nil {
				return err
			}

			continue
		}

		if err := s.handleClientOpMsg(m); err != nil {
			return err
		}
	}
}

// refuseLegacyOpCode refuses a pre-OP_MSG opcode arriving after authentication
// (OP_QUERY, OP_GET_MORE, OP_INSERT/UPDATE/DELETE, OP_KILL_CURSORS).
//
// Post-auth, drivers speak OP_MSG exclusively — we advertise
// maxWireVersion 21 — and MongoDB itself removed the legacy opcodes in 5.1.
// dbbat decodes none of them, so forwarding one verbatim (which is what it used
// to do) put a command on the upstream that never reached
// shared.ValidateMongoCommand: no $db check, no read_only check, nothing shaped
// like a statement in the query log. Against an upstream older than 5.1 that
// was a way around the grant, so the refusal is independent of grant controls,
// exactly like the $db check. Writing decoders for a path MongoDB deleted was
// weighed and rejected: it is a lot of wire code and new attack surface for
// clients that no longer exist. The pre-auth handshake, which legitimately uses
// OP_QUERY for the first hello, is unaffected — this runs only after auth.
func (s *Session) refuseLegacyOpCode(m *message) error {
	name := opCodeName(m.opCode)

	s.logger.WarnContext(s.ctx, "MongoDB legacy opcode refused",
		slog.String("opcode", name),
		slog.Any("error", ErrLegacyOpCode))

	// Logged as a statement so the attempt is visible in the query history
	// rather than only in the process log.
	errStr := ErrLegacyOpCode.Error()
	s.recordQuery(&pendingQuery{command: name, sqlText: name, start: time.Now()}, nil, nil, &errStr)

	if !opCodeExpectsReply(m.opCode) {
		return nil
	}

	return s.replyOpReply(m.requestID, unauthorizedDoc(errStr))
}

// handleClientOpMsg parses, classifies, validates, forwards and registers one
// client OP_MSG.
func (s *Session) handleClientOpMsg(m *message) error {
	parsed, err := parseOpMsg(m.body)
	if err != nil {
		return err
	}

	body, ok := parsed.commandBody()
	if !ok {
		return ErrNoCommandBody
	}

	cmd := commandName(body)
	dbName := lookupString(body, "$db")
	moreToCome := parsed.flags&flagMoreToCome != 0

	// Reject immediately when the grant is revoked or exhausted, so the next
	// command after a mid-session revoke/quota-hit fails cleanly rather than
	// waiting for the watchdog's poll interval (parity with the MySQL proxy).
	if s.revocation != nil && s.revocation.Revoked() {
		return s.rejectCommand(m, cmd, body, moreToCome, shared.ErrGrantRevoked)
	}

	if qerr := checkQuotas(s.grant); qerr != nil {
		return s.rejectCommand(m, cmd, body, moreToCome, qerr)
	}

	if verr := shared.ValidateMongoCommand(cmd, dbName, body, s.database, s.grant); verr != nil {
		return s.rejectCommand(m, cmd, body, moreToCome, verr)
	}

	// A killOperations aimed at this session may be releasing a command parked
	// on a human. Handle it before forwarding.
	if cmd == "killOperations" && s.KillHeldQuery() {
		s.logger.InfoContext(s.ctx, "killOperations released an approval hold")
	}

	sqlText := buildSQLText(cmd, body)

	// Approval hold — after the static validators, before the command is
	// forwarded upstream.
	approvalUID, herr := s.holdIfNeeded(sqlText)
	if herr != nil {
		return s.rejectHeldCommand(m, cmd, body, moreToCome, approvalUID, herr)
	}

	// Register for result capture before forwarding so a fast upstream reply
	// never races an unregistered query.
	pq := &pendingQuery{
		command:     cmd,
		sqlText:     sqlText,
		params:      extractParams(body),
		start:       time.Now(),
		moreToCome:  moreToCome,
		approvalUID: approvalUID,
	}

	// Link a getMore to the find/aggregate cursor it iterates (item 6).
	if cmd == "getMore" {
		s.annotateGetMore(pq, body)
	}

	s.registerPending(m.requestID, pq)

	if err := s.forward(m); err != nil {
		s.takePending(m.requestID)

		return err
	}

	// moreToCome (w:0 unacknowledged writes): no reply is coming, so record the
	// query now rather than waiting for a response that never arrives.
	if moreToCome {
		if pq := s.takePending(m.requestID); pq != nil {
			s.recordQuery(pq, nil, nil, nil)
		}
	}

	return nil
}

// rejectCommand handles a blocked command: for moreToCome (fire-and-forget)
// writes it is dropped silently (the client isn't listening); otherwise an
// Unauthorized error reply is returned to the client. Either way it is logged.
func (s *Session) rejectCommand(m *message, cmd string, body bson.Raw, moreToCome bool, verr error) error {
	s.logger.InfoContext(s.ctx, "MongoDB command blocked",
		slog.String("command", cmd),
		slog.String("db", lookupString(body, "$db")),
		slog.Any("error", verr))

	pq := &pendingQuery{command: cmd, sqlText: buildSQLText(cmd, body), params: extractParams(body), start: time.Now()}
	errStr := verr.Error()
	s.recordQuery(pq, nil, nil, &errStr)

	if moreToCome {
		return nil
	}

	return s.replyOpMsg(m.requestID, unauthorizedDoc(verr.Error()))
}

// forward relays a message verbatim to the upstream and dumps it.
func (s *Session) forward(m *message) error {
	s.dumpPacket(dump.DirClientToServer, m.raw)

	_, err := s.upstream.conn.Write(m.raw)

	return err
}

// registerPending stores an in-flight query keyed by client requestID.
func (s *Session) registerPending(requestID int32, pq *pendingQuery) {
	s.pendingMu.Lock()
	s.pending[requestID] = pq
	s.pendingMu.Unlock()
}

// takePending removes and returns the pending query for responseTo, if any.
func (s *Session) takePending(responseTo int32) *pendingQuery {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	pq, ok := s.pending[responseTo]
	if !ok {
		return nil
	}

	delete(s.pending, responseTo)

	return pq
}

// pendingCommand returns the command name of the in-flight query for responseTo
// without removing it (a peek), or "" if none is registered.
func (s *Session) pendingCommand(responseTo int32) string {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	if pq, ok := s.pending[responseTo]; ok {
		return pq.command
	}

	return ""
}

// holdIfNeeded runs the approval gate for one MongoDB command.
func (s *Session) holdIfNeeded(sqlText string) (uuid.UUID, error) {
	if !s.approvalGate.Active() {
		return uuid.Nil, nil
	}

	pattern, matched := s.approvalGate.Match(sqlText)
	if !matched {
		return uuid.Nil, nil
	}

	var gone <-chan struct{}

	if s.watched != nil {
		gone = s.watched.Park()
		defer s.watched.Unpark()
	}

	return s.approvalGate.Hold(s.ctx, shared.HoldRequest{
		SQL:        sqlText,
		Pattern:    pattern,
		StartedAt:  time.Now(),
		ClientGone: gone,
		Guard:      s.guard,
		OnPending:  s.setHeldQuery,
	})
}

// rejectHeldCommand returns the hold's outcome to the client. When the gate
// already persisted a row for the command, the completion is written onto that
// row rather than inserting a duplicate.
func (s *Session) rejectHeldCommand(
	m *message, cmd string, body bson.Raw, moreToCome bool, approvalUID uuid.UUID, cause error,
) error {
	if approvalUID != uuid.Nil && s.server != nil && s.server.store != nil {
		errStr := cause.Error()

		go safe.RunGuarded(s.ctx, s.logger, goroutineNameHeldCommandCompletion, func() {
			if err := s.server.store.UpdateQueryCompletion(s.ctx, approvalUID, nil, nil, &errStr, false, false); err != nil {
				s.logger.DebugContext(s.ctx, "failed to complete held command", slog.Any("error", err))
			}
		})

		// The row already exists; only send the protocol-native error.
		s.logger.InfoContext(s.ctx, "MongoDB command blocked by approval hold",
			slog.String("command", cmd),
			slog.Any("error", cause))

		if moreToCome {
			return nil
		}

		return s.replyOpMsg(m.requestID, unauthorizedDoc(cause.Error()))
	}

	return s.rejectCommand(m, cmd, body, moreToCome, cause)
}

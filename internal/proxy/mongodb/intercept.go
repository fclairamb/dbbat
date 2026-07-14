package mongodb

import (
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
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
			// Post-auth, drivers speak OP_MSG exclusively (we advertised
			// maxWireVersion >= 6). Forward anything else verbatim.
			if err := s.forward(m); err != nil {
				return err
			}

			continue
		}

		if err := s.handleClientOpMsg(m); err != nil {
			return err
		}
	}
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

	// Register for result capture before forwarding so a fast upstream reply
	// never races an unregistered query.
	pq := &pendingQuery{
		command:    cmd,
		sqlText:    buildSQLText(cmd, body),
		params:     extractParams(body),
		start:      time.Now(),
		moreToCome: moreToCome,
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

package oracle

import (
	"io"
	"sync"

	"github.com/fclairamb/dbbat/internal/safe"
)

// A TTC call is answered before the next one is sent by every driver dbbat
// has been measured against, but nothing on the wire forces it. The proxy keeps
// one in-flight statement (tracker.pendingQuery), so a call that arrives before
// the previous one's response would have that response credited to it: the
// first statement recorded with no result and a duration ending when the second
// arrived, the second one carrying the first's row count or ORA- error.
//
// The fix is the one the SQL Server proxy uses (claimUpstreamTurn): a statement
// call that arrives while the previous call has not been answered at all is
// held until the upstream starts answering. Break/reset markers are not Data
// packets and are read and forwarded at once, so a cancel still works.
//
// "Answered" is deliberately the weakest signal that is reliable: any packet
// from the upstream after the call was sent. TTC's end-of-call is not reliably
// detectable (a statement can stay pending until the next one flushes it, see
// handleResponse), so waiting for it would hold sequential clients forever. The
// cost is that a response spanning several packets can still have its tail
// credited to the next call when the client pipelined it.

// callTurn is the per-session state of that wait.
type callTurn struct {
	// upstreamDone is closed when the upstream leg ends, so a held call does not
	// outlive the session. Nil when the session was not built by proxyMessages.
	upstreamDone chan struct{}
	doneOnce     sync.Once

	// readAhead is the client read started while a call was unanswered. Only the
	// client leg touches it.
	readAhead chan clientRead

	// held are the calls read while one was unanswered, in arrival order.
	held []*TNSPacket

	// answer and armed are guarded by session.trackerMu: answer is closed once
	// the upstream has sent anything since armed was forwarded.
	armed  *pendingOracleQuery
	answer chan struct{}
}

type clientRead struct {
	pkt *TNSPacket
	err error
}

func (t *callTurn) noteUpstreamGone() {
	if t.upstreamDone != nil {
		t.doneOnce.Do(func() { close(t.upstreamDone) })
	}
}

// armCallAnswer notes that the statement now in flight is about to go upstream.
func (s *session) armCallAnswer() {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	p := s.tracker.pendingQuery
	if p == nil || p == s.callTurn.armed {
		return
	}

	s.callTurn.armed = p
	s.callTurn.answer = make(chan struct{})
}

// noteUpstreamResponse records that the upstream has started answering.
func (s *session) noteUpstreamResponse() {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	ch := s.callTurn.answer
	if ch == nil {
		return
	}

	select {
	case <-ch:
	default:
		close(ch)
	}
}

// unansweredCall returns a channel closed when the in-flight statement gets its
// first answer, or nil when nothing is owed.
func (s *session) unansweredCall() <-chan struct{} {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	if s.tracker == nil || s.tracker.pendingQuery == nil ||
		s.tracker.pendingQuery != s.callTurn.armed || s.callTurn.answer == nil {
		return nil
	}

	select {
	case <-s.callTurn.answer:
		return nil
	default:
		return s.callTurn.answer
	}
}

// pipelineQueueMax bounds how many held calls the proxy reads ahead of the
// answer they wait for.
const pipelineQueueMax = 4

// isGatedCall reports whether pkt is a Data packet that opens a call: a TTC
// function message (0x03), a legacy OALL8 (0x0e), or a piggyback (0x11, the
// close-cursors list or session-state message a call is stapled behind).
//
// Any other Data packet is not a new call but the rest of the one in flight,
// and holding it until that call is answered deadlocks the session: the
// upstream answers only once it has the whole message. That is what a
// statement longer than execMaxSQLLen does, whose tail reassembly does not
// read (statementFragmentShortfall refuses the implausible length), so the
// tail arrives here as its own packets — a 2 MB statement from go-ora hung
// TestIntegration_OracleStatementLengthCeiling until the suite timed out.
func isGatedCall(pkt *TNSPacket) bool {
	if pkt.Type != TNSPacketTypeData || len(pkt.Payload) < ttcDataFlagsSize+1 {
		return false
	}

	switch TTCFunctionCode(pkt.Payload[ttcDataFlagsSize]) { //nolint:exhaustive // only the calls whose answer may be held
	case TTCFuncPiggyback, TTCFuncOALL8, TTCFuncOFETCH:
		return true
	default:
		return false
	}
}

// readsAheadOf reports whether the client may be read past pkt while it is held.
// Not when pkt's message continues in packets that reassembly has to read from
// the socket itself.
func (s *session) readsAheadOf(pkt *TNSPacket) bool {
	ttc := extractTTCPayload(pkt.Payload)
	if ttc == nil {
		return true
	}

	_, fragmented := statementFragmentShortfall(ttc, s.clientWide64Encoding)

	return !fragmented
}

// nextClientPacket reads the next client packet.
//
// While a call is unanswered the client is read in the background: a non-Data
// packet (a break) is returned at once, and a Data call that would be gated is
// held until the answer starts. A client that leaves, or an upstream that does,
// while a call is held ends the wait. A packet is only returned once no read is
// in flight, so the reassembly of a fragmented statement still reads the socket
// by itself.
func (s *session) nextClientPacket() (*TNSPacket, error) {
	t := &s.callTurn

	for {
		answer := s.unansweredCall()

		if len(t.held) > 0 && (answer == nil || !isGatedCall(t.held[0])) {
			pkt := t.held[0]
			t.held = t.held[1:]

			return pkt, nil
		}

		if answer == nil && t.readAhead == nil {
			return readTNSPacket(s.clientConn)
		}

		if answer == nil {
			// Answered while a read was in flight: it is the next packet.
			res := <-t.readAhead
			t.readAhead = nil

			if res.err != nil {
				return nil, res.err
			}

			return res.pkt, nil
		}

		if t.readAhead == nil && len(t.held) < pipelineQueueMax &&
			(len(t.held) == 0 || s.readsAheadOf(t.held[len(t.held)-1])) {
			ch := make(chan clientRead, 1)
			t.readAhead = ch

			go safe.RunGuarded(s.ctx, s.logger, "oracle client read-ahead", func() {
				pkt, err := readTNSPacket(s.clientConn)
				ch <- clientRead{pkt: pkt, err: err}
			})
		}

		select {
		case res := <-t.readAhead:
			t.readAhead = nil

			if res.err != nil {
				return nil, res.err
			}

			if !isGatedCall(res.pkt) {
				return res.pkt, nil
			}

			t.held = append(t.held, res.pkt)
		case <-answer:
		case <-t.upstreamDone:
			return nil, io.EOF
		case <-s.ctx.Done():
			return nil, io.EOF
		}
	}
}

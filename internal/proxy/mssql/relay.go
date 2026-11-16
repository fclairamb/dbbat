package mssql

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"github.com/fclairamb/dbbat/internal/safe"
)

// clientMessageHook is the seam stage 3 fills in.
//
// It is handed every complete message the client sends — SQLBatch, RPC,
// Attention, Transaction Manager — before anything is forwarded, and returns
// the payload to forward. Returning a nil payload with a nil error means the
// hook answered the client itself and nothing must reach the upstream (a
// blocked statement); returning an error ends the session.
//
// The nil-means-answered rule has one sharp edge: ATTENTION is a message with
// no payload at all, so a hook must hand back a non-nil empty slice for it
// rather than its own argument. See forwarded() in intercept.go.
type clientMessageHook func(ctx context.Context, msgType byte, payload []byte) ([]byte, error)

// serverPacketHook is the response-side seam. It sees each response packet as
// it is forwarded, with eom marking the last packet of a logical message, and
// is purely observational: it cannot rewrite the stream, because a result set
// is relayed a packet at a time precisely so it never has to be held in memory.
//
// Stage 3 uses it for row and byte accounting.
type serverPacketHook func(ctx context.Context, msgType byte, payload []byte, eom bool)

// clientMessage is one complete message the client reader lifted off the wire.
type clientMessage struct {
	msgType byte
	payload []byte
	// status carries the bits the forward path must preserve, chiefly
	// RESETCONNECTION. It is captured at read time because the codec only ever
	// remembers the *most recent* message's status, and the reader can be a
	// message or two ahead of the pump.
	status byte
}

// clientReadAhead bounds how many complete client messages may sit between the
// reader goroutine and the forward pump.
//
// It fills whenever the forward pump is not taking messages: while a statement
// is parked on an approval hold, and while the pump waits for the upstream to
// answer the previous request before it sends the next one (a client may
// pipeline requests without waiting for their responses, and the proxy does not
// let it: see claimUpstreamTurn). It is bounded because neither wait may let a
// pipelining client make the proxy allocate without limit. A client that fills
// it stops being read, which costs nothing but the in-band cancel detection
// below. The pump's own backlog, while it waits for its turn, is bounded the
// same way, so a pipelining client can have about twice clientReadAhead
// messages buffered at the proxy in all.
const clientReadAhead = 4

// Names the per-session goroutines report themselves under when one of them
// panics. Log labels, not identifiers.
const (
	relayNameClientToUpstream = "mssql client→upstream"
	relayNameUpstreamToClient = "mssql upstream→client"
	relayNameClientReader     = "mssql client reader"
	relayNameWatchdog         = "mssql limit watchdog"

	// The detached store writes. Each is named after what it writes, because
	// that is the only thing the log line can usefully say: these goroutines
	// outlive the call that spawned them, so the recover on handleConnection
	// never sees them and nothing else records which write died.
	goroutineNameQueryRecord         = "mssql query record"
	goroutineNameHeldQueryCompletion = "mssql held query completion"
)

// relay pumps TDS both ways until either side closes.
//
// The two directions run independently rather than as a request/response loop,
// which is what makes ATTENTION (0x06) work: a client canceling a query sends
// it while the response is still streaming, and a lock-step relay would not
// read it until the response it is meant to interrupt had finished.
//
// The client leg is split further still: reading it is a goroutine of its own,
// separate from the pump that forwards. See readClientMessages.
//
// Each packetRW ends up with exactly one reader and one writer, in different
// goroutines. That is safe because the codec's read and write paths share no
// mutable state — see the note on packetRW — and it is the reason neither side
// takes a lock on the hot path.
// All three run under the shared panic guards. None of them is reached by the
// recover on handleConnection — that sits on a different goroutine — so an
// unguarded panic in the TDS decode would end the process, dropping every other
// live session, of every user, on every database.
//
// That a panicking pump still *yields a value* is load-bearing here in a way it
// is not on a proxy that reads errCh once: this function drains it a second
// time, to wait the other pump out. Before the recover existed a pump could not
// die quietly — it took the process with it — so nothing was ever parked here.
// Recovering is what creates the obligation, and RunRelay is what discharges it.
//
// The reader has no error channel and needs none — its own defers close
// clientGone and clientMsgs, which is exactly how it ends the session on a read
// failure. That is RunGuarded's whole precondition; see its doc.
func (s *session) relay(ctx context.Context) error {
	errCh := make(chan error, 2)

	go safe.RunGuarded(ctx, s.logger, relayNameClientReader, s.readClientMessages)

	go func() {
		errCh <- safe.RunRelay(ctx, s.logger, relayNameClientToUpstream, func() error {
			return s.pumpClientToUpstream(ctx)
		})
	}()
	go func() {
		errCh <- safe.RunRelay(ctx, s.logger, relayNameUpstreamToClient, func() error {
			return s.pumpUpstreamToClient(ctx)
		})
	}()

	err := <-errCh

	// Unblock the other pump: whichever direction failed, the session is over.
	_ = s.upstream.Close()
	_ = s.conn.Close()

	// And unblock the reader, which may be holding a message the pump that just
	// died will never take.
	close(s.done)

	// The first error is the one reported; the peer's, caused by the close above, is only logged.
	if peerErr := <-errCh; peerErr != nil {
		s.logger.DebugContext(ctx, "relay: peer pump stopped", slog.Any("error", peerErr))
	}

	if err == nil || isExpectedDisconnect(err) {
		return nil
	}

	return err
}

// readClientMessages owns the read side of the client leg for the whole relay,
// handing complete logical messages to the forward pump.
//
// Whole messages, not packets, because that is what interception needs: a
// SQLBatch's SQL text can span packets, and a hook that saw fragments could be
// evaded by splitting a statement across a packet boundary. The 16 MB
// reassembly cap in ReadMessage is what bounds the memory that costs.
//
// Reading in a goroutine of its own is what makes a client cancel work *during*
// an approval hold. The pump is blocked on a human for as long as a hold lasts;
// this goroutine is not, so it sees the ATTENTION a driver sends when its
// context is canceled or its query timeout fires, and ends the hold there and
// then. It reads decrypted TDS — it sits on the session codec, above the TLS
// layer — so the cancel is honored on an encrypted client leg too, which is
// what a byte-level watcher below TLS could never manage.
//
// It also subsumes what shared.WatchedConn does for the other protocols: the
// client socket is read at all times, so a FIN is noticed the moment it lands
// rather than sitting unread until the session resumes.
func (s *session) readClientMessages() {
	defer close(s.clientGone)
	defer close(s.clientMsgs)

	for {
		msgType, payload, err := s.pkt.ReadMessage()
		if err != nil {
			// The sender asked for the message to be discarded; that is a
			// complete instruction, not a failure.
			if errors.Is(err, ErrMessageIgnored) {
				continue
			}

			s.setClientReadError(err)

			return
		}

		// A cancel that released a hold must not also reach the upstream: the
		// statement it cancels never got there, and forwarding it would cancel
		// whatever the upstream does next instead.
		if msgType == packetTypeAttention && s.cancelHeldStatement() {
			continue
		}

		msg := clientMessage{
			msgType: msgType,
			payload: payload,
			// Carry RESETCONNECTION through. A pooled client sets it on the
			// first packet of a reused connection to ask for a clean session;
			// dropping it would leave the upstream carrying the previous
			// logical session's temp tables and SET options.
			status: s.pkt.lastReadStatus & relayedStatusBits,
		}

		select {
		case s.clientMsgs <- msg:
		case <-s.done:
			return
		}
	}
}

// setClientReadError records why the reader stopped, for the pump to report
// once the queue it may still be draining is empty.
func (s *session) setClientReadError(err error) {
	s.clientErrMu.Lock()
	if s.clientErr == nil {
		s.clientErr = err
	}
	s.clientErrMu.Unlock()
}

// clientReadError returns why the reader stopped.
func (s *session) clientReadError() error {
	s.clientErrMu.Lock()
	defer s.clientErrMu.Unlock()

	return s.clientErr
}

// pumpClientToUpstream runs interception over each client message and forwards
// what survives it, one request at a time.
//
// A request is not intercepted, let alone forwarded, until the upstream has
// answered the previous one. TDS without MARS is request/response, but nothing
// stops a client from sending its next request early, and the response
// accounting keeps a single in-flight statement: a second request forwarded
// early would overwrite the first, whose response would then be logged under
// the second's text while the second went unlogged. Waiting here makes the
// single slot correct by construction. ATTENTION is the exception: it is the
// cancel for the request in flight, so it goes through at once.
func (s *session) pumpClientToUpstream(ctx context.Context) error {
	var (
		backlog []clientMessage
		msgs    = s.clientMsgs
	)

	for {
		var msg clientMessage

		if len(backlog) > 0 {
			msg = backlog[0]
			backlog = backlog[1:]
		} else {
			// awaitUpstreamTurn hands back a nil queue once the reader has closed
			// it. With the backlog drained there is nothing left to forward, and
			// a receive on nil would park this pump for good, and relay with it.
			if msgs == nil {
				return s.clientReadError()
			}

			select {
			case next, ok := <-msgs:
				if !ok {
					return s.clientReadError()
				}

				msg = next
			case <-s.done:
				return nil
			}
		}

		if msg.msgType != packetTypeAttention {
			var proceed bool

			var err error

			proceed, backlog, msgs, err = s.awaitUpstreamTurn(ctx, backlog, msgs)
			if err != nil || !proceed {
				return err
			}
		}

		if err := s.forwardClientMessage(ctx, msg); err != nil {
			return err
		}
	}
}

// awaitUpstreamTurn blocks until the upstream has answered everything it was
// sent, then claims the turn for the next request.
//
// While it waits it keeps taking messages off the reader's queue, so that an
// ATTENTION — the client canceling the request in flight — is forwarded at once
// rather than queued behind the request that waits for that very response.
// Other messages join the backlog, in order, up to clientReadAhead of them.
//
// It returns the backlog and the queue to read from next (nil once the reader
// has closed it, so the backlog drains before the pump reports the read error),
// and false when the session ended while it waited.
func (s *session) awaitUpstreamTurn(
	ctx context.Context, backlog []clientMessage, msgs chan clientMessage,
) (bool, []clientMessage, chan clientMessage, error) {
	for {
		free, claimed := s.claimUpstreamTurn()
		if claimed {
			return true, backlog, msgs, nil
		}

		intake := msgs
		if len(backlog) >= clientReadAhead {
			intake = nil
		}

		select {
		case <-free:
		case <-s.done:
			return false, backlog, msgs, nil
		case next, ok := <-intake:
			switch {
			case !ok:
				msgs = nil
			case next.msgType == packetTypeAttention:
				if err := s.forwardClientMessage(ctx, next); err != nil {
					return false, backlog, msgs, err
				}
			default:
				backlog = append(backlog, next)
			}
		}
	}
}

// claimUpstreamTurn takes the upstream leg for a request if nothing is owed on
// it. Otherwise it returns a channel closed when that changes.
func (s *session) claimUpstreamTurn() (<-chan struct{}, bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	if !s.requestDue && !s.attentionDue {
		s.requestDue = true

		return nil, true
	}

	if s.turnFree == nil {
		s.turnFree = make(chan struct{})
	}

	return s.turnFree, false
}

// releaseRequestTurn gives the turn back without waiting for a response: the
// claimed request was answered by the proxy itself and never reached the
// upstream.
func (s *session) releaseRequestTurn() {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.requestDue = false
	s.wakeTurnLocked()
}

// noteAttentionSent records that an ATTENTION went upstream, which the server
// answers with a DONE_ATTN the next request must not be sent ahead of: that
// acknowledgement may arrive as a message of its own, and would otherwise be
// taken for the next request's response.
func (s *session) noteAttentionSent() {
	s.turnMu.Lock()
	s.attentionDue = true
	s.turnMu.Unlock()
}

// noteResponseEnd records the end of a response message from the upstream.
// Any response ends the request in flight; only one carrying DONE_ATTN ends a
// pending cancel.
func (s *session) noteResponseEnd(attentionAck bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	s.requestDue = false

	if attentionAck {
		s.attentionDue = false
	}

	s.wakeTurnLocked()
}

// wakeTurnLocked wakes a pump waiting for the turn, if the leg is now free.
// Callers hold turnMu.
func (s *session) wakeTurnLocked() {
	if s.requestDue || s.attentionDue || s.turnFree == nil {
		return
	}

	close(s.turnFree)
	s.turnFree = nil
}

// forwardClientMessage runs interception over one client message and forwards
// whatever the upstream is owed for it.
func (s *session) forwardClientMessage(ctx context.Context, msg clientMessage) error {
	// forwarded() normalizes the no-hook case: ATTENTION has no payload at all,
	// and a nil here is the hook's "I answered the client myself" signal.
	forward := forwarded(msg.payload)

	if s.onClientMessage != nil {
		var err error

		forward, err = s.onClientMessage(ctx, msg.msgType, msg.payload)
		if err != nil {
			return err
		}
	}

	// A non-nil payload is forwarded; nil means the hook answered the client on
	// its own and nothing must reach the upstream.
	if forward != nil {
		// Marked before the write, not after: the acknowledgement can come back
		// before this goroutine runs again, and a mark set after it would never
		// be cleared.
		if msg.msgType == packetTypeAttention {
			s.noteAttentionSent()
		}

		s.upstreamWriteMu.Lock()
		err := s.upstream.pkt.WriteMessageWithStatus(msg.msgType, forward, msg.status)
		s.upstreamWriteMu.Unlock()

		if err != nil {
			return err
		}
	} else if msg.msgType != packetTypeAttention {
		// Answered by the proxy: no response is coming from the upstream.
		s.releaseRequestTurn()
	}

	return s.forwardDeferredAttention()
}

// forwardDeferredAttention re-emits a cancel the reader consumed inside the arm
// window but that lost its race to a human approver.
//
// It runs after the message the hook was called with, whether that message was
// forwarded (the hold was approved, so the cancel now has a statement to
// interrupt) or answered locally (the hold was denied, so the cancel interrupts
// nothing and the upstream simply acknowledges it) — which is exactly the order
// the reader would have produced had it forwarded the ATTENTION itself.
func (s *session) forwardDeferredAttention() error {
	if !s.takeAttentionUpstream() {
		return nil
	}

	return s.writeUpstreamAttention()
}

// writeUpstreamAttention sends a TDS ATTENTION on the upstream leg, under the
// lock every upstream write takes.
//
// The lock is what makes this callable from the watchdog goroutine as well as
// from the forward pump: TDS packets are not interleavable, so two writers
// framing at once would corrupt the stream. In practice the pump is blocked
// reading the client while a long statement runs — which is exactly when the
// watchdog wants to write — but "in practice" is not a synchronization
// strategy.
func (s *session) writeUpstreamAttention() error {
	s.upstreamWriteMu.Lock()
	defer s.upstreamWriteMu.Unlock()

	if s.upstream == nil || s.upstream.pkt == nil {
		return nil
	}

	// Before the write, for the reason given in forwardClientMessage.
	s.noteAttentionSent()

	return s.upstream.pkt.WriteMessage(packetTypeAttention, nil)
}

// pumpUpstreamToClient forwards responses to the client a packet at a time.
//
// Deliberately not ReadMessage: a result set is one logical TDS message and can
// be arbitrarily large, so reassembling it would both blow the reassembly cap
// and make the proxy buffer a whole query result before the client sees its
// first row.
func (s *session) pumpUpstreamToClient(ctx context.Context) error {
	start := true

	// The tail of each response message, read for the DONE_ATTN that ends a
	// cancel. See noteResponseEnd.
	var tail messageTail

	for {
		hdr, payload, err := s.upstream.pkt.readPacket()
		if err != nil {
			return err
		}

		eom := hdr.isEOM()

		if s.onServerPacket != nil {
			s.onServerPacket(ctx, hdr.Type, payload, eom)
		}

		// The client codec has two writers now: this pump, and the request pump
		// when it answers a blocked statement itself. packetRW is not safe for
		// concurrent use — its outbound packet id is shared state.
		s.clientWriteMu.Lock()
		err = s.pkt.ForwardPacket(hdr.Type, hdr.Status, payload, start)
		s.clientWriteMu.Unlock()

		if err != nil {
			return err
		}

		// The turn is handed back only once the response has been accounted
		// for (onServerPacket above took its pending statement) and reached the
		// client, so the next request's pending statement can never be taken by
		// this response.
		if hdr.Type == packetTypeReply {
			tail.record(payload)

			if eom {
				done, ok := tail.lastDone()
				s.noteResponseEnd(ok && done.Status&doneAttn != 0)
				tail.reset()
			}
		}

		start = eom
	}
}

// closeUpstream tears the upstream connection down. Safe to call twice, which
// the relay's teardown relies on.
func (s *session) closeUpstream(ctx context.Context) {
	if s.upstream == nil {
		return
	}

	if err := s.upstream.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.logger.DebugContext(ctx, "MSSQL upstream close error", slog.Any("error", err))
	}
}

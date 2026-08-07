package mssql

import (
	"context"
	"errors"
	"log/slog"
	"net"
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

// relay pumps TDS both ways until either side closes.
//
// The two directions run independently rather than as a request/response loop,
// which is what makes ATTENTION (0x06) work: a client canceling a query sends
// it while the response is still streaming, and a lock-step relay would not
// read it until the response it is meant to interrupt had finished.
//
// Each packetRW ends up with exactly one reader and one writer, in different
// goroutines. That is safe because the codec's read and write paths share no
// mutable state — see the note on packetRW — and it is the reason neither side
// takes a lock on the hot path.
func (s *session) relay(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() { errCh <- s.pumpClientToUpstream(ctx) }()
	go func() { errCh <- s.pumpUpstreamToClient(ctx) }()

	err := <-errCh

	// Unblock the other pump: whichever direction failed, the session is over.
	_ = s.upstream.Close()
	_ = s.conn.Close()

	<-errCh

	if err == nil || isExpectedDisconnect(err) {
		return nil
	}

	return err
}

// pumpClientToUpstream forwards client requests upstream, one complete logical
// message at a time.
//
// Whole messages, not packets, because this is where interception has to
// happen: a SQLBatch's SQL text can span packets, and a hook that saw fragments
// could be evaded by splitting a statement across a packet boundary. The 16 MB
// reassembly cap in ReadMessage is what bounds the memory that costs.
func (s *session) pumpClientToUpstream(ctx context.Context) error {
	for {
		msgType, payload, err := s.pkt.ReadMessage()
		if err != nil {
			// The sender asked for the message to be discarded; that is a
			// complete instruction, not a failure.
			if errors.Is(err, ErrMessageIgnored) {
				continue
			}

			return err
		}

		forward := payload

		if s.onClientMessage != nil {
			forward, err = s.onClientMessage(ctx, msgType, payload)
			if err != nil {
				return err
			}

			// The hook answered the client on its own.
			if forward == nil {
				continue
			}
		}

		// Carry RESETCONNECTION through. A pooled client sets it on the first
		// packet of a reused connection to ask for a clean session; dropping it
		// would leave the upstream carrying the previous logical session's temp
		// tables and SET options.
		status := s.pkt.lastReadStatus & relayedStatusBits

		if err := s.upstream.pkt.WriteMessageWithStatus(msgType, forward, status); err != nil {
			return err
		}
	}
}

// pumpUpstreamToClient forwards responses to the client a packet at a time.
//
// Deliberately not ReadMessage: a result set is one logical TDS message and can
// be arbitrarily large, so reassembling it would both blow the reassembly cap
// and make the proxy buffer a whole query result before the client sees its
// first row.
func (s *session) pumpUpstreamToClient(ctx context.Context) error {
	start := true

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

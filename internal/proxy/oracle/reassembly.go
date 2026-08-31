package oracle

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fclairamb/dbbat/internal/dump"
)

// Data-phase reassembly of a statement-carrying TTC message.
//
// TTC has no message-length field. A message larger than the negotiated SDU
// (8192 by default) is written by the client as several TNS Data packets, and
// only the first one carries the TTC op header — every continuation is raw
// bytes behind the same 8-byte TNS header and 2-byte data-flags prefix. The AUTH
// phase has always reassembled (readUpstreamAuthMessages); the data phase read
// one packet at a time and gated each independently, which is the whole of the
// 2026-08-31 incident:
//
//   - the exec header declared sqlLen = 9214 while only ~8110 bytes of SQL were
//     in the packet, so the header-anchored decode and the offset windows both
//     failed and the last-resort keyword scan returned a prefix, cut at the
//     first non-ASCII byte;
//   - the gate then enforced against that prefix and /queries recorded it as if
//     it were the statement — everything past the fragment boundary
//     (oracleBlockedPatterns, the approval patterns, the dynamic-SQL scan) was
//     evadable by padding a statement past the SDU;
//   - and a refusal answered the OER for the first fragment while the orphan
//     continuation was then read on its own and forwarded upstream, which
//     desynchronized the upstream and killed the session (DPY-4011 at the
//     client's next call).
//
// So a statement-carrying op whose declared length runs past its packet is
// buffered here, its continuations are read and appended, and the gate reads the
// reassembled body. The buffered packets are then forwarded **as they arrived**,
// byte for byte: the reassembled buffer is for reading only, and dbbat never
// synthesizes wire bytes toward the upstream. A refusal drops every buffered
// fragment and answers once.
//
// The declared length is attacker-controlled input, so nothing is allocated
// ahead of the bytes actually arriving, the total is capped
// (maxStatementReassembly) and the wait is bounded by a read deadline
// (statementReassemblyTimeout). A client that declares a length it never sends
// is torn down, which is the answer gateUnnameableFrame already gives for the
// same reason: the session is the blast radius, not the gate.

const (
	// maxStatementReassembly bounds a reassembled TTC message. execMaxSQLLen
	// (1MB) is the largest statement the decoders will read; the slack covers
	// the exec header, the bind definitions and the bind values that travel
	// behind the text in the same message.
	maxStatementReassembly = execMaxSQLLen + (1 << 16)

	// statementReassemblyTimeout bounds the wait for the continuation packets of
	// a message whose first fragment already arrived. A client writes the
	// fragments of one message back to back, so this is not a think-time
	// budget — it is the fail-closed bound on a client that declares a length it
	// never sends.
	statementReassemblyTimeout = 30 * time.Second
)

// errReassemblyAborted reports that a statement-carrying message could not be
// collected to its end. The session is the blast radius: the client leg returns
// it and the session ends, because after a partial read the client's byte stream
// is no longer at a message boundary.
var errReassemblyAborted = errors.New("oracle: could not reassemble a fragmented statement message")

// statementFragments is one client TTC message: the packets exactly as the
// client sent them, plus the packet the gate reads.
type statementFragments struct {
	// packets are the client's own packets, in order. Forwarding these
	// unchanged is what keeps the upstream's byte stream identical to a
	// direct connection.
	packets []*TNSPacket

	// gate carries the reassembled TTC body behind the first packet's data
	// flags. It is never written to a socket. For the ordinary one-packet
	// message it is packets[0] itself.
	gate *TNSPacket

	// complete reports that the whole message is in packets. It is false only
	// when the last fragment collected was itself SDU-sized, which means the
	// client may have had more to send: dbbat stops at the declared statement
	// length and never reads past it, because over-reading would swallow the
	// *next* message. A refusal in that state cannot answer with an OER and
	// leave the session running — the residual fragment would be forwarded
	// alone and desynchronize the upstream, which is the failure this whole
	// file exists to remove — so it ends the session instead.
	complete bool
}

// fragmented reports whether more than one client packet was collected.
func (f *statementFragments) fragmented() bool {
	return len(f.packets) > 1
}

// collectStatementMessage returns the packets of the TTC message that starts at
// pkt, reading the continuations off the client socket when the message's own
// header says it does not fit in one packet.
//
// The common case — a message that fits its packet — allocates nothing and
// returns pkt itself, so the single-packet path is exactly what it was.
func (s *session) collectStatementMessage(pkt *TNSPacket) (*statementFragments, error) {
	single := &statementFragments{packets: []*TNSPacket{pkt}, gate: pkt, complete: true}

	ttc := extractTTCPayload(pkt.Payload)
	if ttc == nil {
		return single, nil
	}

	need, ok := statementFragmentShortfall(ttc)
	if !ok {
		return single, nil
	}

	if need > maxStatementReassembly {
		// A declared length past anything dbbat would read is not a message to
		// wait for. Left to the single-packet path, which decodes what it can
		// and marks the statement partial (see decodeExecSQL) rather than
		// blocking on bytes that are not coming.
		s.logger.WarnContext(s.ctx, "oracle: declared statement length beyond the reassembly bound",
			slog.Int("declared_shortfall", need),
			slog.Int("bound", maxStatementReassembly))

		return single, nil
	}

	return s.readStatementFragments(pkt, ttc, need)
}

// readStatementFragments buffers pkt and reads continuation packets until the
// declared statement length is covered.
func (s *session) readStatementFragments(pkt *TNSPacket, ttc []byte, need int) (*statementFragments, error) {
	// Grown as packets arrive, never sized from the declared length.
	body := make([]byte, 0, len(ttc)*2)
	body = append(body, ttc...)

	out := &statementFragments{packets: []*TNSPacket{pkt}}

	if err := s.clientConn.SetReadDeadline(time.Now().Add(statementReassemblyTimeout)); err != nil {
		return nil, fmt.Errorf("%w: set read deadline: %w", errReassemblyAborted, err)
	}

	defer func() { _ = s.clientConn.SetReadDeadline(time.Time{}) }()

	for need > 0 {
		next, err := readTNSPacket(s.clientConn)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errReassemblyAborted, err)
		}

		if s.dump != nil {
			_ = s.dump.WritePacket(dump.DirClientToServer, next.Raw)
		}

		// A continuation is a Data packet carrying the same data-flags prefix
		// and nothing else of its own. Anything else here (a break marker, a
		// control packet) means the message dbbat committed to reading is not
		// the message the client is sending, and there is no safe way to
		// resynchronize a byte stream that is mid-message.
		if next.Type != TNSPacketTypeData || len(next.Payload) <= ttcDataFlagsSize {
			return nil, fmt.Errorf("%w: continuation is a %s packet of %d payload bytes",
				errReassemblyAborted, next.Type, len(next.Payload))
		}

		frag := next.Payload[ttcDataFlagsSize:]

		if len(body)+len(frag) > maxStatementReassembly {
			return nil, fmt.Errorf("%w: message exceeds %d bytes", errReassemblyAborted, maxStatementReassembly)
		}

		body = append(body, frag...)
		out.packets = append(out.packets, next)
		need -= len(frag)

		// A fragment shorter than the first one is the end of the message: the
		// client fills each packet to the SDU and the last one is what is left.
		out.complete = len(next.Raw) < len(pkt.Raw)
	}

	out.gate = &TNSPacket{
		Type:    TNSPacketTypeData,
		Flags:   pkt.Flags,
		Payload: append(append(make([]byte, 0, ttcDataFlagsSize+len(body)), pkt.Payload[:ttcDataFlagsSize]...), body...),
	}

	s.logger.DebugContext(s.ctx, "oracle: reassembled a fragmented statement message",
		slog.Int("packets", len(out.packets)),
		slog.Int("ttc_bytes", len(body)),
		slog.Bool("complete", out.complete))

	return out, nil
}

// statementFragmentShortfall reports how many more TTC bytes a
// statement-carrying message needs beyond this fragment, and false when the
// message is not one, or carries its statement whole.
//
// The trigger is the declared length, never a guess: every statement-carrying op
// dbbat gates says how long its statement is, and reassembly starts exactly when
// that length runs past the bytes in hand.
func statementFragmentShortfall(ttcPayload []byte) (int, bool) {
	if need, ok := execFragmentShortfall(ttcPayload); ok {
		return need, true
	}

	return oall8FragmentShortfall(ttcPayload)
}

// execFragmentShortfall answers for the two exec shapes decodeExecStatementText
// reads: the bare `03 5e` piggyback execute, and the `11 69` close-cursors
// piggyback with that execute stapled behind it.
func execFragmentShortfall(ttcPayload []byte) (int, bool) {
	bodies := [][]byte{ttcPayload}

	if end, ok := closeCursorsEnd(ttcPayload); ok && end < len(ttcPayload) {
		bodies = append(bodies, ttcPayload[end:])
	}

	for _, body := range bodies {
		if !isPiggybackExecHeader(body) {
			continue
		}

		sqlLen, ok := execSQLLength(body)
		if !ok {
			continue
		}

		// locateExecSQLText requires the whole run to sit inside the body, and
		// the run is preceded by at least the exec header. Below that the
		// statement is here and no reassembly is owed — including the frames
		// that decode fine today.
		if need := sqlLen + execHeaderMinLen - len(body); need > 0 {
			return need, true
		}
	}

	return 0, false
}

// oall8FragmentShortfall answers for the legacy OALL8 parse+execute. It is the
// same situation decodeOALL8 reports as ErrSQLLengthInvalid, read before the
// decode rather than after it.
func oall8FragmentShortfall(ttcPayload []byte) (int, bool) {
	if len(ttcPayload) < oall8MinPayloadSize || TTCFunctionCode(ttcPayload[0]) != TTCFuncOALL8 {
		return 0, false
	}

	// func(1) + options(4) + cursor(2), exactly as decodeOALL8 walks it.
	offset := 7

	sqlLen, read, err := decodeVarLen(ttcPayload[offset:])
	if err != nil || sqlLen == 0 || int(sqlLen) > execMaxSQLLen {
		return 0, false
	}

	offset += read

	if need := offset + int(sqlLen) - len(ttcPayload); need > 0 {
		return need, true
	}

	return 0, false
}

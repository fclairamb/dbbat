package oracle

import (
	"encoding/binary"
)

// Cutting a rewritten TTC message back into TNS Data packets.
//
// Everything the data phase forwards today is the client's own packet, written
// out of `pkt.Raw` — `writeTNSPacket` prefers Raw precisely so a v315+ session's
// 4-byte length header survives a round trip through a struct that only has a
// `uint16 Length`. A rewritten message has no Raw to prefer, and the header it
// needs is whichever form the client used, so it is built here rather than by
// `encodeTNSPacket` (which can only write the legacy 2-byte form).
//
// The message also may no longer fit where it fit before: a ~45-byte tag on a
// statement that already filled the negotiated SDU needs one more packet. That
// is why the rewrite happens *after* `collectStatementMessage` — the shortfall
// accounting (`execFragmentShortfall`, `oall8FragmentShortfall`) keeps reading
// the client's own declared length and is untouched by any of this — and why the
// outgoing packets are re-cut here instead of being patched in place.

const (
	// minNegotiatedSDU is the floor for a plausible session data unit. Oracle's
	// own minimum is 512; a value under it means the Accept was not read
	// correctly, and a misread SDU is how a rewritten message would be written
	// in packets the upstream answers with ORA-12592.
	minNegotiatedSDU = 512

	// acceptSDUOffset is where a v315+ TNS Accept carries the negotiated SDU as
	// a big-endian ub4. The legacy ub2 at [12:14] reads as 0 on every v315+
	// Accept, which is what says to look here — the same convention
	// readTNSPacket already applies to the packet length itself.
	//
	// Measured across every recording in testdata/ (Accept versions 317, 318 and
	// 319): 2048 for the sqlplus captures, 8192 for DBeaver, JDBC thin and
	// python-oracledb thin, 65536 for most of the go-ora ones — and the
	// transport data unit follows it at [36:40], which is what makes the offset
	// legible rather than lucky. It sits below acceptFlagsOffset (41), the other
	// field this package reads out of an Accept.
	acceptSDUOffset = 32
)

// acceptNegotiatedSDU reads the session data unit out of a TNS Accept packet:
// the largest TNS packet either end may write for the rest of the session.
//
// It reports false rather than a default. A guessed SDU is worse than no
// rewriting at all — too large and the upstream refuses the packet with
// ORA-12592, too small and every tagged statement is pointlessly fragmented —
// so a session whose Accept does not parse simply never tags.
// The length guard is the legacy field's, not the v315+ one's, and that is
// measured rather than tidied: a pre-v315 Accept is **32 bytes end to end** —
// ojdbc6 11.2.0.4 negotiates version 310 and its Accept stops right where the
// ub4 would start (testdata/ojdbc6_legacy.pcapng, packet #1) — so requiring room
// for a field that layout does not have refused an Accept whose ub2 at [12:14]
// says 8192 perfectly clearly. The ub4 is bounds-checked where it is read
// instead.
func acceptNegotiatedSDU(raw []byte) (int, bool) {
	const legacySDUEnd = 14

	if len(raw) < legacySDUEnd || TNSPacketType(raw[4]) != TNSPacketTypeAccept {
		return 0, false
	}

	sdu := int(binary.BigEndian.Uint16(raw[12:14]))
	if sdu == 0 {
		if len(raw) < acceptSDUOffset+4 {
			return 0, false
		}

		sdu = int(binary.BigEndian.Uint32(raw[acceptSDUOffset : acceptSDUOffset+4]))
	}

	if sdu < minNegotiatedSDU {
		return 0, false
	}

	// readTNSPacket refuses anything past maxTNSPacketSize, so a larger
	// negotiated unit is not a size this proxy can relay in either direction.
	return min(sdu, maxTNSPacketSize), true
}

// encodeDataPacketLike writes payload as a TNS Data packet in the same header
// form as model: the v315+ 4-byte length when model's 2-byte length field reads
// zero, the legacy 2-byte length otherwise. Everything else in the header —
// type, flags, both checksum fields — is carried over from model, so a rewritten
// packet differs from the client's own in its length and its body and in nothing
// else.
func encodeDataPacketLike(model, payload []byte) []byte {
	if len(model) < tnsHeaderSize {
		return nil
	}

	total := tnsHeaderSize + len(payload)
	if total > maxTNSPacketSize {
		return nil
	}

	buf := make([]byte, total)
	copy(buf[:tnsHeaderSize], model[:tnsHeaderSize])

	if binary.BigEndian.Uint16(model[0:2]) != 0 {
		binary.BigEndian.PutUint16(buf[0:2], uint16(total))
	} else {
		binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	}

	copy(buf[tnsHeaderSize:], payload)

	return buf
}

// refragmentStatementMessage cuts a rewritten TTC body into Data packets of at
// most sdu bytes each, modeled on the client's own first packet.
//
// Every fragment carries the first packet's data-flags prefix and nothing else
// of its own, which is exactly the shape collectStatementMessage requires when
// it reads one back — the same contract reframeAuthOK writes to on the other
// leg.
func refragmentStatementMessage(first *TNSPacket, ttcBody []byte, sdu int) ([][]byte, bool) {
	if first == nil || len(first.Raw) < tnsHeaderSize || len(first.Payload) < ttcDataFlagsSize {
		return nil, false
	}

	perPacket := min(sdu, maxTNSPacketSize) - tnsHeaderSize - ttcDataFlagsSize
	if perPacket <= 0 || len(ttcBody) == 0 {
		return nil, false
	}

	flags := first.Payload[:ttcDataFlagsSize]

	out := make([][]byte, 0, len(ttcBody)/perPacket+1)

	for pos := 0; pos < len(ttcBody); pos += perPacket {
		end := min(pos+perPacket, len(ttcBody))

		payload := make([]byte, 0, ttcDataFlagsSize+end-pos)
		payload = append(payload, flags...)
		payload = append(payload, ttcBody[pos:end]...)

		frame := encodeDataPacketLike(first.Raw, payload)
		if frame == nil {
			return nil, false
		}

		out = append(out, frame)
	}

	return out, true
}

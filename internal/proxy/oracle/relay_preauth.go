package oracle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// relayPreAuthNegotiation establishes a transparent TCP relay between the client
// and upstream for the pre-authentication TNS/TTC negotiation phase.
//
// The proxy forwards Connect → Accept → Set Protocol → Set Data Types traffic
// byte-for-byte so each client receives a server response tailored to its own
// request. The relay terminates when the client sends its AUTH Phase 1 packet,
// which is returned to the caller (not forwarded) so dbbat can perform
// terminated O5LOGON authentication.
//
// The upstream socket is returned to the caller (not closed). After dbbat
// completes O5LOGON with the client, it runs an O5LOGON CLIENT against this
// same socket using stored database credentials. Reusing the relay-phase
// socket keeps the negotiated TTC capability levels aligned between the
// client and upstream — closing it and opening a fresh go-ora session would
// shift the upstream cap level and cause ORA-03120 on the first user query.
func (s *session) relayPreAuthNegotiation(connectPkt *TNSPacket) (*TNSPacket, net.Conn, error) {
	initialAddr := net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port))

	// Rewrite SERVICE_NAME in the Connect descriptor to the real upstream Oracle
	// service name. Clients connect using dbbat's logical service name, but the
	// upstream listener only knows the real name.
	upstreamService := s.database.DatabaseName
	if s.database.OracleServiceName != nil && *s.database.OracleServiceName != "" {
		upstreamService = *s.database.OracleServiceName
	}

	upstreamConnect := connectPkt
	if upstreamService != "" && upstreamService != s.serviceName {
		upstreamConnect = rewriteServiceName(connectPkt, s.serviceName, upstreamService)
		s.logger.DebugContext(s.ctx, "pre-auth relay: rewrote SERVICE_NAME for upstream",
			slog.String("from", s.serviceName), slog.String("to", upstreamService))
	}

	upstream, acceptPkt, err := dialUpstreamWithRedirect(s, initialAddr, upstreamConnect)
	if err != nil {
		return nil, nil, err
	}

	if stripAcceptModernAuthFlags(acceptPkt.Raw) {
		s.logger.DebugContext(s.ctx, "pre-auth relay: stripped FAST_AUTH/END_OF_RESPONSE from Accept")
	}

	s.logger.DebugContext(s.ctx, "pre-auth relay: forwarding Accept to client", slog.Int("len", len(acceptPkt.Raw)))

	if _, err := s.clientConn.Write(acceptPkt.Raw); err != nil {
		_ = upstream.Close()

		return nil, nil, fmt.Errorf("forward Accept to client: %w", err)
	}

	// Pre-auth negotiation (Set Protocol / Set Data Types / capability exchange)
	// is NOT a strict 1:1 request/response: a single client packet can elicit
	// several upstream packets, and modern servers (Oracle 23ai, and sqlplus's
	// Native Services break/reset probe) send Control/Marker packets the client
	// must see before it proceeds. A lockstep relay (one upstream read per client
	// packet) deadlocks the moment the counts diverge — the proxy waits for the
	// client while the client waits for an upstream packet still queued behind a
	// marker. So we run a concurrent bidirectional pump: a goroutine forwards
	// upstream→client continuously, while this goroutine forwards client→upstream
	// until it sees AUTH Phase 1, which it returns (unforwarded) to the caller.
	pumpDone := make(chan error, 1)

	go s.pumpPreAuthUpstream(upstream, pumpDone)

	// stopPump hands the upstream socket back to the caller for the O5LOGON
	// handover. Clients pipeline the pre-auth sequence — they send AUTH Phase 1
	// immediately after Set Protocol / Set Data Types, so the main loop can read
	// AUTH Phase 1 before the pump has forwarded those replies. Forcing the
	// deadline into the past here would drop the unforwarded replies and the
	// client would block forever waiting for them. Instead set a short FUTURE
	// deadline: the pump first drains any replies already on the socket (the
	// upstream is quiescent once it has answered Set Data Types and is waiting
	// for AUTH), then its next read times out cleanly between packets and it
	// exits. The grace window is a one-time cost on the auth handover.
	const pumpDrainGrace = 750 * time.Millisecond

	stopPump := func() {
		_ = upstream.SetReadDeadline(time.Now().Add(pumpDrainGrace))
		<-pumpDone
		_ = upstream.SetReadDeadline(time.Time{})
	}

	for {
		clientPkt, err := readTNSPacket(s.clientConn)
		if err != nil {
			stopPump()
			_ = upstream.Close()

			return nil, nil, fmt.Errorf("read client packet during pre-auth relay: %w", err)
		}

		if isAuthPhase1(clientPkt) {
			s.logger.DebugContext(s.ctx, "pre-auth relay: detected AUTH Phase 1, ending relay",
				slog.Int("len", len(clientPkt.Payload)))
			stopPump()

			return clientPkt, upstream, nil
		}

		// Modern clients (python-oracledb thin, recent ODP/ojdbc) pipeline the
		// login: Set Protocol + Set Data Types + AUTH Phase 1 are concatenated
		// into a single TNS Data packet to save round trips. isAuthPhase1 only
		// matches when AUTH Phase 1 is the FIRST op, so we'd otherwise forward
		// the whole bundle — auth included — to the real Oracle, which rejects
		// the dbb_ API key with ORA-01017. Detect the embedded AUTH Phase 1,
		// forward only the Set Protocol / Set Data Types prefix to the upstream
		// (so its TTC session reaches the post-Data-Types state O5LOGON expects),
		// relay the upstream's responses back, then hand the carved-out AUTH
		// Phase 1 to the caller for terminated O5LOGON.
		if prefixMsgs, auth1Payload, ok := splitBundledAuthPhase1(clientPkt.Payload); ok {
			s.logger.DebugContext(s.ctx, "pre-auth relay: de-pipelining fast-auth login",
				slog.Int("prefix_msgs", len(prefixMsgs)),
				slog.Int("auth1_len", len(auth1Payload)))
			stopPump()

			authPkt, err := s.replayDepipelinedPrefix(upstream, prefixMsgs, auth1Payload)
			if err != nil {
				return nil, nil, err
			}

			return authPkt, upstream, nil
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: client→upstream",
			slog.String("type", clientPkt.Type.String()),
			slog.Int("len", len(clientPkt.Raw)))

		if _, err := upstream.Write(clientPkt.Raw); err != nil {
			stopPump()
			_ = upstream.Close()

			return nil, nil, fmt.Errorf("forward client packet to upstream: %w", err)
		}
	}
}

// pumpPreAuthUpstream continuously forwards upstream→client TNS packets during
// the pre-auth relay, reporting the first read/write error (including the
// drain-grace deadline that stopPump arms) on pumpDone. See
// relayPreAuthNegotiation for why the relay is a concurrent pump, not lockstep.
func (s *session) pumpPreAuthUpstream(upstream net.Conn, pumpDone chan<- error) {
	for {
		pkt, err := readTNSPacket(upstream)
		if err != nil {
			pumpDone <- err

			return
		}

		// Set Protocol responses carry ServerCompileTimeCaps; caps[4]&0x20
		// enables the customHash (PBKDF2) combined-key derivation. Record the
		// upstream's customHash capability and forward it to the client
		// unchanged, so a modern client negotiates customHash and dbbat answers
		// with a verifier-18453 challenge (built from the API key's stored 18453
		// verifier). Legacy clients (go-ora) read the verifier type from the
		// challenge's AUTH_VFR_DATA flag and adapt.
		if observeCustomHashFlag(pkt.Raw) {
			s.upstreamCustomHash = true
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: upstream→client",
			slog.String("type", pkt.Type.String()),
			slog.Int("len", len(pkt.Raw)))

		if _, err := s.clientConn.Write(pkt.Raw); err != nil {
			pumpDone <- err

			return
		}
	}
}

// replayDepipelinedPrefix replays a fast-auth packet's wrapped Set Protocol /
// Set Data Types messages to the upstream as classic standalone messages (so its
// TTC session advances to the post-Data-Types state O5LOGON expects), draining
// each reply back to the client, then frames the carved-out AUTH Phase 1 for the
// caller. On any write/drain failure it closes the upstream and returns the error.
func (s *session) replayDepipelinedPrefix(upstream net.Conn, prefixMsgs [][]byte, auth1Payload []byte) (*TNSPacket, error) {
	for _, msg := range prefixMsgs {
		if _, err := upstream.Write(encodeTNSDataV315(msg)); err != nil {
			_ = upstream.Close()

			return nil, fmt.Errorf("forward de-pipelined prefix to upstream: %w", err)
		}

		if err := drainUpstreamToClient(s, upstream); err != nil {
			_ = upstream.Close()

			return nil, err
		}
	}

	raw := encodeTNSDataV315(auth1Payload)

	return &TNSPacket{Type: TNSPacketTypeData, Payload: auth1Payload, Raw: raw}, nil
}

// TTC op signatures (func, sub) for the messages a fast-auth packet bundles.
var (
	opSetProtocol = [2]byte{0x01, 0x06} // Set Protocol  (TNS_MSG_TYPE_PROTOCOL)
	opDataTypes   = [2]byte{0x02, 0x69} // Set Data Types (TNS_MSG_TYPE_DATA_TYPES)
)

const (
	// tnsMsgTypeFastAuth is python-oracledb thin's FAST_AUTH message type, the
	// first byte after the 2-byte data flags of a pipelined login packet.
	tnsMsgTypeFastAuth = 0x22

	// fastAuthHeaderLen is the FAST_AUTH preamble written before the wrapped
	// Set Protocol message: msg type (0x22), version, server-converts-chars
	// flag, and a reserved zero byte.
	fastAuthHeaderLen = 4

	// fastAuthProtoToDataTypesGap is the fixed block FAST_AUTH writes between the
	// Set Protocol and Set Data Types messages: server charset (uint16be),
	// server charset flag (uint8), server ncharset (uint16be) and the TTC field
	// version (uint8) — six bytes that are NOT part of either wrapped message and
	// must be dropped when replaying them as classic standalone messages.
	fastAuthProtoToDataTypesGap = 6
)

// splitBundledAuthPhase1 detects python-oracledb thin's FAST_AUTH login (TNS
// message type 0x22) — a packet that pipelines Set Protocol + Set Data Types +
// AUTH Phase 1 to save round trips. Its first TTC op is NOT AUTH Phase 1, so
// isAuthPhase1 misses it; forwarding it whole would feed the dbb_ API key to the
// real Oracle (→ ORA-01017). On a match it returns the wrapped Set Protocol and
// Set Data Types messages as classic standalone payloads (2-byte data flags +
// message, FAST_AUTH framing stripped) for the caller to replay to the upstream,
// plus a freshly framed AUTH Phase 1 payload for terminated O5LOGON. The carved
// AUTH message is validated by parseAuthPhase1 so a stray 0x03 0x76 inside the
// Set-Data-Types blob can't trigger a false split.
//
// FAST_AUTH wire layout (after the 2-byte data flags):
//
//	[0x22][ver][convChars][0]  [ProtocolMessage]  [charset:2][csFlag:1][ncharset:2][ttcVer:1]  [DataTypesMessage]  [AuthMessage]
func splitBundledAuthPhase1(payload []byte) ([][]byte, []byte, bool) {
	protoStart := ttcDataFlagsSize + fastAuthHeaderLen
	if len(payload) < protoStart+2 {
		return nil, nil, false
	}

	if payload[ttcDataFlagsSize] != tnsMsgTypeFastAuth {
		return nil, nil, false
	}

	auth1Off := indexOfOp(payload, protoStart, len(payload), [2]byte{byte(TTCFuncPiggyback), PiggybackSubAuth1})
	dataTypesOff := indexOfOp(payload, protoStart, len(payload), opDataTypes)

	if auth1Off < 0 || dataTypesOff < 0 || dataTypesOff >= auth1Off {
		return nil, nil, false
	}

	if payload[protoStart] != opSetProtocol[0] || payload[protoStart+1] != opSetProtocol[1] {
		return nil, nil, false
	}

	protoEnd := dataTypesOff - fastAuthProtoToDataTypesGap
	if protoEnd <= protoStart {
		return nil, nil, false
	}

	auth1 := make([]byte, 0, ttcDataFlagsSize+len(payload)-auth1Off)
	auth1 = append(auth1, 0x00, 0x00)
	auth1 = append(auth1, payload[auth1Off:]...)

	if username, err := parseAuthPhase1(auth1); err != nil || username == "" {
		return nil, nil, false
	}

	prefixMsgs := [][]byte{
		framePreAuthMsg(payload[protoStart:protoEnd]),   // Set Protocol
		framePreAuthMsg(payload[dataTypesOff:auth1Off]), // Set Data Types
	}

	return prefixMsgs, auth1, true
}

// framePreAuthMsg prepends the 2-byte TNS data flags to a bare TTC message so it
// can be sent to the upstream as a classic standalone Data payload.
func framePreAuthMsg(msg []byte) []byte {
	out := make([]byte, 0, ttcDataFlagsSize+len(msg))
	out = append(out, 0x00, 0x00)
	out = append(out, msg...)

	return out
}

// indexOfOp returns the offset in payload[start:end) where the 2-byte TTC op
// signature first appears, or -1.
func indexOfOp(payload []byte, start, end int, op [2]byte) int {
	for i := start; i+1 < end; i++ {
		if payload[i] == op[0] && payload[i+1] == op[1] {
			return i
		}
	}

	return -1
}

// drainUpstreamToClient forwards the upstream's responses to the pipelined
// prefix (Set Protocol / Set Data Types replies) to the client, until the
// upstream falls silent — at which point it is waiting for AUTH, which dbbat
// now drives via terminated O5LOGON. A generous first deadline tolerates
// upstream latency; the shorter follow-up deadline detects end-of-burst between
// whole packets (the upstream is quiescent here, so the timeout never truncates
// a packet mid-read).
func drainUpstreamToClient(s *session, upstream net.Conn) error {
	first := true

	for {
		deadline := 800 * time.Millisecond
		if first {
			deadline = 8 * time.Second
		}

		if err := upstream.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return fmt.Errorf("set upstream drain deadline: %w", err)
		}

		pkt, err := readTNSPacket(upstream)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				_ = upstream.SetReadDeadline(time.Time{})

				return nil
			}

			return fmt.Errorf("drain upstream response: %w", err)
		}

		first = false

		if observeCustomHashFlag(pkt.Raw) {
			s.upstreamCustomHash = true
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: upstream→client (pipelined prefix reply)",
			slog.String("type", pkt.Type.String()),
			slog.Int("len", len(pkt.Raw)))

		if _, err := s.clientConn.Write(pkt.Raw); err != nil {
			return fmt.Errorf("forward upstream prefix reply to client: %w", err)
		}
	}
}

// Accept-packet "connect flags" (a big-endian uint32) and the flags dbbat
// clears so every client uses the classic, terminated-O5LOGON flow dbbat fully
// implements instead of 23ai fast paths it would otherwise have to synthesize.
const (
	// acceptFlagsOffset is the byte offset of the 4-byte connect-flags field in a
	// v315+ TNS Accept packet (after the 8-byte header, version, options, SDU/TDU,
	// data length/offset, the legacy connect-flag bytes and the v315 SDU/TDU).
	acceptFlagsOffset = 41

	tnsAcceptFlagFastAuth         = 0x10000000 // client pipelines Set Protocol+Data Types+AUTH (python-oracledb thin, etc.)
	tnsAcceptFlagHasEndOfResponse = 0x02000000 // server appends an end-of-response marker to every reply (23ai)
)

// stripAcceptModernAuthFlags clears FAST_AUTH and HAS_END_OF_RESPONSE from a
// v315+ Accept packet (mutating raw in place) so the client negotiates the
// classic two-phase O5LOGON with no per-message end-of-response markers — the
// exact shape dbbat terminates. Without this, modern clients (python-oracledb
// thin against Oracle 23ai) bundle the login and expect end-of-response markers
// on dbbat's synthesized challenge, which dbbat does not emit, so AUTH stalls.
// Returns whether any bit was cleared.
func stripAcceptModernAuthFlags(raw []byte) bool {
	if len(raw) < acceptFlagsOffset+4 {
		return false
	}

	if TNSPacketType(raw[4]) != TNSPacketTypeAccept {
		return false
	}

	if binary.BigEndian.Uint16(raw[8:10]) < 315 {
		return false
	}

	flags := binary.BigEndian.Uint32(raw[acceptFlagsOffset : acceptFlagsOffset+4])

	const mask = tnsAcceptFlagFastAuth | tnsAcceptFlagHasEndOfResponse
	if flags&mask == 0 {
		return false
	}

	binary.BigEndian.PutUint32(raw[acceptFlagsOffset:acceptFlagsOffset+4], flags&^uint32(mask))

	return true
}

// encodeTNSDataV315 frames a TNS Data payload using the v315+ 4-byte length
// header (the 2-byte legacy length reads as 0x0000) so reconstructed packets
// match the wire format modern clients and servers negotiate.
func encodeTNSDataV315(payload []byte) []byte {
	total := tnsHeaderSize + len(payload)
	buf := make([]byte, total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	buf[4] = byte(TNSPacketTypeData)
	copy(buf[tnsHeaderSize:], payload)

	return buf
}

// observeCustomHashFlag reports whether the Set Protocol response advertises
// customHash (PBKDF2 combined-key derivation) — the first server-capability
// byte's 0x20 bit. The capability array is framed as
// [numCaps][06 01 01 01][caps...], where numCaps is the count of capability
// bytes and varies by server version (0x2a on 19c, 0x36 on Oracle 23ai). We
// anchor on the stable 06 01 01 01 prefix (validating the preceding count byte)
// rather than a version-specific literal, then read the first capability byte.
func observeCustomHashFlag(raw []byte) bool {
	prefix := []byte{0x06, 0x01, 0x01, 0x01}

	idx := bytes.Index(raw, prefix)
	if idx < 1 {
		return false
	}

	// The byte before the prefix is the capability count; sanity-check it so a
	// stray 06 01 01 01 elsewhere in the response can't false-match.
	if numCaps := raw[idx-1]; numCaps < 0x20 || numCaps > 0x60 {
		return false
	}

	capsOff := idx + len(prefix)
	if capsOff >= len(raw) {
		return false
	}

	return raw[capsOff]&0x20 != 0
}

// dialUpstreamWithRedirect opens a TCP connection to the upstream Oracle, sends the
// initial Connect packet, and follows any Redirect packets (for Oracle RAC / SCAN
// listeners that return a different service address). Returns the final connection
// and the Accept packet received from the ultimately responding Oracle node.
func dialUpstreamWithRedirect(s *session, addr string, connectPkt *TNSPacket) (net.Conn, *TNSPacket, error) {
	const maxRedirects = 3

	currentAddr := addr
	currentConnect := connectPkt.Raw

	for redirects := 0; redirects <= maxRedirects; redirects++ {
		conn, err := net.DialTimeout("tcp", currentAddr, 10*time.Second)
		if err != nil {
			return nil, nil, fmt.Errorf("dial upstream %s: %w", currentAddr, err)
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: upstream connected",
			slog.String("addr", currentAddr), slog.Int("redirects", redirects))

		if _, err := conn.Write(currentConnect); err != nil {
			_ = conn.Close()

			return nil, nil, fmt.Errorf("forward Connect to upstream %s: %w", currentAddr, err)
		}

		pkt, err := readTNSPacket(conn)
		if err != nil {
			_ = conn.Close()

			return nil, nil, fmt.Errorf("read upstream response from %s: %w", currentAddr, err)
		}

		if pkt.Type == TNSPacketTypeResend {
			s.logger.DebugContext(s.ctx, "pre-auth relay: upstream requested Resend, re-sending Connect")

			if _, err := conn.Write(currentConnect); err != nil {
				_ = conn.Close()

				return nil, nil, fmt.Errorf("resend Connect to upstream: %w", err)
			}

			pkt, err = readTNSPacket(conn)
			if err != nil {
				_ = conn.Close()

				return nil, nil, fmt.Errorf("read upstream Accept after Resend: %w", err)
			}
		}

		//nolint:exhaustive // pre-auth only expects Accept or Redirect; everything else is rejected via default.
		switch pkt.Type {
		case TNSPacketTypeAccept:
			return conn, pkt, nil
		case TNSPacketTypeRedirect:
			_ = conn.Close()

			newAddr, newConnect, err := parseRedirect(pkt.Payload, connectPkt)
			if err != nil {
				return nil, nil, fmt.Errorf("parse Redirect from %s: %w", currentAddr, err)
			}

			s.logger.DebugContext(s.ctx, "pre-auth relay: following Redirect",
				slog.String("from", currentAddr), slog.String("to", newAddr))

			currentAddr = newAddr
			currentConnect = newConnect
		default:
			_ = conn.Close()

			return nil, nil, fmt.Errorf("%w: got %s from upstream instead of Accept", ErrUnexpectedPacketType, pkt.Type)
		}
	}

	return nil, nil, fmt.Errorf("%w: %d", ErrUpstreamTooManyRedirects, maxRedirects)
}

// parseRedirect extracts the new host/port and the updated connect descriptor from
// a TNS Redirect packet. The Redirect payload contains a new (DESCRIPTION=...)
// connect string pointing to the address that should receive the next Connect.
func parseRedirect(redirectPayload []byte, originalConnect *TNSPacket) (string, []byte, error) {
	// Redirect payloads from v315+ servers are prefixed with a 2-byte length, then
	// the replacement connect descriptor. Older servers send just the descriptor.
	descStart := 0
	if len(redirectPayload) >= 2 {
		declared := int(redirectPayload[0])<<8 | int(redirectPayload[1])
		if declared > 0 && declared <= len(redirectPayload)-2 {
			descStart = 2
		}
	}

	desc := string(redirectPayload[descStart:])
	host, port := extractRedirectHostPort(desc)

	if host == "" || port == 0 {
		return "", nil, fmt.Errorf("%w: %q", ErrRedirectMissingHostPort, desc)
	}

	// Build a new Connect packet containing the redirected descriptor so the final
	// upstream node sees the connect_data addressed to itself.
	newConnect := rebuildConnectForRedirect(originalConnect, redirectPayload[descStart:])

	return net.JoinHostPort(host, fmt.Sprintf("%d", port)), newConnect, nil
}

// extractRedirectHostPort parses HOST=x and PORT=y from a connect descriptor.
func extractRedirectHostPort(desc string) (string, int) {
	host := extractKVValue(desc, "HOST=")
	portStr := extractKVValue(desc, "PORT=")

	port := 0
	for _, ch := range portStr {
		if ch < '0' || ch > '9' {
			break
		}

		port = port*10 + int(ch-'0')
	}

	return host, port
}

// extractKVValue finds `key=VALUE` within a connect descriptor, returning VALUE
// stripped of surrounding whitespace and ) delimiter.
func extractKVValue(desc, key string) string {
	idx := indexOfCI(desc, key)
	if idx < 0 {
		return ""
	}

	start := idx + len(key)

	end := start
	for end < len(desc) && desc[end] != ')' && desc[end] != ' ' {
		end++
	}

	return desc[start:end]
}

// indexOfCI returns the case-insensitive index of substr in s, or -1.
func indexOfCI(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}

	for i := 0; i+len(substr) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(substr)], substr) {
			return i
		}
	}

	return -1
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}

		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}

		if ca != cb {
			return false
		}
	}

	return true
}

// rebuildConnectForRedirect returns a new raw Connect packet whose connect-data
// region is replaced by the descriptor returned in a Redirect packet. The TNS
// metadata header (version, SDU, TDU, NT flags, etc.) is preserved from the
// original Connect.
func rebuildConnectForRedirect(original *TNSPacket, newDesc []byte) []byte {
	// For simplicity, rebuild as a legacy (v<315) Connect: header + fixed metadata + descriptor.
	// Oracle accepts this even from v315+ clients because the server only cares about
	// the connect_data portion being parseable.
	const tnsHdr = 8

	if len(original.Raw) < tnsHdr+20 {
		return original.Raw
	}

	// Keep bytes [0..connect_data_offset) from the original header+metadata and replace the
	// connect data with newDesc. connect_data_offset is at original.Payload[18:20] per
	// Oracle's Connect format.
	cdOffset := int(original.Payload[18])<<8 | int(original.Payload[19])
	if cdOffset < tnsHdr || cdOffset > len(original.Raw) {
		cdOffset = tnsHdr + 26
	}

	out := make([]byte, 0, cdOffset+len(newDesc))
	out = append(out, original.Raw[:cdOffset]...)
	out = append(out, newDesc...)

	// Update total length in the header (big-endian uint16 at [0:2]).
	total := len(out)
	if total <= 0xFFFF {
		out[0] = byte(total >> 8)
		out[1] = byte(total)
	}

	return out
}

// isAuthPhase1 reports whether a TNS Data packet carries the O5LOGON AUTH Phase 1 message.
// AUTH Phase 1 is a piggyback TTC message (func=0x03) with sub-op 0x76.
func isAuthPhase1(pkt *TNSPacket) bool {
	if pkt.Type != TNSPacketTypeData {
		return false
	}

	if len(pkt.Payload) < ttcDataFlagsSize+2 {
		return false
	}

	return pkt.Payload[ttcDataFlagsSize] == byte(TTCFuncPiggyback) &&
		pkt.Payload[ttcDataFlagsSize+1] == PiggybackSubAuth1
}

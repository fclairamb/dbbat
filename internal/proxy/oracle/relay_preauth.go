package oracle

import (
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

	// Force clients onto the classic multi-round-trip login by clearing the
	// FAST_AUTH capability in the Accept we relay. When the upstream advertises
	// FAST_AUTH (Oracle 23ai), modern clients (python-oracledb thin, OCI thick /
	// sqlplus) fold protocol + data-type negotiation and AUTH into a single
	// pipelined message the pre-auth relay cannot split — so dbbat never sees an
	// isolated AUTH Phase 1 to terminate, relays the client's auth (API key as
	// password) straight to the real server, and the login fails. Stripping the
	// bit is safe: the client's negotiation is with dbbat, and dbbat runs its own
	// O5LOGON client against the upstream afterwards.
	if stripUnsupportedAcceptFlags(acceptPkt.Raw) {
		s.logger.DebugContext(s.ctx, "pre-auth relay: cleared FAST_AUTH / END_OF_RESPONSE in Accept")
	}

	s.logger.DebugContext(s.ctx, "pre-auth relay: forwarding Accept to client", slog.Int("len", len(acceptPkt.Raw)))

	if _, err := s.clientConn.Write(acceptPkt.Raw); err != nil {
		_ = upstream.Close()

		return nil, nil, fmt.Errorf("forward Accept to client: %w", err)
	}

	// Pump upstream→client concurrently rather than reading one upstream packet
	// after each client packet. The pre-auth negotiation is NOT strictly
	// lock-step: OCI thick clients (sqlplus) send Set Data Types and then their
	// next message without waiting for a server reply, and the server may batch
	// or defer responses. A single-read-per-write relay deadlocks — it blocks
	// waiting for an upstream reply that only comes after the client's next
	// packet, which the relay hasn't read yet (observed as a 5s upstream i/o
	// timeout → ORA-03135 on sqlplus). A concurrent pump forwards each direction
	// as bytes arrive. The client→upstream direction stays here so we can tap it
	// for AUTH Phase 1 and stop before forwarding it.
	pump := s.startUpstreamPump(upstream)

	for {
		clientPkt, err := readTNSPacket(s.clientConn)
		if err != nil {
			pump.stop()
			_ = upstream.Close()

			return nil, nil, fmt.Errorf("read client packet during pre-auth relay: %w", err)
		}

		if isAuthPhase1(clientPkt) {
			s.logger.DebugContext(s.ctx, "pre-auth relay: detected AUTH Phase 1, ending relay",
				slog.Int("len", len(clientPkt.Payload)))

			// Stop the pump cleanly; the upstream socket stays open and idle
			// (the server is waiting for AUTH) so dbbat can reuse it.
			if err := pump.stop(); err != nil {
				_ = upstream.Close()

				return nil, nil, fmt.Errorf("upstream relay failed: %w", err)
			}

			return clientPkt, upstream, nil
		}

		if err := pump.err(); err != nil {
			pump.stop()
			_ = upstream.Close()

			return nil, nil, fmt.Errorf("upstream relay failed: %w", err)
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: client→upstream",
			slog.String("type", clientPkt.Type.String()),
			slog.Int("len", len(clientPkt.Raw)))

		if _, err := upstream.Write(clientPkt.Raw); err != nil {
			pump.stop()
			_ = upstream.Close()

			return nil, nil, fmt.Errorf("forward client packet to upstream: %w", err)
		}
	}
}

// upstreamPump forwards upstream→client packets in a background goroutine during
// the pre-auth relay, observing the customHash capability as the Set Protocol
// response flows past. It exists so the relay is not lock-step (see
// relayPreAuthNegotiation for why that matters).
type upstreamPump struct {
	upstream net.Conn
	done     chan error
	stopCh   chan struct{}
	fatal    chan error // buffered snapshot of an unexpected upstream error
}

// startUpstreamPump launches the upstream→client forwarding goroutine.
func (s *session) startUpstreamPump(upstream net.Conn) *upstreamPump {
	p := &upstreamPump{
		upstream: upstream,
		done:     make(chan error, 1),
		stopCh:   make(chan struct{}),
		fatal:    make(chan error, 1),
	}

	go p.run(s)

	return p
}

func (p *upstreamPump) run(s *session) {
	for {
		pkt, err := readTNSPacket(p.upstream)
		if err != nil {
			// Distinguish a deliberate stop (we set a past read deadline to
			// unblock this read) from a real upstream failure.
			select {
			case <-p.stopCh:
				p.done <- nil
			default:
				p.fatal <- err
				p.done <- err
			}

			return
		}

		if caps, ok := serverCompileTimeCaps(pkt.Payload); ok && len(caps) > logonCompatibilityCapIndex &&
			caps[logonCompatibilityCapIndex]&capCustomHash != 0 {
			s.upstreamCustomHash = true

			s.logger.DebugContext(s.ctx, "pre-auth relay: observed upstream customHash",
				slog.Int("caps_len", len(caps)),
				slog.String("caps4", fmt.Sprintf("0x%02x", caps[logonCompatibilityCapIndex])))
		}

		s.logger.DebugContext(s.ctx, "pre-auth relay: upstream→client",
			slog.String("type", pkt.Type.String()),
			slog.Int("len", len(pkt.Raw)))

		if _, err := s.clientConn.Write(pkt.Raw); err != nil {
			p.fatal <- err
			p.done <- err

			return
		}
	}
}

// err returns a non-nil error if the pump has already failed with an unexpected
// upstream/client error (non-blocking).
func (p *upstreamPump) err() error {
	select {
	case err := <-p.fatal:
		return err
	default:
		return nil
	}
}

// stop signals the pump to exit and waits for it. The upstream socket is left
// open (deadline cleared) for reuse by the upstream O5LOGON client. Returns the
// pump's terminal error, or nil if it stopped on request.
func (p *upstreamPump) stop() error {
	select {
	case <-p.stopCh:
		// already stopping/stopped
	default:
		close(p.stopCh)
	}

	// Unblock a pump goroutine parked in readTNSPacket.
	_ = p.upstream.SetReadDeadline(time.Now())

	err := <-p.done

	// Restore the socket to a usable state for the upstream auth exchange.
	_ = p.upstream.SetReadDeadline(time.Time{})

	return err
}

const (
	// logonCompatibilityCapIndex is the index into ServerCompileTimeCaps that
	// carries the logon-compatibility flags (go-ora's ServerCompileTimeCaps[4]).
	logonCompatibilityCapIndex = 4
	// capCustomHash (caps[4]&0x20) enables the PBKDF2 customHash combined-key
	// derivation in modern Oracle clients.
	capCustomHash = 0x20

	// acceptFlags2FastAuth is the FAST_AUTH bit in the Accept packet's flags2
	// field (python-oracledb TNS_ACCEPT_FLAG_FAST_AUTH). When set, modern clients
	// fold negotiation + AUTH into a single pipelined message.
	acceptFlags2FastAuth = 0x10000000
	// acceptFlags2EndOfResponse is the HAS_END_OF_RESPONSE bit
	// (python-oracledb TNS_ACCEPT_FLAG_HAS_END_OF_RESPONSE). When set (protocol
	// version >= 319), clients enable end-of-response markers on every server
	// response and advertise the matching TTC cap upstream. dbbat's terminated
	// O5LOGON path emits no such markers, so the client blocks waiting for one
	// after the AUTH challenge. Clearing the bit keeps end-of-response disabled
	// consistently across client, dbbat, and upstream.
	acceptFlags2EndOfResponse = 0x02000000
	// acceptFlags2ProxyUnsupported bundles the Accept capabilities dbbat's
	// terminated-auth relay cannot honor and therefore clears before forwarding.
	acceptFlags2ProxyUnsupported = acceptFlags2FastAuth | acceptFlags2EndOfResponse
	// tnsVersionMinOOBCheck is the protocol version at/above which the Accept
	// packet carries the 4-byte flags2 word (python-oracledb TNS_VERSION_MIN_OOB_CHECK).
	tnsVersionMinOOBCheck = 318
	// acceptFlags2BodyOffset is the offset of the 4-byte flags2 word within the
	// Accept packet body (after the 8-byte TNS header). Layout mirrors
	// python-oracledb's Accept parse: version(2) options(2) skip(10) flags1(1)
	// skip(9) sdu(4) skip(5) flags2(4) = 33.
	acceptFlags2BodyOffset = 33
)

// serverCompileTimeCaps extracts the ServerCompileTimeCaps byte array from a
// Set Protocol (protocol negotiation) response payload. The payload is the TNS
// packet body (data-flags prefix included). Returns ok=false for any packet
// that is not an Accept-protocol response (message code 1) or that is
// truncated. The field walk mirrors go-ora's newTCPNego / python-oracledb's
// protocol parse so dbbat reads the exact caps[4] its client will.
func serverCompileTimeCaps(payload []byte) ([]byte, bool) {
	p := ttcDataFlagsSize
	if p >= len(payload) || payload[p] != 0x01 { // message code 1 = Accept protocol
		return nil, false
	}

	p++    // message code
	p++    // protocol server version
	p++    // reserved byte
	// protocol server string (null-terminated)
	for p < len(payload) && payload[p] != 0x00 {
		p++
	}
	p++ // null terminator

	p += 2 // server charset (uint16)
	p++    // server flags (uint8)
	if p+2 > len(payload) {
		return nil, false
	}

	charsetElem := int(payload[p]) | int(payload[p+1])<<8 // little-endian
	p += 2
	p += charsetElem * 5 // charset table

	if p+2 > len(payload) {
		return nil, false
	}

	len1 := int(payload[p])<<8 | int(payload[p+1]) // big-endian
	p += 2
	p += len1 // reserved array

	if p >= len(payload) {
		return nil, false
	}

	capsLen := int(payload[p])
	p++

	if p+capsLen > len(payload) {
		return nil, false
	}

	return payload[p : p+capsLen], true
}

// stripUnsupportedAcceptFlags clears the Accept flags2 capabilities dbbat's
// terminated-auth relay cannot honor (FAST_AUTH and HAS_END_OF_RESPONSE) in
// place, returning true if any were present and cleared. Only Accept packets
// from protocol version >= 318 carry flags2; older or shorter packets are left
// untouched. See acceptFlags2ProxyUnsupported for why.
func stripUnsupportedAcceptFlags(raw []byte) bool {
	const tnsHdr = 8

	if len(raw) < tnsHdr+2 {
		return false
	}

	body := raw[tnsHdr:]
	version := int(body[0])<<8 | int(body[1])
	if version < tnsVersionMinOOBCheck {
		return false
	}

	if len(body) < acceptFlags2BodyOffset+4 {
		return false
	}

	off := acceptFlags2BodyOffset
	flags2 := uint32(body[off])<<24 | uint32(body[off+1])<<16 | uint32(body[off+2])<<8 | uint32(body[off+3])
	if flags2&acceptFlags2ProxyUnsupported == 0 {
		return false
	}

	flags2 &^= acceptFlags2ProxyUnsupported
	body[off] = byte(flags2 >> 24)
	body[off+1] = byte(flags2 >> 16)
	body[off+2] = byte(flags2 >> 8)
	body[off+3] = byte(flags2)

	return true
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

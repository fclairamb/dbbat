package oracle

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
)

// upstreamAuth performs the full Oracle authentication sequence to the upstream server.
// dbbat acts as an Oracle client, using stored database credentials.
//
// Sequence:
//  1. Send TNS Connect with the database's service name
//  2. Handle Resend loops, receive Accept
//  3. Set Protocol exchange
//  4. Set Data Types exchange
//  5. O5LOGON authentication with stored credentials
//  6. Enter relay mode (handled by caller)
func (s *session) upstreamAuth() error { // Step 1: Connect to upstream
	upstreamAddr := net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port))
	var err error

	s.upstreamConn, err = net.Dial("tcp", upstreamAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to upstream %s: %w", upstreamAddr, err)
	}

	s.logger.InfoContext(s.ctx, "connected to upstream Oracle", slog.String("addr", upstreamAddr))

	// Step 2: Send TNS Connect
	if err := s.upstreamConnect(); err != nil {
		return fmt.Errorf("upstream connect failed: %w", err)
	}

	// Step 3: TTC negotiation — relay Set Protocol and Set Data Types
	if err := s.upstreamNegotiate(); err != nil {
		return fmt.Errorf("upstream negotiation failed: %w", err)
	}

	// Step 4: O5LOGON authentication with stored credentials
	if err := s.upstreamO5Logon(); err != nil {
		return fmt.Errorf("upstream O5LOGON auth failed: %w", err)
	}

	s.logger.InfoContext(s.ctx, "upstream Oracle authentication complete")

	return nil
}

// upstreamConnect sends a TNS Connect packet using the captured header from a real thin client.
func (s *session) upstreamConnect() error {
	serviceName := s.database.DatabaseName
	if s.database.OracleServiceName != nil && *s.database.OracleServiceName != "" {
		serviceName = *s.database.OracleServiceName
	}

	desc := []byte(fmt.Sprintf(
		"(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=%s)(CID=(PROGRAM=dbbat)(HOST=dbbat)(USER=dbbat)))(ADDRESS=(PROTOCOL=TCP)(HOST=%s)(PORT=%d)))",
		serviceName, s.database.Host, s.database.Port,
	))

	// Build Connect using captured header. The header bytes after the 8-byte TNS header
	// contain version 319, SDU 8192, and correct flags. We update data length and offset.
	hdr := make([]byte, len(capturedConnectHeader))
	copy(hdr, capturedConnectHeader)
	headerLen := 8 + len(hdr) // TNS header + Connect header
	binary.BigEndian.PutUint16(hdr[16:18], uint16(len(desc)))
	binary.BigEndian.PutUint16(hdr[18:20], uint16(headerLen))

	totalLen := headerLen + len(desc)
	raw := make([]byte, 0, totalLen)
	// TNS header (8 bytes) — bytes 0-1 = TOTAL packet length (pre-v315 format)
	raw = append(raw, byte(totalLen>>8), byte(totalLen), 0x00, 0x00) // length + checksum
	raw = append(raw, byte(TNSPacketTypeConnect), 0x00, 0x00, 0x00)  // type + reserved
	raw = append(raw, hdr...)
	raw = append(raw, desc...)

	connectPkt := &TNSPacket{Type: TNSPacketTypeConnect, Raw: raw}
	s.logger.DebugContext(s.ctx, "upstream: sending TNS Connect", slog.Int("raw_len", len(raw)), slog.Int("desc_len", len(desc)))

	resp, err := s.sendUpstreamConnect(connectPkt)
	if err != nil {
		return err
	}

	if resp.Type == TNSPacketTypeRefuse {
		return ErrUpstreamRefused
	}

	if resp.Type != TNSPacketTypeAccept {
		return fmt.Errorf("%w: got %s for upstream connect", ErrUnexpectedPacketType, resp.Type)
	}

	return nil
}

// buildUpstreamAuthPhase1 builds a TTC AUTH Phase 1 message using the exact format
// captured from python-oracledb thin. The preamble has a fixed 12-byte structure
// with username length at offsets 3 and 11. The suffix contains AUTH_ KV pairs.
func buildUpstreamAuthPhase1(username string) []byte {
	u := []byte(username)
	buf := make([]byte, 0, 16+len(u)+len(capturedAuthPhase1Suffix))

	// Header: data_flags(2) + func(0x03) + sub(0x76)
	buf = append(buf, 0x00, 0x00, 0x03, 0x76)

	// Preamble (12 bytes): captured template with username length at positions 3 and 11
	preamble := []byte{0x01, 0x01, 0x01, byte(len(u)), 0x01, 0x01, 0x01, 0x01, 0x05, 0x01, 0x01, byte(len(u))}
	buf = append(buf, preamble...)

	// Username
	buf = append(buf, u...)

	// AUTH_ KV pairs suffix (captured from real thin client, with dbbat-specific values)
	buf = append(buf, capturedAuthPhase1Suffix...)

	return buf
}

// capturedAuthPhase1Suffix contains the AUTH_ key-value pairs after the username.
// Matches the TTC DLC+CLR encoding from a real python-oracledb thin AUTH Phase 1.
// Values are dbbat-specific (AUTH_TERMINAL=dbbat, AUTH_PROGRAM_NM=dbbat, etc.)
var capturedAuthPhase1Suffix = []byte{
	// AUTH_TERMINAL = "dbbat"
	0x01, 0x0d, 0x0d, 0x41, 0x55, 0x54, 0x48, 0x5f, 0x54, 0x45, 0x52, 0x4d, 0x49, 0x4e, 0x41, 0x4c,
	0x01, 0x05, 0x05, 0x64, 0x62, 0x62, 0x61, 0x74, 0x00,
	// AUTH_PROGRAM_NM = "dbbat"
	0x01, 0x0f, 0x0f, 0x41, 0x55, 0x54, 0x48, 0x5f, 0x50, 0x52, 0x4f, 0x47, 0x52, 0x41, 0x4d, 0x5f,
	0x4e, 0x4d, 0x01, 0x05, 0x05, 0x64, 0x62, 0x62, 0x61, 0x74, 0x00,
	// AUTH_MACHINE = "dbbat"
	0x01, 0x0c, 0x0c, 0x41, 0x55, 0x54, 0x48, 0x5f, 0x4d, 0x41, 0x43, 0x48, 0x49, 0x4e, 0x45,
	0x01, 0x05, 0x05, 0x64, 0x62, 0x62, 0x61, 0x74, 0x00,
	// AUTH_PID = "1"
	0x01, 0x08, 0x08, 0x41, 0x55, 0x54, 0x48, 0x5f, 0x50, 0x49, 0x44,
	0x01, 0x01, 0x01, 0x31, 0x00,
	// AUTH_SID = "dbbat"
	0x01, 0x08, 0x08, 0x41, 0x55, 0x54, 0x48, 0x5f, 0x53, 0x49, 0x44,
	0x01, 0x05, 0x05, 0x64, 0x62, 0x62, 0x61, 0x74, 0x00,
}

// capturedConnectHeader (76 bytes) — Connect-specific header captured from python-oracledb thin.
// Fields at offsets 16-17 (data length) and 18-19 (data offset) are updated dynamically.
var capturedConnectHeader = []byte{
	0x01, 0x3f, 0x01, 0x2c, 0x04, 0x01, 0x20, 0x00, 0x20, 0x00, 0x4f, 0x98, 0x00, 0x00, 0x00, 0x01,
	0x00, 0xee, 0x00, 0x4a, 0x00, 0x00, 0x00, 0x00, 0x84, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x01, 0x00, 0xf8, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00,
}

// sendUpstreamConnect sends the Connect packet and handles Resend loops.
func (s *session) sendUpstreamConnect(connectPkt *TNSPacket) (*TNSPacket, error) {
	if err := writeTNSPacket(s.upstreamConn, connectPkt); err != nil {
		return nil, fmt.Errorf("failed to send connect: %w", err)
	}

	for range maxResendAttempts {
		resp, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read upstream response: %w", err)
		}

		if resp.Type == TNSPacketTypeResend {
			// Resend the Connect packet
			if err := writeTNSPacket(s.upstreamConn, connectPkt); err != nil {
				return nil, fmt.Errorf("failed to resend connect: %w", err)
			}

			continue
		}

		return resp, nil
	}

	return nil, ErrMaxResendExceeded
}

// writeV315Data writes a TTC payload as a v315+ TNS Data packet to the upstream connection.

// upstreamNegotiate handles Set Protocol and Set Data Types exchange with upstream.
// All post-Accept packets use v315+ format (4-byte length).
// The sequence matches what Oracle thin clients do:
//  1. Send Marker packet (type 12)
//  2. Send Set Protocol (Data)
//  3. Read responses (skip non-Data packets like type-14 notifications)
func (s *session) upstreamNegotiate() error {
	// Send Set Protocol + Set Data Types using captured packets from the real
	// python-oracledb thin client. The raw packets include the v315+ TNS header.
	// This is the same approach as the server-side negotiation — replay real Oracle traffic.
	// Send Marker (type 12) then Set Protocol — matching real thin client sequence
	capturedMarker := []byte{0x00, 0x00, 0x00, 0x0b, 0x0c, 0x00, 0x00, 0x00, 0x01, 0x00, 0x02}
	if _, err := s.upstreamConn.Write(capturedMarker); err != nil {
		return fmt.Errorf("failed to send Marker: %w", err)
	}

	s.logger.DebugContext(s.ctx, "upstream: sending Set Protocol")
	if _, err := s.upstreamConn.Write(capturedClientSetProtocol); err != nil {
		return fmt.Errorf("failed to send Set Protocol: %w", err)
	}

	s.logger.DebugContext(s.ctx, "upstream: waiting for Set Protocol response")
	// Read responses — skip non-Data packets (type-14 notifications, etc.)
	var resp *TNSPacket
	var err error

	for {
		resp, err = readTNSPacket(s.upstreamConn)
		if err != nil {
			return fmt.Errorf("failed to read Set Protocol response: %w", err)
		}

		if resp.Type == TNSPacketTypeData {
			break
		}

		s.logger.DebugContext(s.ctx, "upstream: skipping non-Data packet", slog.String("type", resp.Type.String()))
	}

	if _, err := s.upstreamConn.Write(capturedClientSetDataTypes); err != nil {
		return fmt.Errorf("failed to send Set Data Types: %w", err)
	}

	resp, err = readTNSPacket(s.upstreamConn)
	if err != nil {
		return fmt.Errorf("failed to read Set Data Types response: %w", err)
	}

	if resp.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for upstream Set Data Types", ErrUnexpectedPacketType, resp.Type)
	}

	return nil
}

// upstreamO5Logon performs client-side O5LOGON authentication with the upstream Oracle server.
// This uses the stored database credentials (username/password).
func (s *session) upstreamO5Logon() error { // Decrypt the database password
	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("failed to decrypt database password: %w", err)
	}

	// AUTH Phase 1: send username with AUTH_ metadata KV pairs.
	// Uses captured template format — username is replaced, other fields use fixed values.
	phase1 := buildUpstreamAuthPhase1(s.database.Username)
	if _, err := s.upstreamConn.Write(encodeV315DataPacket(phase1)); err != nil {
		return fmt.Errorf("failed to send AUTH Phase 1: %w", err)
	}

	// Read AUTH Challenge from upstream
	challengeResp, err := readTNSPacket(s.upstreamConn)
	if err != nil {
		return fmt.Errorf("failed to read AUTH challenge: %w", err)
	}

	if challengeResp.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for upstream AUTH challenge", ErrUnexpectedPacketType, challengeResp.Type)
	}

	// Parse all AUTH_ KV pairs from the challenge
	challenge := parseUpstreamAuthKVPairs(challengeResp.Payload)

	s.logger.DebugContext(s.ctx, "upstream AUTH challenge",
		slog.Int("sesskey_len", len(challenge.sessKey)),
		slog.Int("salt_len", len(challenge.salt)),
		slog.String("pbkdf2_csk_salt", challenge.pbkdf2CskSalt),
		slog.Int("pbkdf2_vgen_count", challenge.pbkdf2VgenCount))

	// Generate PBKDF2 client response (Oracle 19c uses verifier type 18453)
	resp, err := generatePBKDF2ClientResponse(s.database.Password, &challenge)
	if err != nil {
		return fmt.Errorf("failed to generate PBKDF2 response: %w", err)
	}

	// AUTH Phase 2: send encrypted session key, password, and speedy key
	phase2 := buildUpstreamAuthPhase2(s.database.Username, resp)
	if _, err := s.upstreamConn.Write(encodeV315DataPacket(phase2)); err != nil {
		return fmt.Errorf("failed to send AUTH Phase 2: %w", err)
	}

	// Read AUTH response
	authResp, err := readTNSPacket(s.upstreamConn)
	if err != nil {
		return fmt.Errorf("failed to read AUTH response: %w", err)
	}

	if authResp.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for upstream AUTH response", ErrUnexpectedPacketType, authResp.Type)
	}

	// Check if auth succeeded (response func code 0x08 with return code 0)
	if err := checkUpstreamAuthResponse(authResp.Payload); err != nil {
		return err
	}

	return nil
}

// checkUpstreamAuthResponse checks if the upstream AUTH response indicates success.

// parseUpstreamAuthChallenge extracts AUTH_SESSKEY and AUTH_VFR_DATA from the upstream's
// AUTH challenge response.

// generateO5LogonClientResponse generates the client-side O5LOGON auth response.
// This mirrors what an Oracle client does: derive verifier from password+salt,
// decrypt server session key, generate client session key, encrypt password.

// buildTNSConnect constructs the TNS Connect packet payload.

// buildClientSetProtocol constructs a client Set Protocol request.
// Format: data_flags(2) + func(0x01) + version(0x06) + compat(0x00) + platform_string + null.

// buildClientSetDataTypes constructs a client Set Data Types request.

// buildClientAuthPhase1 constructs a TTC AUTH Phase 1 message.
// This is func=0x03 (piggyback), sub=0x76 (AUTH1), with the username.

// buildClientAuthPhase2 constructs a TTC AUTH Phase 2 message.
// Contains the encrypted client session key and encrypted password.

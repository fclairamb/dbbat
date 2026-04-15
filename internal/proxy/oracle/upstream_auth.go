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

// upstreamConnect sends a TNS Connect packet and handles Accept/Resend/Refuse.
func (s *session) upstreamConnect() error { // Build TNS Connect descriptor
	serviceName := s.database.DatabaseName
	if s.database.OracleServiceName != nil && *s.database.OracleServiceName != "" {
		serviceName = *s.database.OracleServiceName
	}

	connectDescriptor := fmt.Sprintf(
		"(DESCRIPTION=(CONNECT_DATA=(SERVICE_NAME=%s)(CID=(PROGRAM=dbbat)(HOST=dbbat)(USER=dbbat)))(ADDRESS=(PROTOCOL=TCP)(HOST=%s)(PORT=%d)))",
		serviceName, s.database.Host, s.database.Port,
	)

	connectPayload := buildTNSConnect(connectDescriptor)
	connectPkt := &TNSPacket{
		Type:    TNSPacketTypeConnect,
		Payload: connectPayload,
		Raw:     encodeTNSPacket(TNSPacketTypeConnect, connectPayload),
	}

	// Send Connect and handle response (with Resend loop)
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
func (s *session) writeV315Data(payload []byte) error {
	_, err := s.upstreamConn.Write(encodeV315DataPacket(payload))

	return err
}

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

	// AUTH Phase 1: send username (v315+ format)
	if err := s.writeV315Data(buildClientAuthPhase1(s.database.Username)); err != nil {
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

	// Parse the challenge to extract AUTH_SESSKEY and AUTH_VFR_DATA
	encServerSessKey, authVfrData, err := parseUpstreamAuthChallenge(challengeResp.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse AUTH challenge: %w", err)
	}

	// Generate client-side O5LOGON response
	clientEncSessKey, encPassword, err := generateO5LogonClientResponse(
		s.database.Password, encServerSessKey, authVfrData,
	)
	if err != nil {
		return fmt.Errorf("failed to generate O5LOGON response: %w", err)
	}

	// AUTH Phase 2: send encrypted password (v315+ format)
	if err := s.writeV315Data(buildClientAuthPhase2(s.database.Username, clientEncSessKey, encPassword)); err != nil {
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
func checkUpstreamAuthResponse(payload []byte) error {
	if len(payload) <= ttcDataFlagsSize+2 {
		return nil // Too short to contain error info, assume success
	}

	funcCode := payload[ttcDataFlagsSize]
	if funcCode != byte(TTCFuncResponse) {
		return nil // Not a response packet
	}

	if len(payload) <= ttcDataFlagsSize+3 {
		return nil
	}

	retCode := payload[ttcDataFlagsSize+2]
	if retCode != 0 {
		return ErrAuthFailed
	}

	return nil
}

// parseUpstreamAuthChallenge extracts AUTH_SESSKEY and AUTH_VFR_DATA from the upstream's
// AUTH challenge response.
func parseUpstreamAuthChallenge(tnsDataPayload []byte) (string, string, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+3 {
		return "", "", ErrAuthPhase1TooShort
	}

	// Parse key-value pairs from the response
	offset := ttcDataFlagsSize + 2 // Skip data flags + response func code + sequence
	if offset >= len(tnsDataPayload) {
		return "", "", ErrAuthPhase1NoData
	}

	pairs := parseTTCKVPairs(tnsDataPayload[offset:])

	var sessKey, vfrData string

	for _, p := range pairs {
		switch p.Key {
		case authKeySessKey:
			sessKey = p.Value
		case authKeyVfrData:
			vfrData = p.Value
		}
	}

	if sessKey == "" {
		return "", "", ErrAuthPhase2MissingSessKey
	}

	if vfrData == "" {
		return "", "", ErrAuthPhase2MissingPassword
	}

	return sessKey, vfrData, nil
}

// generateO5LogonClientResponse generates the client-side O5LOGON auth response.
// This mirrors what an Oracle client does: derive verifier from password+salt,
// decrypt server session key, generate client session key, encrypt password.
func generateO5LogonClientResponse(password, encServerSessKey, authVfrData string) (string, string, error) {
	// AUTH_VFR_DATA is the hex-encoded salt (verifier type is in the KV flag, not the value)
	if len(authVfrData) < 2 {
		return "", "", ErrAuthPhase1TooShort
	}

	salt, err := hexDecodeBytes(authVfrData)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode salt from AUTH_VFR_DATA: %w", err)
	}

	// Derive verifier key from password + salt
	verifierKey := deriveVerifierKey(password, salt)

	// Decrypt server session key
	decKey := deriveAESKey(verifierKey)
	encServerSessKeyBytes, err := hexDecodeBytes(encServerSessKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode server session key: %w", err)
	}

	serverSessionKey, err := aes192CBCDecrypt(decKey, encServerSessKeyBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to decrypt server session key: %w", err)
	}

	// Generate client session key
	clientSessKeyBytes := make([]byte, o5LogonSessionKeyLength)
	if _, err := cryptoRandRead(clientSessKeyBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate client session key: %w", err)
	}

	// Encrypt client session key
	encClientSessKeyBytes, err := aes192CBCEncrypt(decKey, clientSessKeyBytes)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt client session key: %w", err)
	}

	// Derive combined key
	combinedKey := deriveCombinedKey(serverSessionKey, clientSessKeyBytes)

	// Encrypt password: random_prefix(16 bytes) + password
	prefix := make([]byte, o5LogonPasswordPrefixLen)
	if _, err := cryptoRandRead(prefix); err != nil {
		return "", "", fmt.Errorf("failed to generate password prefix: %w", err)
	}

	passwordPayload := make([]byte, 0, len(prefix)+len(password))
	passwordPayload = append(passwordPayload, prefix...)
	passwordPayload = append(passwordPayload, []byte(password)...)
	encPasswordBytes, err := aes192CBCEncrypt(combinedKey, passwordPayload)
	if err != nil {
		return "", "", fmt.Errorf("failed to encrypt password: %w", err)
	}

	return hexEncode(encClientSessKeyBytes), hexEncode(encPasswordBytes), nil
}

// buildTNSConnect constructs the TNS Connect packet payload.
func buildTNSConnect(connectDescriptor string) []byte { // TNS Connect header is 58 bytes, followed by the connect descriptor.
	// This is a simplified version — real Connect packets have many more fields.
	descriptorBytes := []byte(connectDescriptor)
	headerLen := 58

	header := make([]byte, headerLen)
	// Version (2 bytes) — TNS 315
	binary.BigEndian.PutUint16(header[0:2], 315)
	// Compatible version (2 bytes)
	binary.BigEndian.PutUint16(header[2:4], 300)
	// Service options (2 bytes)
	binary.BigEndian.PutUint16(header[4:6], 0)
	// SDU size (2 bytes) — 8192
	binary.BigEndian.PutUint16(header[6:8], 8192)
	// TDU size (2 bytes) — 65535
	binary.BigEndian.PutUint16(header[8:10], 65535)
	// Protocol characteristics (2 bytes)
	binary.BigEndian.PutUint16(header[10:12], 0x8001)
	// Max packets before ACK (2 bytes)
	binary.BigEndian.PutUint16(header[12:14], 0)
	// Byte order/endianness (2 bytes)
	binary.BigEndian.PutUint16(header[14:16], 1)
	// Data length (2 bytes) — connect descriptor length
	binary.BigEndian.PutUint16(header[16:18], uint16(len(descriptorBytes)))
	// Data offset (2 bytes) — offset from start of connect header to descriptor
	binary.BigEndian.PutUint16(header[18:20], uint16(headerLen))
	// Max receivable connect data (4 bytes)
	binary.BigEndian.PutUint32(header[20:24], 0)
	// Connect flags 0 and 1 (2 bytes)
	header[24] = 0x41 // flag0
	header[25] = 0x41 // flag1
	// Remaining bytes are zero (trace info, padding, etc.)

	result := make([]byte, 0, len(header)+len(descriptorBytes))
	result = append(result, header...)
	result = append(result, descriptorBytes...)

	return result
}

// buildClientSetProtocol constructs a client Set Protocol request.
// Format: data_flags(2) + func(0x01) + version(0x06) + compat(0x00) + platform_string + null.

// buildClientSetDataTypes constructs a client Set Data Types request.

// buildClientAuthPhase1 constructs a TTC AUTH Phase 1 message.
// This is func=0x03 (piggyback), sub=0x76 (AUTH1), with the username.
func buildClientAuthPhase1(username string) []byte {
	payload := make([]byte, 0, 6+len(username))
	payload = append(payload,
		0x00, 0x00, // data flags
		0x03,                // func = piggyback
		PiggybackSubAuth1,   // sub-op = AUTH Phase 1
		byte(len(username)), // username length
	)
	payload = append(payload, []byte(username)...)
	payload = append(payload, 0x01) // AUTH_MODE_LOGON

	return payload
}

// buildClientAuthPhase2 constructs a TTC AUTH Phase 2 message.
// Contains the encrypted client session key and encrypted password.
func buildClientAuthPhase2(username, clientEncSessKey, encPassword string) []byte {
	payload := make([]byte, 0, 6+len(username)+len(clientEncSessKey)+len(encPassword)+50)
	payload = append(payload,
		0x00, 0x00, // data flags
		0x03,                // func = piggyback
		PiggybackSubAuth2,   // sub-op = AUTH Phase 2
		byte(len(username)), // username length
	)
	payload = append(payload, []byte(username)...)

	// Key-value pairs count
	payload = append(payload, 0x02)

	// AUTH_SESSKEY
	payload = append(payload, encodeTTCKVPair(authKeySessKey, clientEncSessKey)...)

	// AUTH_PASSWORD
	payload = append(payload, encodeTTCKVPair(authKeyPassword, encPassword)...)

	return payload
}

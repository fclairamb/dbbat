package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// LogonMode flags from go-ora/v2/connection.go. Inlined so the upstream AUTH
// path doesn't depend on the go-ora package.
const (
	logonModeNoNewPass   = 0x1
	logonModeUserAndPass = 0x100
)

const defaultDriverName = "dbbat-oracle-proxy"

// driverIdentity holds optional process-level descriptors sent in AUTH KV
// pairs. The values are informational; Oracle uses them for v$session.
type driverIdentity struct {
	HostName    string
	ProgramName string
	PID         int
	OSUser      string
	DriverName  string
}

func defaultDriverIdentity() driverIdentity {
	host, _ := os.Hostname()

	prog := os.Args[0]
	if prog == "" {
		prog = defaultDriverName
	}

	user := os.Getenv("USER")
	if user == "" {
		user = "dbbat"
	}

	return driverIdentity{
		HostName:    host,
		ProgramName: prog,
		PID:         os.Getpid(),
		OSUser:      user,
		DriverName:  defaultDriverName,
	}
}

// runUpstreamClientAuth drives Oracle AUTH on the relay-phase upstream socket
// using stored database credentials. The caller must already have a
// post-Set-Data-Types socket open on s.upstreamConn.
// upstreamAuthPhaseState carries the identity and derived secrets from the
// upstream AUTH Phase 1 exchange to Phase 2. dbbat runs Phase 1 before the
// client O5LOGON challenge so it can capture the upstream's real end-of-call
// marker (see session.upstreamEndMarker), then Phase 2 after the client has
// authenticated.
type upstreamAuthPhaseState struct {
	identity driverIdentity
	username string
	mode     uint32
	sec      *upstreamAuthSecrets
}

// runUpstreamAuthPhase1 drives the upstream AUTH Phase 1 exchange on the
// relay-phase socket: it sends the (rewritten) client Phase 1, reads the
// challenge, derives the upstream auth secrets, and captures the challenge's
// end-of-call marker into s.upstreamEndMarker for the client-facing challenge.
func (s *session) runUpstreamAuthPhase1() error {
	if s.upstreamConn == nil {
		return ErrUpstreamConnNotSet
	}

	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("decrypt database password: %w", err)
	}

	identity := defaultDriverIdentity()
	username := strings.ToUpper(s.database.Username)
	password := s.database.Password
	mode := uint32(logonModeNoNewPass)

	if err := s.sendUpstreamAuthPhase1(username, identity, mode); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 1: %w", err)
	}

	authResp, phase1Raw, _, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 1 response: %w", err)
	}

	if authResp.OracleErr != 0 {
		return fmt.Errorf("%w: ORA-%05d %s", ErrUpstreamAuthPhase1Rejected, authResp.OracleErr, authResp.OracleErrText)
	}

	if marker := extractEndOfCallMarker(phase1Raw, s.clientWideFormat, s.clientBigClrChunks); marker != nil {
		s.upstreamEndMarker = marker
		s.logger.DebugContext(s.ctx, "captured upstream end-of-call marker",
			slog.Int("len", len(marker)))
	}

	sec, err := buildSecretsFromPhase1Response(authResp, s.upstreamCustomHash)
	if err != nil {
		return fmt.Errorf("interpret AUTH Phase 1 response: %w", err)
	}

	if err := computeUpstreamAuthSecrets(password, authResp.encServerSessKey, sec); err != nil {
		return fmt.Errorf("derive upstream auth secrets: %w", err)
	}

	s.upstreamAuthState = &upstreamAuthPhaseState{
		identity: identity,
		username: username,
		mode:     mode,
		sec:      sec,
	}

	return nil
}

// runUpstreamAuthPhase2 completes the upstream AUTH exchange using the state
// captured by runUpstreamAuthPhase1. It must be called after that.
func (s *session) runUpstreamAuthPhase2() error {
	st := s.upstreamAuthState
	if st == nil {
		return ErrUpstreamConnNotSet
	}

	if err := s.sendUpstreamAuthPhase2(st.username, st.identity, st.sec, st.mode|logonModeUserAndPass); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 2: %w", err)
	}

	finalResp, finalRaw, finalTTC, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 2 response: %w", err)
	}

	if finalResp.OracleErr != 0 {
		return fmt.Errorf("%w: ORA-%05d %s", ErrUpstreamAuthPhase2Rejected, finalResp.OracleErr, finalResp.OracleErrText)
	}

	s.upstreamAuthOKResponse = finalRaw
	s.upstreamAuthOKTTC = finalTTC

	s.logger.InfoContext(s.ctx, "upstream Oracle AUTH complete on relay-phase socket",
		slog.String("user", s.database.Username),
		slog.Int("verifier_type", st.sec.verifierType),
		slog.Bool("custom_hash", st.sec.customHash))

	return nil
}

// extractEndOfCallMarker pulls the trailing end-of-call (0x04) Summary out of a
// raw upstream AUTH Phase 1 challenge packet. Layout: TNS header (8) + data
// flags (2) + [0x08 KV dictionary][0x04 Summary...]. Returns nil if the packet
// does not match (caller falls back to the built-in marker).
func extractEndOfCallMarker(raw []byte, wide, bigChunks bool) []byte {
	const tnsHdr = 8

	off := tnsHdr + ttcDataFlagsSize
	if off >= len(raw) || raw[off] != 0x08 {
		return nil
	}

	consumed, ok := parseAuthKVDictionary(raw[off+1:], &upstreamAuthResponse{properties: make(map[string]string)}, wide, bigChunks)
	if !ok {
		return nil
	}

	markerStart := off + 1 + consumed
	if markerStart >= len(raw) || raw[markerStart] != 0x04 {
		return nil
	}

	marker := make([]byte, len(raw)-markerStart)
	copy(marker, raw[markerStart:])

	return marker
}

// sendUpstreamAuthPhase1 writes the upstream-facing AUTH Phase 1 packet.
//
// When the relay handed us the client's actual Phase 1 packet, we forward
// it with only the username field swapped to the upstream DB user
// (rewriteAuthPhase1Username) and the original data-flag bits preserved.
// Reusing the client's wire shape — flags, TTC trailer, KV-pair set —
// keeps the upstream's TTC-cap-conditioned parser happy (clients negotiate
// caps differently; SQLcl's JDBC thin Set Data Types is substantially
// richer than go-ora's, and a hand-built go-ora-style Phase 1 fed to a
// SQLcl-conditioned upstream draws a TNS Marker (interrupt + reset) pair
// followed by a 60-second silence ending in RST).
//
// When phase1Pkt is unavailable (legacy non-relay path / tests) we fall
// back to the synthetic builder with `00 00` data flags.
func (s *session) sendUpstreamAuthPhase1(username string, identity driverIdentity, mode uint32) error {
	if s.clientAuthPhase1Pkt == nil || len(s.clientAuthPhase1Pkt.Payload) <= ttcDataFlagsSize {
		return s.writeUpstreamData(buildClientAuthPhase1(username, identity, mode))
	}

	clientPayload := s.clientAuthPhase1Pkt.Payload
	dataFlags := clientPayload[:ttcDataFlagsSize]
	clientBody := clientPayload[ttcDataFlagsSize:]

	rewritten, err := rewriteAuthPhase1Username(clientBody, username)
	if err != nil {
		s.logger.WarnContext(s.ctx, "Phase 1 rewrite failed; falling back to synthetic Phase 1",
			slog.Any("error", err))

		return s.writeUpstreamData(buildClientAuthPhase1(username, identity, mode))
	}

	s.logger.DebugContext(s.ctx, "upstream AUTH: forwarding rewritten client Phase 1",
		slog.Int("original_body_len", len(clientBody)),
		slog.Int("rewritten_body_len", len(rewritten)),
		slog.String("upstream_user", username))

	full := make([]byte, 0, ttcDataFlagsSize+len(rewritten))
	full = append(full, dataFlags...)
	full = append(full, rewritten...)

	return s.writeUpstreamPayload(full)
}

// sendUpstreamAuthPhase2 writes the upstream-facing AUTH Phase 2 packet.
//
// When the relay handed us the client's actual Phase 2 packet, we forward
// it with the username swapped to the upstream DB user and AUTH_SESSKEY /
// AUTH_PASSWORD / AUTH_PBKDF2_SPEEDY_KEY values replaced with the upstream-
// derived ones (rewriteAuthPhase2). All other KV pairs — AUTH_CONNECT_STRING,
// AUTH_COPYRIGHT, AUTH_ACL, AUTH_ALTER_SESSION, the SESSION_CLIENT_DRIVER_*
// triplet — pass through verbatim.
//
// This matters because the upstream's AUTH OK is conditioned on the client's
// Phase 2 KV-pair set: JDBC thin sends a richer set than go-ora, and a
// hand-built go-ora-style Phase 2 forwarded to a JDBC-conditioned upstream
// produces an AUTH OK that JDBC's T4CTTIfun.receive default-cases on
// (ORA-17401 protocol violation).
//
// When clientAuthPhase2Pkt is unavailable (legacy non-relay path / tests) we
// fall back to the synthetic builder.
func (s *session) sendUpstreamAuthPhase2(username string, identity driverIdentity, sec *upstreamAuthSecrets, mode uint32) error {
	if s.clientAuthPhase2Pkt == nil || len(s.clientAuthPhase2Pkt.Payload) <= ttcDataFlagsSize {
		return s.writeUpstreamData(buildClientAuthPhase2(username, identity, sec, mode))
	}

	clientPayload := s.clientAuthPhase2Pkt.Payload
	dataFlags := clientPayload[:ttcDataFlagsSize]
	clientBody := clientPayload[ttcDataFlagsSize:]

	var (
		rewritten []byte
		err       error
	)

	if s.clientWideFormat {
		rewritten, err = rewriteAuthPhase2Wide(clientBody, username, sec)
	} else {
		rewritten, err = rewriteAuthPhase2(clientBody, username, sec, s.clientBigClrChunks)
	}

	if err != nil {
		s.logger.WarnContext(s.ctx, "Phase 2 rewrite failed; falling back to synthetic Phase 2",
			slog.Any("error", err))

		return s.writeUpstreamData(buildClientAuthPhase2(username, identity, sec, mode))
	}

	s.logger.DebugContext(s.ctx, "upstream AUTH: forwarding rewritten client Phase 2",
		slog.Int("original_body_len", len(clientBody)),
		slog.Int("rewritten_body_len", len(rewritten)),
		slog.String("upstream_user", username))

	full := make([]byte, 0, ttcDataFlagsSize+len(rewritten))
	full = append(full, dataFlags...)
	full = append(full, rewritten...)

	return s.writeUpstreamPayload(full)
}

// writeUpstreamPayload writes a v315+ TNS Data packet whose payload is given
// in full (including the leading 2 data-flag bytes). Used by Phase 1
// forwarding so the client's data-flag bits are preserved verbatim.
func (s *session) writeUpstreamPayload(payload []byte) error {
	pkt := encodeV315DataPacket(payload)

	s.logger.DebugContext(s.ctx, "upstream AUTH: writing packet (preserved flags)",
		slog.Int("pkt_len", len(pkt)),
		slog.Int("payload_len", len(payload)))

	if _, err := s.upstreamConn.Write(pkt); err != nil {
		return fmt.Errorf("upstream write: %w", err)
	}

	return nil
}

// writeUpstreamData wraps a TTC body in a v315+ TNS Data packet with a
// zero data-flag prefix and writes it to the upstream socket.
func (s *session) writeUpstreamData(body []byte) error {
	payload := make([]byte, 0, ttcDataFlagsSize+len(body))
	payload = append(payload, 0x00, 0x00) // data flags
	payload = append(payload, body...)

	pkt := encodeV315DataPacket(payload)

	s.logger.DebugContext(s.ctx, "upstream AUTH: writing packet",
		slog.Int("pkt_len", len(pkt)),
		slog.Int("body_len", len(body)))

	if _, err := s.upstreamConn.Write(pkt); err != nil {
		return fmt.Errorf("upstream write: %w", err)
	}

	return nil
}

// upstreamAuthResponse aggregates fields parsed from one or more AUTH response
// TTC streams.
type upstreamAuthResponse struct {
	encServerSessKey string
	salt             string // hex
	verifierType     int
	pbkdf2ChkSalt    string
	pbkdf2VgenCount  int
	pbkdf2SderCount  int
	OracleErr        int
	OracleErrText    string
	properties       map[string]string
}

// readUpstreamAuthMessages reads TNS Data packets from upstream until an
// end-of-call message code (4 or 9) appears. Multiple Data packets may
// carry pieces of the same TTC stream.
//
// The last raw Data packet read is returned alongside the parsed response.
// Callers (notably runUpstreamClientAuth's Phase 2 read) reuse it as the
// AUTH OK packet forwarded to the client after AUTH_SVR_RESPONSE patching.
func (s *session) readUpstreamAuthMessages() (*upstreamAuthResponse, []byte, []byte, error) {
	resp := &upstreamAuthResponse{properties: make(map[string]string)}

	var (
		buf    []byte
		allRaw []byte
	)

	for {
		pkt, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read upstream packet: %w", err)
		}

		s.logger.DebugContext(s.ctx, "upstream AUTH: received packet",
			slog.String("type", pkt.Type.String()),
			slog.Int("raw_len", len(pkt.Raw)),
			slog.Int("payload_len", len(pkt.Payload)))

		if pkt.Type != TNSPacketTypeData {
			s.logger.DebugContext(s.ctx, "upstream AUTH: skipping non-Data packet",
				slog.String("type", pkt.Type.String()),
				slog.Int("len", len(pkt.Raw)))

			continue
		}

		if len(pkt.Payload) < ttcDataFlagsSize {
			continue
		}

		// Accumulate every raw response packet: the AUTH OK (and, on richer
		// clients like sqlplus/OCI, the Phase 1 challenge) can span multiple
		// TNS packets. Forwarding only the last one truncates the response and
		// trips ORA-03106 on the client.
		allRaw = append(allRaw, pkt.Raw...)
		buf = append(buf, pkt.Payload[ttcDataFlagsSize:]...)

		if parseAuthMessageStream(buf, resp, s.clientWideFormat, s.clientBigClrChunks) {
			return resp, allRaw, buf, nil
		}
	}
}

// parseAuthMessageStream walks a TTC byte stream and returns true when the
// AUTH response is sufficiently complete: either after consuming a 0x08
// dictionary (which carries the entire Phase 1 challenge or Phase 2 session
// properties) or after seeing an explicit end-of-call code 0x04 / 0x09 with
// an Oracle error.
//
// Codes:
//
//	0x08              dictionary (KV pairs) — terminal: contains everything
//	                  we need for Phase 1 (AUTH_SESSKEY + AUTH_VFR_DATA +
//	                  AUTH_PBKDF2_*) or Phase 2 (AUTH_VERSION_STRING + ...).
//	0x04 / 0x09       end-of-call (with a Summary structure). Auth flows use
//	                  this only when the upstream rejects (no preceding 0x08)
//	                  or as a trailing marker after a dict (already handled).
//	0x0F              warning (6 bytes, skipped).
//
// The function is tolerant: when it cannot decode a region it returns false
// so the caller reads more bytes.
func parseAuthMessageStream(buf []byte, resp *upstreamAuthResponse, wide, bigChunks bool) bool {
	pos := 0

	for pos < len(buf) {
		msgCode := buf[pos]
		pos++

		switch msgCode {
		case 0x08:
			if _, ok := parseAuthKVDictionary(buf[pos:], resp, wide, bigChunks); !ok {
				return false
			}

			return true

		case 0x04, 0x09:
			parseAuthSummary(buf[pos:], resp)

			return true

		case 0x0F:
			if pos+6 > len(buf) {
				return false
			}

			pos += 6

		default:
			return false
		}
	}

	return false
}

// parseAuthKVDictionary parses a TTC AUTH KV dictionary that follows a 0x08
// message code. Returns ok=false when the dictionary is truncated.
//
// The dictionary length is a TTC compressed integer, matching go-ora's
// session.GetInt(2, bigEndian=true, compress=true). A 1-byte size prefix is
// followed by `size` big-endian bytes encoding the count of KV pairs.
func parseAuthKVDictionary(buf []byte, resp *upstreamAuthResponse, wide, bigChunks bool) (int, bool) {
	if wide {
		return parseWideAuthKVDictionary(buf, resp)
	}

	dictLen, n := readCompressedInt(buf)
	if n == 0 {
		return 0, false
	}

	pos := n

	for i := 0; i < dictLen; i++ {
		pair, ok := readAuthKVPair(buf[pos:], bigChunks)
		if !ok {
			return 0, false
		}

		pos += pair.Consumed
		recordAuthProperty(string(pair.Key), string(pair.Value), pair.Flag, resp)
	}

	return pos, true
}

// parseWideAuthKVDictionary parses an OCI wide-encoded AUTH KV dictionary:
// [uint16-LE pair count] followed by wide KV pairs.
func parseWideAuthKVDictionary(buf []byte, resp *upstreamAuthResponse) (int, bool) {
	if len(buf) < 2 {
		return 0, false
	}

	count := int(binary.LittleEndian.Uint16(buf))
	pos := 2

	for i := 0; i < count; i++ {
		pair, ok := readWideKVPair(buf[pos:])
		if !ok {
			return 0, false
		}

		pos += pair.Consumed
		recordAuthProperty(string(pair.Key), string(pair.Value), pair.Flag, resp)
	}

	return pos, true
}

// authKVPairResult is the parsed form of a single TTC AUTH KV pair.
type authKVPairResult struct {
	Key      []byte
	Value    []byte
	Flag     int
	Consumed int
}

// readAuthKVPair reads keyLen + keyCLR + valueLen + valueCLR + flag, mirroring
// session.GetKeyVal in go-ora. ok=false signals the buffer is truncated.
func readAuthKVPair(buf []byte, bigChunks bool) (authKVPairResult, bool) {
	pos := 0
	out := authKVPairResult{}

	keyLen, n := readCompressedInt(buf[pos:])
	if n == 0 {
		return authKVPairResult{}, false
	}

	pos += n

	if keyLen > 0 {
		// Keys are short AUTH_* names — always 1-byte CLR framing.
		k, kn := readCLR(buf[pos:])
		if kn == 0 {
			return authKVPairResult{}, false
		}

		out.Key = k
		pos += kn
	}

	vLen, vn := readCompressedInt(buf[pos:])
	if vn == 0 {
		return authKVPairResult{}, false
	}

	pos += vn

	if vLen > 0 {
		v, vClrN := readCLRVariant(buf[pos:], bigChunks)
		if vClrN == 0 {
			return authKVPairResult{}, false
		}

		out.Value = v
		pos += vClrN
	}

	flagVal, fn := readCompressedInt(buf[pos:])
	if fn == 0 {
		return authKVPairResult{}, false
	}

	pos += fn
	out.Flag = flagVal
	out.Consumed = pos

	return out, true
}

// recordAuthProperty stores a parsed KV pair in the response. Specific keys
// are exposed as named fields used by the auth crypto.
func recordAuthProperty(key, value string, flag int, resp *upstreamAuthResponse) {
	resp.properties[key] = value

	switch key {
	case "AUTH_SESSKEY":
		resp.encServerSessKey = value
	case "AUTH_VFR_DATA":
		resp.salt = value
		resp.verifierType = flag
	case "AUTH_PBKDF2_CSK_SALT":
		resp.pbkdf2ChkSalt = value
	case "AUTH_PBKDF2_VGEN_COUNT":
		if n, err := strconv.Atoi(value); err == nil {
			resp.pbkdf2VgenCount = n
		}
	case "AUTH_PBKDF2_SDER_COUNT":
		if n, err := strconv.Atoi(value); err == nil {
			resp.pbkdf2SderCount = n
		}
	}
}

// parseAuthSummary walks the Summary structure that follows a code 4 / 9
// message, extracting any embedded ORA-* error. Best-effort.
//
// The Summary's first compressed int is EndOfCallStatus (not the Oracle
// error code). For Phase 1 challenge responses Oracle returns EndOfCallStatus
// non-zero with RetCode=0, which is not an error. Only set OracleErr when an
// "ORA-NNNNN" string is actually present in the buffer (matching go-ora's
// HasError behavior).
func parseAuthSummary(buf []byte, resp *upstreamAuthResponse) {
	idx := indexOf(buf, []byte("ORA-"))
	if idx < 0 {
		return
	}

	end := idx
	for end < len(buf) && buf[end] != 0x00 && buf[end] != 0x0A {
		end++
	}

	resp.OracleErrText = string(buf[idx:end])

	if end-idx >= 4+5 {
		if code, err := strconv.Atoi(string(buf[idx+4 : idx+4+5])); err == nil {
			resp.OracleErr = code
		}
	}
}

// buildClientAuthPhase1 returns the TTC body for an AUTH Phase 1 message.
// Layout matches go-ora's doAuth.
func buildClientAuthPhase1(username string, ident driverIdentity, mode uint32) []byte {
	buf := make([]byte, 0, 256)
	buf = append(buf, byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x00, 0x01)

	buf = append(buf, ttcCompressedUint(uint64(len(username)))...)
	buf = append(buf, ttcCompressedUint(uint64(mode))...)
	buf = append(buf, 0x01, 0x01, 0x05, 0x01, 0x01)
	buf = append(buf, ttcClr([]byte(username))...)

	pairs := []struct {
		key, value string
	}{
		{"AUTH_TERMINAL", ident.HostName},
		{"AUTH_PROGRAM_NM", ident.ProgramName},
		{"AUTH_MACHINE", ident.HostName},
		{"AUTH_PID", strconv.Itoa(ident.PID)},
		{"AUTH_SID", ident.OSUser},
	}

	for _, p := range pairs {
		buf = append(buf, ttcKeyVal(p.key, p.value, 0)...)
	}

	return buf
}

// buildClientAuthPhase2 returns the TTC body for an AUTH Phase 2 message.
// Layout mirrors AuthObject.Write in go-ora/v2/auth_object.go.
func buildClientAuthPhase2(username string, ident driverIdentity, sec *upstreamAuthSecrets, mode uint32) []byte {
	type kv struct {
		key, value string
		flag       int
	}

	tz := localTimezoneString(time.Now())

	pairs := []kv{
		{"AUTH_SESSKEY", sec.encClientSessKey, 1},
		{"AUTH_PASSWORD", sec.encPassword, 0},
	}

	if sec.eSpeedyKey != "" {
		pairs = append(pairs, kv{"AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0})
	}

	pairs = append(pairs,
		kv{"AUTH_TERMINAL", ident.HostName, 0},
		kv{"AUTH_PROGRAM_NM", ident.ProgramName, 0},
		kv{"AUTH_MACHINE", ident.HostName, 0},
		kv{"AUTH_PID", strconv.Itoa(ident.PID), 0},
		kv{"AUTH_SID", ident.OSUser, 0},
		kv{"SESSION_CLIENT_CHARSET", "871", 0},
		kv{"SESSION_CLIENT_LIB_TYPE", "0", 0},
		kv{"SESSION_CLIENT_DRIVER_NAME", ident.DriverName, 0},
		kv{"SESSION_CLIENT_VERSION", "2.0.0.0", 0},
		kv{"SESSION_CLIENT_LOBATTR", "1", 0},
		kv{"AUTH_ALTER_SESSION", fmt.Sprintf(
			"ALTER SESSION SET NLS_LANGUAGE='AMERICAN' NLS_TERRITORY='AMERICA'  TIME_ZONE='%s'\x00", tz), 1},
	)

	buf := make([]byte, 0, 512)
	buf = append(buf, byte(TTCFuncPiggyback), PiggybackSubAuth2, 0x00)

	usernameBytes := []byte(username)

	if len(usernameBytes) > 0 {
		buf = append(buf, 0x01)
		buf = append(buf, ttcCompressedUint(uint64(len(usernameBytes)))...)
	} else {
		buf = append(buf, 0x00, 0x00)
	}

	buf = append(buf, ttcCompressedUint(uint64(mode))...)
	buf = append(buf, 0x01)
	buf = append(buf, ttcCompressedUint(uint64(len(pairs)))...)
	buf = append(buf, 0x01, 0x01)

	if len(usernameBytes) > 0 {
		buf = append(buf, ttcClr(usernameBytes)...)
	}

	for _, p := range pairs {
		buf = append(buf, ttcKeyVal(p.key, p.value, p.flag)...)
	}

	return buf
}

// localTimezoneString returns Oracle's expected TIME_ZONE string (e.g.
// "+01:00") for the given time.
func localTimezoneString(t time.Time) string {
	_, offset := t.Zone()

	if offset == 0 {
		return "+00:00"
	}

	hours := offset / 3600

	minutes := (offset / 60) % 60
	if minutes < 0 {
		minutes = -minutes
	}

	return fmt.Sprintf("%+03d:%02d", hours, minutes)
}

// buildSecretsFromPhase1Response converts an upstreamAuthResponse into the
// secrets struct used by the crypto helpers.
func buildSecretsFromPhase1Response(resp *upstreamAuthResponse, customHash bool) (*upstreamAuthSecrets, error) {
	if resp.encServerSessKey == "" {
		return nil, ErrPhase1MissingSessKey
	}

	if resp.salt == "" {
		return nil, ErrPhase1MissingVerifierData
	}

	salt, err := hexDecodeUpper(resp.salt)
	if err != nil {
		return nil, fmt.Errorf("decode AUTH_VFR_DATA: %w", err)
	}

	sec := &upstreamAuthSecrets{
		verifierType:    resp.verifierType,
		customHash:      customHash,
		salt:            salt,
		pbkdf2VgenCount: resp.pbkdf2VgenCount,
		pbkdf2SderCount: resp.pbkdf2SderCount,
	}

	if resp.pbkdf2ChkSalt != "" {
		csk, err := hexDecodeUpper(resp.pbkdf2ChkSalt)
		if err != nil {
			return nil, fmt.Errorf("decode AUTH_PBKDF2_CSK_SALT: %w", err)
		}

		sec.pbkdf2ChkSalt = csk
	}

	if sec.pbkdf2VgenCount < 4096 || sec.pbkdf2VgenCount > 100000000 {
		sec.pbkdf2VgenCount = 4096
	}

	if sec.pbkdf2SderCount < 3 || sec.pbkdf2SderCount > 100000000 {
		sec.pbkdf2SderCount = 3
	}

	return sec, nil
}

// hexDecodeUpper decodes an uppercase hex string. Tolerant of mixed case for
// synthetic test inputs.
func hexDecodeUpper(s string) ([]byte, error) {
	s = strings.ToUpper(s)

	if len(s)%2 != 0 {
		return nil, ErrInvalidHexLength
	}

	out := make([]byte, len(s)/2)

	for i := 0; i < len(out); i++ {
		hi, err := hexNibble(s[2*i])
		if err != nil {
			return nil, err
		}

		lo, err := hexNibble(s[2*i+1])
		if err != nil {
			return nil, err
		}

		out[i] = hi<<4 | lo
	}

	return out, nil
}

func hexNibble(b byte) (byte, error) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', nil
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, nil
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, nil
	default:
		return 0, fmt.Errorf("%w: byte %d", ErrInvalidHex, b)
	}
}

// errors specific to the upstream AUTH client.
var (
	ErrUpstreamConnNotSet         = errors.New("upstream connection not set before AUTH")
	ErrPhase1MissingSessKey       = errors.New("AUTH Phase 1 response: missing AUTH_SESSKEY")
	ErrPhase1MissingVerifierData  = errors.New("AUTH Phase 1 response: missing AUTH_VFR_DATA")
	ErrInvalidHex                 = errors.New("invalid hex character")
	ErrInvalidHexLength           = errors.New("invalid hex string length")
	ErrUpstreamAuthPhase1Rejected = errors.New("upstream AUTH Phase 1 rejected")
	ErrUpstreamAuthPhase2Rejected = errors.New("upstream AUTH Phase 2 rejected")
)

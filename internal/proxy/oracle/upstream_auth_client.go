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
//
// The exchange is split in two stages so the session can interleave them with
// client-side O5LOGON: beginUpstreamAuth (Phase 1 → upstream challenge) may run
// BEFORE dbbat challenges the client, so the client challenge can borrow the
// upstream's caps-correct end-of-call summary (see clientChallengeTrailer);
// finishUpstreamAuth (Phase 2 → AUTH OK) runs after.
func (s *session) runUpstreamClientAuth() error {
	if s.upstreamAuthResp == nil {
		if err := s.beginUpstreamAuth(); err != nil {
			return err
		}
	}

	return s.finishUpstreamAuth()
}

// beginUpstreamAuth sends AUTH Phase 1 upstream (reusing the client's Phase 1
// wire-shape with the username swapped) and reads the upstream's challenge,
// which it stores on the session for finishUpstreamAuth — and for the client
// challenge builder, which reuses the challenge's trailing end-of-call summary.
func (s *session) beginUpstreamAuth() error {
	if s.upstreamConn == nil {
		return ErrUpstreamConnNotSet
	}

	if err := s.database.DecryptPassword(s.encryptionKey); err != nil {
		return fmt.Errorf("decrypt database password: %w", err)
	}

	identity := defaultDriverIdentity()
	username := strings.ToUpper(s.database.Username)
	mode := uint32(logonModeNoNewPass)

	if err := s.sendUpstreamAuthPhase1(username, identity, mode); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 1: %w", err)
	}

	authResp, _, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 1 response: %w", err)
	}

	if authResp.OracleErr != 0 {
		return fmt.Errorf("%w: ORA-%05d %s", ErrUpstreamAuthPhase1Rejected, authResp.OracleErr, authResp.OracleErrText)
	}

	s.upstreamAuthResp = authResp

	return nil
}

// finishUpstreamAuth derives the auth secrets from the challenge captured by
// beginUpstreamAuth, sends AUTH Phase 2 upstream, and reads the AUTH OK.
func (s *session) finishUpstreamAuth() error {
	authResp := s.upstreamAuthResp
	if authResp == nil {
		return ErrUpstreamAuthNotBegun
	}

	identity := defaultDriverIdentity()
	username := strings.ToUpper(s.database.Username)
	password := s.database.Password // decrypted in beginUpstreamAuth
	mode := uint32(logonModeNoNewPass)

	sec, err := buildSecretsFromPhase1Response(authResp, s.upstreamCustomHash)
	if err != nil {
		return fmt.Errorf("interpret AUTH Phase 1 response: %w", err)
	}

	if err := computeUpstreamAuthSecrets(password, authResp.encServerSessKey, sec); err != nil {
		return fmt.Errorf("derive upstream auth secrets: %w", err)
	}

	if err := s.sendUpstreamAuthPhase2(username, identity, sec, mode|logonModeUserAndPass); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 2: %w", err)
	}

	finalResp, finalRaw, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 2 response: %w", err)
	}

	if finalResp.OracleErr != 0 {
		return fmt.Errorf("%w: ORA-%05d %s", ErrUpstreamAuthPhase2Rejected, finalResp.OracleErr, finalResp.OracleErrText)
	}

	s.upstreamAuthOKResponse = finalRaw
	s.upstreamAuthOKFlags = finalResp.dataFlags
	s.upstreamAuthOKFragLens = finalResp.fragTTCLens

	s.logger.InfoContext(s.ctx, "upstream Oracle AUTH complete on relay-phase socket",
		slog.String("user", s.database.Username),
		slog.Int("verifier_type", sec.verifierType),
		slog.Bool("custom_hash", sec.customHash))

	return nil
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

	rewritten, err := rewriteAuthPhase2(clientBody, username, sec)
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

	// challengeTrailer is everything that followed the Phase 1 challenge's KV
	// dictionary — the end-of-call summary (message code 0x04 + Summary bytes).
	// Its width depends on the TTC compile-time caps the client negotiated
	// (e.g. 80 bytes for instantclient 23.3, 153 for the 23.26 DB-bundled OCI
	// client), so the client challenge dbbat builds reuses these live bytes
	// instead of a hard-coded capture — a wrong-width summary leaves unread
	// bytes in the client's TTC buffer and OCI aborts the AUTH call with a
	// break/reset marker exchange. See clientChallengeTrailer.
	challengeTrailer []byte

	// dataFlags is the 2-byte TTC data-flags prefix of the first Data packet in
	// the response. fragTTCLens is the TTC byte length (payload minus data
	// flags) of each Data packet the upstream sent. Together they let the AUTH
	// OK be re-fragmented at the upstream's original packet boundaries after the
	// AUTH_SVR_RESPONSE patch — a single merged packet can exceed the client's
	// negotiated SDU and be rejected with ORA-12592. See reframeAuthOK.
	dataFlags   []byte
	fragTTCLens []int
}

// readUpstreamAuthMessages reads TNS Data packets from upstream until an
// end-of-call message code (4 or 9) appears. Multiple Data packets may
// carry pieces of the same TTC stream.
//
// It returns a single reassembled TNS Data packet — the first packet's data
// flags followed by every fragment's TTC bytes — alongside the parsed response.
// finishUpstreamAuth reuses it as the AUTH OK forwarded to the client after
// AUTH_SVR_RESPONSE patching. Reassembling into one packet (rather than
// concatenating the raw framed packets) matters for OCI: the AUTH OK exceeds the
// upstream leg's SDU and arrives as two Data packets (observed 1967+557 bytes
// from Oracle 23ai), with the AUTH_SVR_RESPONSE hex value straddling the
// boundary. Concatenating raw packets injects an inner 8-byte TNS header mid-
// value, so the AUTH_SVR_RESPONSE patcher can't find the contiguous 96-char hex
// run (and a truncated single packet draws ORA-03106 on the client). The merged
// packet is what a direct client reassembles anyway.
func (s *session) readUpstreamAuthMessages() (*upstreamAuthResponse, []byte, error) {
	resp := &upstreamAuthResponse{properties: make(map[string]string)}

	var (
		buf       []byte
		dataFlags []byte
		sawBreak  bool
		haveFlags bool
	)

	for {
		pkt, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			return nil, nil, fmt.Errorf("read upstream packet: %w", err)
		}

		s.logger.DebugContext(s.ctx, "upstream AUTH: received packet",
			slog.String("type", pkt.Type.String()),
			slog.Int("raw_len", len(pkt.Raw)),
			slog.Int("payload_len", len(pkt.Payload)))

		if pkt.Type != TNSPacketTypeData {
			// Oracle 12c+/23ai runs an OOB break/reset probe during AUTH when the
			// session negotiated it (modern thin clients enable it, and dbbat
			// relayed their Set Data Types to the upstream). As the upstream's
			// O5LOGON *client*, dbbat must answer the break with a reset marker or
			// the upstream stalls waiting for it — mirroring readPhase2Packet on
			// the client-facing side. Without this, go-ora connections (which
			// don't enable the probe) work but python-oracledb thin / SQLcl hang.
			if isBreakMarker(pkt) {
				sawBreak = true
			}

			if isResetMarker(pkt) && sawBreak {
				if _, err := s.upstreamConn.Write(buildResetMarker()); err != nil {
					return nil, nil, fmt.Errorf("send upstream reset marker: %w", err)
				}

				s.logger.DebugContext(s.ctx, "upstream AUTH: answered break with reset marker")

				sawBreak = false
			}

			continue
		}

		if len(pkt.Payload) < ttcDataFlagsSize {
			continue
		}

		if !haveFlags {
			dataFlags = append([]byte(nil), pkt.Payload[:ttcDataFlagsSize]...)
			haveFlags = true
		}

		fragTTC := pkt.Payload[ttcDataFlagsSize:]
		resp.fragTTCLens = append(resp.fragTTCLens, len(fragTTC))
		buf = append(buf, fragTTC...)

		if parseAuthMessageStream(buf, resp, s.clientWideEncoding) {
			resp.dataFlags = dataFlags

			merged := make([]byte, 0, ttcDataFlagsSize+len(buf))
			merged = append(merged, dataFlags...)
			merged = append(merged, buf...)

			return resp, encodeV315DataPacket(merged), nil
		}
	}
}

// reframeAuthOK re-fragments a merged single-packet AUTH OK back into the TNS
// Data packets the upstream originally sent (payload sizes in fragTTCLens, each
// carrying the same 2-byte data-flags prefix). Forwarding a single merged AUTH
// OK can exceed the client's negotiated SDU — OCI rejects it with ORA-12592
// "bad packet" — while re-fragmenting at the upstream's boundaries reproduces
// exactly what a direct client accepts (real Oracle 23ai splits an OCI AUTH OK
// into 1967+557 byte packets). Because the AUTH_SVR_RESPONSE patch is a
// same-length in-place replacement, splitting at the original TTC offsets keeps
// every fragment valid even when the patched value straddles a boundary.
//
// mergedPacket is a v315 Data packet: 8-byte header + 2-byte data flags + TTC.
// When there are 0/1 fragments, or the sizes don't add up, mergedPacket is
// returned unchanged.
func reframeAuthOK(mergedPacket, dataFlags []byte, fragTTCLens []int) []byte {
	if len(fragTTCLens) <= 1 || len(dataFlags) != ttcDataFlagsSize {
		return mergedPacket
	}

	total := 0
	for _, n := range fragTTCLens {
		total += n
	}

	ttcStart := tnsHeaderSize + ttcDataFlagsSize
	if len(mergedPacket) < ttcStart || len(mergedPacket)-ttcStart != total {
		return mergedPacket
	}

	ttc := mergedPacket[ttcStart:]

	out := make([]byte, 0, len(mergedPacket)+len(fragTTCLens)*(tnsHeaderSize+ttcDataFlagsSize))
	pos := 0

	for _, n := range fragTTCLens {
		frag := make([]byte, 0, ttcDataFlagsSize+n)
		frag = append(frag, dataFlags...)
		frag = append(frag, ttc[pos:pos+n]...)
		out = append(out, encodeV315DataPacket(frag)...)
		pos += n
	}

	return out
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
func parseAuthMessageStream(buf []byte, resp *upstreamAuthResponse, wide bool) bool {
	pos := 0

	for pos < len(buf) {
		msgCode := buf[pos]
		pos++

		switch msgCode {
		case 0x08:
			consumed, ok := parseAuthKVDictionary(buf[pos:], resp, wide)
			if !ok {
				return false
			}

			// Capture the end-of-call summary that trails the dictionary. Its
			// exact width is conditioned on the negotiated TTC caps, so the
			// client challenge builder reuses it verbatim (clientChallengeTrailer).
			if rest := buf[pos+consumed:]; len(rest) > 0 && rest[0] == byte(TTCFuncOERR) {
				resp.challengeTrailer = append([]byte(nil), rest...)
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
func parseAuthKVDictionary(buf []byte, resp *upstreamAuthResponse, wide bool) (int, bool) {
	var dictLen, pos int

	if wide {
		// OCI: dictionary count is a 2-byte little-endian integer.
		if len(buf) < 2 {
			return 0, false
		}

		dictLen = int(binary.LittleEndian.Uint16(buf[0:2]))
		pos = 2
	} else {
		var n int

		dictLen, n = readCompressedInt(buf)
		if n == 0 {
			return 0, false
		}

		pos = n
	}

	for i := 0; i < dictLen; i++ {
		pair, ok := readAuthKVPair(buf[pos:], wide)
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
// session.GetKeyVal in go-ora. ok=false signals the buffer is truncated. wide
// selects the OCI fixed 4-byte little-endian length/flag encoding.
func readAuthKVPair(buf []byte, wide bool) (authKVPairResult, bool) {
	if wide {
		return readAuthKVPairWide(buf)
	}

	pos := 0
	out := authKVPairResult{}

	keyLen, n := readCompressedInt(buf[pos:])
	if n == 0 {
		return authKVPairResult{}, false
	}

	pos += n

	if keyLen > 0 {
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
		v, vClrN := readCLR(buf[pos:])
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

// readAuthKVPairWide is the OCI (4-byte little-endian) counterpart of
// readAuthKVPair: keyLen:4 LE + keyCLR + valueLen:4 LE + valueCLR + flag:4 LE.
func readAuthKVPairWide(buf []byte) (authKVPairResult, bool) {
	pos := 0
	out := authKVPairResult{}

	if pos+4 > len(buf) {
		return authKVPairResult{}, false
	}

	keyLen := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4

	if keyLen > 0 {
		k, kn := readCLR(buf[pos:])
		if kn == 0 {
			return authKVPairResult{}, false
		}

		out.Key = k
		pos += kn
	}

	if pos+4 > len(buf) {
		return authKVPairResult{}, false
	}

	vLen := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4

	if vLen > 0 {
		v, vClrN := readCLR(buf[pos:])
		if vClrN == 0 {
			return authKVPairResult{}, false
		}

		out.Value = v
		pos += vClrN
	}

	if pos+4 > len(buf) {
		return authKVPairResult{}, false
	}

	out.Flag = int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4
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
	ErrUpstreamAuthNotBegun       = errors.New("upstream AUTH Phase 2 requested before Phase 1")
	ErrPhase1MissingSessKey       = errors.New("AUTH Phase 1 response: missing AUTH_SESSKEY")
	ErrPhase1MissingVerifierData  = errors.New("AUTH Phase 1 response: missing AUTH_VFR_DATA")
	ErrInvalidHex                 = errors.New("invalid hex character")
	ErrInvalidHexLength           = errors.New("invalid hex string length")
	ErrUpstreamAuthPhase1Rejected = errors.New("upstream AUTH Phase 1 rejected")
	ErrUpstreamAuthPhase2Rejected = errors.New("upstream AUTH Phase 2 rejected")
)

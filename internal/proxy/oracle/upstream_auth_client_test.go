package oracle

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	dbbcrypto "github.com/fclairamb/dbbat/internal/crypto"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestBuildClientAuthPhase1Layout verifies the byte layout of an AUTH Phase 1
// message — TTC piggyback marker, sub-op, preamble, username, and the five
// expected KV pairs in order.
func TestBuildClientAuthPhase1Layout(t *testing.T) {
	t.Parallel()

	ident := driverIdentity{
		HostName:    "host",
		ProgramName: "prog",
		PID:         1234,
		OSUser:      "osuser",
		DriverName:  "drv",
	}

	body := buildClientAuthPhase1("ADMIN", ident, logonModeNoNewPass)

	wantHeader := []byte{byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x00, 0x01}
	if !bytes.HasPrefix(body, wantHeader) {
		t.Fatalf("phase1 body does not start with %x: got %x", wantHeader, body[:8])
	}

	wantClr := append([]byte{0x05}, []byte("ADMIN")...)
	if idx := bytes.Index(body, wantClr); idx < 0 {
		t.Fatalf("phase1 body missing CLR-encoded username, body=%x", body)
	}

	keys := []string{"AUTH_TERMINAL", "AUTH_PROGRAM_NM", "AUTH_MACHINE", "AUTH_PID", "AUTH_SID"}
	pos := 0

	for _, k := range keys {
		idx := bytes.Index(body[pos:], []byte(k))
		if idx < 0 {
			t.Fatalf("phase1 body missing key %q in remaining bytes %x", k, body[pos:])
		}

		pos += idx
	}
}

// TestBuildClientAuthPhase2Layout verifies the byte layout of an AUTH Phase 2
// message — TTC piggyback marker, sub-op, the KV pair list, and that
// AUTH_SESSKEY / AUTH_PASSWORD appear before any session-* KV pair.
func TestBuildClientAuthPhase2Layout(t *testing.T) {
	t.Parallel()

	sec := &upstreamAuthSecrets{
		verifierType:     VerifierType18453,
		customHash:       true,
		encClientSessKey: strings.Repeat("AB", 32),
		encPassword:      strings.Repeat("CD", 32),
		eSpeedyKey:       strings.Repeat("EF", 32),
	}

	ident := driverIdentity{
		HostName:    "host",
		ProgramName: "prog",
		PID:         42,
		OSUser:      "osuser",
		DriverName:  "drv",
	}

	body := buildClientAuthPhase2("ADMIN", ident, sec, logonModeNoNewPass|logonModeUserAndPass)

	if body[0] != byte(TTCFuncPiggyback) {
		t.Fatalf("phase2 body[0] = %x, want piggyback %x", body[0], byte(TTCFuncPiggyback))
	}

	if body[1] != PiggybackSubAuth2 {
		t.Fatalf("phase2 body[1] = %x, want sub %x", body[1], PiggybackSubAuth2)
	}

	if body[2] != 0x00 {
		t.Fatalf("phase2 body[2] = %x, want 0x00", body[2])
	}

	authSessKeyIdx := bytes.Index(body, []byte("AUTH_SESSKEY"))
	if authSessKeyIdx < 0 {
		t.Fatalf("phase2 missing AUTH_SESSKEY")
	}

	authPasswordIdx := bytes.Index(body, []byte("AUTH_PASSWORD"))
	if authPasswordIdx < 0 {
		t.Fatalf("phase2 missing AUTH_PASSWORD")
	}

	if authPasswordIdx <= authSessKeyIdx {
		t.Fatalf("AUTH_PASSWORD must come after AUTH_SESSKEY: ses=%d pwd=%d", authSessKeyIdx, authPasswordIdx)
	}

	speedyIdx := bytes.Index(body, []byte("AUTH_PBKDF2_SPEEDY_KEY"))
	if speedyIdx < 0 {
		t.Fatalf("phase2 missing AUTH_PBKDF2_SPEEDY_KEY for verifier 18453")
	}

	if speedyIdx <= authPasswordIdx {
		t.Fatalf("AUTH_PBKDF2_SPEEDY_KEY must come after AUTH_PASSWORD")
	}

	tzIdx := bytes.Index(body, []byte("TIME_ZONE='"))
	if tzIdx < 0 {
		t.Fatalf("phase2 AUTH_ALTER_SESSION missing TIME_ZONE")
	}
}

// TestBuildClientAuthPhase2NoSpeedyKeyFor6949 confirms verifier 6949 omits
// the AUTH_PBKDF2_SPEEDY_KEY pair.
func TestBuildClientAuthPhase2NoSpeedyKeyFor6949(t *testing.T) {
	t.Parallel()

	sec := &upstreamAuthSecrets{
		verifierType:     VerifierType6949,
		customHash:       false,
		encClientSessKey: strings.Repeat("01", 24),
		encPassword:      strings.Repeat("02", 32),
	}

	body := buildClientAuthPhase2("ADMIN", driverIdentity{HostName: "h", DriverName: "d"}, sec, logonModeNoNewPass)

	if bytes.Contains(body, []byte("AUTH_PBKDF2_SPEEDY_KEY")) {
		t.Fatalf("phase2 for 6949 must not include AUTH_PBKDF2_SPEEDY_KEY")
	}
}

// TestPhase1ResponseParserHappyPath constructs a synthetic AUTH Phase 1
// response (using the same KV-pair encoder dbbat uses on the server side)
// and verifies our parser extracts AUTH_SESSKEY, AUTH_VFR_DATA, and the
// PBKDF2 fields.
func TestPhase1ResponseParserHappyPath(t *testing.T) {
	t.Parallel()

	resp := &upstreamAuthResponse{properties: map[string]string{}}

	encServerKey := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32)))
	salt := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x11}, 16)))
	csk := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))

	stream := buildSyntheticAuthPhase1Response(encServerKey, salt, csk, "8192", "10", VerifierType18453)

	if !parseAuthMessageStream(stream, resp, false) {
		t.Fatalf("parseAuthMessageStream returned false on a complete stream")
	}

	if resp.encServerSessKey != encServerKey {
		t.Fatalf("encServerSessKey: got=%q want=%q", resp.encServerSessKey, encServerKey)
	}

	if resp.salt != salt {
		t.Fatalf("salt: got=%q want=%q", resp.salt, salt)
	}

	if resp.verifierType != VerifierType18453 {
		t.Fatalf("verifierType: got=%d want=%d", resp.verifierType, VerifierType18453)
	}

	if resp.pbkdf2ChkSalt != csk {
		t.Fatalf("pbkdf2ChkSalt: got=%q want=%q", resp.pbkdf2ChkSalt, csk)
	}

	if resp.pbkdf2VgenCount != 8192 {
		t.Fatalf("pbkdf2VgenCount: got=%d want=8192", resp.pbkdf2VgenCount)
	}

	if resp.pbkdf2SderCount != 10 {
		t.Fatalf("pbkdf2SderCount: got=%d want=10", resp.pbkdf2SderCount)
	}
}

// TestPhase1ResponseParserSplitAcrossPackets feeds the parser the synthetic
// response in two halves to verify it correctly waits for more bytes.
func TestPhase1ResponseParserSplitAcrossPackets(t *testing.T) {
	t.Parallel()

	encServerKey := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32)))
	salt := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x11}, 16)))
	csk := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))

	stream := buildSyntheticAuthPhase1Response(encServerKey, salt, csk, "4096", "3", VerifierType18453)

	resp := &upstreamAuthResponse{properties: map[string]string{}}

	half := len(stream) / 2

	if parseAuthMessageStream(stream[:half], resp, false) {
		t.Fatalf("parseAuthMessageStream returned true on partial stream")
	}

	if !parseAuthMessageStream(stream, resp, false) {
		t.Fatalf("parseAuthMessageStream did not finish on full stream")
	}
}

// TestPhase1ResponseSurfacesOracleError verifies that an end-of-call message
// carrying a non-zero retCode surfaces as resp.OracleErr.
func TestPhase1ResponseSurfacesOracleError(t *testing.T) {
	t.Parallel()

	codeBytes := ttcCompressedUint(1017)
	msg := []byte("ORA-01017 invalid username/password\x00")

	stream := make([]byte, 0, 1+len(codeBytes)+len(msg))
	stream = append(stream, 0x04)
	stream = append(stream, codeBytes...)
	stream = append(stream, msg...)

	resp := &upstreamAuthResponse{properties: map[string]string{}}

	if !parseAuthMessageStream(stream, resp, false) {
		t.Fatalf("parseAuthMessageStream did not finish on a complete stream")
	}

	if resp.OracleErr != 1017 {
		t.Fatalf("OracleErr: got=%d want=1017", resp.OracleErr)
	}

	if !strings.Contains(resp.OracleErrText, "ORA-01017") {
		t.Fatalf("OracleErrText missing ORA-01017: %q", resp.OracleErrText)
	}
}

// TestRunUpstreamClientAuthHandshake exercises the full client AUTH against a
// scripted in-memory upstream that mirrors a minimal Oracle AUTH handler.
func TestRunUpstreamClientAuthHandshake(t *testing.T) {
	t.Parallel()

	clientPipe, serverPipe := net.Pipe()

	defer func() { _ = clientPipe.Close() }()
	defer func() { _ = serverPipe.Close() }()

	username := "ADMIN"
	password := "ScriptedPassword!"

	scriptedSrv := &scriptedAuthServer{
		conn:     serverPipe,
		password: password,
	}

	go scriptedSrv.run()

	logger := slog.New(slog.DiscardHandler)

	encryptionKey := bytes.Repeat([]byte{0x99}, 32)

	dbUID := uuid.New()
	aad := dbbcrypto.ServerAAD(dbUID.String())

	encPw, err := dbbcrypto.Encrypt([]byte(password), encryptionKey, aad)
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}

	db := &store.Server{
		UID:               dbUID,
		Username:          username,
		PasswordEncrypted: encPw,
	}

	s := &session{
		ctx:                context.Background(),
		logger:             logger,
		upstreamConn:       clientPipe,
		upstreamCustomHash: true,
		database:           db,
		encryptionKey:      encryptionKey,
	}

	if err := clientPipe.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if err := s.runUpstreamClientAuth(); err != nil {
		t.Fatalf("runUpstreamClientAuth: %v", err)
	}

	if scriptedSrv.err != nil {
		t.Fatalf("scripted server: %v", scriptedSrv.err)
	}
}

// scriptedAuthServer handles one full Oracle-style AUTH exchange against an
// in-memory pipe. It accepts a known password, generates the same crypto our
// client expects, and verifies the encrypted password the client sends back
// recovers to the original plaintext.
type scriptedAuthServer struct {
	conn     net.Conn
	password string
	err      error
}

func (s *scriptedAuthServer) run() {
	defer func() { _ = s.conn.Close() }()

	if err := s.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.err = err

		return
	}

	if err := s.handle(); err != nil {
		s.err = err
	}
}

func (s *scriptedAuthServer) handle() error {
	pkt, err := readTNSPacket(s.conn)
	if err != nil {
		return err
	}

	if !isAuthPhase1(pkt) {
		return errExpectedAuthPhase1
	}

	salt := bytes.Repeat([]byte{0x44}, 16)
	pbkdf2ChkSalt := bytes.Repeat([]byte{0x55}, 32)
	vgen := 4096
	sder := 3

	speedyKey := pbkdf2SpeedyKey(append(append([]byte{}, salt...), []byte(pbkdf2SpeedyKeyLabel)...), []byte(s.password), vgen)

	hash := sha512.New()
	hash.Write(append(speedyKey, salt...))

	verifierKey := hash.Sum(nil)[:32]
	serverSessKey := bytes.Repeat([]byte{0x66}, 32)

	encServer, err := aesCBCEncryptZeroIV(verifierKey, serverSessKey, false)
	if err != nil {
		return err
	}

	stream := buildSyntheticAuthPhase1Response(
		strings.ToUpper(hex.EncodeToString(encServer)),
		strings.ToUpper(hex.EncodeToString(salt)),
		strings.ToUpper(hex.EncodeToString(pbkdf2ChkSalt)),
		"4096", "3", VerifierType18453,
	)

	if err := writeAsTNSDataPacket(s.conn, stream); err != nil {
		return err
	}

	phase2Pkt, err := readTNSPacket(s.conn)
	if err != nil {
		return err
	}

	encClientHex, encPwHex, err := scrapePhase2KVValues(phase2Pkt.Payload)
	if err != nil {
		return err
	}

	encClient, err := hex.DecodeString(encClientHex)
	if err != nil {
		return err
	}

	clientSessKey, err := aesCBCDecryptZeroIV(verifierKey, encClient, false)
	if err != nil {
		return err
	}

	if len(clientSessKey) != len(serverSessKey) {
		return errClientKeyLength
	}

	combined := referencePasswordEncKey(serverSessKey, clientSessKey, &upstreamAuthSecrets{
		verifierType:    VerifierType18453,
		pbkdf2ChkSalt:   pbkdf2ChkSalt,
		pbkdf2SderCount: sder,
	}, true)

	encPw, err := hex.DecodeString(encPwHex)
	if err != nil {
		return err
	}

	pwBuf, err := aesCBCDecryptZeroIV(combined, encPw, true)
	if err != nil {
		return err
	}

	if len(pwBuf) <= 16 {
		return errPasswordRoundTrip
	}

	if got := string(pwBuf[16:]); got != s.password {
		return errPasswordRoundTrip
	}

	zero := ttcCompressedUint(0)
	endStream := make([]byte, 0, 1+len(zero)+32)
	endStream = append(endStream, 0x04)
	endStream = append(endStream, zero...)
	endStream = append(endStream, bytes.Repeat([]byte{0x00}, 32)...)

	return writeAsTNSDataPacket(s.conn, endStream)
}

// scrapePhase2KVValues finds AUTH_SESSKEY and AUTH_PASSWORD values in the AUTH
// Phase 2 payload using the existing dbbat parser.
func scrapePhase2KVValues(payload []byte) (string, string, error) {
	if len(payload) < ttcDataFlagsSize+2 {
		return "", "", errPhase2TooShort
	}

	body := payload[ttcDataFlagsSize+2:]

	encClient := findKVByKeyBytes(body, []byte("AUTH_SESSKEY"))
	encPw := findKVByKeyBytes(body, []byte("AUTH_PASSWORD"))

	if encClient == "" || encPw == "" {
		return "", "", errPhase2MissingKey
	}

	return encClient, encPw, nil
}

func writeAsTNSDataPacket(conn net.Conn, body []byte) error {
	payload := append([]byte{0x00, 0x00}, body...)
	pkt := encodeV315DataPacket(payload)

	if _, err := conn.Write(pkt); err != nil {
		return err //nolint:wrapcheck // helper used for tests; clearer un-wrapped
	}

	return nil
}

// buildSyntheticAuthPhase1Response constructs a stream that begins with a 0x08
// dictionary message containing the standard AUTH_SESSKEY / AUTH_VFR_DATA /
// AUTH_PBKDF2_* pairs, followed by an end-of-call code 4 with retCode=0.
func buildSyntheticAuthPhase1Response(encServerKey, salt, csk, vgen, sder string, verifierType int) []byte {
	pairs := []struct {
		key, value string
		flag       int
	}{
		{"AUTH_SESSKEY", encServerKey, 1},
		{"AUTH_VFR_DATA", salt, verifierType},
		{"AUTH_PBKDF2_CSK_SALT", csk, 0},
		{"AUTH_PBKDF2_VGEN_COUNT", vgen, 0},
		{"AUTH_PBKDF2_SDER_COUNT", sder, 0},
	}

	buf := make([]byte, 0, 256)
	buf = append(buf, 0x08)
	buf = append(buf, ttcCompressedUint(uint64(len(pairs)))...)

	for _, p := range pairs {
		buf = append(buf, ttcKeyVal(p.key, p.value, p.flag)...)
	}

	buf = append(buf, 0x04)
	buf = append(buf, ttcCompressedUint(0)...)
	buf = append(buf, bytes.Repeat([]byte{0x00}, 32)...)

	return buf
}

// syntheticAuthTailLen is the size of the end-of-call tail appended by
// buildSyntheticAuthPhase1Response: 0x04 code + 1-byte retCode(0) + 32-byte Summary.
const syntheticAuthTailLen = 34

// TestClientChallengeTrailerReusesUpstreamCapture verifies the challenge-
// trailer-reuse branch: for a wide-encoding (OCI) client, the AUTH challenge
// trailer is the end-of-call summary captured verbatim from the live upstream
// Phase 1 challenge — the real server sized it for the exact TTC caps the
// client negotiated. A hand-built capture of the wrong width leaves stale
// bytes in the client's TTC read buffer and OCI aborts the AUTH call with a
// break/reset marker exchange.
func TestClientChallengeTrailerReusesUpstreamCapture(t *testing.T) {
	t.Parallel()

	captured := append([]byte{byte(TTCFuncOERR)}, bytes.Repeat([]byte{0x7a}, 152)...)

	s := &session{
		clientWideEncoding: true,
		upstreamAuthResp:   &upstreamAuthResponse{challengeTrailer: captured},
	}

	got := s.clientChallengeTrailer(VerifierType18453)
	if !bytes.Equal(got, captured) {
		t.Fatalf("trailer not reused from upstream capture: got=%x want=%x", got, captured)
	}
}

// TestClientChallengeTrailerFallsBackToHandBuilt covers every branch where the
// captured upstream summary must NOT be used: thin clients keep the proven
// hand-built summaries even when a capture exists, and wide clients fall back
// to the hand-built wide summary when no usable capture is available.
func TestClientChallengeTrailerFallsBackToHandBuilt(t *testing.T) {
	t.Parallel()

	captured := append([]byte{byte(TTCFuncOERR)}, bytes.Repeat([]byte{0x7a}, 152)...)

	tests := []struct {
		name string
		s    *session
		want []byte
	}{
		{
			name: "thin client ignores capture",
			s: &session{
				clientWideEncoding: false,
				upstreamAuthResp:   &upstreamAuthResponse{challengeTrailer: captured},
			},
			want: buildAuthChallengeEndMarker(VerifierType18453, false),
		},
		{
			name: "wide client without upstream response",
			s:    &session{clientWideEncoding: true},
			want: buildAuthChallengeEndMarker(VerifierType18453, true),
		},
		{
			name: "wide client with empty capture",
			s: &session{
				clientWideEncoding: true,
				upstreamAuthResp:   &upstreamAuthResponse{},
			},
			want: buildAuthChallengeEndMarker(VerifierType18453, true),
		},
		{
			name: "wide client with non-end-of-call capture",
			s: &session{
				clientWideEncoding: true,
				upstreamAuthResp:   &upstreamAuthResponse{challengeTrailer: []byte{0x08, 0x01, 0x02}},
			},
			want: buildAuthChallengeEndMarker(VerifierType18453, true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.s.clientChallengeTrailer(VerifierType18453)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("trailer: got=%x want=%x", got, tt.want)
			}
		})
	}
}

// TestParseAuthMessageStreamCapturesChallengeTrailer verifies that the parser
// captures the end-of-call summary trailing the 0x08 dictionary — the bytes
// clientChallengeTrailer later replays to the client. The capture is
// verifier-type agnostic (a 6949 challenge here).
func TestParseAuthMessageStreamCapturesChallengeTrailer(t *testing.T) {
	t.Parallel()

	encServerKey := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32)))
	salt := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x11}, 16)))
	csk := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))

	stream := buildSyntheticAuthPhase1Response(encServerKey, salt, csk, "4096", "3", VerifierType6949)
	wantTrailer := stream[len(stream)-syntheticAuthTailLen:]

	resp := &upstreamAuthResponse{properties: map[string]string{}}
	if !parseAuthMessageStream(stream, resp, false) {
		t.Fatalf("parseAuthMessageStream returned false on a complete stream")
	}

	if len(resp.challengeTrailer) == 0 || resp.challengeTrailer[0] != byte(TTCFuncOERR) {
		t.Fatalf("challengeTrailer not captured or missing end-of-call code: %x", resp.challengeTrailer)
	}

	if !bytes.Equal(resp.challengeTrailer, wantTrailer) {
		t.Fatalf("challengeTrailer: got=%x want=%x", resp.challengeTrailer, wantTrailer)
	}
}

// TestReadUpstreamAuthMessagesMergesMultiPacket exercises the motivating OCI
// scenario: the upstream splits an AUTH response across two TNS Data packets
// (real Oracle 23ai sends an OCI AUTH OK as 1967+557 bytes), preceded by the
// 23ai OOB break/reset probe. readUpstreamAuthMessages must answer the break
// with a reset marker, merge the fragments into one Data packet (so the
// AUTH_SVR_RESPONSE patcher sees a contiguous TTC stream), and record the
// per-fragment TTC lengths so reframeAuthOK can split it back.
func TestReadUpstreamAuthMessagesMergesMultiPacket(t *testing.T) {
	t.Parallel()

	proxyEnd, upstreamEnd := net.Pipe()

	defer func() { _ = proxyEnd.Close() }()
	defer func() { _ = upstreamEnd.Close() }()

	if err := proxyEnd.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if err := upstreamEnd.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	encServerKey := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32)))
	salt := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x11}, 16)))
	csk := strings.ToUpper(hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))

	stream := buildSyntheticAuthPhase1Response(encServerKey, salt, csk, "4096", "3", VerifierType18453)
	half := len(stream) / 2

	srvErr := make(chan error, 1)

	go func() {
		srvErr <- func() error {
			// 23ai OOB probe during AUTH: break marker then reset marker.
			if _, err := upstreamEnd.Write(encodeTNSPacket(TNSPacketTypeControl, []byte{0x01, 0x00, markerTypeBreak})); err != nil {
				return err //nolint:wrapcheck // test helper; clearer un-wrapped
			}

			if _, err := upstreamEnd.Write(encodeTNSPacket(TNSPacketTypeControl, []byte{0x01, 0x00, markerTypeReset})); err != nil {
				return err //nolint:wrapcheck // test helper; clearer un-wrapped
			}

			// dbbat (as the upstream's client) must answer with a reset marker.
			ack, err := readTNSPacket(upstreamEnd)
			if err != nil {
				return err //nolint:wrapcheck // test helper; clearer un-wrapped
			}

			if !isResetMarker(ack) {
				return errExpectedResetMarker
			}

			// AUTH response split across two Data packets, the summary
			// straddling nothing in particular — the split point is mid-dict.
			if err := writeAsTNSDataPacket(upstreamEnd, stream[:half]); err != nil {
				return err
			}

			return writeAsTNSDataPacket(upstreamEnd, stream[half:])
		}()
	}()

	s := &session{
		ctx:          context.Background(),
		logger:       slog.New(slog.DiscardHandler),
		upstreamConn: proxyEnd,
	}

	resp, merged, err := s.readUpstreamAuthMessages()
	if err != nil {
		t.Fatalf("readUpstreamAuthMessages: %v", err)
	}

	if err := <-srvErr; err != nil {
		t.Fatalf("scripted upstream: %v", err)
	}

	wantFlags := []byte{0x00, 0x00}
	if !bytes.Equal(resp.dataFlags, wantFlags) {
		t.Fatalf("dataFlags: got=%x want=%x", resp.dataFlags, wantFlags)
	}

	wantFrags := []int{half, len(stream) - half}
	if len(resp.fragTTCLens) != len(wantFrags) {
		t.Fatalf("fragTTCLens: got=%v want=%v", resp.fragTTCLens, wantFrags)
	}

	for i, n := range wantFrags {
		if resp.fragTTCLens[i] != n {
			t.Fatalf("fragTTCLens[%d]: got=%d want=%d", i, resp.fragTTCLens[i], n)
		}
	}

	wantMerged := encodeV315DataPacket(append(append([]byte{}, wantFlags...), stream...))
	if !bytes.Equal(merged, wantMerged) {
		t.Fatalf("merged packet mismatch:\ngot  %x\nwant %x", merged, wantMerged)
	}

	if resp.encServerSessKey != encServerKey {
		t.Fatalf("encServerSessKey: got=%q want=%q", resp.encServerSessKey, encServerKey)
	}

	// The end-of-call summary of the reassembled stream must be captured for
	// clientChallengeTrailer.
	if !bytes.Equal(resp.challengeTrailer, stream[len(stream)-syntheticAuthTailLen:]) {
		t.Fatalf("challengeTrailer not captured from merged stream: %x", resp.challengeTrailer)
	}
}

// TestReframeAuthOKSplitsAtOriginalBoundaries verifies that a merged (and
// patched) AUTH OK is re-fragmented into TNS Data packets at the upstream's
// original TTC boundaries — forwarding one merged packet exceeds the client's
// negotiated SDU and OCI rejects it with ORA-12592. Uses the fragment sizes
// observed from real Oracle 23ai (1967+557) and patches a 96-byte run
// straddling the boundary, like the AUTH_SVR_RESPONSE hex value does.
func TestReframeAuthOKSplitsAtOriginalBoundaries(t *testing.T) {
	t.Parallel()

	dataFlags := []byte{0x00, 0x00}
	fragLens := []int{1967, 557}

	ttc := make([]byte, fragLens[0]+fragLens[1])
	for i := range ttc {
		ttc[i] = byte(i % 251)
	}

	merged := encodeV315DataPacket(append(append([]byte{}, dataFlags...), ttc...))

	// Simulate the same-length AUTH_SVR_RESPONSE patch straddling the fragment
	// boundary: rewrite 96 bytes centered on it, in place.
	patchStart := tnsHeaderSize + ttcDataFlagsSize + fragLens[0] - 48
	for i := range 96 {
		merged[patchStart+i] = 0xEE
	}

	out := reframeAuthOK(merged, dataFlags, fragLens)

	if bytes.Equal(out, merged) {
		t.Fatalf("reframeAuthOK did not re-fragment a two-fragment AUTH OK")
	}

	var gotTTC []byte

	pos := 0

	for i, n := range fragLens {
		wantLen := tnsHeaderSize + ttcDataFlagsSize + n

		if len(out)-pos < wantLen {
			t.Fatalf("fragment %d truncated: %d bytes left, want %d", i, len(out)-pos, wantLen)
		}

		frag := out[pos : pos+wantLen]

		if got := int(binary.BigEndian.Uint32(frag[0:4])); got != wantLen {
			t.Fatalf("fragment %d header length = %d, want %d", i, got, wantLen)
		}

		if frag[4] != byte(TNSPacketTypeData) {
			t.Fatalf("fragment %d type = %#x, want Data (%#x)", i, frag[4], byte(TNSPacketTypeData))
		}

		if !bytes.Equal(frag[tnsHeaderSize:tnsHeaderSize+ttcDataFlagsSize], dataFlags) {
			t.Fatalf("fragment %d missing data-flags prefix: %x", i, frag[tnsHeaderSize:tnsHeaderSize+ttcDataFlagsSize])
		}

		gotTTC = append(gotTTC, frag[tnsHeaderSize+ttcDataFlagsSize:]...)
		pos += wantLen
	}

	if pos != len(out) {
		t.Fatalf("trailing bytes after last fragment: %d", len(out)-pos)
	}

	// The patched TTC content (including the bytes straddling the boundary)
	// must round-trip through re-fragmentation.
	if !bytes.Equal(gotTTC, merged[tnsHeaderSize+ttcDataFlagsSize:]) {
		t.Fatalf("TTC content did not round-trip through re-fragmentation")
	}
}

// TestReframeAuthOKPassthrough covers the guard branches: a single-packet
// (thin-client) AUTH OK, missing fragment metadata, inconsistent sizes, or a
// malformed data-flags prefix must all return the merged packet unchanged.
func TestReframeAuthOKPassthrough(t *testing.T) {
	t.Parallel()

	dataFlags := []byte{0x00, 0x00}
	ttc := bytes.Repeat([]byte{0x42}, 64)
	merged := encodeV315DataPacket(append(append([]byte{}, dataFlags...), ttc...))

	tests := []struct {
		name     string
		flags    []byte
		fragLens []int
	}{
		{name: "single fragment", flags: dataFlags, fragLens: []int{64}},
		{name: "no fragments", flags: dataFlags, fragLens: nil},
		{name: "sizes do not add up", flags: dataFlags, fragLens: []int{40, 40}},
		{name: "bad data flags width", flags: []byte{0x00}, fragLens: []int{32, 32}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if out := reframeAuthOK(merged, tt.flags, tt.fragLens); !bytes.Equal(out, merged) {
				t.Fatalf("reframeAuthOK must return the merged packet unchanged, got %x", out)
			}
		})
	}
}

var (
	errExpectedAuthPhase1  = errors.New("expected AUTH Phase 1")
	errExpectedResetMarker = errors.New("expected reset marker answer")
	errClientKeyLength     = errors.New("client session key length mismatch")
	errPasswordRoundTrip   = errors.New("password round-trip mismatch")
	errPhase2TooShort      = errors.New("phase 2 too short")
	errPhase2MissingKey    = errors.New("phase 2 missing AUTH_SESSKEY or AUTH_PASSWORD")
)

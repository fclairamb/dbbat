package oracle

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
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

	done, err := parseAuthMessageStream(stream, resp)
	if err != nil {
		t.Fatalf("parseAuthMessageStream: %v", err)
	}

	if !done {
		t.Fatalf("parseAuthMessageStream returned done=false on a complete stream")
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

	done, err := parseAuthMessageStream(stream[:half], resp)
	if err != nil {
		t.Fatalf("parseAuthMessageStream first half: %v", err)
	}

	if done {
		t.Fatalf("parseAuthMessageStream returned done=true on partial stream")
	}

	done, err = parseAuthMessageStream(stream, resp)
	if err != nil {
		t.Fatalf("parseAuthMessageStream full: %v", err)
	}

	if !done {
		t.Fatalf("parseAuthMessageStream did not finish on full stream")
	}
}

// TestPhase1ResponseSurfacesOracleError verifies that an end-of-call message
// carrying a non-zero retCode surfaces as resp.OracleErr.
func TestPhase1ResponseSurfacesOracleError(t *testing.T) {
	t.Parallel()

	stream := []byte{0x04}
	stream = append(stream, ttcCompressedUint(1017)...)
	stream = append(stream, []byte("ORA-01017 invalid username/password\x00")...)

	resp := &upstreamAuthResponse{properties: map[string]string{}}

	done, err := parseAuthMessageStream(stream, resp)
	if err != nil {
		t.Fatalf("parseAuthMessageStream: %v", err)
	}

	if !done {
		t.Fatalf("parseAuthMessageStream not done")
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

	defer clientPipe.Close()
	defer serverPipe.Close()

	username := "ADMIN"
	password := "ScriptedPassword!"

	scriptedSrv := &scriptedAuthServer{
		conn:     serverPipe,
		password: password,
	}

	go scriptedSrv.run()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	encryptionKey := bytes.Repeat([]byte{0x99}, 32)

	dbUID := uuid.New()
	aad := dbbcrypto.DatabaseAAD(dbUID.String())

	encPw, err := dbbcrypto.Encrypt([]byte(password), encryptionKey, aad)
	if err != nil {
		t.Fatalf("encrypt password: %v", err)
	}

	db := &store.Database{
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

	endStream := []byte{0x04}
	endStream = append(endStream, ttcCompressedUint(0)...)
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
	buf := []byte{0x08}

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

	dictLenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(dictLenBuf, uint16(len(pairs)))

	buf = append(buf, dictLenBuf...)

	for _, p := range pairs {
		buf = append(buf, ttcKeyVal(p.key, p.value, p.flag)...)
	}

	buf = append(buf, 0x04)
	buf = append(buf, ttcCompressedUint(0)...)
	buf = append(buf, bytes.Repeat([]byte{0x00}, 32)...)

	return buf
}

var (
	errExpectedAuthPhase1 = errors.New("expected AUTH Phase 1")
	errClientKeyLength    = errors.New("client session key length mismatch")
	errPasswordRoundTrip  = errors.New("password round-trip mismatch")
	errPhase2TooShort     = errors.New("phase 2 too short")
	errPhase2MissingKey   = errors.New("phase 2 missing AUTH_SESSKEY or AUTH_PASSWORD")
)

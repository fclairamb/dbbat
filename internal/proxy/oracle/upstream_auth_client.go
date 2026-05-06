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
func (s *session) runUpstreamClientAuth() error {
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

	phase1 := buildClientAuthPhase1(username, identity, mode)
	if err := s.writeUpstreamData(phase1); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 1: %w", err)
	}

	authResp, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 1 response: %w", err)
	}

	if authResp.OracleErr != 0 {
		return fmt.Errorf("upstream AUTH Phase 1 rejected: ORA-%05d %s", authResp.OracleErr, authResp.OracleErrText)
	}

	sec, err := buildSecretsFromPhase1Response(authResp, s.upstreamCustomHash)
	if err != nil {
		return fmt.Errorf("interpret AUTH Phase 1 response: %w", err)
	}

	if err := computeUpstreamAuthSecrets(password, authResp.encServerSessKey, sec); err != nil {
		return fmt.Errorf("derive upstream auth secrets: %w", err)
	}

	phase2 := buildClientAuthPhase2(username, identity, sec, mode|logonModeUserAndPass)
	if err := s.writeUpstreamData(phase2); err != nil {
		return fmt.Errorf("write upstream AUTH Phase 2: %w", err)
	}

	finalResp, err := s.readUpstreamAuthMessages()
	if err != nil {
		return fmt.Errorf("read upstream AUTH Phase 2 response: %w", err)
	}

	if finalResp.OracleErr != 0 {
		return fmt.Errorf("upstream AUTH Phase 2 rejected: ORA-%05d %s", finalResp.OracleErr, finalResp.OracleErrText)
	}

	s.logger.InfoContext(s.ctx, "upstream Oracle AUTH complete on relay-phase socket",
		slog.String("user", s.database.Username),
		slog.Int("verifier_type", sec.verifierType),
		slog.Bool("custom_hash", sec.customHash))

	return nil
}

// writeUpstreamData wraps a TTC body in a v315+ TNS Data packet with a
// zero data-flag prefix and writes it to the upstream socket.
func (s *session) writeUpstreamData(body []byte) error {
	payload := make([]byte, 0, ttcDataFlagsSize+len(body))
	payload = append(payload, 0x00, 0x00) // data flags
	payload = append(payload, body...)

	pkt := encodeV315DataPacket(payload)

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
func (s *session) readUpstreamAuthMessages() (*upstreamAuthResponse, error) {
	resp := &upstreamAuthResponse{properties: make(map[string]string)}

	var buf []byte

	for {
		pkt, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			return nil, fmt.Errorf("read upstream packet: %w", err)
		}

		if pkt.Type != TNSPacketTypeData {
			s.logger.DebugContext(s.ctx, "upstream AUTH: skipping non-Data packet",
				slog.String("type", pkt.Type.String()),
				slog.Int("len", len(pkt.Raw)))

			continue
		}

		if len(pkt.Payload) < ttcDataFlagsSize {
			continue
		}

		buf = append(buf, pkt.Payload[ttcDataFlagsSize:]...)

		done, err := parseAuthMessageStream(buf, resp)
		if err != nil {
			return nil, err
		}

		if done {
			return resp, nil
		}
	}
}

// parseAuthMessageStream walks a TTC byte stream and returns done=true when
// an end-of-call message code (4 or 9) is observed.
//
// Codes:
//
//	0x08              dictionary (KV pairs)
//	0x04 / 0x09       end-of-call (with a Summary structure)
//	0x0F              warning (skipped)
//
// The function is tolerant: when it cannot decode a region it returns
// done=false so the caller reads more bytes.
func parseAuthMessageStream(buf []byte, resp *upstreamAuthResponse) (bool, error) {
	pos := 0

	for pos < len(buf) {
		msgCode := buf[pos]
		pos++

		switch msgCode {
		case 0x08:
			n, ok := parseAuthKVDictionary(buf[pos:], resp)
			if !ok {
				return false, nil
			}

			pos += n

		case 0x04, 0x09:
			parseAuthSummary(buf[pos:], resp)

			return true, nil

		case 0x0F:
			if pos+6 > len(buf) {
				return false, nil
			}

			pos += 6

		default:
			return false, nil
		}
	}

	return false, nil
}

// parseAuthKVDictionary parses a TTC AUTH KV dictionary that follows a 0x08
// message code. Returns ok=false when the dictionary is truncated.
func parseAuthKVDictionary(buf []byte, resp *upstreamAuthResponse) (int, bool) {
	const dictLenSize = 2

	if len(buf) < dictLenSize {
		return 0, false
	}

	dictLen := int(binary.BigEndian.Uint16(buf[:dictLenSize]))
	pos := dictLenSize

	for i := 0; i < dictLen; i++ {
		key, value, valueFlag, consumed, ok := readAuthKVPair(buf[pos:])
		if !ok {
			return 0, false
		}

		pos += consumed
		recordAuthProperty(string(key), string(value), valueFlag, resp)
	}

	return pos, true
}

// readAuthKVPair reads keyLen + keyCLR + valueLen + valueCLR + flag, mirroring
// session.GetKeyVal in go-ora.
func readAuthKVPair(buf []byte) (key, value []byte, flag, consumed int, ok bool) {
	pos := 0

	keyLen, n := readCompressedInt(buf[pos:])
	if n == 0 {
		return nil, nil, 0, 0, false
	}

	pos += n

	if keyLen > 0 {
		k, kn := readCLR(buf[pos:])
		if kn == 0 {
			return nil, nil, 0, 0, false
		}

		key = k
		pos += kn
	}

	vLen, vn := readCompressedInt(buf[pos:])
	if vn == 0 {
		return nil, nil, 0, 0, false
	}

	pos += vn

	if vLen > 0 {
		v, vClrN := readCLR(buf[pos:])
		if vClrN == 0 {
			return nil, nil, 0, 0, false
		}

		value = v
		pos += vClrN
	}

	flagVal, fn := readCompressedInt(buf[pos:])
	if fn == 0 {
		return nil, nil, 0, 0, false
	}

	pos += fn
	flag = flagVal
	consumed = pos
	ok = true

	return
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
func parseAuthSummary(buf []byte, resp *upstreamAuthResponse) {
	const minSummaryLen = 6

	if len(buf) < minSummaryLen {
		return
	}

	retCode, _ := readCompressedInt(buf)
	if retCode != 0 {
		resp.OracleErr = retCode
	}

	if idx := indexOf(buf, []byte("ORA-")); idx >= 0 {
		end := idx
		for end < len(buf) && buf[end] != 0x00 && buf[end] != 0x0A {
			end++
		}

		resp.OracleErrText = string(buf[idx:end])
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
	ErrUpstreamConnNotSet        = errors.New("upstream connection not set before AUTH")
	ErrPhase1MissingSessKey      = errors.New("AUTH Phase 1 response: missing AUTH_SESSKEY")
	ErrPhase1MissingVerifierData = errors.New("AUTH Phase 1 response: missing AUTH_VFR_DATA")
	ErrInvalidHex                = errors.New("invalid hex character")
	ErrInvalidHexLength          = errors.New("invalid hex string length")
)

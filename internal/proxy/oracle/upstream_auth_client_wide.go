package oracle

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"
)

// The synthetic AUTH builders in upstream_auth_client.go emit the compressed
// (thin) encoding every Go/Java/Python driver speaks. An OCI client — sqlplus,
// the Instant Client — negotiates a different dialect during the pre-auth
// relay, and the upstream parses AUTH at *those* caps, so a thin body handed to
// an OCI-conditioned upstream is unreadable.
//
// That premise is measured, not assumed: with the wide dispatch below disabled
// so a thin body goes out on an OCI session, a real sqlplus login through the
// proxy dies with ORA-03113 (end-of-file on communication channel) and never
// authenticates — the upstream stops answering rather than reporting anything.
// TestIntegration_SqlplusLoginThroughSyntheticAuth is the same login with the
// dispatch on, and it passes. (The thin *preamble* bug this file's thin
// counterpart once had presented differently, as two break markers and
// ORA-03120; a wholly wrong dialect does not get that far.)
//
// Everything below is the wide (OCI) counterpart. It was measured against
// testdata/sqlplus_cursor_reexec.pcapng (macOS Instant Client 23.3 → Oracle
// 23ai Free), cross-checked against the DB-bundled 23.26 client fixtures in
// oci_instantclient_test.go. Five things differ from the thin body:
//
//  1. TNS data flags are 0x2000, not 0x0000 (both captured AUTH packets).
//  2. The preamble is pointer runs and 4-byte little-endian fields, not
//     compressed integers — see wideAuthFraming.
//  3. Every key/value length is a 4-byte little-endian field, and on the
//     Instant Client it is a UTF-8 max-expansion BUFFER size (3x the CLR byte
//     length), not the plain length. The DB-bundled client sends plain lengths.
//     Mixing the two conventions draws ORA-28041 — the same trap
//     replaceAuthKVValueWide documents for the rewrite path.
//  4. An empty value is 4 zero length bytes followed straight by the flag —
//     there is NO CLR byte at all (captured: AUTH_TERMINAL in Phase 1 is
//     `27 00 00 00 0d "AUTH_TERMINAL" 00000000 00000000`). ttcKeyValWide would
//     write a 0x00 CLR there that readAuthKVPairWide never consumes.
//  5. The logon mode carries client-specific high bits (0x40000000 in the
//     captured Phase 2), so it is copied from the client rather than invented.
//
// The username field, by contrast, is the same CLR form in both dialects.
//
// Which of the five the upstream actually enforces was measured the same way,
// by breaking one at a time and re-running that login against Oracle 23ai Free.
// Only the structure (2) is enforced — it is what separates a readable body
// from ORA-03113. Oracle 23ai accepted the login with thin data flags (1), and
// accepted plain length fields in place of the 3x buffer sizes (3) as long as
// every field agreed: the ORA-28041 trap is MIXING the two conventions inside
// one body, which is the rewrite path's hazard (see replaceAuthKVValueWide),
// not picking the wrong flavor wholesale. (4) is unreachable unless a value is
// actually empty, which needs a host with no name. They are reproduced anyway,
// because "this server tolerates it" is a weaker contract than "this is what
// the client we are impersonating sends", and the tolerance is unlikely to be
// uniform across the Oracle versions and OCI builds dbbat fronts.

// wideAuthPointerRun is the 8-byte placeholder OCI writes where a marshaled
// pointer would go. Four of them frame the Phase 1/Phase 2 preamble, and the
// first is the anchor every field offset below is measured from — exactly as
// findUserIDLenPos anchors the rewrite path.
var wideAuthPointerRun = []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// wideAuthDataFlags is the TTC data-flags prefix an OCI client puts on its AUTH
// packets (captured 0x2000 on both Phase 1 and Phase 2; thin clients send
// 0x0000). The rewrite path forwards the client's own flags, so the synthetic
// path has to reproduce them or the two paths disagree on the same session.
var wideAuthDataFlags = []byte{0x20, 0x00}

// maxWideAuthLead bounds how far past the [03 <sub> <seq>] opener the first
// pointer run may sit before the body stops looking like an OCI preamble. The
// Instant Client puts 2 bytes there and the DB-bundled client 8; 16 is slack.
const maxWideAuthLead = 16

// wideAuthFraming is the set of OCI framing conventions dbbat cannot derive
// from first principles and therefore reads off the client's own AUTH packet.
// Every *value* in the synthetic body is dbbat's own; only the framing is
// borrowed, which is the same division of labor the rewrite path makes when it
// preserves the client's wire shape and swaps the fields it owns.
type wideAuthFraming struct {
	// prefix is everything from the TTC function header up to (not including)
	// the first pointer run: `03 <sub> <seq>` plus the client's lead bytes
	// (`01 03` on the Instant Client's Phase 1, `01 04` on its Phase 2,
	// `02 00 00 00 00 00 00 00` on the DB-bundled client). It is copied
	// verbatim because nothing in it is a length or a count — the fields dbbat
	// must recompute all sit after the anchor.
	prefix []byte

	// mode is the 4-byte logon mode the client declared.
	mode uint32

	// bufferSized selects the 3x UTF-8 max-expansion convention for every
	// 4-byte length field (Instant Client) over plain byte lengths (DB-bundled).
	bufferSized bool

	// pairCountPad records the extra zero dword the DB-bundled client writes
	// after the KV pair count.
	pairCountPad bool
}

// defaultWideAuthFraming is the shape assumed when a client packet is present
// but its preamble does not parse: the Instant Client's, which is the flavor in
// testdata and the more widely deployed of the two. sub/seq are the caller's
// (the same values syntheticAuthHeader would stamp) and the lead byte pair is
// `01 <sub-ordinal>` — 3 for Phase 1 and 4 for Phase 2, as captured.
func defaultWideAuthFraming(sub, seq byte, mode uint32) wideAuthFraming {
	lead := byte(0x03)
	if sub == PiggybackSubAuth2 {
		lead = 0x04
	}

	return wideAuthFraming{
		prefix:      []byte{byte(TTCFuncPiggyback), sub, seq, 0x01, lead},
		mode:        mode,
		bufferSized: true,
	}
}

// detectWideAuthFraming reads the framing conventions above off a client's own
// wide AUTH body (the TTC bytes, i.e. payload minus the 2 data-flag bytes).
// ok=false means the body is not a wide AUTH preamble this reader recognizes;
// the caller falls back to defaultWideAuthFraming.
func detectWideAuthFraming(body []byte) (wideAuthFraming, bool) {
	if len(body) < 3 || body[0] != byte(TTCFuncPiggyback) {
		return wideAuthFraming{}, false
	}

	anchor := bytes.Index(body, wideAuthPointerRun)
	if anchor < 3 || anchor > 3+maxWideAuthLead {
		return wideAuthFraming{}, false
	}

	// [ptr:8][userLen:4][mode:4][ptr:8][pairCount:4] — the pad dword and the
	// two trailing pointer runs are probed below, so only this much is required.
	const framedFieldsLen = 8 + 4 + 4 + 8 + 4

	if len(body) < anchor+framedFieldsLen+4 {
		return wideAuthFraming{}, false
	}

	f := wideAuthFraming{
		prefix: append([]byte(nil), body[:anchor]...),
		mode:   binary.LittleEndian.Uint32(body[anchor+12 : anchor+16]),
		// Undecidable below (no locatable username) keeps the Instant Client
		// convention, so a detected framing and defaultWideAuthFraming answer
		// the same thing rather than disagreeing on the same client.
		bufferSized: true,
	}

	// The 3x-vs-plain convention is decided by comparing the user-len field
	// against the username it describes — the one length in the body whose true
	// value dbbat can establish independently (locateAnchoredUsername anchors on
	// the AUTH_* keys, not on any length field).
	userLenField := binary.LittleEndian.Uint32(body[anchor+8 : anchor+12])

	if start, end, ok := locateAnchoredUsername(body); ok && end > start {
		f.bufferSized = userLenField == uint32(3*(end-start))
	}

	padPos := anchor + framedFieldsLen
	if !bytes.HasPrefix(body[padPos:], wideAuthPointerRun) &&
		binary.LittleEndian.Uint32(body[padPos:padPos+4]) == 0 {
		f.pairCountPad = true
	}

	return f, true
}

// wideAuthFramingFor returns the framing to build a synthetic wide AUTH body of
// one phase with, preferring the client's own packet for that same phase and
// falling back to the Instant Client shape. The caller's mode bits are OR-ed
// into the client's: a Phase 2 built from a Phase 1-derived framing would
// otherwise lack logonModeUserAndPass and offer the upstream no password.
func (s *session) wideAuthFramingFor(pkt *TNSPacket, sub, seq byte, mode uint32) wideAuthFraming {
	f := defaultWideAuthFraming(sub, seq, mode)

	if pkt != nil && len(pkt.Payload) > ttcDataFlagsSize {
		if detected, ok := detectWideAuthFraming(pkt.Payload[ttcDataFlagsSize:]); ok {
			f = detected
			f.prefix[1] = sub
		}
	}

	f.mode |= mode

	return f
}

// wideAuthSizeField renders a 4-byte length field in the client's convention:
// the plain byte length, or the 3x UTF-8 max-expansion buffer size.
func wideAuthSizeField(n int, bufferSized bool) uint32 {
	if bufferSized {
		return uint32(3 * n)
	}

	return uint32(n)
}

// ttcKeyValWideSized writes one wide KV pair —
// [keyLen:4 LE][CLR key][valLen:4 LE][CLR val][flag:4 LE] — in the client's
// length convention. An empty value is written as a zero length field with no
// CLR byte, which is what OCI puts on the wire and what readAuthKVPairWide
// expects; ttcKeyValWide's unconditional CLR is a byte the reader would then
// mistake for the flag.
func ttcKeyValWideSized(key, value string, flag int, bufferSized bool) []byte {
	keyBytes := []byte(key)
	valBytes := []byte(value)

	buf := make([]byte, 0, len(keyBytes)+len(valBytes)+20)
	buf = binary.LittleEndian.AppendUint32(buf, wideAuthSizeField(len(keyBytes), bufferSized))
	buf = append(buf, ttcClr(keyBytes)...)
	buf = binary.LittleEndian.AppendUint32(buf, wideAuthSizeField(len(valBytes), bufferSized))

	if len(valBytes) > 0 {
		buf = append(buf, ttcClr(valBytes)...)
	}

	buf = binary.LittleEndian.AppendUint32(buf, uint32(flag))

	return buf
}

// buildWideAuthBody assembles a wide AUTH body from the framing, the username
// and the KV pairs. The layout is the one measured in
// sqlplus_cursor_reexec.pcapng, identical for Phase 1 and Phase 2:
//
//	<prefix> <ptr> <userLen:4> <mode:4> <ptr> <pairCount:4> [pad:4] <ptr> <ptr>
//	<CLR username> <KV pairs>
func buildWideAuthBody(f wideAuthFraming, username string, pairs []authKV) []byte {
	buf := make([]byte, 0, 256)
	buf = append(buf, f.prefix...)
	buf = append(buf, wideAuthPointerRun...)
	buf = binary.LittleEndian.AppendUint32(buf, wideAuthSizeField(len(username), f.bufferSized))
	buf = binary.LittleEndian.AppendUint32(buf, f.mode)
	buf = append(buf, wideAuthPointerRun...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(pairs)))

	if f.pairCountPad {
		buf = binary.LittleEndian.AppendUint32(buf, 0)
	}

	buf = append(buf, wideAuthPointerRun...)
	buf = append(buf, wideAuthPointerRun...)
	buf = append(buf, ttcClr([]byte(username))...)

	for _, p := range pairs {
		buf = append(buf, ttcKeyValWideSized(p.key, p.value, p.flag, f.bufferSized)...)
	}

	return buf
}

// authKV is one AUTH dictionary entry, shared by the thin and wide builders so
// the two dialects cannot drift apart on which keys dbbat sends.
type authKV struct {
	key, value string
	flag       int
}

// authPhase1Pairs is the Phase 1 dictionary dbbat sends upstream: the same five
// informational keys go-ora sends and the same five sqlplus sends (captured), so
// the two dialects differ only in encoding.
func authPhase1Pairs(ident driverIdentity) []authKV {
	return []authKV{
		{"AUTH_TERMINAL", ident.HostName, 0},
		{authKeyProgramNM, ident.ProgramName, 0},
		{"AUTH_MACHINE", ident.HostName, 0},
		{"AUTH_PID", strconv.Itoa(ident.PID), 0},
		{"AUTH_SID", ident.OSUser, 0},
	}
}

// authPhase2Pairs is the Phase 2 dictionary: the credentials derived against the
// upstream challenge, then the same informational and session-setup keys.
//
// wide adds the four keys an OCI client sends that no thin client does
// (AUTH_RTT, AUTH_CLNT_MEM, AUTH_ACL, AUTH_FLAGS) and declares
// SESSION_CLIENT_LIB_TYPE=2 (OCI) rather than 0 (thin), matching the captured
// sqlplus login — the upstream conditions its AUTH OK on this dictionary, and
// that AUTH OK is forwarded to a real OCI client.
//
// Three keys the captured sqlplus sends are deliberately NOT reproduced:
// AUTH_CONNECT_STRING, AUTH_LOGICAL_SESSION_ID and AUTH_FAILOVER_ID. All three
// are informational client state, and the last two would have to be invented —
// the synthetic body declares what dbbat is, not a fabricated OCI identity. The
// thin dictionary omits them too and authenticates, so they are not what the
// upstream gates AUTH OK on. For the same reason SESSION_CLIENT_CHARSET and
// SESSION_CLIENT_VERSION keep the thin path's values rather than sqlplus's:
// they are dbbat's own declaration in both dialects.
func authPhase2Pairs(ident driverIdentity, sec *upstreamAuthSecrets, wide bool) []authKV {
	pairs := []authKV{
		{authKeySessKey, sec.encClientSessKey, 1},
		{authKeyPassword, sec.encPassword, 0},
	}

	if sec.eSpeedyKey != "" {
		pairs = append(pairs, authKV{"AUTH_PBKDF2_SPEEDY_KEY", sec.eSpeedyKey, 0})
	}

	if wide {
		pairs = append(pairs,
			authKV{"AUTH_RTT", "21103", 0},
			authKV{"AUTH_CLNT_MEM", "4096", 0},
		)
	}

	libType := "0"
	if wide {
		libType = "2"
	}

	pairs = append(pairs,
		authKV{"AUTH_TERMINAL", ident.HostName, 0},
		authKV{authKeyProgramNM, ident.ProgramName, 0},
		authKV{"AUTH_MACHINE", ident.HostName, 0},
		authKV{"AUTH_PID", strconv.Itoa(ident.PID), 0},
		authKV{"AUTH_SID", ident.OSUser, 0},
		authKV{"SESSION_CLIENT_CHARSET", "871", 0},
		authKV{"SESSION_CLIENT_LIB_TYPE", libType, 0},
		authKV{"SESSION_CLIENT_DRIVER_NAME", ident.DriverName, 0},
		authKV{"SESSION_CLIENT_VERSION", "2.0.0.0", 0},
		authKV{"SESSION_CLIENT_LOBATTR", "1", 0},
	)

	if wide {
		pairs = append(pairs,
			authKV{"AUTH_ACL", "8800", 0},
			authKV{"AUTH_FLAGS", "1", 0},
		)
	}

	return append(pairs, authKV{"AUTH_ALTER_SESSION", fmt.Sprintf(
		"ALTER SESSION SET NLS_LANGUAGE='AMERICAN' NLS_TERRITORY='AMERICA'  TIME_ZONE='%s'\x00",
		localTimezoneString(time.Now())), 1})
}

// buildClientAuthPhase1Wide is the OCI counterpart of buildClientAuthPhase1.
func buildClientAuthPhase1Wide(f wideAuthFraming, username string, ident driverIdentity) []byte {
	return buildWideAuthBody(f, username, authPhase1Pairs(ident))
}

// buildClientAuthPhase2Wide is the OCI counterpart of buildClientAuthPhase2.
func buildClientAuthPhase2Wide(f wideAuthFraming, username string, ident driverIdentity, sec *upstreamAuthSecrets) []byte {
	return buildWideAuthBody(f, username, authPhase2Pairs(ident, sec, true))
}

package oracle

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestRewriteAuthPhase2_BasicSwap_CLR confirms the three load-bearing AUTH_*
// values are replaced with upstream-derived ones and other KV pairs pass
// through verbatim. CLR-prefixed username (go-ora-style).
func TestRewriteAuthPhase2_BasicSwap_CLR(t *testing.T) {
	t.Parallel()

	pairs := []phase2KVForTest{
		{"AUTH_PASSWORD", "OLDPWD_HEX", 0},
		{"AUTH_PBKDF2_SPEEDY_KEY", "OLDSPEEDY_HEX", 0},
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_TERMINAL", "unknown", 0},
		{"AUTH_PROGRAM_NM", "SourceLauncher", 0},
		{"AUTH_CONNECT_STRING", "(DESCRIPTION=...)", 0},
		{"AUTH_COPYRIGHT", "Oracle blah", 0},
		{"AUTH_ACL", "4400", 0},
	}
	body := buildPhase2Body(t, "CONNECTOR", pairs, true /*CLR*/, 0x00 /*b0*/)

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
		eSpeedyKey:       "NEWSPEEDY_HEX",
	}

	out, err := rewriteAuthPhase2(body, "LABEOMNGR_DEV", sec, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	expectedPairs := []phase2KVForTest{
		{"AUTH_PASSWORD", "NEWPWD_HEX", 0},
		{"AUTH_PBKDF2_SPEEDY_KEY", "NEWSPEEDY_HEX", 0},
		{"AUTH_SESSKEY", "NEWSESS_HEX", 1},
		{"AUTH_TERMINAL", "unknown", 0},
		{"AUTH_PROGRAM_NM", "SourceLauncher", 0},
		{"AUTH_CONNECT_STRING", "(DESCRIPTION=...)", 0},
		{"AUTH_COPYRIGHT", "Oracle blah", 0},
		{"AUTH_ACL", "4400", 0},
	}
	expected := buildPhase2Body(t, "LABEOMNGR_DEV", expectedPairs, true, 0x00)

	if !bytes.Equal(out, expected) {
		t.Fatalf("phase 2 rewrite mismatch:\n got %x\nwant %x", out, expected)
	}
}

// TestRewriteAuthPhase2_BasicSwap_Bare covers the JDBC-thin wire shape: bare
// username (no CLR length prefix) and a 0x02 byte at offset 2 of the body.
// The b0 byte must be preserved verbatim.
func TestRewriteAuthPhase2_BasicSwap_Bare(t *testing.T) {
	t.Parallel()

	pairs := []phase2KVForTest{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_PASSWORD", "OLDPWD_HEX", 0},
	}
	body := buildPhase2Body(t, "CONNECTOR", pairs, false /*bare*/, 0x02 /*b0*/)

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
	}

	out, err := rewriteAuthPhase2(body, "LABEOMNGR_DEV", sec, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	expectedPairs := []phase2KVForTest{
		{"AUTH_SESSKEY", "NEWSESS_HEX", 1},
		{"AUTH_PASSWORD", "NEWPWD_HEX", 0},
	}
	expected := buildPhase2Body(t, "LABEOMNGR_DEV", expectedPairs, false, 0x02)

	if !bytes.Equal(out, expected) {
		t.Fatalf("phase 2 bare-rewrite mismatch:\n got %x\nwant %x", out, expected)
	}
}

// TestRewriteAuthPhase2_NoSpeedyKey confirms the SPEEDY_KEY pair is left
// verbatim when the upstream secrets don't have one (verifier 6949 path).
func TestRewriteAuthPhase2_NoSpeedyKey(t *testing.T) {
	t.Parallel()

	pairs := []phase2KVForTest{
		{"AUTH_PASSWORD", "OLDPWD_HEX", 0},
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_TERMINAL", "unknown", 0},
	}
	body := buildPhase2Body(t, "CONNECTOR", pairs, true, 0x00)

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
		eSpeedyKey:       "", // verifier 6949
	}

	out, err := rewriteAuthPhase2(body, "USR2", sec, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	expectedPairs := []phase2KVForTest{
		{"AUTH_PASSWORD", "NEWPWD_HEX", 0},
		{"AUTH_SESSKEY", "NEWSESS_HEX", 1},
		{"AUTH_TERMINAL", "unknown", 0},
	}
	expected := buildPhase2Body(t, "USR2", expectedPairs, true, 0x00)

	if !bytes.Equal(out, expected) {
		t.Fatalf("phase 2 rewrite mismatch:\n got %x\nwant %x", out, expected)
	}
}

// TestRewriteAuthPhase2_RealJDBC verifies the parser handles the actual
// SQLcl/JDBC thin Phase 2 captured from a real upstream session.
//
// Captured body (after 2-byte data flags): 1187 bytes from the wire trace at
// /tmp/sniff_p2.log line 16. Username is "LABEOMNGR_DEV" and we substitute
// "NEWUSER_X" plus all three AUTH_* values.
func TestRewriteAuthPhase2_RealJDBC(t *testing.T) {
	t.Parallel()

	// Real JDBC Phase 2 body from sniff_p2.log line 16 (after data flags).
	bodyHex := "03730201010d02010101010e01014c4142454f4d4e47525f444556" +
		"010d0d415554485f50415353574f524401606032374244333444344237393842353531313737414644413743323132434145444330393636364144413232443130333037453631373039343832333842383032443631464546463837393544314235303945423032423938453444453134384600" +
		"011616415554485f50424b4446325f5350454544595f4b455901a0a03630383130414632463044334632424344443632333731423836413744304434453236343541413335324138383736304239424339394230303245454632394645373338383144343846333935373830314435423146373243314641374535424233363836433733453342393242424444394334353338414332304538333441424245414434324337413331423132364446434445414537393636393337373500" +
		"010c0c415554485f534553534b45590140403943384245353736363937303038383539434431373130443935433537454233304333343641434145313641374433363342383937363535393131323838414601" +
		"01010d0d415554485f5445524d494e414c010707756e6b6e6f776e00" +
		"010f0f415554485f50524f4752414d5f4e4d010e0e536f757263654c61756e6368657200" +
		"010c0c415554485f4d414348494e45011a1a466c6f72656e74732d4d6163426f6f6b2d4169722e6c6f63616c00" +
		"010808415554485f5049440104043132333400" +
		"011212415554485f414c5445525f53455353494f4e015c5c414c5445522053455353494f4e205345542054494d455f5a4f4e453d274575726f70652f506172697327204e4c535f4c414e47554147453d27414d45524943414e2720204e4c535f5445525249544f52593d27414d455249434127000101" +
		"011a1a53455353494f4e5f434c49454e545f4452495645525f4e414d450117176a6462637468696e203a2032332e372e302e32352e303100" +
		"01161653455353494f4e5f434c49454e545f56455253494f4e0109093338363333353132310001161653455353494f4e5f434c49454e545f4c4f4241545452010101310001131" +
		"3415554485f434f4e4e4543545f535452494e47016565284445534352495054494f4e3d28414444524553533d2850524f544f434f4c3d5443502928484f53543d6c6f63616c686f73742928504f52543d31353330292928434f4e4e4543545f444154413d28534552564943455f4e414d453d54455354303129292900" +
		"010e0e415554485f434f5059524947485401f1f1224f7261636c650a4576657279626f647920666f6c6c6f77730a53706565647920626974732065786368616e67650a537461727320617761697420746f20676c6f77220a54686520707265636564696e67206b657920697320636f707972696768746564206279204f7261636c6520436f72706f726174696f6e2e0a4475706c69636174696f6e206f662074686973206b6579206973206e6f7420616c6c6f77656420776974686f7574207065726d697373696f6e0a66726f6d204f7261636c6520436f72706f726174696f6e2e20436f707972696768742032303033204f7261636c6520436f72706f726174696f6e2e00" +
		"010808415554485f41434c0104043434303000"

	body, err := hex.DecodeString(strings.ReplaceAll(bodyHex, " ", ""))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEW_SESSION_KEY_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		encPassword:      "NEW_PASSWORD_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		eSpeedyKey:       "NEW_SPEEDY_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
	}

	out, err := rewriteAuthPhase2(body, "NEWUSER_X", sec, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// Output must be a valid Phase 2 body that round-trips through the
	// existing parseAuthPhase2 parser.
	withFlags := append([]byte{0x00, 0x00}, out...)

	gotSess, gotPwd, err := parseAuthPhase2(withFlags, false)
	if err != nil {
		t.Fatalf("parseAuthPhase2 on rewritten body: %v", err)
	}

	if gotSess != sec.encClientSessKey {
		t.Errorf("AUTH_SESSKEY: got %q want %q", gotSess, sec.encClientSessKey)
	}

	if gotPwd != sec.encPassword {
		t.Errorf("AUTH_PASSWORD: got %q want %q", gotPwd, sec.encPassword)
	}

	// Verify untouched KV pairs survived: AUTH_CONNECT_STRING / AUTH_ACL /
	// AUTH_COPYRIGHT must still be present.
	if !bytes.Contains(out, []byte("AUTH_CONNECT_STRING")) {
		t.Error("AUTH_CONNECT_STRING missing from rewritten body")
	}

	if !bytes.Contains(out, []byte("AUTH_COPYRIGHT")) {
		t.Error("AUTH_COPYRIGHT missing from rewritten body")
	}

	if !bytes.Contains(out, []byte("AUTH_ACL")) {
		t.Error("AUTH_ACL missing from rewritten body")
	}

	if !bytes.Contains(out, []byte("NEWUSER_X")) {
		t.Error("substituted username not in rewritten body")
	}

	if bytes.Contains(out, []byte("LABEOMNGR_DEV")) {
		t.Error("original username still present in rewritten body")
	}
}

// TestParseAuthPhase2Header_ExtendedFuncHeader covers the 4-byte function
// header go-ora v3 and JDBC thin open Phase 2 with (`03 73 02 00`, see
// testdata/go_ora_cursor_reexec.pcapng). The parser used to assume a fixed
// 3-byte header, so it read that extension byte as the has-username flag and
// walked the rest of the preamble one byte early; the header must also survive
// reassembly intact, or the upstream reads the rewritten body at the wrong
// offset.
func TestParseAuthPhase2Header_ExtendedFuncHeader(t *testing.T) {
	t.Parallel()

	pairs := []phase2KVForTest{
		{"AUTH_SESSKEY", "DEADBEEF", 1},
		{"AUTH_PASSWORD", "CAFEBABE", 0},
	}

	narrow := buildPhase2Body(t, "CONNECTOR", pairs, true, 0x02)

	// Same body, extended header: splice the trailing 0x00 in after the
	// sequence number.
	extended := append([]byte{}, narrow[:3]...)
	extended = append(extended, 0x00)
	extended = append(extended, narrow[3:]...)

	hdr, err := parseAuthPhase2Header(extended)
	if err != nil {
		t.Fatalf("parse extended header: %v", err)
	}

	if hdr.headerLen != 4 {
		t.Fatalf("headerLen = %d, want 4", hdr.headerLen)
	}

	if !hdr.hasUsername || !hdr.hasCLRPrefix {
		t.Fatalf("hasUsername=%v hasCLRPrefix=%v, want both true", hdr.hasUsername, hdr.hasCLRPrefix)
	}

	if hdr.pairCount != len(pairs) {
		t.Fatalf("pairCount = %d, want %d", hdr.pairCount, len(pairs))
	}

	out := assembleAuthPhase2(extended, hdr, "CONNECTOR", extended[hdr.usernameEnd:])
	if !bytes.Equal(out, extended) {
		t.Fatalf("round-trip changed the body:\n got %x\nwant %x", out, extended)
	}
}

// TestRewriteAuthPhase2_Errors covers obviously malformed bodies.
func TestRewriteAuthPhase2_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"truncated header", []byte{0x03, 0x73}},
		{"wrong sub-op", []byte{0x03, 0x76, 0x00, 0x01, 0x01, 0x09}},
	}

	sec := &upstreamAuthSecrets{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := rewriteAuthPhase2(tc.body, "X", sec, false); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// phase2KVForTest is a test helper for building Phase 2 bodies.
type phase2KVForTest struct {
	key, value string
	flag       int
}

// buildPhase2Body returns a TTC AUTH Phase 2 body. When clrUsername is true
// the username is CLR-prefixed (go-ora-style); otherwise it's bare (JDBC
// thin-style). b0 is the byte at offset 2 (0x00 for go-ora, 0x02 for JDBC).
// Mode is hard-coded to 0x101 (UserAndPass | NoNewPass) — the only value
// observed in real Phase 2 traffic.
func buildPhase2Body(t *testing.T, username string, pairs []phase2KVForTest, clrUsername bool, b0 byte) []byte {
	t.Helper()

	const mode uint32 = 0x101

	usernameBytes := []byte(username)

	buf := []byte{byte(TTCFuncPiggyback), PiggybackSubAuth2, b0}

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
		if clrUsername {
			buf = append(buf, byte(len(usernameBytes)))
		}

		buf = append(buf, usernameBytes...)
	}

	for _, p := range pairs {
		buf = append(buf, ttcKeyVal(p.key, p.value, p.flag)...)
	}

	return buf
}

// --- AUTH_ALTER_SESSION gate ------------------------------------------------

// logonGrants is every grant shape the gate has to answer for: a full-write
// grant, each control on its own, and a grant with no definition (which reports
// every control).
func logonGrants() map[string]*store.Grant {
	return map[string]*store.Grant{
		"full write":    {Definition: &store.GrantDefinition{}},
		"read_only":     {Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}},
		"block_ddl":     {Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}},
		"block_copy":    {Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockCopy}}},
		"no definition": {},
	}
}

// phase2WithAlterSession is a thin Phase 2 body carrying one AUTH_ALTER_SESSION.
func phase2WithAlterSession(t *testing.T, alter string) []byte {
	t.Helper()

	return buildPhase2Body(t, "CONNECTOR", []phase2KVForTest{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_PASSWORD", "OLDPWD_HEX", 0},
		{"AUTH_PROGRAM_NM", "SourceLauncher", 0},
		{"AUTH_ALTER_SESSION", alter, 1},
	}, true, 0x00)
}

// gatePhase2 runs the gate the way sendUpstreamAuthPhase2 does.
func gatePhase2(body []byte, grant *store.Grant, bigChunks bool) ([]string, error) {
	values, err := logonAlterSessionValues(body, payloadUsesWideKVEncoding(body), bigChunks)
	if err != nil {
		return nil, refuseLogonAlterSession(err)
	}

	return checkLogonAlterSession(values, grant, nil)
}

// TestLogonAlterSession_ContainerRefusedUnderEveryGrant is the escape the
// CONTAINER block exists for, attempted at logon instead of mid-session: before
// the gate, the value was forwarded verbatim and Oracle switched the pluggable
// database while every queries row still named the granted one.
func TestLogonAlterSession_ContainerRefusedUnderEveryGrant(t *testing.T) {
	t.Parallel()

	for _, alter := range []string{
		"ALTER SESSION SET CONTAINER=PDB2\x00",
		"alter session set container = pdb2",
		"ALTER SESSION SET TIME_ZONE='+02:00' CONTAINER=PDB2\x00",
		`ALTER SESSION SET "CONTAINER"=PDB2`,
	} {
		for name, grant := range logonGrants() {
			_, err := gatePhase2(phase2WithAlterSession(t, alter), grant, false)
			require.ErrorIs(t, err, ErrLogonAlterSessionRefused, "%s: %q", name, alter)

			// A grant with read_only or block_ddl refuses on the control first;
			// one with neither is where the outright block is the only thing
			// standing, and it has to.
			if !grant.IsReadOnly() && !grant.ShouldBlockDDL() {
				require.ErrorIs(t, err, shared.ErrOraclePatternBlocked, "%s: %q", name, alter)
			}
		}
	}
}

// TestLogonAlterSession_UnlistedParameterRefusedUnderControls: the parameter
// allowlist read_only and block_ddl apply mid-session applies at logon too, and
// a full-write grant, which has neither control, still accepts it.
func TestLogonAlterSession_UnlistedParameterRefusedUnderControls(t *testing.T) {
	t.Parallel()

	body := phase2WithAlterSession(t, "ALTER SESSION SET SQL_TRACE=TRUE\x00")
	grants := logonGrants()

	_, err := gatePhase2(body, grants["read_only"], false)
	require.ErrorIs(t, err, ErrLogonAlterSessionRefused)
	require.ErrorIs(t, err, shared.ErrReadOnlyViolation)

	_, err = gatePhase2(body, grants["block_ddl"], false)
	require.ErrorIs(t, err, shared.ErrDDLBlocked)

	applied, err := gatePhase2(body, grants["full write"], false)
	require.NoError(t, err)
	require.Equal(t, []string{"ALTER SESSION SET SQL_TRACE=TRUE"}, applied)
}

// TestLogonAlterSession_UnreadableOrMisshapenRefused: what the gate cannot read
// to the byte, or that is not one ALTER SESSION statement, refuses the login —
// forwarding it would forward a statement nobody checked.
func TestLogonAlterSession_UnreadableOrMisshapenRefused(t *testing.T) {
	t.Parallel()

	full := logonGrants()["full write"]

	for _, alter := range []string{
		"BEGIN NULL; END;",
		"ALTER SESSION SET TIME_ZONE='+02:00'; ALTER SESSION SET CONTAINER=PDB2",
		"ALTER SESSION SET TIME_ZONE='+02:00'\x00ALTER SESSION SET CONTAINER=PDB2",
		"ALTER SYSTEM SET X=1",
	} {
		_, err := gatePhase2(phase2WithAlterSession(t, alter), full, false)
		require.ErrorIs(t, err, ErrLogonAlterSessionRefused, "%q", alter)
	}

	// The key name somewhere that is not a key boundary (inside another value):
	// fail closed rather than guess which occurrence the server reads.
	decoy := buildPhase2Body(t, "CONNECTOR", []phase2KVForTest{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_PROGRAM_NM", "x AUTH_ALTER_SESSION", 0},
	}, true, 0x00)
	_, err := gatePhase2(decoy, full, false)
	require.ErrorIs(t, err, ErrLogonAlterSessionRefused)

	// Every occurrence is checked, not just the first: a harmless pair ahead of
	// a CONTAINER switch does not shield it.
	twice := buildPhase2Body(t, "CONNECTOR", []phase2KVForTest{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_ALTER_SESSION", "ALTER SESSION SET TIME_ZONE='+02:00'\x00", 1},
		{"auth_alter_session", "ALTER SESSION SET CONTAINER=PDB2\x00", 1},
	}, true, 0x00)
	_, err = gatePhase2(twice, full, false)
	require.ErrorIs(t, err, shared.ErrOraclePatternBlocked)

	// A declared length that disagrees with the value it frames.
	body := phase2WithAlterSession(t, "ALTER SESSION SET TIME_ZONE='+02:00'")
	idx := bytes.Index(body, []byte("AUTH_ALTER_SESSION")) + len("AUTH_ALTER_SESSION")
	require.Equal(t, byte(0x01), body[idx], "compressed-int width byte")
	body[idx+1]++
	_, err = gatePhase2(body, full, false)
	require.ErrorIs(t, err, ErrLogonAlterSessionRefused)
}

// TestLogonAlterSession_ApprovalPatternRefused: a login cannot be parked on a
// human, so a logon statement an approval pattern would hold is refused.
func TestLogonAlterSession_ApprovalPatternRefused(t *testing.T) {
	t.Parallel()

	patterns := shared.CompileApprovalPatterns([]string{`(?i)NLS_SORT`})
	_, err := checkLogonAlterSession([]string{"ALTER SESSION SET NLS_SORT=BINARY\x00"},
		logonGrants()["full write"], patterns)
	require.ErrorIs(t, err, ErrLogonAlterSessionRefused)

	applied, err := checkLogonAlterSession([]string{"ALTER SESSION SET TIME_ZONE='+02:00'\x00"},
		logonGrants()["full write"], patterns)
	require.NoError(t, err)
	require.Len(t, applied, 1)
}

// TestLogonAlterSession_WideAndChunkedForms covers the two encodings beyond the
// thin short form: OCI's 4-byte declared length (plain and 3x) and a value long
// enough for the negotiated CLR long form.
func TestLogonAlterSession_WideAndChunkedForms(t *testing.T) {
	t.Parallel()

	wideBody := buildWideAuthBody(wideAuthFraming{}, "CONNECTOR", []authKV{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_ALTER_SESSION", "ALTER SESSION SET CONTAINER=PDB2\x00", 1},
	}, false)
	require.True(t, payloadUsesWideKVEncoding(wideBody))

	_, err := gatePhase2(wideBody, logonGrants()["full write"], false)
	require.ErrorIs(t, err, shared.ErrOraclePatternBlocked)

	long := "ALTER SESSION SET NLS_DATE_FORMAT='" + strings.Repeat("YYYY", 80) + "'\x00"
	chunked := buildPhase2Body(t, "CONNECTOR", nil, true, 0x00)
	chunked = append(chunked, ttcKeyValChunked("AUTH_ALTER_SESSION", long, 1, true)...)

	applied, err := gatePhase2(chunked, logonGrants()["read_only"], true)
	require.NoError(t, err)
	require.Equal(t, []string{strings.TrimRight(long, "\x00")}, applied)
}

// TestLogonAlterSession_RecordedClientsForwardedUnchanged is the positive
// control: the AUTH_ALTER_SESSION every recorded client sends (time zone and
// NLS settings, one statement, NUL-terminated) passes under every grant, and
// the rewrite still forwards its bytes verbatim.
func TestLogonAlterSession_RecordedClientsForwardedUnchanged(t *testing.T) {
	t.Parallel()

	seen := 0

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, dpkt := range td.Packets {
			if dpkt.Direction != dump.DirClientToServer {
				continue
			}

			pkt, err := parseTNSFromDumpPacket(dpkt.Data)
			if err != nil || pkt == nil || pkt.Type != TNSPacketTypeData || len(pkt.Payload) < ttcDataFlagsSize+2 {
				continue
			}

			body := pkt.Payload[ttcDataFlagsSize:]
			if body[0] != byte(TTCFuncPiggyback) || body[1] != PiggybackSubAuth2 {
				continue
			}

			seen++

			for grantName, grant := range logonGrants() {
				applied, err := gatePhase2(body, grant, false)
				require.NoError(t, err, "%s under %s", name, grantName)
				require.Len(t, applied, 1, "%s: one statement", name)
				require.True(t, strings.HasPrefix(applied[0], "ALTER SESSION SET "), "%s: %q", name, applied[0])
			}

			keyAt := bytes.Index(body, []byte("AUTH_ALTER_SESSION"))
			require.Positive(t, keyAt, name)

			end := keyAt + 120
			if end > len(body) {
				end = len(body)
			}

			original := body[keyAt:end]

			out, err := rewriteAuthPhase2(body, "UPSTREAM_USER", &upstreamAuthSecrets{
				encClientSessKey: "NEWSESS", encPassword: "NEWPWD",
			}, false)
			require.NoError(t, err, name)
			require.True(t, bytes.Contains(out, original), "%s: AUTH_ALTER_SESSION forwarded verbatim", name)
		}
	}

	require.Positive(t, seen, "the corpus carries recorded Phase 2 logins")
}

// TestSendUpstreamAuthPhase2_RefusedAlterSessionNeverLeaves drives the real
// forwarding path: a refused value returns the refusal, nothing is written
// upstream, and the client gets ORA-01031 naming the reason.
func TestSendUpstreamAuthPhase2_RefusedAlterSessionNeverLeaves(t *testing.T) {
	t.Parallel()

	upstream := &recordingConn{}
	body := phase2WithAlterSession(t, "ALTER SESSION SET CONTAINER=PDB2\x00")

	s := &session{
		ctx:                 context.Background(),
		logger:              testLogger(),
		upstreamConn:        upstream,
		grant:               logonGrants()["full write"],
		clientAuthPhase2Pkt: &TNSPacket{Payload: append([]byte{0x00, 0x00}, body...)},
	}

	err := s.sendUpstreamAuthPhase2("UPSTREAM_USER", driverIdentity{ProgramName: "dbbat"},
		&upstreamAuthSecrets{encClientSessKey: "S", encPassword: "P"}, logonModeNoNewPass|logonModeUserAndPass)
	require.ErrorIs(t, err, ErrLogonAlterSessionRefused)
	require.Zero(t, upstream.written, "the client's Phase 2 must not reach the upstream")

	code, message := upstreamAuthRejectFor(fmt.Errorf("upstream auth failed: %w",
		fmt.Errorf("write upstream AUTH Phase 2: %w", err)))
	require.Equal(t, ORA01031, code)
	require.Contains(t, message, "AUTH_ALTER_SESSION")
	require.Contains(t, message, shared.ErrOraclePatternBlocked.Error())
}

// TestRecordLogonAlterSession puts what the logon applied on file as the
// connection's first queries rows.
func TestRecordLogonAlterSession(t *testing.T) {
	t.Parallel()

	rec := newRecordingCompletionStore()
	s := newTestSession(logonGrants()["read_only"])
	s.completionStore = rec

	body := phase2WithAlterSession(t, "ALTER SESSION SET TIME_ZONE='+02:00'\x00")
	s.clientAuthPhase2Pkt = &TNSPacket{Payload: append([]byte{0x00, 0x00}, body...)}
	require.NoError(t, s.gateLogonAlterSession(body))

	s.recordLogonAlterSession()

	select {
	case q := <-rec.created:
		require.Equal(t, "ALTER SESSION SET TIME_ZONE='+02:00'", q.SQLText)
		require.Equal(t, s.connectionUID, q.ConnectionID)
	default:
		t.Fatal("no queries row for the logon AUTH_ALTER_SESSION")
	}
}

// recordingConn is an upstream that counts what is written to it.
type recordingConn struct {
	net.Conn
	written int
}

func (c *recordingConn) Write(b []byte) (int, error) {
	c.written += len(b)

	return len(b), nil
}

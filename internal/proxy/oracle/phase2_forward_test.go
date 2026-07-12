package oracle

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
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

	out, err := rewriteAuthPhase2(body, "LABEOMNGR_DEV", sec)
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

	out, err := rewriteAuthPhase2(body, "LABEOMNGR_DEV", sec)
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

	out, err := rewriteAuthPhase2(body, "USR2", sec)
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

	out, err := rewriteAuthPhase2(body, "NEWUSER_X", sec)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// Output must be a valid Phase 2 body that round-trips through the
	// existing parseAuthPhase2 parser.
	withFlags := append([]byte{0x00, 0x00}, out...)

	gotSess, gotPwd, err := parseAuthPhase2(withFlags)
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

			if _, err := rewriteAuthPhase2(tc.body, "X", sec); err == nil {
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

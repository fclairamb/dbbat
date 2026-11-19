package oracle

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jdbcThinPhase1Preamble is the preamble of the AUTH Phase 1 ojdbc11 23.26.3
// sends through dbbat to 23ai Free, captured on 2026-11-16: data flags 08 00,
// the 03 76 01 00 function header, the username-present marker, user_id_len 11,
// logon mode 1, the [01 01 05 01 01] block, then the username *bare*: no CLR
// length byte in front of CURSORPROBE.
var jdbcThinPhase1Preamble = []byte{
	0x08, 0x00, 0x03, 0x76, 0x01, 0x00, 0x01, 0x01, 0x0b, 0x01, 0x01, 0x01, 0x01, 0x05, 0x01, 0x01,
	'C', 'U', 'R', 'S', 'O', 'R', 'P', 'R', 'O', 'B', 'E',
}

// goOraPhase1Preamble is the go-ora v3 counterpart from the same run: same
// layout, with the username CLR-prefixed (0b).
var goOraPhase1Preamble = []byte{
	0x00, 0x00, 0x03, 0x76, 0x01, 0x00, 0x01, 0x01, 0x0b, 0x01, 0x01, 0x01, 0x01, 0x05, 0x01, 0x01,
	0x0b, 'c', 'u', 'r', 's', 'o', 'r', 'p', 'r', 'o', 'b', 'e',
}

// capturedThinPhase1 appends the five KV pairs both captures carry to a
// preamble (values neutralized: the host name was the test machine's).
func capturedThinPhase1(preamble []byte) *TNSPacket {
	payload := append([]byte{}, preamble...)
	for _, kv := range []struct{ k, v string }{
		{"AUTH_TERMINAL", "unknown"},
		{"AUTH_PROGRAM_NM", "Main"},
		{"AUTH_MACHINE", "workstation"},
		{"AUTH_PID", "1234"},
		{"AUTH_SID", "connector"},
	} {
		payload = append(payload, ttcKeyValChunked(kv.k, kv.v, 0, false)...)
	}

	return &TNSPacket{Type: TNSPacketTypeData, Payload: payload}
}

func TestThinPhase1UsernameIsBare(t *testing.T) {
	t.Parallel()

	jdbc := capturedThinPhase1(jdbcThinPhase1Preamble)
	goOra := capturedThinPhase1(goOraPhase1Preamble)

	assert.True(t, (&session{clientAuthPhase1Pkt: jdbc}).clientSendsBareUsername(),
		"JDBC thin writes the username bare")
	assert.False(t, (&session{clientAuthPhase1Pkt: goOra}).clientSendsBareUsername(),
		"go-ora CLR-prefixes the username")
	assert.False(t, (&session{}).clientSendsBareUsername(),
		"no capture keeps the go-ora form the synthetic builders always wrote")
	assert.False(t, thinPhase1UsernameIsBare([]byte{0x03, 0x76}),
		"an unreadable body keeps the CLR-prefixed form")
}

// TestSyntheticAuthPhase1_JDBCThin_MatchesRewritePreamble pins the synthetic
// Phase 1 of a JDBC session against the rewrite of the client's own packet,
// which 23ai accepts: up to the first KV pair they must be byte-identical. The
// CLR-prefixed username the builder used to write here made the upstream answer
// ORA-03120, and dbbat surfaced it to the JDBC client as ORA-01017.
func TestSyntheticAuthPhase1_JDBCThin_MatchesRewritePreamble(t *testing.T) {
	t.Parallel()

	jdbc := capturedThinPhase1(jdbcThinPhase1Preamble)
	sess := &session{clientAuthPhase1Pkt: jdbc}

	rewritten, err := rewriteAuthPhase1Username(jdbc.Payload[ttcDataFlagsSize:], "SYSTEM")
	require.NoError(t, err)

	synthetic := buildClientAuthPhase1(sess.syntheticAuthHeader(PiggybackSubAuth1, authPhase1FuncSeq),
		"SYSTEM", defaultDriverIdentity(), logonModeNoNewPass, false, sess.clientSendsBareUsername())

	want := rewritten[:bytes.Index(rewritten, []byte("AUTH_"))]
	got := synthetic[:bytes.Index(synthetic, []byte("AUTH_"))]
	assert.Equal(t, want, got, "synthetic and rewritten JDBC Phase 1 preambles must agree")
	assert.True(t, bytes.HasSuffix(got[:len(got)-3], []byte{0x05, 0x01, 0x01, 'S', 'Y', 'S', 'T', 'E', 'M'}),
		"the username must follow the pair-count block bare: % x", got)
}

// TestSyntheticAuthPhase2_JDBCThin_BareUsername is the Phase 2 side: the
// preamble the 23ai upstream accepted from the rewrite path ends
// [01 01] SYSTEM, with no CLR prefix, and the synthetic one must as well.
func TestSyntheticAuthPhase2_JDBCThin_BareUsername(t *testing.T) {
	t.Parallel()

	ident, sec := thinSyntheticAuthFixtures()
	sess := &session{clientAuthPhase1Pkt: capturedThinPhase1(jdbcThinPhase1Preamble)}
	header := sess.syntheticAuthHeader(PiggybackSubAuth2, authPhase2FuncSeq)
	mode := uint32(logonModeNoNewPass | logonModeUserAndPass)

	bare := buildClientAuthPhase2(header, "SYSTEM", ident, sec, mode, false, sess.clientSendsBareUsername())
	clr := buildClientAuthPhase2(header, "SYSTEM", ident, sec, mode, false, false)

	pairs := authPhase2Pairs(ident, sec, false)
	preamble := thinSyntheticPhase2PreambleLen(header, "SYSTEM", mode, len(pairs))

	// The CLR form is one byte longer, the prefix; everything else is the same.
	require.Len(t, bare, len(clr)-1)
	assert.Equal(t, []byte{0x01, 0x01, 'S', 'Y', 'S', 'T', 'E', 'M'}, bare[preamble-1-8:preamble-1],
		"the username must follow the [01 01] block bare: % x", bare[:preamble-1])
	assert.Equal(t, clr[preamble:], bare[preamble-1:], "the KV pairs must be unchanged")
	assert.Equal(t,
		decodeKVPairs(t, clr[preamble:], len(pairs), false),
		decodeKVPairs(t, bare[preamble-1:], len(pairs), false),
		"an upstream must walk the same pair set behind either username framing")
}

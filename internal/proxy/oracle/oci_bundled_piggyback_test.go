package oracle

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// bundledOCIFirstCall is the first message the DB-bundled OCI client (sqlplus
// 23.26, from gvenzl/oracle-free:23-slim) sends once AUTH completes, recorded
// off the wire against a live proxy. It is the TNS Data packet payload: two
// data-flag bytes, then the TTC message.
//
// It is here rather than in a pcapng because it is one packet and the point is
// the bytes themselves — the unit half of this regression needs no Oracle
// container, which is what made the fix cheap to iterate on.
func bundledOCIFirstCall(t *testing.T) []byte {
	t.Helper()

	return recordedFrames(t, "testdata/oci_bundled_first_call.hex")[0]
}

// bundledOCICloseCursors is the same client's close-cursors piggybacks, in the
// 64-bit header the wide walk had to learn.
func bundledOCICloseCursors(t *testing.T) [][]byte {
	t.Helper()

	return recordedFrames(t, "testdata/oci_bundled_close_cursors.hex")
}

// recordedFrames reads a hex recording: one TNS Data payload per non-comment
// line, `#` for the commentary that says where the bytes came from.
func recordedFrames(t *testing.T, path string) [][]byte {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var frames [][]byte

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		frame, err := hex.DecodeString(line)
		require.NoError(t, err)
		frames = append(frames, frame)
	}

	require.NotEmpty(t, frames, "%s carries no frames", path)

	return frames
}

// TestBundledOCIFirstCallIsTheFrameThatWasMisread guards the fixture, so every
// assertion below is about the real defect rather than about some other bytes.
// It asserts the two readings that made this a bug: dbbat calls the message a
// fetch, and the (now deleted) fetch layout pulled 27396 out of it — the exact
// cursor id in the WARN this spec was filed from. The layout is spelled out
// here rather than decoded, because the decoder that read it is gone; it read a
// big-endian uint16 at bytes 1..3, which on this frame is (function, sequence).
func TestBundledOCIFirstCallIsTheFrameThatWasMisread(t *testing.T) {
	t.Parallel()

	payload := bundledOCIFirstCall(t)

	funcCode, err := parseTTCFunctionCode(payload)
	require.NoError(t, err)
	assert.Equal(t, TTCFuncOFETCH, funcCode, "the message type dbbat still calls OFETCH")

	ttc := extractTTCPayload(payload)
	require.NotNil(t, ttc)
	assert.Equal(t, "0x6b", ttcOpFunction(ttc), "the TTC function this piggyback really carries")

	require.GreaterOrEqual(t, len(ttc), 3)
	assert.Equal(t, uint16(27396), binary.BigEndian.Uint16(ttc[1:3]),
		"the fetch layout read (function, sequence) as a cursor id — this is the misread")

	assert.False(t, IsCloseCursorsPiggyback(ttc), "and it is not the one 0x11 frame dbbat can walk")
	assert.False(t, IsExecSQL(ttc), "nor an exec it can identify from its contents")
}

// TestClientCallNumberDeclinesAnUnwalkablePiggyback is the hang's own half.
//
// The client is parked on the call stapled *behind* this piggyback (`03 3b`,
// sequence 5), which sits past a body dbbat cannot walk. Naming the piggyback's
// own sequence (4) would make a refusal end a call nobody is waiting for, and
// the client then waits for the real one forever — measured: sqlplus never came
// back and never printed a line, not even the output of the statement before
// the refused one.
func TestClientCallNumberDeclinesAnUnwalkablePiggyback(t *testing.T) {
	t.Parallel()

	ttc := extractTTCPayload(bundledOCIFirstCall(t))

	assert.Equal(t, byte(0x04), ttc[ttcOpSeqOffset],
		"the piggyback's own sequence is right there, which is what made this tempting")
	assert.Contains(t, string(ttc), "\x03\x3b\x05",
		"and the call the client is parked on is stapled behind it, one higher")

	_, ok := clientCallNumber(ttc)
	assert.False(t, ok, "dbbat must decline to name a call it cannot see")
}

// TestBundledOCICloseCursorsWalkFindsTheStapledCall is the other half of the
// hang, and the half fail-open could not have covered: the frame it is about
// carries an INSERT, which a `read_only` grant has to refuse. dbbat cannot get
// out of the way of that one — it has to answer it, and answer the right call.
//
// The call is stapled behind a close-cursors list written in the 64-bit OCI
// header, which the wide walk did not recognize: the list did not decode, the
// staple was never found, and the refusal went out carrying whatever sequence
// number dbbat had last seen. sqlplus waited for the answer to sequence 14
// forever.
func TestBundledOCICloseCursorsWalkFindsTheStapledCall(t *testing.T) {
	t.Parallel()

	frames := bundledOCICloseCursors(t)
	require.Len(t, frames, 2)

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotNil(t, ttc)

		require.True(t, IsCloseCursorsPiggyback(ttc))
		assert.False(t, isCloseCursorsWideHeader(ttc),
			"frame %d is not the 4-byte OCI header — that is the whole point", i)
		assert.True(t, isCloseCursorsWide8Header(ttc), "frame %d must be read as the 64-bit header", i)

		ids, err := decodeCloseCursors(ttc)
		require.NoErrorf(t, err, "frame %d", i)
		assert.Equal(t, []uint16{2}, ids, "frame %d closes the one cursor it names", i)
	}

	// The second frame is the INSERT's, and 14 is the sequence its stapled
	// `03 5e` carries. Anything else strands the client.
	call, ok := clientCallNumber(extractTTCPayload(frames[1]))
	require.True(t, ok)
	assert.Equal(t, byte(0x0e), call,
		"the refusal must end the call stapled behind the close list, not the piggyback")
}

// TestBundledOCIOERIsLearnedAsTheWideSixtyFourBitShape is the third layer, and
// the one that finally let the refusal through: naming the right call is no use
// if the frame carrying it is marshaled at the wrong widths.
//
// The two recorded summaries are what Oracle 23ai Free sends *this* client, on
// the leg that negotiated with this client's own forwarded capabilities, so
// they are the definition of what it can parse. The learner has to recognize
// them — before this it recognized neither, left the session on the unlearned
// default, and dbbat answered a 64-bit client with a 32-bit frame.
func TestBundledOCIOERIsLearnedAsTheWideSixtyFourBitShape(t *testing.T) {
	t.Parallel()

	for i, payload := range recordedFrames(t, "testdata/oci_bundled_oer.hex") {
		shape := defaultOERShape()

		require.Truef(t, learnOERShape(&shape, extractTTCPayload(payload)),
			"summary %d taught nothing", i)

		assert.True(t, shape.fixedWidth, "summary %d is a fixed-width OCI shape", i)
		assert.True(t, shape.fixedWidth64, "summary %d is the 64-bit one", i)
		assert.Equal(t, 2, shape.extraTailFields, "summary %d", i)
		assert.True(t, shape.endOfResponse, "summary %d ends with the 0x1d marker", i)
	}
}

// TestBundledOCIRefusalMatchesTheServersOwnFrame is the encoder half: dbbat's
// synthesized refusal has to put its fields where the server puts them, or the
// client reads past every one of them.
//
// The recorded summary is the reference. Same layout, same total length up to
// the message, and the message itself where the server's CLR starts.
func TestBundledOCIRefusalMatchesTheServersOwnFrame(t *testing.T) {
	t.Parallel()

	reference := extractTTCPayload(recordedFrames(t, "testdata/oci_bundled_oer.hex")[0])

	shape := defaultOERShape()
	require.True(t, learnOERShape(&shape, reference))

	body := encodeOER(shape, oerSummary{
		CallStatus:   1,
		SeqNumber:    5,
		ErrorCode:    1031,
		ErrorMessage: "ORA-01031: insufficient privileges",
		CursorID:     2,
		CallNumber:   7,
	})

	// Every field the client reads, at the offsets the server used.
	assert.Equal(t, byte(TTCFuncOERR), body[0])
	assert.Equal(t, uint16(1031), binary.LittleEndian.Uint16(body[oerFixed64Layout.errNum:]))
	assert.Equal(t, uint16(2), binary.LittleEndian.Uint16(body[oerFixed64Layout.cursorID:]))
	assert.Equal(t, uint32(7), binary.LittleEndian.Uint32(body[oerFixed64Layout.callNumber:]))
	assert.Equal(t, uint32(1031), binary.LittleEndian.Uint32(body[oerFixed64Layout.retCode:]))
	assert.Equal(t, uint16(5), binary.LittleEndian.Uint16(body[oerFixed64Layout.ecid:]))

	// And the message starts where the server's does — 152 in the recording,
	// which is what pins the row count's width and the tail-field count
	// together rather than one at a time.
	clrStart := bytes.Index(reference, []byte("ORA-")) - 1
	require.Positive(t, clrStart)
	assert.Equal(t, clrStart, bytes.Index(body, []byte("ORA-"))-1,
		"the refusal's message must begin where the server's does")

	// The frame dbbat writes must be readable back as the same shape it was
	// built for: a round trip is what catches a layout that only looks right.
	back := defaultOERShape()
	require.True(t, learnOERShape(&back, append(body, ttcEndOfResponse)))
	assert.True(t, back.fixedWidth64)
	assert.Equal(t, shape.extraTailFields, back.extraTailFields)
}

// TestUnnameableFrameCannotSmuggleAStatementPastTheGate is the bound on the
// fail-open, and the reason the fail-open is not simply "forward it".
//
// A piggyback is by construction a frame with something stapled behind it —
// dbbat's own recordings show `11 69 <list> 03 5e <exec>` — so a client can put
// a statement behind a body dbbat cannot walk. Forwarding that unread would
// hand an authenticated user under a `read_only` grant exactly the bypass the
// grant exists to prevent, which is a worse outcome than the hang the fail-open
// was introduced to fix.
//
// The frame here is the real recorded first call with an INSERT stapled behind
// it, and the two assertions are the two things that both have to hold: the
// statement does not travel, and the client is not answered with an OER ending
// a call it is not parked on (which is what would hang it).
func TestUnnameableFrameCannotSmuggleAStatementPastTheGate(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	upstream, upstreamPeer := net.Pipe()
	t.Cleanup(func() { _ = upstreamPeer.Close() })
	s.upstreamConn = upstream

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: unnameableFrameCarrying(t, "INSERT INTO secrets VALUES (1)"),
	}

	assert.True(t, s.interceptClientMessage(pkt),
		"a statement stapled behind an unwalkable piggyback must not reach the upstream under read_only")

	assert.Equal(t, 1, logs.count(logMsgUnnameableStatementRefused),
		"and the refusal must be in the log, since the client only sees a dropped socket")
	assert.Empty(t, answered(),
		"nothing may be written back: dbbat cannot name the call it would be ending")
	assert.Error(t, upstream.SetDeadline(time.Now()), "the session must be torn down")
}

// unnameableFrameCarrying builds the shape this whole bound is about: the real
// recorded piggyback dbbat cannot walk, with a statement stapled behind it
// exactly the way a close-cursors list carries one.
func unnameableFrameCarrying(t *testing.T, sql string) []byte {
	t.Helper()

	frame := append(bundledOCIFirstCall(t), byte(TTCFuncPiggyback), PiggybackSubExecSQL, 0x06, 0x00)
	frame = append(frame, make([]byte, 24)...)
	frame = append(frame, []byte(sql)...)

	_, ok := clientCallNumber(extractTTCPayload(frame))
	require.False(t, ok, "the fixture must be a frame dbbat cannot name, or it proves nothing")

	return frame
}

// TestUnnameableFrameRunsTheAlwaysOnValidators is the asymmetry the bound had
// left open. `ValidateOracleQuery` enforces the Oracle blocked patterns and the
// password-change guard *regardless of grant controls* — the JDBC exec path
// pins exactly that ("an Oracle blocked pattern applies with no controls at
// all") — so short-circuiting this path on hasStatementControls made an
// unwalkable piggyback the one place `ALTER SYSTEM KILL SESSION` could travel
// under an ordinary full-access grant while the identical statement in a
// nameable frame was refused.
//
// The grant here carries no controls at all, which is the point.
func TestUnnameableFrameRunsTheAlwaysOnValidators(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"ALTER SYSTEM KILL SESSION '123,456'",
		"BEGIN UTL_HTTP.REQUEST('http://evil'); END;",
		"ALTER USER bob IDENTIFIED BY PASSWORD 'secret'",
	} {
		t.Run(truncateSQL(sql, 30), func(t *testing.T) {
			t.Parallel()

			logs := newCountingHandler()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			s.logger = slog.New(logs)

			client, answered := recordingPipe(t)
			s.clientConn = client

			pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: unnameableFrameCarrying(t, sql)}

			assert.True(t, s.interceptClientMessage(pkt),
				"a statement the always-on validators refuse must not travel, controls or no controls")
			assert.Equal(t, 1, logs.count(logMsgUnnameableStatementRefused))
			assert.Empty(t, answered())
		})
	}
}

// TestUnnameableFrameRecordsTheStatementItAllows is the audit trail's half.
// "Every query logged" is the product's premise, and a path that forwards a
// statement while writing nothing would make a client dialect whose close list
// never walks the one place a session's SQL escapes it entirely.
//
// The recorded pending query is also what makes MaxQueryCounts apply here: the
// quota is charged when a pending query completes on the response leg, so a
// statement nobody tracks is a statement nobody counts.
func TestUnnameableFrameRecordsTheStatementItAllows(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: unnameableFrameCarrying(t, "SELECT id FROM emp"),
	}

	require.False(t, s.interceptClientMessage(pkt), "the read is allowed and must travel")

	// Tracked exactly as handleJDBCExec tracks one — which is what the store
	// write (persistQueryRecord, shared verbatim with that path) and the
	// completion on the response leg both key off.
	require.NotNil(t, s.tracker.pendingQuery, "an allowed statement must be tracked, not just forwarded")
	require.NotNil(t, s.tracker.pendingQuery.cursor)
	assert.Equal(t, "SELECT id FROM emp", s.tracker.pendingQuery.cursor.sql)

	assert.Equal(t, 1, logs.count(logMsgUnnameableStatementRecorded))
	assert.Zero(t, logs.count(logMsgQueryIntercepted),
		"and not under the message the cursor-learning measurement counts parses with")
	assert.Empty(t, answered())
}

// TestUnnameableFrameEnforcesTheQueryQuota closes the other half of the
// pre-flight gap: handleJDBCExec consults checkQuotas before anything else, and
// this path had no equivalent. Revocation, expiry and the byte quota are still
// caught on the response leg by LimitGuard, but MaxQueryCounts is not — it is
// charged per recorded query, so it only applies where a query is gated.
func TestUnnameableFrameEnforcesTheQueryQuota(t *testing.T) {
	t.Parallel()

	s := newTestSession(exhaustedGrant())
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	client, answered := recordingPipe(t)
	s.clientConn = client

	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: unnameableFrameCarrying(t, "SELECT id FROM emp"),
	}

	assert.True(t, s.interceptClientMessage(pkt),
		"a statement over max_query_counts must not travel just because its frame is unnameable")

	select {
	case created := <-recorder.created:
		assert.Equal(t, "SELECT id FROM emp", created.SQLText)
		require.NotNil(t, created.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("the quota refusal was never recorded")
	}

	assert.Empty(t, answered(), "and is not answered: dbbat cannot name the call")
}

// TestUnnameableExecFrameIsGatedOnItsOwnPayload is the carve-out in the
// anchoring, written down because it is a real one rather than an oversight.
//
// statementOpOffsets counts offset 0, so for a frame whose *own* header is a
// statement-carrying op (`11 69`, `11 98`) the anchor matches immediately and
// the scan covers the whole payload — exactly the unanchored behavior the
// anchor was introduced to avoid. That is deliberate: such a frame really is an
// execute (it only reaches this path because its close list did not walk), its
// SQL really is in there, and declining to look would forward a live statement
// ungated. The cost is the one this fixture shows: a `11 69` frame whose
// stapled set-end-to-end-attrs strings read as a refused statement ends the
// session.
//
// The trade is fail-closed on a shape no tested client produces — every one of
// the 54 recorded `11 69` frames in testdata/dbeaver.pcapng walks, and so do
// both bundled-client ones — against fail-open on a live exec.
//
// The carve-out survived the measurement that was meant to settle it (the
// 2026-08-13-05 spec), and costs less than it did: decodeExecSQL reads the
// execute's declared length now, so the ordinary `11 69 <closes> 03 5e <exec>`
// shape is decoded from the stapled op's own header. This fixture is the
// residue — a close list that does not walk, with no `03 5e` anchor behind it,
// which is the only remaining way to reach the whole-payload scan.
func TestUnnameableExecFrameIsGatedOnItsOwnPayload(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	// `11 69` whose close list does not walk, with a set-end-to-end-attrs
	// piggyback stapled behind it carrying an application's module string.
	frame := make([]byte, 0, 64)
	frame = append(frame, byte(TTCFuncOFETCH), execSubOpJDBC, 0x07, 0x00, 0x00, 0x01, 0x01, 0x01, 0x02)
	frame = append(frame, make([]byte, 20)...)
	frame = append(frame, byte(TTCFuncOFETCH), 0x87, 0x08, 0x00)
	frame = append(frame, []byte("DELETE FROM orders nightly job")...)

	_, ok := clientCallNumber(frame)
	require.False(t, ok, "the fixture must be unnameable")
	require.Equal(t, []int{0}, statementOpOffsets(frame),
		"and its only statement-op anchor must be its own header, which is the carve-out")

	assert.True(t, s.interceptClientMessage(&TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, frame...),
	}), "an exec frame is read whole, so this ends the session — the deliberate fail-closed side")

	assert.Equal(t, 1, logs.count(logMsgUnnameableStatementRefused))
	assert.Empty(t, answered())
}

// TestUnnameableFrameIgnoresIncidentalASCII is the deliberate judgement on the
// other failure mode: ending a live session because a frame dbbat could not
// parse happened to contain bytes that read as SQL.
//
// It is not hypothetical. The piggybacks that reach this path carry
// caller-supplied text by design — `11 87` set-end-to-end-attrs carries the
// module, action and client-identifier an application chooses — so a bare
// keyword scan would kill the session of anyone whose client sets its module to
// "DELETE ORDERS". A refused call on the nameable path is an ORA error the
// client retries; here it is the connection.
//
// The bound chosen is structural rather than lexical: the statement is only
// read from the start of an op that can carry one. Bytes with no such header
// are bytes the *upstream* will not execute either, so nothing that could
// actually run is let through by ignoring them — which is why this is a bound
// and not a weakening. TestUnnameableFrameCannotSmuggleAStatementPastTheGate
// is the other half, on the same fixture with a real op header in front.
func TestUnnameableFrameIgnoresIncidentalASCII(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	// A set-end-to-end-attrs-shaped piggyback whose payload is an application's
	// own module string. No statement-carrying op header anywhere in it.
	frame := append(bundledOCIFirstCall(t), byte(TTCFuncOFETCH), 0x87, 0x06, 0x00)
	frame = append(frame, make([]byte, 24)...)
	frame = append(frame, []byte("DELETE FROM orders nightly job")...)

	require.Empty(t, statementOpOffsets(extractTTCPayload(frame)),
		"the fixture must carry no statement op, or it tests the wrong thing")

	assert.False(t, s.interceptClientMessage(&TNSPacket{Type: TNSPacketTypeData, Payload: frame}),
		"a session must not be killed because an opaque frame contained bytes that read as SQL")
	assert.Zero(t, logs.count(logMsgUnnameableStatementRefused))
	assert.Empty(t, answered())
}

// TestUnnameableFrameStillForwardsAnAllowedStatement is the other side of that
// bound: the gate is the grant's, not "any statement in an unnameable frame".
// A SELECT under `read_only` is allowed, so the frame travels — otherwise this
// would be a fail-closed rule wearing a fail-open name, and the first client
// whose framing dbbat misreads would lose its session on a legal query.
func TestUnnameableFrameStillForwardsAnAllowedStatement(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})

	client, answered := recordingPipe(t)
	s.clientConn = client

	allowed := unnameableFrameCarrying(t, "SELECT 1 FROM DUAL")

	assert.False(t, s.interceptClientMessage(&TNSPacket{Type: TNSPacketTypeData, Payload: allowed}),
		"a read the grant permits must still travel")
	assert.Empty(t, answered())
}

// TestUnlearnedRefusalFollowsTheClientsOwnDialect covers the window no
// integration test can reach: a refusal that fires before any upstream OER has
// taught the session a shape. sqlplus runs its own login SELECTs first, so the
// live suite always learns first — but an approval pattern matching the opening
// statement, an already-exhausted quota, or a first statement that is a write
// under `read_only` all land here, and a 64-bit client handed the 32-bit layout
// hangs exactly as it did everywhere else.
func TestUnlearnedRefusalFollowsTheClientsOwnDialect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		wide   bool
		wide64 bool
		want   oerFixedLayout
	}{
		{name: "the 64-bit OCI client", wide: true, wide64: true, want: oerFixed64Layout},
		{name: "the 32-bit OCI client", wide: true, want: oerFixed32Layout},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			s.clientWideEncoding = tc.wide
			s.clientWide64Encoding = tc.wide64

			shape, _, _ := s.nextOERFrame()

			require.False(t, shape.tailLearned, "this is the unlearned path or it tests nothing")
			assert.True(t, shape.fixedWidth)
			assert.Equal(t, tc.wide64, shape.fixedWidth64)
			assert.Equal(t, tc.want, shape.layoutFor())
		})
	}
}

// TestBundledOCIAuthPhase1IsRecognizedAsTheSixtyFourBitDialect is what makes
// the fallback above reachable: the dialect has to be known from the client's
// own AUTH, since that is the only evidence a session has before its first
// upstream OER.
//
// The negative case comes from a real Instant Client recording rather than a
// hand-built one, because "the other flavor is not detected as this one" is the
// claim that would otherwise rot silently.
func TestBundledOCIAuthPhase1IsRecognizedAsTheSixtyFourBitDialect(t *testing.T) {
	t.Parallel()

	bundled := recordedFrames(t, "testdata/oci_bundled_auth_phase1.hex")[0]

	assert.True(t, payloadUsesWideKVEncoding(bundled), "it is an OCI client")
	assert.True(t, usesWide64OpHeader(extractTTCPayload(bundled)), "and it is the 64-bit one")

	// And a session reads both flags off it, which is what nextOERFrame leans
	// on before anything has been learned.
	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.observeClientAuthEncoding(bundled)
	assert.True(t, s.clientWideEncoding)
	assert.True(t, s.clientWide64Encoding)

	var instantClientPhase1 []byte

	for _, ttc := range clientTTCPayloads(t, "sqlplus_cursor_reexec.pcapng") {
		if len(ttc) > 2 && ttc[0] == byte(TTCFuncPiggyback) && ttc[1] == PiggybackSubAuth1 {
			instantClientPhase1 = ttc

			break
		}
	}

	require.NotNil(t, instantClientPhase1, "the Instant Client recording must carry an AUTH Phase 1")
	assert.False(t, usesWide64OpHeader(instantClientPhase1),
		"the Instant Client is wide but not 64-bit, which is the whole distinction")
}

// TestObserveClientCallNumberKeepsTheLastNamedCall pins the second-order half:
// an unnameable message must not overwrite the last call dbbat did name. The
// response leg's mid-stream limit refusal reads that number while the client is
// parked on a call, so poisoning it with a piggyback's sequence would strand an
// OCI client on a path that has nothing to do with cursors.
func TestObserveClientCallNumberKeepsTheLastNamedCall(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

	require.True(t, s.observeClientCallNumber([]byte{0x03, 0x5e, 0x09, 0x00}))
	require.Equal(t, byte(0x09), s.oerCallNumber)

	assert.False(t, s.observeClientCallNumber(extractTTCPayload(bundledOCIFirstCall(t))))
	assert.Equal(t, byte(0x09), s.oerCallNumber,
		"a call dbbat could not name must leave the last one it could alone")
}

// TestBundledOCIFirstCallIsForwardedUnderARestrictiveGrant is the regression
// itself, and it is deliberately run under the grant that broke: `read_only`
// makes hasStatementControls true, which is what turned the misread into a
// refusal rather than a WARN.
//
// Two assertions, one per symptom. The frame travels upstream (it is session
// state the client needs, not an execution), and dbbat writes nothing back — a
// refusal here is an OER for a call the client is not parked on, which is the
// hang.
func TestBundledOCIFirstCallIsForwardedUnderARestrictiveGrant(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{
		Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
	})
	s.logger = slog.New(logs)

	client, answered := recordingPipe(t)
	s.clientConn = client

	pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: bundledOCIFirstCall(t)}

	assert.False(t, s.interceptClientMessage(pkt),
		"the bundled OCI client's first message must reach its upstream")

	assert.Zero(t, logs.count(logMsgUntrackedCursorRefused),
		"and must not be refused as a re-execution of a cursor nobody opened")
	assert.Equal(t, 1, logs.count(logMsgUnnamedCallForwarded),
		"the fail-open must be recorded, or a future misread would be silent again")

	assert.Empty(t, answered(), "nothing may be written back: the client is parked on another call")
}

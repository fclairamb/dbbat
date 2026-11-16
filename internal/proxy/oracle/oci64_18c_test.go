package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The sqlplus 18.4 bundled in gvenzl/oracle-xe:18.4.0-slim speaks the 64-bit
// OCI dialect, but against a server below TTC field version 23.1, so its op
// headers carry no token block (wide64FieldsAt) and its column records end
// after the two classic trailing integers (wide64ColumnRecordTailLenPre23).
// Every fixture here was recorded on the nightly 18c leg; see the files'
// own headers.
const (
	oci64_18cAuthPhase1       = "testdata/oci64_18c_auth_phase1.hex"
	oci64_18cParseExecs       = "testdata/oci64_18c_parse_execs.hex"
	oci64_18cRefCursorDrives  = "testdata/oci64_18c_refcursor_drives.hex"
	oci64_18cBindOutputs      = "testdata/oci64_18c_refcursor_bind_output.hex"
	oci64_18cLongStatementLen = 40610
)

// oci64_18cOERShape is what an 18c sqlplus session learns: the 64-bit
// fixed-width encoding, from a server advertising a field version below 23.1.
func oci64_18cOERShape() oerShape {
	shape := oci64OERShape()
	shape.serverFieldVersion = ttcFieldVersion20_1 - 1

	return shape
}

// The dialect is learned off the client's own AUTH Phase 1. Until the
// tokenless header was read, an 18c sqlplus session was taken for the 4-byte
// dialect, and every 64-bit walk below was never asked.
func TestOCI64_18cAuthPhase1IsLearnedAsTheWide64Dialect(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64_18cAuthPhase1)
	require.Len(t, frames, 1)

	ttc := extractTTCPayload(frames[0])
	require.NotEmpty(t, ttc)
	require.Equal(t, []byte{0x03, 0x76}, ttc[:2], "the fixture is an AUTH Phase 1")

	at, ok := wide64FieldsAt(ttc)
	require.True(t, ok)
	assert.Equal(t, 3, at, "18c writes no token block: the fields follow the sequence byte")
	assert.True(t, isWide64AuthPhase1(ttc), "18c sqlplus is the 64-bit dialect")

	s := &session{}
	s.observeClientAuthEncoding(frames[0])
	assert.True(t, s.clientWide64Encoding, "and the session must learn it")

	// The 23ai Phase 1 of the same client family still reads as before.
	bundled := extractTTCPayload(recordedFrames(t, "testdata/oci_bundled_auth_phase1.hex")[0])
	at, ok = wide64FieldsAt(bundled)
	require.True(t, ok)
	assert.Equal(t, closeCursorsWide8HeaderLen, at, "23ai carries the token block")
	assert.True(t, isWide64AuthPhase1(bundled))

	// A 4-byte client's tokenless Phase 1 (a ub4 key/value count, so the third
	// sentinel sits four bytes earlier) must not be taken for this dialect.
	fourByte := make([]byte, 0, len(ttc))
	fourByte = append(fourByte, ttc[:3+28]...)
	fourByte = append(fourByte, ttc[3+32:]...)
	assert.False(t, isWide64AuthPhase1(fourByte), "a ub4 count is the 4-byte dialect")
}

// The statement behind an 18c close list is read whole, by its header length,
// including the first fragment of a statement six packets long — which is
// what tells the reassembly to wait for the rest.
func TestOCI64_18cParseExecsAreReadByTheirHeader(t *testing.T) {
	t.Parallel()

	want := []string{
		"BEGIN DBMS_OUTPUT.DISABLE; END;",
		"BEGIN DBMS_APPLICATION_INFO.SET_MODULE(:1,NULL); END;",
		"SELECT DECODE('A','A','1','2') FROM SYS.DUAL",
		"INSERT INTO dbbat_blocked_probe VALUES (99)",
	}
	closed := [][]uint16{{2}, {2}, {3}, {3}}

	frames := recordedFrames(t, oci64_18cParseExecs)
	require.Len(t, frames, len(want)+1)

	for i, sql := range want {
		ttc := extractTTCPayload(frames[i])
		require.NotEmptyf(t, ttc, "frame %d", i)

		ids, err := decodeCloseCursors(ttc)
		require.NoErrorf(t, err, "frame %d opens with a tokenless 64-bit close list", i)
		assert.Equalf(t, closed[i], ids, "frame %d closes", i)

		result, err := decodePiggybackExecSQL(ttc, true)
		require.NoErrorf(t, err, "frame %d", i)
		assert.Equalf(t, sql, result.SQL, "frame %d", i)
		assert.False(t, result.Truncated)

		_, reexec := execNoStatementCursor(ttc, true)
		assert.Falsef(t, reexec, "frame %d carries a statement, so it is no re-execution", i)

		_, short := statementFragmentShortfall(ttc, true)
		assert.Falsef(t, short, "frame %d is whole", i)
	}

	first := extractTTCPayload(frames[len(want)])
	require.Equal(t, []byte{0x03, 0x5e}, first[:2])

	length, ok := execSQLLength(first, true)
	require.True(t, ok, "the 18c exec header declares the statement's length")
	assert.Equal(t, oci64_18cLongStatementLen, length)

	need, ok := statementFragmentShortfall(first, true)
	require.True(t, ok, "the first fragment must be reported short, or no reassembly starts")
	assert.Positive(t, need)

	// Before the session knew the dialect, the 4-byte reading answered nothing.
	_, ok = execSQLLength(first, false)
	assert.False(t, ok)
}

// The REF cursor ids the 18c server hands back are read out of its call
// responses, and the client's next frames drive exactly those cursors.
func TestOCI64_18cRefCursorIDsMatchTheCursorsTheClientDrives(t *testing.T) {
	t.Parallel()

	want := []uint16{2, 3}

	frames := recordedFrames(t, oci64_18cBindOutputs)
	learned := make([]uint16, 0, len(frames))

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.Equalf(t, byte(ttcMsgIOVector), ttc[0], "frame %d is a call response", i)

		learned = append(learned, refCursorIDsInBindOutput(oci64_18cOERShape(), ttc)...)

		assert.Emptyf(t, refCursorIDsInBindOutput(oci64OERShape(), ttc),
			"frame %d: read with the 23ai record tail, the descriptor must not walk", i)
	}

	assert.Equal(t, want, learned)

	drives := recordedFrames(t, oci64_18cRefCursorDrives)
	driven := make([]uint16, 0, len(drives))

	for i, payload := range drives {
		ttc := extractTTCPayload(payload)

		id, ok := execNoStatementCursor(ttc, true)
		require.Truef(t, ok, "drive %d declares no statement, so it is a re-execution", i)

		driven = append(driven, id)
	}

	assert.Equal(t, want, driven)
}

// A token-form header that fails its own guard is not re-read as the
// tokenless form: the fields that form would read are not where a 23ai frame
// keeps them.
func TestOCI64TokenFormIsNotReadAsTheTokenlessOne(t *testing.T) {
	t.Parallel()

	for i, payload := range recordedFrames(t, oci64ParseExecs) {
		ttc := extractTTCPayload(payload)
		if !strings.HasPrefix(string(ttc[:2]), "\x03\x5e") {
			continue
		}

		broken := append([]byte(nil), ttc...)
		broken[execWide64SeqPadAt] ^= 0xff

		_, ok := execSQLLength(broken, true)
		assert.Falsef(t, ok, "frame %d with a broken sequence pad", i)
	}
}

// replay18cFetch runs one describe and the fetches behind it through a fresh
// 18c session, and returns the rows it captured.
func replay18cFetch(t *testing.T, shape oerShape, describe []byte, fetches ...[]byte) []map[string]interface{} {
	t.Helper()

	s, rowStore, _ := newCapturingSession(t, 10000)
	s.oer = shape

	s.trackerMu.Lock()
	s.handleQueryResultV2(extractTTCPayload(describe))

	pending := s.tracker.pendingQuery
	if pending == nil {
		s.trackerMu.Unlock()

		return nil
	}

	sink := pending.rowSink

	for _, f := range fetches {
		s.handleContinuation(extractTTCPayload(f))
	}
	s.trackerMu.Unlock()

	sink.Flush(t.Context())

	return rowStore.rowData(t)
}

// The 18c describes parse whole, column names included, and the rows they and
// their fetches carry are captured under those names. Read with the 23ai
// record tail, the multi-column describes do not walk.
func TestOCI64_18cRowCaptureCarriesTheDescribesValues(t *testing.T) {
	t.Parallel()

	describes := recordedFrames(t, "testdata/oci64_18c_describe.hex")
	require.Len(t, describes, 4)

	for i, payload := range describes {
		ttc := extractTTCPayload(payload)
		require.Equalf(t, byte(TTCFuncQueryResult), ttc[0], "frame %d is a describe", i)
		assert.NotEmptyf(t, parseColumnDescribes(ttc, oci64_18cOERShape()), "frame %d", i)
	}

	assert.Nil(t, parseColumnDescribes(extractTTCPayload(describes[2]), oci64OERShape()),
		"the 23ai tail misaligns the second record")

	rows := replay18cFetch(t, oci64_18cOERShape(), describes[2])
	require.Len(t, rows, 1)
	assert.Equal(t, "x", rows[0]["BIG"])
	assert.Equal(t, "ab   ", rows[0]["C5"])
	assert.Equal(t, "7a7a", rows[0]["R"])
	assert.Equal(t, ociDescribeObjectValue, rows[0]["OBJ"])

	seven := recordedFrames(t, "testdata/oci64_18c_sevencols.hex")
	require.Len(t, seven, 3)
	assert.Equal(t, []map[string]interface{}{{
		"C1": "1", "C2": "v1", "C3": "10", "C4": "w1", "C5": "100", "C6": "x1", "C7": "1000",
	}}, replay18cFetch(t, oci64_18cOERShape(), seven[2]))

	long := recordedFrames(t, "testdata/oci64_18c_long.hex")
	require.Len(t, long, 7)
	assert.Equal(t, []map[string]interface{}{
		{"C1": "aaaaaa", "C2": "bbbbbb", "C3": "cccccc", "L1": "longvalue-0123456789"},
		{"C1": "aaaaaa", "C2": "bbbbbb", "C3": "cccccc", "L1": ""},
	}, replay18cFetch(t, oci64_18cOERShape(), long[2], long[3], long[4]))
}

// An 18c LOB column is the locator straight behind its length. Read with the
// 23ai prefix (a LOB size and a chunk size), the row is lost; read as 18c
// sends it, every ordinary column survives the LOBs between them.
func TestOCI64_18cLOBFetchKeepsEveryOrdinaryColumn(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, "testdata/oci64_18c_lob.hex")
	require.Len(t, frames, 4)

	assert.Equal(t, []map[string]interface{}{{
		"C1": "aaaaaa", "D1": "<CLOB locator>",
		"C2": "bbbbbb", "D2": "<CLOB locator>",
		"C3": "cccccc", "D3": "<BLOB locator>",
		"C4": "dddddd", "D4": "<CLOB locator>",
		"C5": "eeeeee", "X1": ociLOBXMLValue,
		"C6": "ffffff", "D5": "",
		"C7": "gggggg",
	}}, replay18cFetch(t, oci64_18cOERShape(), frames[2], frames[3]))

	// The 23ai field version reads the record tail at its 23ai width, so the
	// describe itself refuses to walk and no row is offered a wrong layout.
	assert.Empty(t, replay18cFetch(t, oci64OERShape(), frames[2], frames[3]))
}

// The refusal family on 18c: the INSERT stapled behind a tokenless close list
// is decoded and refused under read_only, and the read behind the same kind of
// list goes through, both read by the header length on the dialect the
// session learned.
func TestOCI64_18cCallBehindACloseListIsGated(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64_18cParseExecs)
	require.GreaterOrEqual(t, len(frames), 4)

	newSession := func() *session {
		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientWide64Encoding = true
		s.clientConn = drainedPipe(t)

		return s
	}

	insert := extractTTCPayload(frames[3])
	require.ErrorIs(t, newSession().handlePiggybackExec(insert), shared.ErrReadOnlyViolation,
		"read_only must refuse the 18c INSERT")

	read := extractTTCPayload(frames[2])
	require.NoError(t, newSession().handlePiggybackExec(read), "the DECODE probe is a read")
}

// execLengthOf reads the statement length an exec declares, past the close
// list a piggyback staples in front of it.
func execLengthOf(t *testing.T, ttc []byte) int {
	t.Helper()

	body := ttc
	if end, ok := closeCursorsEnd(ttc); ok {
		body = ttc[end:]
	}

	length, ok := execSQLLength(body, true)
	require.True(t, ok, "the exec header must declare its statement's length")

	return length
}

// The 18c parse-execs go through the statement tagger the way the proxy hands
// them over: the tag reaches the wire in front of the client's own text, the
// PL/SQL BEGIN … END; calls included, and the 64-bit length field is
// rewritten to the tagged statement's length.
func TestOCI64_18cParseExecsAreTagged(t *testing.T) {
	t.Parallel()

	want := []string{
		"BEGIN DBMS_OUTPUT.DISABLE; END;",
		"BEGIN DBMS_APPLICATION_INFO.SET_MODULE(:1,NULL); END;",
		"SELECT DECODE('A','A','1','2') FROM SYS.DUAL",
		"INSERT INTO dbbat_blocked_probe VALUES (99)",
	}
	terminated := []int{0, 1, 0, 0}

	frames := recordedFrames(t, oci64_18cParseExecs)
	require.Len(t, frames, len(want)+1)

	s := taggedSession(t, 8192)
	s.clientWide64Encoding = true

	for i, sql := range want {
		ttc := extractTTCPayload(frames[i])
		require.NotEmptyf(t, ttc, "frame %d", i)

		// SET_MODULE declares its NUL terminator inside the length, so the
		// field is one past the text there; the tag grows it by its own size.
		before := execLengthOf(t, ttc)
		require.Equalf(t, len(sql)+terminated[i], before, "frame %d declares its own text", i)

		out, ok := s.rewriteStatementMessage(messageOf(ttc))
		require.Truef(t, ok, "frame %d: the 18c session must certify and tag", i)
		require.True(t, s.tagging.certified)

		wire := upstreamTTC(t, out)

		got, ok := decodeExecStatement(wire, true)
		require.Truef(t, ok, "frame %d: the rewritten frame must still decode", i)
		assert.Equalf(t, taggingTagPrefix+sql, got, "frame %d", i)

		assert.Equalf(t, len(taggingTagPrefix)+before, execLengthOf(t, wire),
			"frame %d: the length field declares the tagged statement", i)

		if ids, err := decodeCloseCursors(ttc); err == nil {
			gotIDs, err := decodeCloseCursors(wire)
			require.NoErrorf(t, err, "frame %d: the close list survives the rewrite", i)
			assert.Equalf(t, ids, gotIDs, "frame %d", i)
		}
	}
}

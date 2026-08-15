package oracle

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// What an OCI client's *successful* calls end with, and what dbbat used to make
// of it: nothing at all.
//
// decodeOERFieldsAtLayout taught the decoder to read the fixed-width summary
// object, and decodeErrorOER and findPlausibleOERInResponse were routed into it —
// which is what stopped an OCI client's failures being recorded as successes, and
// what made its DML row counts land off the OER embedded in a Response. The
// standalone summary object was left out, and it is the one that ends a *fetch*:
// every SELECT an OCI client runs finishes on a bare `func 0x04` reporting
// ORA-01403, with CallStatus 0x1 and no end-of-call bit. decodeOERAt refused all
// of them, so the statement stayed pending until the next one's
// flushPendingQuery closed it — a duration_ms measuring the client's think time
// and, for a DML, no rows_affected at all.
//
// These are the fixtures for that. Both OCI recordings in testdata/ carry the
// shape: sqlplus_cursor_reexec.pcapng is five statements each ending on its own
// standalone status OER, and sqlplus_midfetch_fail.pcapng opens with the same
// login probe before its failures.

// ociStatusDumps is the recordings whose statements end on a standalone
// fixed-width status OER, with how many such OERs each carries.
func ociStatusDumps() map[string]int {
	return map[string]int{
		sqlplusReexecDump:   5, // the login probe plus four `SELECT 1 AS n FROM dual`
		sqlplusMidFetchDump: 1, // the login probe; its other two OERs are failures
	}
}

// standaloneStatusOER is one server packet that reached handleOERStatus at byte 0
// while no row stream was open — the routing condition of the branch this
// measures — together with what each of the two readings makes of it and whether
// that very packet ended the pending statement's call.
type standaloneStatusOER struct {
	index      int
	sql        string
	fields     *oerInfo
	compressed bool // could the end-of-call-bit reading of the compressed encoding see it?
	accepted   bool // does decodeOERAt accept it now?
	completed  bool // did this packet end the call, rather than the next statement?
}

// isStatus reports whether the OER carries a status rather than a diagnostic —
// success or end-of-data, which is the half decodeErrorOER cannot prove and
// therefore the half this change is about.
func (o standaloneStatusOER) isStatus() bool {
	return o.fields != nil && (o.fields.ErrorCode == 0 || o.fields.ErrorCode == oraNoDataFound)
}

// walkStandaloneStatusOERs replays a recording through the real intercept
// pipeline and returns every standalone `func 0x04` that arrived outside a row
// stream, recording whether the statement pending at that moment was completed by
// it.
//
// `pause` is slept after each of those packets. It stands in for the client's
// think time, which is the whole point of the duration half of the symptom: a
// statement left pending is charged everything that happens before the *next*
// statement flushes it.
func walkStandaloneStatusOERs(t *testing.T, s *session, name string, pause time.Duration) []standaloneStatusOER {
	t.Helper()

	s.clientConn = drainedPipe(t)

	var found []standaloneStatusOER

	for i, pkt := range loadTestDump(t, name).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		ttc := extractTTCPayload(tns.Payload)

		s.trackerMu.Lock()
		standalone := len(ttc) >= 2 && TTCFunctionCode(ttc[0]) == TTCFuncOERR && !s.rowStreamActive()

		sql := ""
		if standalone && s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil {
			sql = s.tracker.pendingQuery.cursor.sql
		}
		s.trackerMu.Unlock()

		if !standalone {
			s.interceptUpstreamMessage(tns)

			continue
		}

		shape := s.oerShapeSnapshot()
		fields, _ := decodeOERFieldsForShape(shape, ttc, 0)

		got := standaloneStatusOER{
			index:      i,
			sql:        sql,
			fields:     fields,
			compressed: decodeCompressedEndOfCallOERAt(ttc, 0) != nil,
			accepted:   decodeOERAt(shape, ttc, 0) != nil,
		}

		s.interceptUpstreamMessage(tns)

		s.trackerMu.Lock()
		got.completed = s.tracker.pendingQuery == nil
		s.trackerMu.Unlock()

		found = append(found, got)

		if pause > 0 {
			time.Sleep(pause)
		}
	}

	return found
}

// TestDumpReplay_OCIStatusOERsCompleteTheirOwnStatement is the regression: on an
// OCI session a bare status OER has to end the call it reports on.
//
// It also pins the measurement that chose the predicate. Demanding the
// end-of-call bit in the fixed-width call status was the conservative option and
// would have completed *nothing*: every one of these carries CallStatus 0x1. The
// only OCI call status in the corpus with the bit set is the ORA-00942 of a failed
// DROP, and decodeErrorOER already read that one.
func TestDumpReplay_OCIStatusOERsCompleteTheirOwnStatement(t *testing.T) {
	t.Parallel()

	for name, want := range ociStatusDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

			var statuses []standaloneStatusOER

			for _, got := range walkStandaloneStatusOERs(t, s, name, 0) {
				if got.isStatus() {
					statuses = append(statuses, got)
				}
			}

			require.Len(t, statuses, want,
				"the recording must carry the standalone status OERs this is about")

			for _, got := range statuses {
				require.NotNilf(t, got.fields, "packet #%d must decode at all", got.index)

				assert.Zerof(t, got.fields.CallStatus&oerEndOfCallBit,
					"packet #%d CallStatus=%#x: an OCI status OER carries no end-of-call bit, which is "+
						"why demanding one would complete nothing", got.index, got.fields.CallStatus)
				assert.Falsef(t, got.compressed,
					"packet #%d: the compressed end-of-call reading is exactly what could not see it",
					got.index)

				assert.Truef(t, got.accepted, "packet #%d: decodeOERAt must read it now", got.index)
				assert.Truef(t, got.completed,
					"packet #%d: %q must be completed by its own OER, not left pending for the next "+
						"statement's flushPendingQuery", got.index, got.sql)
			}
		})
	}
}

// ociThinkTime is how long the replay below sits idle after each status OER. It
// stands in for a client that got its answer and has not sent anything since —
// exactly the gap a statement left pending is charged for.
const ociThinkTime = 250 * time.Millisecond

// TestDumpReplay_OCIStatusOERDoesNotChargeTheClientsThinkTime is the duration_ms
// half of the symptom, measured rather than reasoned about.
//
// The replay pauses after every standalone status OER. A statement completed by
// its own OER is timed to the OER; one left pending until the next statement
// flushes it is timed past the pause. So every recorded duration has to come in
// under the pause — and before this change none of the five did, because the last
// one was not even recorded until cleanup.
func TestDumpReplay_OCIStatusOERDoesNotChargeTheClientsThinkTime(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := &collectingCompletionStore{}
	s.completionStore = recorder

	statuses := walkStandaloneStatusOERs(t, s, sqlplusReexecDump, ociThinkTime)
	require.Len(t, statuses, ociStatusDumps()[sqlplusReexecDump])

	// Both store writes are asynchronous.
	var recorded []*store.Query

	deadline := time.Now().Add(5 * time.Second)
	for {
		recorded = recorder.createdQueries()
		if len(recorded) >= len(statuses) || time.Now().After(deadline) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.NotEmpty(t, recorded, "the statements must be persisted at all")
	assert.Len(t, recorded, len(statuses),
		"every statement must be recorded by its own OER — the last one used to wait for cleanup")

	for _, q := range recorded {
		require.NotNilf(t, q.DurationMs, "%.40q must be timed", q.SQLText)
		assert.Lessf(t, *q.DurationMs, float64(ociThinkTime.Milliseconds()),
			"%.40q was logged at %.0fms: a statement completed by its own OER cannot be charged "+
				"the idle time that follows it", q.SQLText, *q.DurationMs)
	}
}

// recordedOCIStatusOER returns the bytes of the genuine standalone fixed-width
// status OER the sqlplus recording's login probe ends on — packet #20, CallStatus
// 0x1, ORA-01403, cursor 1 — together with the shape the session had learned by
// then.
//
// The tests below mutate these rather than hand-laying a second copy of the
// layout's offsets: there is no encoder to round-trip against here, because
// encodeOER always writes an error and always sets the bit.
func recordedOCIStatusOER(t *testing.T) ([]byte, oerShape) {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

	for _, got := range walkStandaloneStatusOERs(t, s, sqlplusMidFetchDump, 0) {
		if !got.isStatus() {
			continue
		}

		require.Equal(t, oraNoDataFound, got.fields.ErrorCode)

		// The shape is re-read off a fresh replay stopped at that packet: the
		// walk above runs to the end of the recording.
		return replayUntilPacket(t, sqlplusMidFetchDump, got.index)
	}

	t.Fatal("the recording carries no standalone status OER")

	return nil, oerShape{}
}

// replayUntilPacket replays a recording up to (not including) `index` and returns
// the TTC payload of the packet at `index` plus the shape the session had learned
// by then.
func replayUntilPacket(t *testing.T, name string, index int) ([]byte, oerShape) {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = drainedPipe(t)

	for i, pkt := range loadTestDump(t, name).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if i == index {
			return extractTTCPayload(tns.Payload), s.oerShapeSnapshot()
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		s.interceptUpstreamMessage(tns)
	}

	t.Fatalf("%s has no packet #%d", name, index)

	return nil, oerShape{}
}

// patchFixedStatusOER rewrites the error number (and the RetCode that has to
// repeat it), the cursor id and the row count of a recorded 32-bit fixed-width
// summary object. The row count is the one field that layout leaves as a TTC
// compressed integer, so it is spliced rather than overwritten.
func patchFixedStatusOER(t *testing.T, block []byte, errCode, cursorID, rowCount int) []byte {
	t.Helper()

	require.Greater(t, len(block), oerFixedWidthPrefixLen)

	out := append([]byte(nil), block[:oerFixedWidthPrefixLen]...)
	binary.LittleEndian.PutUint16(out[oerFixed32Layout.errNum:], uint16(errCode))
	binary.LittleEndian.PutUint32(out[oerFixed32Layout.retCode:], uint32(errCode))
	binary.LittleEndian.PutUint16(out[oerFixed32Layout.cursorID:], uint16(cursorID))

	_, width := readCompressedInt(block[oerFixedWidthPrefixLen:])
	require.Positive(t, width, "the recorded row count must decode")

	out = append(out, ttcCompressedUint(uint64(rowCount))...)
	out = append(out, block[oerFixedWidthPrefixLen+width:]...)

	return out
}

// TestHandleOERStatus_FixedWidthSuccessOERRecordsItsRowCount is the
// rows_affected half of the symptom.
//
// A DML's standalone status OER reports success with the affected-row count in
// CurRowNumber. Refused, the statement was closed by the next one's
// flushPendingQuery, which knows no row count at all and left rows_affected NULL.
// The frame is the recorded ORA-01403 summary with its error number zeroed and a
// row count spliced in, so the layout under test is one a real server sent.
func TestHandleOERStatus_FixedWidthSuccessOERRecordsItsRowCount(t *testing.T) {
	t.Parallel()

	const (
		wantRows = 20000
		cursorID = 7
	)

	recorded, shape := recordedOCIStatusOER(t)
	require.True(t, shape.fixedWidth, "the OCI session must have learned the fixed-width encoding")

	oer := patchFixedStatusOER(t, recorded, 0, cursorID, wantRows)

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.oer = shape
	recorder := &collectingCompletionStore{}
	s.completionStore = recorder

	require.NoError(t, s.handleOALL8(buildOALL8("UPDATE emp SET sal = sal * 2", nil, cursorID)))
	require.False(t, s.rowStreamActive(), "a DML opens no row stream")

	s.handleOERStatus(oer)
	require.Nil(t, s.tracker.pendingQuery, "the success status must end the call")

	var recordedQueries []*store.Query

	deadline := time.Now().Add(5 * time.Second)
	for {
		recordedQueries = recorder.createdQueries()
		if len(recordedQueries) > 0 || time.Now().After(deadline) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	require.Len(t, recordedQueries, 1, "the DML must be persisted")

	got := recordedQueries[0]
	assert.Nil(t, got.Error, "a success status is not an error")
	require.NotNil(t, got.RowsAffected,
		"the affected-row count is on the wire; refusing the OER is what left it NULL")
	assert.Equal(t, int64(wantRows), *got.RowsAffected)
}

// TestDecodeFixedStatusOERAt_KeepsItsAnchors pins what the bit-less fixed-width
// status reading refuses. Each case is a way real bytes could arrive looking like
// one, and a false positive here ends a live statement early — truncating its
// capture and stopping mid-stream quota enforcement.
func TestDecodeFixedStatusOERAt_KeepsItsAnchors(t *testing.T) {
	t.Parallel()

	recorded, shape := recordedOCIStatusOER(t)

	require.NotNil(t, decodeFixedStatusOERAt(shape, recorded, 0),
		"the control must decode, or the cases below prove nothing")

	// Offset. The reading is for a 0x04 that was the packet's own leading byte,
	// never one a scan found inside a payload — which is what leaves
	// findOERInResponse's mid-row-stream scan as strict as it was.
	shifted := append([]byte{0x08, 0x01, 0x06}, recorded...)
	assert.Nil(t, decodeFixedStatusOERAt(shape, shifted, 3),
		"a summary object found at a non-zero offset must not be read this way")
	assert.Nil(t, findOERInResponse(shape, shifted),
		"...and the Response locator, which only ever scans non-zero offsets, must still find nothing")

	// A learned compressed session is never offered the reading at all.
	assert.Nil(t, decodeFixedStatusOERAt(thinOERShape(), recorded, 0),
		"a session that learned the compressed encoding must not read a fixed-width block")

	// Neither is a session that has learned nothing yet. This reading runs before
	// decodeErrorOER, so the unlearned two-layout fallback would be a way for a
	// bit-less *error* OER to be completed as a status and lose its ORA text.
	unlearned := defaultOERShape()
	require.False(t, unlearned.tailLearned)
	assert.Nil(t, decodeFixedStatusOERAt(unlearned, recorded, 0),
		"the shape must be learned; the unlearned fallback is not offered here")

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "a real failure is decodeErrorOER's business, not a status",
			payload: patchFixedStatusOER(t, recorded, 1722, 5, 14999),
		},
		{
			name:    "an OER assigning no cursor",
			payload: patchFixedStatusOER(t, recorded, oraNoDataFound, 0, 1),
		},
		{
			name:    "the RetCode does not repeat the error number",
			payload: mutateFixedStatusOER(recorded, oerFixed32Layout.retCode, 943),
		},
		{
			name:    "no call status",
			payload: mutateFixedStatusOER(recorded, oerFixed32Layout.callStatus, 0),
		},
		{name: "truncated before the row count", payload: recorded[:oerFixedWidthPrefixLen]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Nil(t, decodeFixedStatusOERAt(shape, tc.payload, 0))
		})
	}
}

// mutateFixedStatusOER overwrites one 32-bit field of a copy of block.
func mutateFixedStatusOER(block []byte, offset int, value uint32) []byte {
	out := append([]byte(nil), block...)
	binary.LittleEndian.PutUint32(out[offset:], value)

	return out
}

// TestHandleOERStatus_MidRowStreamRefusesABitLessStatusOER is the caller's half of
// the bound, and the one the corpus measurement demands.
//
// An OCI fetch response carries a genuine summary object of exactly this shape
// inside every continuation packet — naming the streaming cursor, reporting the
// fetch's running row count — so naming the cursor proves nothing here and only a
// packet boundary separates such an object from byte 0. Mid-stream, only a proven
// diagnostic may end a call; a bare status may not, however well formed.
//
// An OER that carries the end-of-call bit is unaffected: that is the protocol
// saying the call is over, and it is the reading that always applied.
func TestHandleOERStatus_MidRowStreamRefusesABitLessStatusOER(t *testing.T) {
	t.Parallel()

	const streamingCursor = 5

	recorded, shape := recordedOCIStatusOER(t)
	status := patchFixedStatusOER(t, recorded, oraNoDataFound, streamingCursor, 14999)

	withBit := append([]byte(nil), status...)
	binary.LittleEndian.PutUint32(withBit[oerFixed32Layout.callStatus:], oerEndOfCallBit|1)

	tests := []struct {
		name     string
		oer      []byte
		complete bool
	}{
		{name: "a bit-less status naming the streaming cursor", oer: status},
		{name: "the same status with the end-of-call bit", oer: withBit, complete: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			s.oer = shape

			require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, streamingCursor)))
			s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
			require.True(t, s.rowStreamActive())

			s.handleOERStatus(tc.oer)

			if tc.complete {
				assert.Nil(t, s.tracker.pendingQuery,
					"the end-of-call bit is the protocol saying the call is over")

				return
			}

			assert.NotNil(t, s.tracker.pendingQuery,
				"mid-fetch, a bare status is the object every continuation packet carries — "+
					"completing on it ends the call in the middle of the stream")
		})
	}
}

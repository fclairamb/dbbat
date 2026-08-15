package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// This file covers the OERs that carry no end-of-call bit on a *successful*
// call — which, on the successful calls, is a client shape.
//
// The scoping matters: read unqualified, "the client whose OERs carry no bit"
// is false. Measured later across six failure shapes, the bit tracks the call
// rather than the client, and go-ora emits bit-less OERs too — see
// failed_stmt_replay_test.go. What follows is true of the successful DML this
// file replays, and of nothing wider.
//
// python-oracledb thin negotiates a session in which the server's OERs come
// with CallStatus 1–2 rather than the 0x010001 go-ora sees, and dbbat used to
// require that bit before it would complete a query from an OER. Every
// INSERT/UPDATE/DELETE such a client ran therefore stayed pending until the
// *next* statement flushed it: rows_affected NULL, and a duration_ms that
// measured how long the human took to type the next line — 74 seconds, on the
// live session that produced the byte sequence below.
//
// The 0x04 run replayed here is verbatim from that session (dbbat.tools.stonal.io,
// connection 019ff66d-d9c3-7f29-8685-abd2373d59b1, an UPDATE that changed one
// row); the return-parameter block it is embedded in is modeled on the
// python-oracledb and go-ora captures in testdata rather than copied from the
// live dump, which is not in the repository. So the OER is real and the
// envelope around it is derived — enough for the decode path under test, which
// only ever locates the 0x04 run inside a Response payload.

// pythonThinOERBytes is the OER as the live session sent it:
//
//	04 | 01 02 | 02 df 62 | 01 01 | 00 | 00 | 00 | 01 04
//	     status=2  seq=57186  rows=1  err=0            cursor=4
//
// Note what is and is not there: CurRowNumber is 1 — the affected-row count is
// on the wire and always was — and CallStatus is 2, so `CallStatus &
// oerEndOfCallBit` is zero and decodeOERAt refuses it.
var pythonThinOERBytes = []byte{0x04, 0x01, 0x02, 0x02, 0xdf, 0x62, 0x01, 0x01, 0x00, 0x00, 0x00, 0x01, 0x04}

// pythonThinOEROffset is where the run sits in the Response payload.
const pythonThinOEROffset = 19

// pythonThinDMLResponse builds the 91-byte Response (func=0x08) that closes a
// python-oracledb thin DML call: a return-parameter block, the OER above at
// offset 19, then the summary tail.
//
// Two properties of the envelope are load-bearing and asserted below rather
// than assumed. Nothing before offset 19 may decode as a plausible OER, or the
// scan would stop on the wrong run; and the payload must end non-zero, because
// that is what makes the legacy fixed-offset decoder read MoreData and fall
// through — the fall-through being exactly how the pending DML survived to be
// flushed by the next statement.
func pythonThinDMLResponse() []byte {
	payload := []byte{
		byte(TTCFuncResponse), 0x01, 0x06, 0x03, 0x20, 0xdf, 0x62, 0x00, 0x01, 0x09,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	payload = append(payload, pythonThinOERBytes...)

	for len(payload) < 89 {
		payload = append(payload, 0x00)
	}

	return append(payload, 0x01, 0x01)
}

// upstreamPacket wraps a TTC payload the way the relay hands it to the
// interceptor: TNS Data, two data-flag bytes, then the function code.
func upstreamPacket(ttcPayload []byte) *TNSPacket {
	return &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, ttcPayload...)}
}

// awaitCreated waits for the asynchronous CreateQuery a DML completion makes.
func awaitCreated(t *testing.T, r *recordingCompletionStore) *store.Query {
	t.Helper()

	select {
	case got := <-r.created:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("no query record was created")

		return nil
	}
}

// TestPythonThinDMLResponse_FixtureShape pins the fixture itself, so a failure
// in the tests below is a failure of the decode path and not of the bytes they
// are fed.
func TestPythonThinDMLResponse_FixtureShape(t *testing.T) {
	t.Parallel()

	payload := pythonThinDMLResponse()
	require.Len(t, payload, 91)

	info, _ := decodeOERFieldsAt(payload, pythonThinOEROffset)
	require.NotNil(t, info, "the live OER must decode as seven compressed ints")
	assert.Equal(t, 2, info.CallStatus)
	assert.Equal(t, 57186, info.SeqNumber)
	assert.Equal(t, 1, info.CurRowNumber, "the affected-row count is on the wire")
	assert.Equal(t, 0, info.ErrorCode)
	assert.Equal(t, 4, info.CursorID)

	assert.Zero(t, info.CallStatus&oerEndOfCallBit, "this client's OERs carry no end-of-call bit")
	assert.Nil(t, findOERInResponse(thinOERShape(), payload), "which is why the strict locator finds nothing here")

	// Nothing earlier in the envelope may be mistaken for the OER — first match
	// wins, so a decoy in the return-parameter block would make the tests below
	// pass on the wrong bytes.
	assert.Nil(t, findPlausibleOERInResponse(thinOERShape(), payload[:pythonThinOEROffset]),
		"the return-parameter block must hold no OER-shaped decoy")

	found := findPlausibleOERInResponse(thinOERShape(), payload)
	require.NotNil(t, found, "the relaxed scan is what has to find it")
	assert.Equal(t, 1, found.CurRowNumber)
	assert.Equal(t, 4, found.CursorID)

	resp, err := decodeTTCResponse(payload)
	require.NoError(t, err)
	assert.True(t, resp.MoreData,
		"the legacy decoder must fall through — that fall-through is what left the DML pending")
}

// TestHandleResponse_DMLCompletesWithoutTheEndOfCallBit is the regression: the
// Response that ends a python-oracledb thin UPDATE must finish the query then
// and there, with the row count the OER carries.
//
// It is driven through interceptUpstreamMessage rather than handleResponse so
// the routing is covered too: the same payload is what cursor-id learning reads,
// and both now come off one scan.
func TestHandleResponse_DMLCompletesWithoutTheEndOfCallBit(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	require.NoError(t, s.handleOALL8(buildOALL8("UPDATE emp SET sal = sal + 1 WHERE id = 7", nil, 4)))
	require.NotNil(t, s.tracker.pendingQuery)

	s.interceptUpstreamMessage(upstreamPacket(pythonThinDMLResponse()))

	require.Nil(t, s.tracker.pendingQuery,
		"the DML must be complete when its own response arrives, not when the next statement shows up")

	got := awaitCreated(t, recorder)
	require.NotNil(t, got.RowsAffected)
	assert.Equal(t, int64(1), *got.RowsAffected)
	assert.Nil(t, got.Error)
	assert.Equal(t, "UPDATE emp SET sal = sal + 1 WHERE id = 7", got.SQLText)
}

// TestHandleResponse_DMLDurationStopsAtTheResponse is the other half of the
// live symptom. The 74-second UPDATE was not slow; it was timed to whatever the
// client did next. Completing on the response is what bounds duration_ms to the
// call, so an idle gap afterwards must not land in it.
func TestHandleResponse_DMLDurationStopsAtTheResponse(t *testing.T) {
	t.Parallel()

	const idle = 300 * time.Millisecond

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	require.NoError(t, s.handleOALL8(buildOALL8("DELETE FROM emp WHERE id = 7", nil, 4)))
	s.interceptUpstreamMessage(upstreamPacket(pythonThinDMLResponse()))

	// The session then sits idle before anything else happens — the next
	// statement, or the disconnect. Either way it must not be charged here.
	time.Sleep(idle)
	s.flushPendingQuery()

	got := awaitCreated(t, recorder)
	require.NotNil(t, got.DurationMs)
	assert.Lessf(t, *got.DurationMs, float64(idle.Milliseconds()),
		"duration_ms measured the idle gap, not the call")

	select {
	case second := <-recorder.created:
		t.Fatalf("the query was recorded twice, the second time as %q", second.SQLText)
	default:
	}
}

// TestSessionClose_LastDMLIsSealedByItsOwnResponse covers the close path the
// spec asked about. cleanup() finishes a session by taking trackerMu and calling
// flushPendingQuery, which completes with completeQuery(nil, nil) — no row
// count, and a duration running from the statement to the disconnect. Before
// this fix that is what sealed a session whose last statement was DML: the
// worst case of the bug, since a client that closes its connection after an
// UPDATE has no "next statement" to flush it any earlier.
//
// The fix removes the case rather than improving it — by the time close runs
// there is nothing pending — which is what this pins.
func TestSessionClose_LastDMLIsSealedByItsOwnResponse(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	require.NoError(t, s.handleOALL8(buildOALL8("INSERT INTO emp (id) VALUES (7)", nil, 4)))
	s.interceptUpstreamMessage(upstreamPacket(pythonThinDMLResponse()))

	require.Nil(t, s.tracker.pendingQuery, "nothing is left for close to seal")

	got := awaitCreated(t, recorder)
	require.NotNil(t, got.RowsAffected)
	assert.Equal(t, int64(1), *got.RowsAffected, "the row count survives to the record close would have missed")

	// What cleanup() does, minus the socket teardown it also does.
	s.trackerMu.Lock()
	s.flushPendingQuery()
	s.trackerMu.Unlock()

	select {
	case second := <-recorder.created:
		t.Fatalf("close recorded the DML a second time as %q", second.SQLText)
	default:
	}
}

// TestHandleOERStatus_StillRequiresTheEndOfCallBit is the guard on the path the
// relaxation must not reach. A standalone func=0x04 message is routed on its
// first byte alone, so anything whose first byte is 0x04 — a row value's length
// prefix among them — arrives here claiming to be an OER.
//
// For a *success* OER, which is what this replays, the bit is still the only
// thing separating those, and it stays required: there is no error text such a
// run could be proved with. A later measurement found that failures do arrive
// here without the bit and carry a diagnostic that proves them — see
// failed_stmt_replay_test.go, which pins how narrow that opening is.
func TestHandleOERStatus_StillRequiresTheEndOfCallBit(t *testing.T) {
	t.Parallel()

	t.Run("no bit, no completion", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		require.NoError(t, s.handleOALL8(buildOALL8("UPDATE emp SET sal = 1", nil, 4)))

		// The very bytes handleResponse now completes on, arriving as a
		// standalone marker instead of inside a Response.
		s.handleOERStatus(pythonThinOERBytes)

		assert.NotNil(t, s.tracker.pendingQuery,
			"a bare 0x04 run without the end-of-call bit must not complete a query")
	})

	t.Run("with the bit, completion as before", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		require.NoError(t, s.handleOALL8(buildOALL8("UPDATE emp SET sal = 1", nil, 4)))

		s.handleOERStatus(buildOER(oerEndOfCallBit|2, 57186, 1, 0))

		assert.Nil(t, s.tracker.pendingQuery, "a genuine end-of-call OER still completes")
	})
}

// TestHandleResponse_MidRowStreamStillRequiresTheEndOfCallBit is the second
// guard. Mid-fetch, a payload whose lead byte reads as 0x08 is row-stream
// content, and a 0x04 run inside it is row data — which is precisely the false
// positive the bit was introduced to reject. Completion there still demands it;
// the relaxed scan runs only once the stream is over.
func TestHandleResponse_MidRowStreamStillRequiresTheEndOfCallBit(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 4)))

	// Columns known = rows are streaming.
	s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
	require.True(t, s.rowStreamActive())

	payload := pythonThinDMLResponse()
	require.NotNil(t, findPlausibleOERInResponse(thinOERShape(), payload), "the relaxed scan would accept these bytes")

	s.handleResponse(payload)

	assert.NotNil(t, s.tracker.pendingQuery,
		"mid-fetch, a bit-less OER-shaped run is row data and must not end the fetch")
}

// TestFindPlausibleOERInResponse_KeepsItsAnchors pins the bounds the relaxed
// scan inherited from cursor-id learning. They are what stands in for the bit
// outside a row stream, so loosening one silently is the way this fix becomes a
// bug of its own.
func TestFindPlausibleOERInResponse_KeepsItsAnchors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		oer    []byte
		accept bool
	}{
		{name: "success with a cursor", oer: buildOER(2, 57186, 1, 0), accept: true},
		{name: "end of data", oer: buildOER(2, 57186, 0, oraNoDataFound), accept: true},
		{name: "a real failure assigns nothing", oer: buildOER(2, 57186, 0, 942), accept: false},
		{name: "no cursor id", oer: buildOER(2, 57186, 1, 0), accept: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			oer := tc.oer
			if tc.name != "no cursor id" {
				// buildOER leaves the cursor id at 0; give the accepted shapes
				// the plausible one the live capture carried.
				oer = append(oer[:len(oer)-1], ttcCompressedUint(4)...)
			}

			payload := append([]byte{byte(TTCFuncResponse), 0x01}, oer...)

			if tc.accept {
				require.NotNil(t, findPlausibleOERInResponse(thinOERShape(), payload))
			} else {
				require.Nil(t, findPlausibleOERInResponse(thinOERShape(), payload))
			}
		})
	}
}

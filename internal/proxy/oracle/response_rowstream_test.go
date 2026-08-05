package oracle

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// This file covers the mid-fetch false-error defect: a TNS packet arriving in
// the middle of a result set whose first byte happens to be 0x08 was routed to
// handleResponse, decoded through the legacy fixed-offset layout, and turned
// into a bogus Query.Error — which also cleared pendingQuery and so silently
// stopped row capture, mid-stream quota enforcement (upstreamToClient gates
// s.guard.Check() on pendingQuery != nil) and per-query byte accounting for the
// rest of the fetch.

// capturedRowStreamPayloads pulls the real row-stream packets of the DBeaver
// capture's large SELECT (SEGMENT_NAME, TABLE_SIZE) and returns them with their
// lead byte rewritten from 0x06 to 0x08. The bytes after the lead byte are
// untouched capture data.
//
// Scope of what this proves, precisely: relabeling byte 0 reproduces the
// *routing* condition faithfully — interceptUpstreamMessage reads byte 0 as a
// function code and hands the packet to handleResponse — so these cases prove
// the query is not falsely completed and its rows are still decoded. They do
// NOT prove byte-accurate capture of a genuine mid-fetch 0x08 packet: there,
// byte 0 is row-stream *data* (a value-length prefix, or the first byte of the
// 08 01 06 footer), whereas parseContinuationRows skips index 0 as a function
// code and starts scanning at 1. If a real packet of that shape carries a value
// at offset 0, the decode would be one byte out of alignment. No capture in
// testdata contains one — every fixture's continuations are cleanly framed as
// func=0x06 — so that alignment case is untested here rather than faked.
func capturedRowStreamPayloads(t *testing.T) ([]columnDef, [][]byte) {
	t.Helper()

	td := loadTestDump(t, "dbeaver.pcapng")

	var (
		columns  []columnDef
		payloads [][]byte
	)

	for _, pkt := range td.Packets {
		if pkt.Direction != dump.DirServerToClient {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		ttcPayload := extractTTCPayload(tns.Payload)

		switch TTCFunctionCode(ttcPayload[0]) { //nolint:exhaustive // only the two row-stream carriers matter here
		case TTCFuncQueryResult:
			// The first QueryResult of the capture opens the row stream and is
			// what teaches the session its column definitions.
			if columns != nil {
				continue
			}

			result := decodeQueryResultV2(ttcPayload)
			if result == nil || len(result.Columns) == 0 {
				continue
			}

			columns = make([]columnDef, len(result.Columns))
			for i, name := range result.Columns {
				columns[i] = columnDef{Name: name}
				if i < len(result.ColumnTypes) {
					columns[i].TypeCode = uint8(result.ColumnTypes[i])
				}
			}
		case TTCFuncContinuation:
			if columns == nil {
				continue
			}

			relabeled := make([]byte, len(ttcPayload))
			copy(relabeled, ttcPayload)
			relabeled[0] = byte(TTCFuncResponse)

			payloads = append(payloads, relabeled)
		default:
		}

		// The large SELECT's continuations all arrive before the next
		// QueryResult; a handful is plenty for the regression.
		if len(payloads) >= 4 {
			break
		}
	}

	require.NotEmpty(t, columns, "capture should yield column definitions")
	require.NotEmpty(t, payloads, "capture should yield continuation packets")

	return columns, payloads
}

// TestHandleResponse_MidFetchRowStream is the regression: while rows are
// streaming, a payload whose lead byte reads as TTCFuncResponse must be decoded
// as row-stream continuation — capturing its rows and leaving pendingQuery in
// place — instead of being read as a legacy Response.
func TestHandleResponse_MidFetchRowStream(t *testing.T) {
	t.Parallel()

	captureColumns, capturePayloads := capturedRowStreamPayloads(t)

	tests := []struct {
		name    string
		columns []columnDef
		payload []byte
	}{
		{
			name:    "synthetic row stream shaped like the production packet",
			columns: []columnDef{{Name: "NUM_ATTR"}, {Name: "NUM_COMP"}, {Name: "VALEUR"}},
			payload: buildMidFetchRowStreamPayload(3, 40),
		},
		{
			name:    "captured continuation relabeled as a Response",
			columns: captureColumns,
			payload: capturePayloads[0],
		},
		{
			name:    "captured continuation relabeled as a Response (large packet)",
			columns: captureColumns,
			payload: capturePayloads[len(capturePayloads)-1],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, rowStore, _ := newCapturingSession(t, 10000)
			s.tracker.pendingQuery.cursor.columns = tt.columns

			s.handleResponse(tt.payload)

			// The query must still be pending. This is what keeps row capture
			// running, keeps s.guard.Check() armed in upstreamToClient, and
			// keeps the rest of the stream's bytes charged to this query.
			pending := s.tracker.pendingQuery
			require.NotNil(t, pending, "a mid-fetch 0x08 packet must not complete the query")

			pending.rowSink.Flush(context.Background())

			assert.NotEmpty(t, rowStore.rowNumbers(), "the packet's rows must be captured")
			assert.False(t, pending.truncated, "capture must not be marked truncated")

			// And nothing in those bytes may be readable as an error: the
			// legacy decoder either rejects them outright or finds no error.
			resp, decErr := decodeTTCResponse(tt.payload)
			if decErr == nil {
				assert.False(t, resp.IsError,
					"row-stream bytes must never yield an error string: %q", resp.ErrorMessage)
			} else {
				assert.ErrorIs(t, decErr, ErrNotLegacyResponse)
			}
		})
	}
}

// TestHandleResponse_MidFetchOERStillCompletes keeps the real call boundary
// working: an embedded OER carries the end-of-call bit, so a Response holding
// one completes the query even while the row stream is open.
func TestHandleResponse_MidFetchOERStillCompletes(t *testing.T) {
	t.Parallel()

	s := newTestSessionWithStorage(&store.Grant{}, true, 100, 1<<20)
	require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 1)))

	s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}

	payload := append([]byte{byte(TTCFuncResponse), 0x01}, buildOER(oerEndOfCallBit, 1, 7, 0)...)

	s.handleResponse(payload)

	assert.Nil(t, s.tracker.pendingQuery, "an end-of-call OER must complete the query")
}

// completedQuery is what a query recorded when it finished.
type completedQuery struct {
	uid          uuid.UUID
	rowsAffected *int64
	queryError   *string
	truncated    bool
}

// recordingCompletionStore stands in for the store on the completion path so a
// test can assert what a completed query actually persists.
type recordingCompletionStore struct {
	completed chan completedQuery
	created   chan *store.Query
}

func newRecordingCompletionStore() *recordingCompletionStore {
	return &recordingCompletionStore{
		completed: make(chan completedQuery, 4),
		created:   make(chan *store.Query, 4),
	}
}

func (r *recordingCompletionStore) CreateQuery(_ context.Context, query *store.Query) (*store.Query, error) {
	r.created <- query

	return query, nil
}

func (r *recordingCompletionStore) UpdateQueryCompletion(
	_ context.Context,
	uid uuid.UUID,
	_ *float64,
	rowsAffected *int64,
	queryError *string,
	resultsTruncated bool,
	_ bool,
) error {
	r.completed <- completedQuery{
		uid:          uid,
		rowsAffected: rowsAffected,
		queryError:   queryError,
		truncated:    resultsTruncated,
	}

	return nil
}

func (r *recordingCompletionStore) IncrementConnectionStats(_ context.Context, _ uuid.UUID, _ int64) error {
	return nil
}

// awaitCompletion waits for the asynchronous completion write.
func (r *recordingCompletionStore) awaitCompletion(t *testing.T) completedQuery {
	t.Helper()

	select {
	case got := <-r.completed:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("query completion was never written")

		return completedQuery{}
	}
}

// TestHandleResponse_MidFetchErrorOERIsRecorded is the over-correction guard:
// the dangerous way to get item 1 wrong is to swallow a genuine ORA error that
// arrives while rows are streaming. A Response carrying an error OER must both
// complete the query and land its message in the stored record.
func TestHandleResponse_MidFetchErrorOERIsRecorded(t *testing.T) {
	t.Parallel()

	const message = "ORA-00942: table or view does not exist"

	s, _, queryUID := newCapturingSession(t, 10000)

	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	// Rows are streaming: columns are known, so the mid-fetch guard is active.
	s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}

	oer := buildOER(oerEndOfCallBit, 1, 0, 942)

	payload := make([]byte, 0, 4+len(oer)+len(message))
	payload = append(payload, byte(TTCFuncResponse), 0x01)
	payload = append(payload, oer...)
	payload = append(payload, 0x00, 0x00)
	payload = append(payload, message...)

	s.handleResponse(payload)

	require.Nil(t, s.tracker.pendingQuery, "an error OER must complete the query")

	got := recorder.awaitCompletion(t)
	assert.Equal(t, queryUID, got.uid)
	require.NotNil(t, got.queryError, "a genuine mid-fetch ORA error must not be swallowed")
	assert.Equal(t, message, *got.queryError)
}

// TestCompleteQuery_DropsNonDiagnosticError checks the sink guard end to end on
// the Oracle path: bytes that are not readable text never reach the record.
func TestCompleteQuery_DropsNonDiagnosticError(t *testing.T) {
	t.Parallel()

	s, _, _ := newCapturingSession(t, 10000)

	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	rowData := "\x15\x03\x07\x01\xc2\x0a1-Habitation\x07Non défini"
	s.completeQuery(nil, &rowData)

	got := recorder.awaitCompletion(t)
	assert.Nil(t, got.queryError, "row bytes must never be persisted as an error")
}

// buildMidFetchRowStreamPayload synthesizes the production packet's shape: a
// lead byte of 0x08, a continuation-style header, then numRows compressed rows
// with 0x15 column descriptors between them. The bytes at the legacy layout's
// fixed offsets (2:6 error code, 12:14 error flag, 14:16 message length) are
// row content, which is exactly what the old decoder read as a 772-byte error.
func buildMidFetchRowStreamPayload(numCols, numRows int) []byte {
	payload := []byte{
		byte(TTCFuncResponse), // lead byte read as a TTC function code
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x07, // header: bitmask byte then the 0x07 that starts the rows
	}

	for row := range numRows {
		for col := range numCols {
			value := strings.Repeat("A", 1+col) + "-" + string(rune('0'+row%10))
			payload = append(payload, byte(len(value)))
			payload = append(payload, value...)
		}

		if row == numRows-1 {
			break
		}

		// 0x15 [flag] [count] [bitmask] 0x07 — every column changes next row.
		payload = append(payload, continuationDescriptorMarker, 0x00, byte(numCols))
		payload = append(payload, byte(1<<numCols-1), 0x07)
	}

	// End-of-rows footer.
	return append(payload, 0x08, 0x01, 0x06)
}

// TestDecodeTTCResponse_LegacyErrorGate covers the second half of the fix: the
// legacy fixed-offset layout may only report an error when the bytes it
// extracts really are an Oracle diagnostic.
func TestDecodeTTCResponse_LegacyErrorGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		errCode   uint32
		message   string
		wantError bool
	}{
		{name: "real ORA diagnostic", errCode: 942, message: "ORA-00942: table or view does not exist", wantError: true},
		{name: "real PLS diagnostic", errCode: 201, message: "PLS-00201: identifier must be declared", wantError: true},
		{name: "real TNS diagnostic", errCode: 12514, message: "TNS-12514: listener does not know of service", wantError: true},
		{name: "row text without a diagnostic prefix", errCode: 942, message: "1-Habitation Non défini PVC NR", wantError: false},
		{name: "implausible error code", errCode: 805376787, message: "ORA-00942: table or view does not exist", wantError: false},
		{name: "binary row bytes", errCode: 942, message: "\x15\x03\x07\x01\xc2\x0a", wantError: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			payload := make([]byte, 16+len(tt.message))
			payload[0] = byte(TTCFuncResponse)
			payload[1] = 0x01
			binary.BigEndian.PutUint32(payload[2:6], tt.errCode)
			binary.BigEndian.PutUint16(payload[12:14], 1)
			binary.BigEndian.PutUint16(payload[14:16], uint16(len(tt.message)))
			copy(payload[16:], tt.message)

			resp, err := decodeTTCResponse(payload)

			if !tt.wantError {
				require.ErrorIs(t, err, ErrNotLegacyResponse,
					"non-diagnostic bytes must be rejected, never surfaced as an error")

				return
			}

			require.NoError(t, err)
			assert.True(t, resp.IsError)
			assert.Equal(t, tt.message, resp.ErrorMessage)
		})
	}
}

// TestDecodeTTCResponse_NoFabricatedErrorsInCaptures replays every Response
// packet of every capture: none of them may yield an error string, because none
// of these sessions ran a failing statement. Before the fix each one produced a
// bogus "ORA-<huge number>" from misread bytes.
func TestDecodeTTCResponse_NoFabricatedErrorsInCaptures(t *testing.T) {
	t.Parallel()

	files := []string{
		"go_ora.pcapng", "go_ora_largeresult.pcapng", "go_ora_compressed.pcapng",
		"dbeaver.pcapng", "python_thin.pcapng",
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			td := loadTestDump(t, file)

			for _, pkt := range td.Packets {
				if pkt.Direction != dump.DirServerToClient {
					continue
				}

				tns, err := parseTNSFromDumpPacket(pkt.Data)
				if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
					continue
				}

				ttcPayload := extractTTCPayload(tns.Payload)
				if TTCFunctionCode(ttcPayload[0]) != TTCFuncResponse {
					continue
				}

				resp, decErr := decodeTTCResponse(ttcPayload)
				if decErr == nil {
					assert.False(t, resp.IsError,
						"no capture contains a failing statement: %q", resp.ErrorMessage)
				}
			}
		})
	}
}

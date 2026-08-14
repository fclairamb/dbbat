package oracle

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// What a failure raised *after* rows have started flowing puts on the wire,
// measured rather than reasoned about.
//
// failed_stmt_replay_test.go measured six failures and found every one of them
// raised before the first row: the server sends the OER *instead of* the
// QueryResult, so dbbat is never inside a row stream when it arrives. That left
// the mid-fetch case unmeasured, and a `rowStreamActive()` guard dropping it.
//
// These two recordings close that. A TO_NUMBER over a 20 000-row table whose row
// 15 000 will not convert, no ORDER BY (a sort would materialize the set and
// raise before the first row, which is the shape already measured), array size
// 100: both clients fetch 14 900 rows and then take an ORA-01722 that arrives as
// a **standalone func 0x04 with CallStatus 0x1** — the same bit-less shape, only
// now with the column definitions and dozens of fetch round trips behind it.
//
// Regenerate with
// `go test -tags capture -run 'TestCapture_.*MidFetchFailure' ./internal/proxy/oracle/`.
const (
	pythonMidFetchDump = "python_thin_midfetch_fail.pcapng"
	goOraMidFetchDump  = "go_ora_midfetch_fail.pcapng"
)

// oraMidFetchCode is the ORA code both recordings raise mid-fetch, and
// oerMidFetchCallStatus the CallStatus both carry it with — no end-of-call bit,
// which is what used to make it undecodable.
const (
	oraMidFetchCode       = 1722
	oerMidFetchCallStatus = 0x1
	oerMidFetchSeqNumber  = 7
)

// midFetchDumps is the pair, for the tests that assert on both.
func midFetchDumps() []string {
	return []string{pythonMidFetchDump, goOraMidFetchDump}
}

// buildOERNamingCursor is buildOER with the seventh field — the cursor id — set.
// buildOER always writes it as 0, and that field is the one the mid-fetch anchor
// reads, so it needs its own builder rather than a wider signature on the shared
// one.
func buildOERNamingCursor(curRowNumber, errNum, cursorID int) []byte {
	out := make([]byte, 0, 16)
	out = append(out, 0x04)

	for _, v := range []int{oerMidFetchCallStatus, oerMidFetchSeqNumber, curRowNumber, errNum, 0, 0, cursorID} {
		out = append(out, ttcCompressedUint(uint64(v))...)
	}

	return out
}

// midStreamOER is one server packet that reached handleOERStatus *while the
// session considered a row stream active* — the routing condition and the
// session state together, which is what the relaxation turns on.
type midStreamOER struct {
	dump     string
	index    int
	payload  []byte
	fields   *oerInfo
	relaxed  *oerInfo
	strict   bool
	streamed uint16 // the cursor whose rows were on the wire at that moment
}

// walkMidStreamOERs replays a recording through the real intercept pipeline and
// returns every server packet whose TTC payload begins at byte 0 with func 0x04
// while `rowStreamActive()` is true — plus, for the calibration the tests need,
// how many mid-row-stream server packets went past in total.
//
// It reads `rowStreamActive()` off a real session rather than reconstructing it,
// because that predicate — a pending query whose cursor already has column
// definitions — is exactly what handleOERStatus consults, and a second
// implementation of it in a test would measure the wrong thing.
func walkMidStreamOERs(t *testing.T, name string) ([]midStreamOER, int) {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = drainedPipe(t)

	var (
		found            []midStreamOER
		midStreamPackets int
	)

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
		active := s.rowStreamActive()

		var streaming uint16
		if active {
			streaming = s.tracker.pendingQuery.cursor.cursorID
		}
		s.trackerMu.Unlock()

		if active && len(ttc) >= 2 {
			midStreamPackets++

			if TTCFunctionCode(ttc[0]) == TTCFuncOERR {
				fields, _ := decodeOERFieldsAt(ttc, 0)

				found = append(found, midStreamOER{
					dump:     name,
					index:    i,
					payload:  ttc,
					fields:   fields,
					relaxed:  decodeErrorOER(ttc),
					strict:   decodeOERAt(ttc, 0) != nil,
					streamed: streaming,
				})
			}
		}

		s.interceptUpstreamMessage(tns)
	}

	return found, midStreamPackets
}

// TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER is the measurement the
// spec asked for, pinned. The premise it settles is that a mid-fetch failure
// might not arrive as a standalone 0x04 at all — it might be folded into the row
// stream, in which case there would be nothing to relax. It is not folded in: it
// is the same shape as every failure already measured, minus the end-of-call
// bit, and it names the cursor whose rows it interrupted.
func TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER(t *testing.T) {
	t.Parallel()

	for _, name := range midFetchDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			midStream, total := walkMidStreamOERs(t, name)
			require.Positive(t, total, "the recording must contain a row stream at all")
			require.Len(t, midStream, 1, "exactly one standalone 0x04 arrives mid-fetch")

			got := midStream[0]
			require.NotNil(t, got.fields)

			assert.Equal(t, oraMidFetchCode, got.fields.ErrorCode, "packet #%d", got.index)
			assert.Zero(t, got.fields.CallStatus&oerEndOfCallBit,
				"packet #%d CallStatus=%#x — a mid-fetch failure carries the bit no more than a pre-first-row one does",
				got.index, got.fields.CallStatus)
			assert.False(t, got.strict, "the strict decoder is exactly why this used to be dropped")

			require.NotNil(t, got.relaxed, "packet #%d must be provable as a diagnostic", got.index)
			assert.Contains(t, got.relaxed.ErrorMessage, "ORA-01722")

			assert.Equal(t, int(got.streamed), got.fields.CursorID,
				"packet #%d: the OER names the cursor whose rows it interrupted", got.index)
		})
	}
}

// midStreamCorpus is every dump in testdata/, which is the ready-made corpus for
// the false-positive question: how often does a packet that arrives mid-row-
// stream look enough like a failure OER to end the call?
func midStreamCorpus(t *testing.T) []string {
	t.Helper()

	entries, err := filepath.Glob(filepath.Join("testdata", "*.pcapng"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Base(e))
	}

	sort.Strings(out)

	return out
}

// TestDumpReplay_MidStreamOERFalsePositiveRate is the number that justifies the
// relaxation, measured rather than argued.
//
// Across every recording in testdata/, replayed through a real session so
// `rowStreamActive()` is the session's own: how many server packets arrive
// mid-row-stream, how many of those begin with 0x04 at byte 0, and how many of
// *those* decodeErrorOER is willing to call a diagnostic. The answer has to be
// that the only ones accepted are the two genuine mid-fetch failures — anything
// else is row data read as an error, which is the production incident this
// guard descends from.
//
// The stress count is the same predicate run at every 0x04 offset *inside* those
// mid-stream packets. Nothing routes that way — handleOERStatus only ever sees
// byte 0 — so it is not a rate, it is a measure of how much real row data would
// have to be misread before the predicate is the weak link.
func TestDumpReplay_MidStreamOERFalsePositiveRate(t *testing.T) {
	t.Parallel()

	genuine := map[string]bool{pythonMidFetchDump: true, goOraMidFetchDump: true}

	var midStreamPackets, leading0x04, accepted, stress int

	for _, name := range midStreamCorpus(t) {
		found, total := walkMidStreamOERs(t, name)
		midStreamPackets += total
		leading0x04 += len(found)

		for _, got := range found {
			if got.relaxed == nil {
				continue
			}

			accepted++

			assert.Truef(t, genuine[name],
				"%s packet #%d: row data accepted as ORA-%05d (%q) — this is a false positive",
				name, got.index, got.relaxed.ErrorCode, got.relaxed.ErrorMessage)
		}
	}

	for _, name := range midStreamCorpus(t) {
		stress += midStreamStressAcceptances(t, name)
	}

	t.Logf("mid-row-stream server packets: %d; leading with 0x04: %d; accepted as diagnostics: %d; "+
		"stress acceptances at non-zero offsets: %d",
		midStreamPackets, leading0x04, accepted, stress)

	require.Positive(t, midStreamPackets, "the corpus must contain row streams to measure against")
	assert.Equal(t, len(genuine), accepted, "only the genuine mid-fetch failures may be accepted")
	assert.Zero(t, stress,
		"real row bytes must never satisfy the diagnostic proof, at any offset")
}

// midStreamStressAcceptances runs decodeErrorOER at every 0x04 offset inside
// every mid-row-stream packet of one recording. See the test above for why this
// is a stress figure and not a rate.
func midStreamStressAcceptances(t *testing.T, name string) int {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = drainedPipe(t)

	acceptances := 0

	for _, pkt := range loadTestDump(t, name).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		s.trackerMu.Lock()
		active := s.rowStreamActive()
		s.trackerMu.Unlock()

		if active {
			ttc := extractTTCPayload(tns.Payload)

			for off := 1; off < len(ttc); off++ {
				if ttc[off] == 0x04 && decodeErrorOER(ttc[off:]) != nil {
					acceptances++
				}
			}
		}

		s.interceptUpstreamMessage(tns)
	}

	return acceptances
}

// collectingCompletionStore keeps every query a replay persists. The buffered
// recordingCompletionStore cannot be used here: a whole recording completes far
// more statements than its channels hold, and the replay would deadlock on the
// setup DDL long before reaching the failure.
type collectingCompletionStore struct {
	mu      sync.Mutex
	created []*store.Query
}

func (c *collectingCompletionStore) CreateQuery(_ context.Context, query *store.Query) (*store.Query, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.created = append(c.created, query)

	return query, nil
}

func (c *collectingCompletionStore) UpdateQueryCompletion(
	_ context.Context, _ uuid.UUID, _ *float64, _ *int64, _ *string, _ bool, _ bool,
) error {
	return nil
}

func (c *collectingCompletionStore) IncrementConnectionStats(_ context.Context, _ uuid.UUID, _ int64) error {
	return nil
}

// errorsFor returns the recorded error text of every query whose SQL contains
// needle, once the asynchronous writes have settled.
func (c *collectingCompletionStore) errorsFor(t *testing.T, needle string) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		c.mu.Lock()

		var out []string

		for _, q := range c.created {
			if !contains(q.SQLText, needle) {
				continue
			}

			if q.Error == nil {
				out = append(out, "")

				continue
			}

			out = append(out, *q.Error)
		}

		c.mu.Unlock()

		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestDumpReplay_MidFetchFailureRecordsItsORAText is the regression, end to end:
// drive the whole recording through the proxy's own intercept paths and the
// failing SELECT must reach the store carrying ORA-01722.
//
// Before this change it reached the store carrying nothing at all — the OER was
// dropped, the statement stayed pending, and the DROP that followed closed it as
// a success through flushPendingQuery.
func TestDumpReplay_MidFetchFailureRecordsItsORAText(t *testing.T) {
	t.Parallel()

	for _, name := range midFetchDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			recorder := &collectingCompletionStore{}
			s.completionStore = recorder

			replaySession(t, s, name)

			errs := recordingErrorsForSelect(t, recorder)
			require.NotEmpty(t, errs, "the failing SELECT must be persisted at all")

			for _, got := range errs {
				assert.Contains(t, got, "ORA-01722",
					"a statement that died mid-fetch must record its ORA text, not close as a success")
			}
		})
	}
}

// recordingErrorsForSelect narrows to the one statement the fixture is about.
func recordingErrorsForSelect(t *testing.T, recorder *collectingCompletionStore) []string {
	t.Helper()

	return recorder.errorsFor(t, "TO_NUMBER(txt)")
}

// TestHandleOERStatus_MidFetchRequiresTheOERToNameTheStreamingCursor pins the
// width of the mid-stream opening, and replaces the flat refusal that used to
// live here.
//
// The proof decodeErrorOER applies is necessary but not sufficient once rows are
// flowing: a result set whose rows *carry* ORA- text — `SELECT message FROM
// error_log` — is a shape no fixture in testdata/ contains, and a row that
// happened to be laid out as an OER would clear it. Naming the streaming cursor
// is the second anchor, and it fails closed: the call simply stays open, which
// is exactly the behavior this change replaced.
func TestHandleOERStatus_MidFetchRequiresTheOERToNameTheStreamingCursor(t *testing.T) {
	t.Parallel()

	const streamingCursor = 4

	// The recorded failure's own CallStatus, row number and diagnostic; only the
	// field the anchor reads moves between the cases.
	tail := []byte("\x00\x00KORA-01722: unable to convert string value containing 'n' to a number: TXT")

	oerNaming := func(cursor int) []byte {
		return append(buildOERNamingCursor(14999, oraMidFetchCode, cursor), tail...)
	}

	require.NotNil(t, decodeErrorOER(oerNaming(streamingCursor)),
		"the synthesized OER must be provable, or this test is measuring the wrong refusal")
	require.NotNil(t, decodeErrorOER(oerNaming(streamingCursor+1)),
		"...and so must the one naming another cursor, so the cursor is what separates them")

	tests := []struct {
		name     string
		cursor   int
		complete bool
	}{
		{name: "names the streaming cursor", cursor: streamingCursor, complete: true},
		{name: "names another cursor", cursor: streamingCursor + 1},
		{name: "names no cursor", cursor: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, streamingCursor)))

			s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
			require.True(t, s.rowStreamActive())

			s.handleOERStatus(oerNaming(tc.cursor))

			if tc.complete {
				assert.Nil(t, s.tracker.pendingQuery,
					"a proven diagnostic naming the streaming cursor ends the fetch")

				return
			}

			assert.NotNil(t, s.tracker.pendingQuery,
				"mid-fetch, a 0x04 run that does not name the streaming cursor is row data")
		})
	}
}

// TestHandleOERStatus_MidFetchStillRefusesEverythingWithoutADiagnostic keeps the
// first anchor honest: the cursor id is an *addition* mid-stream, not a
// replacement. A bit-less standalone OER reporting success or end-of-data
// completes nothing there either, however well it names the cursor.
func TestHandleOERStatus_MidFetchStillRefusesEverythingWithoutADiagnostic(t *testing.T) {
	t.Parallel()

	const streamingCursor = 4

	tests := []struct {
		name string
		oer  []byte
	}{
		{name: "success", oer: buildOERNamingCursor(100, 0, streamingCursor)},
		{name: "end of data", oer: buildOERNamingCursor(100, oraNoDataFound, streamingCursor)},
		{name: "a code with no diagnostic behind it", oer: buildOERNamingCursor(100, oraMidFetchCode, streamingCursor)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, streamingCursor)))

			s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
			require.True(t, s.rowStreamActive())

			s.handleOERStatus(tc.oer)

			assert.NotNil(t, s.tracker.pendingQuery,
				"only a run that proves it is a diagnostic may end a call without the bit")
		})
	}
}

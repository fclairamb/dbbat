package oracle

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// What a failing statement actually puts on the wire, measured rather than
// reasoned about.
//
// The premise this file overturns is that the OER end-of-call bit is a *client*
// trait: set on everything go-ora's server leg sends, absent on
// python-oracledb thin's. Two recordings of the same six failures — one per
// client, `go test -tags capture -run 'TestCapture_.*FailedStatements'` against
// Oracle Free 23ai — say it is a property of the call instead. Only the failed
// DDL carries it, on *both* clients. The other five (a SELECT on a missing
// table, a unique-key violation, a divide-by-zero, a PL/SQL RAISE, a PL/SQL
// compile error) arrive with CallStatus 1 or 5 on both.
//
// Every one of them arrives as a **standalone func=0x04** message after a
// marker exchange — never embedded in a Response — which is why the fix landed
// in handleOERStatus and not in the relaxed Response scan the todo expected to
// need loosening. findPlausibleOERInResponse is untouched; so is cursor-id
// learning, which reads it.
const (
	pythonFailedStmtDump = "python_thin_failed_stmt.pcapng"
	goOraFailedStmtDump  = "go_ora_failed_stmt.pcapng"
)

// recordedOER is one standalone func=0x04 message out of a recording.
type recordedOER struct {
	index   int
	payload []byte
	fields  *oerInfo
	message string
}

// recordedFailureOERs returns every standalone OER a recording carries that
// reports a nonzero error code, in wire order.
func recordedFailureOERs(t *testing.T, name string) []recordedOER {
	t.Helper()

	var out []recordedOER

	for i, pkt := range loadTestDump(t, name).Packets {
		if pkt.Direction != dump.DirServerToClient {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		ttc := extractTTCPayload(tns.Payload)
		if len(ttc) < 2 || TTCFunctionCode(ttc[0]) != TTCFuncOERR {
			continue
		}

		fields, rest := decodeOERFieldsAt(ttc, 0)
		if fields == nil || fields.ErrorCode == 0 {
			continue
		}

		out = append(out, recordedOER{
			index:   i,
			payload: ttc,
			fields:  fields,
			message: extractORAMessage(ttc[rest:]),
		})
	}

	return out
}

// wantFailure is one row of the measurement: which ORA code came back, and
// whether the OER that carried it had the end-of-call bit.
type wantFailure struct {
	what   string
	code   int
	hasBit bool
}

// measuredFailures is the recording, transcribed. Both clients produced exactly
// this, in this order — which is the whole point: the bit does not distinguish
// them.
var measuredFailures = []wantFailure{
	{what: "DROP of a missing table (the setup DDL)", code: 942, hasBit: true},
	{what: "SELECT against a missing table", code: 942},
	{what: "INSERT violating a unique constraint", code: 1},
	{what: "DROP of a missing table", code: 942, hasBit: true},
	{what: "SELECT whose row cannot be produced (1/0)", code: 1476},
	{what: "PL/SQL RAISE_APPLICATION_ERROR", code: 20001},
	{what: "PL/SQL block that will not compile", code: 6550},
}

// TestDumpReplay_FailuresArriveAsBitLessStandaloneOERs is the measurement the
// todo asked for, pinned. If a future Oracle release starts setting the bit on
// these, this test is where that shows up.
func TestDumpReplay_FailuresArriveAsBitLessStandaloneOERs(t *testing.T) {
	t.Parallel()

	for _, name := range []string{pythonFailedStmtDump, goOraFailedStmtDump} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			recorded := recordedFailureOERs(t, name)

			// go-ora's first DROP runs against a database its own CREATE has
			// not made yet only on the first pass; both recordings carry the
			// same set, so compare on the tail when one has an extra setup
			// failure.
			require.GreaterOrEqual(t, len(recorded), len(measuredFailures)-1,
				"the recording lost failures: got %d", len(recorded))

			want := measuredFailures[len(measuredFailures)-len(recorded):]

			for i, got := range recorded {
				t.Run(want[i].what, func(t *testing.T) {
					assert.Equal(t, want[i].code, got.fields.ErrorCode, "packet #%d", got.index)

					hasBit := got.fields.CallStatus&oerEndOfCallBit != 0
					assert.Equalf(t, want[i].hasBit, hasBit,
						"packet #%d CallStatus=%#x — the bit is a property of the call, not of the client",
						got.index, got.fields.CallStatus)

					// The strict decoder is what dbbat used before this fix.
					assert.Equal(t, want[i].hasBit, decodeOERAt(got.payload, 0) != nil,
						"packet #%d: the strict decoder tracks the bit exactly", got.index)

					// And the relaxed decoder reads all of them, because every
					// diagnostic names its own code.
					relaxed := decodeErrorOERAt(got.payload, 0)
					require.NotNilf(t, relaxed, "packet #%d: %q", got.index, got.message)
					assert.Equal(t, want[i].code, relaxed.ErrorCode)
					assert.Equal(t, fmt.Sprintf("ORA-%05d", want[i].code), relaxed.ErrorMessage[:9])
				})
			}
		})
	}
}

// TestDumpReplay_NoFailureArrivesEmbeddedInAResponse rules out the shape the
// todo assumed was the one to fix. If a failure ever did arrive inside a
// Response, the relaxed Response scan would be the place to change — and this
// is what would tell us.
func TestDumpReplay_NoFailureArrivesEmbeddedInAResponse(t *testing.T) {
	t.Parallel()

	for _, name := range []string{pythonFailedStmtDump, goOraFailedStmtDump} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for i, pkt := range loadTestDump(t, name).Packets {
				if pkt.Direction != dump.DirServerToClient {
					continue
				}

				tns, err := parseTNSFromDumpPacket(pkt.Data)
				if err != nil || tns.Type != TNSPacketTypeData {
					continue
				}

				ttc := extractTTCPayload(tns.Payload)
				if len(ttc) < 2 || TTCFunctionCode(ttc[0]) != TTCFuncResponse {
					continue
				}

				if oer := findPlausibleOERInResponse(ttc); oer != nil {
					assert.Zerof(t, oer.ErrorCode,
						"packet #%d: a Response carrying a failing OER would change where the fix belongs", i)
				}
			}
		})
	}
}

// replayFailure drives one recorded failure through a session that has the
// matching statement pending, exactly as interceptUpstreamMessage would, and
// returns the query row dbbat wrote.
func replayFailure(t *testing.T, sql string, oer []byte) *store.Query {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := newRecordingCompletionStore()
	s.completionStore = recorder

	require.NoError(t, s.handleOALL8(buildOALL8(sql, nil, 4)))
	require.NotNil(t, s.tracker.pendingQuery)

	s.interceptUpstreamMessage(upstreamPacket(oer))

	require.Nil(t, s.tracker.pendingQuery,
		"the statement must be finished by its own error, not by whatever comes next")

	return awaitCreated(t, recorder)
}

// TestHandleOERStatus_RecordsTheORATextOfEveryRecordedFailure is the
// regression. Before the fix, five of these seven recorded errors were dropped
// on the floor: the statement stayed pending and the *next* statement's
// flushPendingQuery closed it as a success with no error text at all.
func TestHandleOERStatus_RecordsTheORATextOfEveryRecordedFailure(t *testing.T) {
	t.Parallel()

	for _, name := range []string{pythonFailedStmtDump, goOraFailedStmtDump} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, got := range recordedFailureOERs(t, name) {
				t.Run(fmt.Sprintf("ORA-%05d at #%d", got.fields.ErrorCode, got.index), func(t *testing.T) {
					t.Parallel()

					row := replayFailure(t, "UPDATE emp SET sal = sal + 1", got.payload)

					require.NotNil(t, row.Error, "a failed statement must record its ORA text")
					assert.Contains(t, *row.Error, fmt.Sprintf("ORA-%05d", got.fields.ErrorCode))
					assert.Nil(t, row.RowsAffected, "a statement that failed affected nothing")
				})
			}
		})
	}
}

// TestHandleOERStatus_RelaxationIsErrorsOnly pins the width of the change. The
// bit still guards everything it guarded before *except* a proven diagnostic:
// a bit-less standalone OER reporting success, or end-of-data, still completes
// nothing, because there is no ORA text to prove those bytes with.
func TestHandleOERStatus_RelaxationIsErrorsOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		oer  []byte
	}{
		{name: "success", oer: pythonThinOERBytes},
		{name: "end of data", oer: buildOER(2, 57186, 0, oraNoDataFound)},
		{name: "a code with no diagnostic behind it", oer: buildOER(2, 57186, 0, 942)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.NoError(t, s.handleOALL8(buildOALL8("UPDATE emp SET sal = 1", nil, 4)))

			s.handleOERStatus(tc.oer)

			assert.NotNil(t, s.tracker.pendingQuery,
				"only a run that proves it is a diagnostic may end a call without the bit")
		})
	}
}

// TestHandleOERStatus_MidRowStreamStillRequiresTheEndOfCallBit is the guard
// that keeps this fix from becoming the bug it is descended from. Mid-fetch a
// 0x04 lead byte is a row value's length prefix, so the strict decoder stays
// the only thing allowed to end a call there — even for a payload the relaxed
// decoder would happily accept.
func TestHandleOERStatus_MidRowStreamStillRequiresTheEndOfCallBit(t *testing.T) {
	t.Parallel()

	failures := recordedFailureOERs(t, pythonFailedStmtDump)
	require.NotEmpty(t, failures)

	var bitless []byte

	for _, f := range failures {
		if f.fields.CallStatus&oerEndOfCallBit == 0 {
			bitless = f.payload

			break
		}
	}

	require.NotNil(t, bitless, "the recording must contain a bit-less failure to try this with")
	require.NotNil(t, decodeErrorOERAt(bitless, 0), "outside a row stream these bytes complete a call")

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, 4)))

	s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
	require.True(t, s.rowStreamActive())

	s.handleOERStatus(bitless)

	assert.NotNil(t, s.tracker.pendingQuery,
		"mid-fetch, a bit-less 0x04 run is row data and must not end the fetch")
}

// TestDecodeErrorOERAt_DemandsThatTheDiagnosticNameTheCode is the anchor that
// replaces the bit. Seven decodable ints and an ORA-shaped tail are not enough
// on a path routed by byte 0: the tail has to name the code the fields
// reported, which arbitrary bytes will not.
func TestDecodeErrorOERAt_DemandsThatTheDiagnosticNameTheCode(t *testing.T) {
	t.Parallel()

	withTail := func(code int, tail string) []byte {
		return append(buildOER(2, 57186, 0, code), []byte(tail)...)
	}

	tests := []struct {
		name   string
		oer    []byte
		accept bool
	}{
		{name: "the diagnostic names its code", oer: withTail(942, "\x00\x00KORA-00942: table or view does not exist"), accept: true},
		{name: "a PL/SQL diagnostic does too", oer: withTail(6550, "\x00\x00KORA-06550: line 1, column 7:"), accept: true},
		{name: "a diagnostic naming another code", oer: withTail(942, "\x00\x00KORA-01476: divisor is equal to zero")},
		{name: "no diagnostic at all", oer: withTail(942, "\x00\x00\x00\x11\x22\x33")},
		{name: "an implausible code", oer: withTail(999999, "\x00\x00KORA-999999: nothing like this exists")},
		{name: "end of data is not a failure", oer: withTail(oraNoDataFound, "\x00\x00KORA-01403: no data found")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := decodeErrorOERAt(tc.oer, 0)
			if tc.accept {
				require.NotNil(t, got)
				assert.NotEmpty(t, got.ErrorMessage)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

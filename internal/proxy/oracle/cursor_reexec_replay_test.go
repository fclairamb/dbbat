package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The recordings replayed here answer the question cursor_reexec_gate_test.go
// could not: what does a real client put on the wire when it re-runs a
// statement it already parsed?
//
// The answer, from five captures against Oracle Free 23ai (regenerate with
// `go test -tags capture -run 'TestCapture_.*Reexec' ./internal/proxy/oracle/`):
// never the SQL-less OALL8 the gate was first written for. Modern thin clients
// send a **piggyback re-execution** — func 0x03, sub-op 0x4e for a SELECT and
// 0x04 for anything else — carrying the cursor id and no statement text at all.
// See docs/oracle.md.
const (
	goOraReexecDump    = "go_ora_cursor_reexec.pcapng"
	goOraDMLReexecDump = "go_ora_dml_cursor_reexec.pcapng"
	pythonReexecDump   = "python_thin_cursor_reexec.pcapng"
	jdbcReexecDump     = "jdbc_thin_cursor_reexec.pcapng"
	sqlplusReexecDump  = "sqlplus_cursor_reexec.pcapng"
)

// clientTTCPayloads returns the TTC payloads of every client→server Data packet
// in a recording, in order.
func clientTTCPayloads(t *testing.T, name string) [][]byte {
	t.Helper()

	var out [][]byte

	for _, pkt := range loadTestDump(t, name).Packets {
		if pkt.Direction != dump.DirClientToServer {
			continue
		}

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if ttc := extractTTCPayload(tns.Payload); ttc != nil {
			out = append(out, ttc)
		}
	}

	return out
}

// recordedReexecs returns every piggyback cursor re-execution in a recording,
// with the cursor id each one names.
func recordedReexecs(t *testing.T, name string) ([][]byte, []uint16) {
	t.Helper()

	var (
		payloads  [][]byte
		cursorIDs []uint16
	)

	for _, ttc := range clientTTCPayloads(t, name) {
		if TTCFunctionCode(ttc[0]) != TTCFuncPiggyback || !IsPiggybackCursorReexec(ttc) {
			continue
		}

		cursorID, err := decodeCursorReexec(ttc)
		require.NoErrorf(t, err, "recorded re-execution must decode: % x", ttc)

		payloads = append(payloads, ttc)
		cursorIDs = append(cursorIDs, cursorID)
	}

	return payloads, cursorIDs
}

// TestDumpReplay_CursorReexecFramesAreRealAndSQLLess pins what the captures
// actually contain: each client parsed its statement once and then re-ran it
// three times by cursor id alone. If a future decoder change stopped
// recognizing these frames, this fails before the enforcement tests do.
func TestDumpReplay_CursorReexecFramesAreRealAndSQLLess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dump    string
		subOp   byte
		wantSQL string
	}{
		{
			name: "go-ora re-executes a prepared SELECT", dump: goOraReexecDump,
			subOp: PiggybackSubReexecSel, wantSQL: "SELECT 1 AS n FROM dual",
		},
		{
			name: "go-ora re-executes a prepared INSERT", dump: goOraDMLReexecDump,
			subOp: PiggybackSubReexecDML, wantSQL: "INSERT INTO dbbat_reexec_test VALUES (1)",
		},
		{
			name: "python-oracledb thin re-executes on one cursor", dump: pythonReexecDump,
			subOp: PiggybackSubReexecSel, wantSQL: "SELECT 1 AS n FROM dual",
		},
		{
			name: "JDBC thin re-executes a cached PreparedStatement", dump: jdbcReexecDump,
			subOp: PiggybackSubReexecSel, wantSQL: "SELECT 1 AS n FROM dual",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payloads, cursorIDs := recordedReexecs(t, tc.dump)

			// The capture ran the statement four times: one parse carrying the
			// SQL, then three re-executions.
			require.Len(t, payloads, 3, "recorded re-executions")

			for i, ttc := range payloads {
				assert.Equal(t, tc.subOp, ttc[1], "sub-op of re-execution %d", i)
				assert.Positive(t, cursorIDs[i], "cursor id of re-execution %d", i)
				assert.Equal(t, cursorIDs[0], cursorIDs[i], "every re-execution names the same cursor")
				assert.NotContains(t, string(ttc), tc.wantSQL,
					"a re-execution must not carry the statement text — that is the whole point")
			}

			// And the statement really is in the capture: it was sent once, on
			// the execute that created the cursor.
			var carried int

			for _, ttc := range clientTTCPayloads(t, tc.dump) {
				if IsPiggybackExecSQL(ttc) && contains(string(ttc), tc.wantSQL) {
					carried++
				}
			}

			assert.Equal(t, 1, carried, "the SQL is sent exactly once, on the parse")
		})
	}
}

// contains is strings.Contains, spelled out to keep the import list of this
// file to what the assertions need.
func contains(haystack, needle string) bool {
	return len(needle) <= len(haystack) && findBytes([]byte(haystack), []byte(needle)) >= 0
}

// replaySession feeds a recording through the real intercept pipeline, in wire
// order and in both directions, and reports which client packets were blocked.
func replaySession(t *testing.T, s *session, name string) int {
	t.Helper()

	blocked := 0
	s.clientConn = drainedPipe(t)

	for _, pkt := range loadTestDump(t, name).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			if s.interceptClientMessage(tns) {
				blocked++
			}

			continue
		}

		s.interceptUpstreamMessage(tns)
	}

	return blocked
}

// TestDumpReplay_CursorReexecIsGatedOnRealFrames is the end-to-end claim: run
// the recorded sessions through the proxy's own client and upstream intercept
// paths and every re-execution is recognized, resolved to the statement its
// cursor was parsed with, and gated as a query of its own.
//
// Two things have to work for that, and neither is exercised by the hand-built
// fixture: the piggyback re-execution frame must be routed to the gate at all,
// and the cursor id — which the modern execute paths never send on the request
// — must have been learned from the server's response.
func TestDumpReplay_CursorReexecIsGatedOnRealFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dump    string
		wantSQL string
	}{
		{name: "go-ora SELECT", dump: goOraReexecDump, wantSQL: "SELECT 1 AS n FROM dual"},
		{name: "go-ora INSERT", dump: goOraDMLReexecDump, wantSQL: "INSERT INTO dbbat_reexec_test VALUES (1)"},
		{name: "python-oracledb thin SELECT", dump: pythonReexecDump, wantSQL: "SELECT 1 AS n FROM dual"},
		{name: "JDBC thin SELECT", dump: jdbcReexecDump, wantSQL: "SELECT 1 AS n FROM dual"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, cursorIDs := recordedReexecs(t, tc.dump)
			require.NotEmpty(t, cursorIDs)

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.Zero(t, replaySession(t, s, tc.dump), "a full-write grant blocks nothing")

			cursor, ok := s.tracker.cursors[cursorIDs[0]]
			require.Truef(t, ok, "cursor %d was never learned from the server's response", cursorIDs[0])
			assert.Equal(t, tc.wantSQL, cursor.sql,
				"the re-executed cursor must resolve to the statement it was parsed with")
		})
	}
}

// TestDumpReplay_CursorReexecIsRefusedUnderReadOnly is the enforcement claim on
// a frame Oracle actually sent: the recorded re-execution of an INSERT is
// blocked by `read_only`, and blocked by the dispatcher — the code that decides
// whether bytes travel upstream — not merely reported as an error somewhere.
//
// The cursor is planted rather than replayed because under `read_only` the
// INSERT would never have been parsed in the first place; the exposure being
// covered is a control that becomes relevant after the parse (a grant swap, an
// approval pattern) or a cursor learned before it did.
func TestDumpReplay_CursorReexecIsRefusedUnderReadOnly(t *testing.T) {
	t.Parallel()

	payloads, cursorIDs := recordedReexecs(t, goOraDMLReexecDump)
	require.NotEmpty(t, payloads)

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}})
	s.clientConn = drainedPipe(t)
	s.tracker.cursors[cursorIDs[0]] = &trackedCursor{
		cursorID: cursorIDs[0],
		sql:      "INSERT INTO dbbat_reexec_test VALUES (1)",
		parsedAt: time.Now(),
	}

	// The handler refuses it...
	require.ErrorIs(t, s.handlePiggybackReexec(payloads[0]), shared.ErrReadOnlyViolation)
	assert.Nil(t, s.tracker.pendingQuery, "a refused re-execution must not be tracked as in flight")

	// ...and so does the dispatcher, for every recorded re-execution.
	for i, ttc := range payloads {
		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, ttc...)}
		assert.Truef(t, s.interceptClientMessage(pkt), "recorded re-execution %d must not be forwarded", i)
	}
}

// TestDumpReplay_SQLPlusResendsItsSQLInstead is the negative half of the
// measurement, and the reason the client table in docs/oracle.md can name who
// does *not* do this: sqlplus (OCI thick) has no prepared-statement API and its
// statement cache does not re-execute by cursor id — it puts the full text on
// the wire on every run, so the gate sees it on the ordinary path.
func TestDumpReplay_SQLPlusResendsItsSQLInstead(t *testing.T) {
	t.Parallel()

	payloads, _ := recordedReexecs(t, sqlplusReexecDump)
	assert.Empty(t, payloads, "sqlplus emitted no cursor re-execution")

	var carried int

	for _, ttc := range clientTTCPayloads(t, sqlplusReexecDump) {
		if contains(string(ttc), "SELECT 1 AS n FROM dual") {
			carried++
		}
	}

	assert.Equal(t, 4, carried, "every one of the four runs carried its statement text")
}

// TestDumpReplay_CursorReexecOfAnUntrackedCursorFailsClosed is the symmetry
// claim, on a frame Oracle actually sent: the piggyback re-execution now answers
// an untracked cursor exactly like the SQL-less OALL8 and the fresh-query
// OFETCH — refused under a grant carrying a statement-shaped control, forwarded
// with a WARN under one carrying none.
//
// The asymmetry it replaces was real and load-bearing: while cursor-id learning
// silently stopped part-way through every session (the OER sequence bound in
// findCursorIDInResponse), refusing here would have broken the second execution
// of ordinary read-only work. Once that was fixed and measured — 124 live
// re-executions from go-ora and python-oracledb thin, none untracked, see
// TestIntegration_CursorIDLearningMissRate — the frame everybody sends stopped
// being the one that enforced nothing.
func TestDumpReplay_CursorReexecOfAnUntrackedCursorFailsClosed(t *testing.T) {
	t.Parallel()

	payloads, _ := recordedReexecs(t, goOraDMLReexecDump)
	require.NotEmpty(t, payloads)

	t.Run("refused under a statement-shaped control", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientConn = drainedPipe(t)

		require.ErrorIs(t, s.handlePiggybackReexec(payloads[0]), ErrUnknownCursor)
		assert.Nil(t, s.tracker.pendingQuery, "a refused execution must not be tracked as in flight")

		// And the dispatcher agrees: the bytes do not travel upstream.
		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, payloads[0]...)}
		assert.True(t, s.interceptClientMessage(pkt), "the frame must not be forwarded")
	})

	t.Run("forwarded without one", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

		require.NoError(t, s.handlePiggybackReexec(payloads[0]),
			"a grant with no statement-shaped control must not be broken by an unidentified execution")
		assert.Nil(t, s.tracker.pendingQuery, "a forwarded but unidentified execution is not tracked")
	})
}

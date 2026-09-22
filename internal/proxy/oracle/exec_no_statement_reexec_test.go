package oracle

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The third re-execution shape, and the one that was going upstream ungated.
//
// cursor_reexec_replay_test.go covers the two frames modern thin clients send
// (`03 4e` for a SELECT, `03 04` for anything else) and the legacy SQL-less
// OALL8. This file covers the one an actually-old driver sends: **ojdbc6
// 11.2.0.4** re-runs a PreparedStatement by resending the *parse* op, `03 5e`,
// with its statement length set to zero. `IsPiggybackExecSQL` says yes (it tests
// the sub-op byte and nothing else), the statement decoders find no statement,
// and until execNoStatementCursor existed that combination meant "undecodable,
// forward it" — so every execution after the first ran with no read_only check,
// no block_ddl check, no ValidateOracleQuery, no approval pattern, no `queries`
// row and no quota accounting.
//
// Recorded live against Oracle 23ai Free on 2026-09-19 with
// capture_legacy_oall8_test.go; the frame is packet #20 of the fixture.
const ojdbc6LegacyDump = "ojdbc6_legacy.pcapng"

// recordedNoStatementExecs returns every SQL-less execute op in the ojdbc6
// recording, with the cursor id each one names. It is the counterpart of
// recordedReexecs, which does the same for the `03 4e` / `03 04` shape.
func recordedNoStatementExecs(t *testing.T) ([][]byte, []uint16) {
	t.Helper()

	var (
		payloads  [][]byte
		cursorIDs []uint16
	)

	for _, ttc := range clientTTCPayloads(t, ojdbc6LegacyDump) {
		cursorID, ok := execNoStatementCursor(ttc, false)
		if !ok {
			continue
		}

		payloads = append(payloads, ttc)
		cursorIDs = append(cursorIDs, cursorID)
	}

	return payloads, cursorIDs
}

// TestDumpReplay_OJDBC6ReexecIsSQLLessAndNamesItsCursor pins what the recording
// contains, byte for byte, before anything is claimed about enforcement: one
// `03 5e` carrying no statement, naming a cursor the session really did parse.
func TestDumpReplay_OJDBC6ReexecIsSQLLessAndNamesItsCursor(t *testing.T) {
	t.Parallel()

	payloads, cursorIDs := recordedNoStatementExecs(t)
	require.Len(t, payloads, 1, "the capture executes its PreparedStatement twice: one parse, one re-execution")

	ttc := payloads[0]

	// The op is the *parse* op, which is the whole difficulty: nothing in the
	// header says "re-execution" except the length field reading zero.
	assert.Equal(t, byte(TTCFuncPiggyback), ttc[0])
	assert.Equal(t, PiggybackSubExecSQL, ttc[1], "ojdbc6 re-executes under the parse sub-op, not 0x4e/0x04")
	assert.False(t, IsPiggybackCursorReexec(ttc), "the two known re-execution sub-ops do not cover this frame")
	assert.True(t, IsPiggybackExecSQL(ttc), "which is exactly why it reached the exec handler")

	// The header the classifier reads: options, cursor 3, flag, length 0.
	assert.Equal(t, []byte{0x03, 0x5e, 0x06, 0x02, 0x80, 0x60, 0x01, 0x03, 0x00, 0x00}, ttc[:10])
	assert.Positive(t, cursorIDs[0], "a re-execution names the cursor it re-runs")

	header, ok := execThinHeader(ttc)
	require.True(t, ok, "the exec header must walk")
	assert.Equal(t, int(cursorIDs[0]), header.cursorID)
	assert.Equal(t, 0, header.sqlLen.value, "the header declares no statement — that is the finding")

	// And there is no statement in it to find, by any of the three readings.
	assert.NotContains(t, string(ttc), "SELECT", "a re-execution carries no statement text")

	_, err := decodePiggybackExecSQL(ttc, false)
	require.ErrorIs(t, err, ErrPiggybackExecNoSQL, "it is reported as a re-execution, not as a decode failure")

	var noSQL *PiggybackExecNoSQLError

	require.ErrorAs(t, err, &noSQL)
	assert.Equal(t, cursorIDs[0], noSQL.CursorID, "the condition carries the cursor id the gate needs")

	// The statement really is in the capture — sent once, on the parse that
	// created the cursor.
	var carried int

	for _, frame := range clientTTCPayloads(t, ojdbc6LegacyDump) {
		if contains(string(frame), "SELECT :1  AS n FROM dual") {
			carried++
		}
	}

	assert.Equal(t, 1, carried, "the prepared statement's text is sent exactly once")
}

// TestDumpReplay_OJDBC6ReexecIsGatedOnRealFrames is the end-to-end claim: replay
// the whole recorded session through the proxy's own intercept paths and the
// SQL-less execute is recognized, resolved to the statement its cursor was
// parsed with, and gated as a query of its own.
func TestDumpReplay_OJDBC6ReexecIsGatedOnRealFrames(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.logger = slog.New(logs)

	require.Zero(t, replaySession(t, s, ojdbc6LegacyDump), "a full-write grant blocks nothing")

	require.Equal(t, 1, logs.count(logMsgReexecGated),
		"the SQL-less 03 5e must reach the re-execution gate")
	assert.Zero(t, logs.count(logMsgUntrackedCursorForwarded),
		"its cursor was parsed in this very session, so it must resolve rather than be waved through")

	for _, got := range logs.sqlsFor(logMsgReexecGated) {
		assert.Equal(t, "SELECT :1  AS n FROM dual", got,
			"the re-executed cursor must resolve to the statement it was parsed with")
	}
}

// TestDumpReplay_OJDBC6ReexecIsRefusedUnderStatementControls is the enforcement claim on
// the frame ojdbc6 actually sent: with the cursor tracked, the re-execution of a
// write is refused by `read_only` and by `block_ddl` — and refused by the
// dispatcher, the code that decides whether bytes travel upstream, not merely
// reported as an error somewhere.
//
// The cursor is planted rather than replayed, exactly as in
// TestDumpReplay_CursorReexecIsRefusedUnderReadOnly: under either control the
// statement would never have been parsed in the first place, and the exposure
// being covered is a control that becomes relevant after the parse (a grant
// swap, an approval pattern) or a cursor learned before it did.
func TestDumpReplay_OJDBC6ReexecIsRefusedUnderStatementControls(t *testing.T) {
	t.Parallel()

	payloads, cursorIDs := recordedNoStatementExecs(t)
	require.NotEmpty(t, payloads)

	tests := []struct {
		name    string
		control string
		sql     string
		wantErr error
	}{
		{
			name:    "read_only refuses the re-executed write",
			control: store.ControlReadOnly,
			sql:     "INSERT INTO dbbat_reexec_test VALUES (1)",
			wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name:    "block_ddl refuses the re-executed DDL",
			control: store.ControlBlockDDL,
			sql:     "CREATE TABLE dbbat_reexec_test (n NUMBER)",
			wantErr: shared.ErrDDLBlocked,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{
				Definition: &store.GrantDefinition{Controls: []string{tc.control}},
			})
			s.clientConn = drainedPipe(t)
			s.tracker.cursors[cursorIDs[0]] = &trackedCursor{
				cursorID: cursorIDs[0],
				sql:      tc.sql,
				parsedAt: time.Now(),
			}

			// The handler refuses it...
			require.ErrorIs(t, s.handlePiggybackExec(payloads[0]), tc.wantErr)
			assert.Nil(t, s.tracker.pendingQuery, "a refused re-execution must not be tracked as in flight")

			// ...and so does the dispatcher: the bytes do not travel upstream.
			pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, payloads[0]...)}
			assert.True(t, s.interceptClientMessage(pkt), "the recorded re-execution must not be forwarded")
		})
	}
}

// TestDumpReplay_OJDBC6ReexecOfAnUntrackedCursorFailsClosed is the symmetry
// claim: the SQL-less `03 5e` answers an untracked cursor exactly like the other
// two re-execution frames — refused under a grant carrying a statement-shaped
// control, forwarded with a WARN under one carrying none. The wire op a client
// picks cannot change the answer.
func TestDumpReplay_OJDBC6ReexecOfAnUntrackedCursorFailsClosed(t *testing.T) {
	t.Parallel()

	payloads, _ := recordedNoStatementExecs(t)
	require.NotEmpty(t, payloads)

	t.Run("refused under a statement-shaped control", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientConn = drainedPipe(t)

		require.ErrorIs(t, s.handlePiggybackExec(payloads[0]), ErrUnknownCursor)
		assert.Nil(t, s.tracker.pendingQuery, "a refused execution must not be tracked as in flight")

		pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: append([]byte{0x00, 0x00}, payloads[0]...)}
		assert.True(t, s.interceptClientMessage(pkt), "the frame must not be forwarded")
	})

	t.Run("forwarded without one", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

		require.NoError(t, s.handlePiggybackExec(payloads[0]),
			"a grant with no statement-shaped control must not be broken by an unidentified execution")
		assert.Nil(t, s.tracker.pendingQuery, "a forwarded but unidentified execution is not tracked")
	})
}

// TestOJDBC6ReexecDoesNotDisturbTheParsePath is the no-regression half, and the
// reason the classifier is consulted only after the statement decoders have
// spoken: a `03 5e` that *does* carry a statement is read as one, gated as one
// and tagged as one, on every client shape in the corpus.
//
// The floor is the whole corpus rather than a hand-built frame because
// frameCarriesStatement is shared logic — the gate, the per-session tagging
// verdict and the rewriter all read it — so a change to it that only a synthetic
// fixture covered would be a change nobody measured.
func TestOJDBC6ReexecDoesNotDisturbTheParsePath(t *testing.T) {
	t.Parallel()

	var statements int

	reexecs := map[string]int{}

	for _, name := range surveyCorpus(t) {
		for _, ttc := range surveyClientTTC(t, loadTestDump(t, name)) {
			for _, body := range surveyExecOps(ttc) {
				sql, located := decodeExecStatement(body, false)
				_, reexec := execNoStatementCursor(body, false)

				require.Falsef(t, located && reexec,
					"%s: a frame whose statement locates must never read as a re-execution: %q",
					name, truncateSQL(sql, 60))

				switch {
				case located:
					statements++

					assert.Truef(t, frameCarriesStatement(body, false),
						"%s: a frame carrying %q must stay a statement frame", name, truncateSQL(sql, 60))
				case reexec:
					reexecs[name]++

					assert.Falsef(t, frameCarriesStatement(body, false),
						"%s: a frame declaring no statement is not a statement frame", name)
				}
			}
		}
	}

	t.Logf("exec ops across the corpus: %d carrying a statement, %v re-executing a cursor", statements, reexecs)

	assert.Positive(t, statements, "the corpus must still be full of ordinary statements")

	// Per recording rather than a total, because the two sources of a SQL-less
	// execute are different things and a total would let one drift into the
	// other: ojdbc6 re-running a PreparedStatement, and a thin client **driving
	// a REF cursor** the server opened inside a procedure.
	//
	// The REF-cursor recordings each call the procedure three times, so three
	// drives apiece. python-oracledb's count is five because *it* also
	// re-executes the call itself this way — calls 2 and 3 of `begin
	// dbbat_cap_refcur(:1); end;` go out as a statement-less `03 5e` naming
	// cursor 5, stapled behind its close-cursors piggyback, where go-ora and
	// JDBC send a `03 04` piggyback re-execution instead (which is a different
	// frame and counted nowhere here).
	//
	// sqlplus is the **OCI wide** entry, and the only one: its two are the
	// `PRINT rc` that drives each REF cursor, read by execWideNoStatementCursor
	// rather than by the thin walk. That reading had no recording in this corpus
	// at all until sqlplus_refcursor.pcapng landed — it was pinned against the
	// `oci_refcursor_drives.hex` fixture pair alone — so this is the line that
	// makes a regression in it fail here. Two rather than three because sqlplus
	// drives the cursor the script asks it to print, and the script prints twice.
	// go_ora_lob.pcapng is the one entry that is not a REF cursor, and it is
	// the LOB round trip's own shape: with a LOB in the select list Oracle
	// turns row prefetch off, the describe comes back with no rows, and go-ora
	// goes and fetches them — a statement-less execute naming the cursor it was
	// just given. Its streamed sibling (go_ora_lob_stream.pcapng) is absent for
	// the same reason inverted: asked for locators rather than bodies, the
	// server prefetches the row into the describe and there is nothing to fetch.
	assert.Equal(t, map[string]int{
		"ojdbc6_legacy.pcapng":         1,
		"go_ora_refcursor.pcapng":      3,
		"jdbc_thin_refcursor.pcapng":   3,
		"python_thin_refcursor.pcapng": 5,
		"sqlplus_refcursor.pcapng":     2,
		"go_ora_lob.pcapng":            1,
	}, reexecs, "only these recordings carry an execute that declares no statement")
}

// TestExecNoStatementCursorRefusesWhatItCannotRead pins the reading's edges,
// because the cost of a false positive here is a re-execution refused on a
// client that was working.
func TestExecNoStatementCursorRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	// The recorded frame, as the baseline every variant below is a mutation of.
	recorded, cursorIDs := recordedNoStatementExecs(t)
	require.NotEmpty(t, recorded)

	got, ok := execNoStatementCursor(recorded[0], false)
	require.True(t, ok)
	require.Equal(t, cursorIDs[0], got)

	t.Run("a cursor id of zero is a parse, not a re-execution", func(t *testing.T) {
		t.Parallel()

		frame := append([]byte(nil), recorded[0]...)
		frame[7] = 0x00 // the compressed-int cursor id's single value byte

		_, ok := execNoStatementCursor(frame, false)
		assert.False(t, ok, "cursor 0 means allocate one")
	})

	t.Run("a declared statement is not a re-execution", func(t *testing.T) {
		t.Parallel()

		frame := append([]byte(nil), recorded[0]...)
		frame[9] = 0x01 // sqlLen's compressed-int size byte: one byte of length follows

		_, ok := execNoStatementCursor(frame, false)
		assert.False(t, ok, "a non-zero length field means the frame is meant to carry a statement")
	})

	t.Run("a different op is left alone", func(t *testing.T) {
		t.Parallel()

		frame := append([]byte(nil), recorded[0]...)
		frame[1] = PiggybackSubReexecSel

		_, ok := execNoStatementCursor(frame, false)
		assert.False(t, ok, "only the execute op is read here; 0x4e has its own decoder")
	})

	t.Run("a truncated frame yields nothing", func(t *testing.T) {
		t.Parallel()

		for n := range len(recorded[0]) {
			if n >= 10 {
				break
			}

			_, ok := execNoStatementCursor(recorded[0][:n], false)
			assert.Falsef(t, ok, "a %d-byte frame must not resolve to a cursor", n)
		}
	})
}

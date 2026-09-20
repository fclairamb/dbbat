package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The fourth re-execution shape, and the OCI half of the defect
// exec_no_statement_reexec_test.go covers on ojdbc6.
//
// sqlplus, Instant Client and SQL*Developer-over-OCI write the wide exec header,
// whose cursor id sits in the header prefix rather than where the thin walk
// looks — so execNoStatementCursorAt refused that header outright, the frame
// decoded as "could not find SQL text", and a decode failure is forwarded
// ungated. From the second execution of any cursor on an OCI session (a REF
// cursor driven by `PRINT rc`, or an ordinary prepared statement re-run),
// read_only, block_ddl, ValidateOracleQuery, the approval patterns, the
// `queries` row and the quota all applied to the parse alone.
//
// The fixtures are the ones the REF-cursor work recorded: two SQL-less wide
// execs, and beside them the call responses that handed those very cursors back.

// ociDriveCursorIDs runs the reading under test over the recorded sqlplus
// drives and returns the ids it reports, in recording order.
func ociDriveCursorIDs(t *testing.T) []uint16 {
	t.Helper()

	frames := recordedFrames(t, ociRefCursorDrives)
	ids := make([]uint16, 0, len(frames))

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		cursorID, ok := execNoStatementCursor(ttc)
		require.Truef(t, ok, "drive %d declares no statement, so it must read as a re-execution", i)

		ids = append(ids, cursorID)
	}

	return ids
}

// TestDumpReplay_OCIDriveReadsTheCursorTheClientIsDriving is the cross-check the
// offset rests on, and it is made against two witnesses that share no code with
// the walk under test.
//
// The first is the client's own next frame, read by hand in ociDrivenCursorID.
// The second is stronger: refCursorIDsInBindOutput reads the ids out of the call
// responses recorded beside these drives, in the same session, selected by
// position — a part of the wire this walk never touches. Agreement in order is
// what says the field was located rather than merely decoded consistently.
//
// The ids are spelled out rather than derived, so a regenerated capture that
// shifts them fails loudly instead of agreeing with itself.
func TestDumpReplay_OCIDriveReadsTheCursorTheClientIsDriving(t *testing.T) {
	t.Parallel()

	read := ociDriveCursorIDs(t)

	assert.Equal(t, []uint16{2, 5}, read, "the ids dbbat now reads out of the sqlplus drives")
	assert.Equal(t, ociRefCursorBindOutputIDs(t), read,
		"every id the gate reads must be the id the server handed back in the call's bind output, in order")

	byHand := make([]uint16, 0, len(read))
	for _, payload := range recordedFrames(t, ociRefCursorDrives) {
		byHand = append(byHand, ociDrivenCursorID(t, extractTTCPayload(payload)))
	}

	assert.Equal(t, byHand, read, "and the id the independent hand-walk reads out of the same frame")
}

// TestWideExecCarryingAStatementIsNeverAReexecution is the negative half, and it
// matters as much as the positive one: an OCI parse must keep reading as a
// parse. A false positive here does not merely mis-log — it gates the parse
// against whatever cursor those four bytes happened to hold, which is the
// accounting-overwrite class the REF-cursor work spent two specs avoiding.
//
// Swept over the whole corpus rather than a hand-built frame, because "every
// recorded OCI parse" is the property, not "one of them".
func TestWideExecCarryingAStatementIsNeverAReexecution(t *testing.T) {
	t.Parallel()

	parses := map[string]int{}

	for _, name := range surveyCorpus(t) {
		for _, ttc := range surveyClientTTC(t, loadTestDump(t, name)) {
			for _, body := range surveyExecOps(ttc) {
				field, wide := execSQLLengthWideField(body)
				if !wide {
					continue
				}

				parses[name]++

				assert.Positivef(t, field.value, "%s: a wide header that fits declares a statement", name)

				_, reexec := execNoStatementCursor(body)
				assert.Falsef(t, reexec,
					"%s: a wide exec carrying a %d-byte statement must stay a parse", name, field.value)

				// The reason it stays a parse, stated as the measurement it is:
				// a parse asks the server to allocate a cursor, so the field the
				// re-execution reading uses is zero on every one of them.
				assert.Equalf(t, []byte{0, 0, 0, 0}, body[execWideCursorIDAt:execWideCursorIDAt+4],
					"%s: a parse names no cursor", name)

				assert.Truef(t, frameCarriesStatement(body),
					"%s: and it must still be a statement frame for the gate and the rewriter", name)
			}
		}
	}

	t.Logf("wide exec ops carrying a statement, by recording: %v", parses)
	assert.NotEmpty(t, parses, "the corpus must still contain OCI parses, or this proves nothing")
}

// TestExecWideNoStatementCursorRefusesWhatItCannotRead pins the reading's edges.
// The cost of a false positive is a re-execution refused on a client that was
// working, so each mutation below is one byte away from the recorded frame.
func TestExecWideNoStatementCursorRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	recorded := extractTTCPayload(recordedFrames(t, ociRefCursorDrives)[0])

	end, ok := closeCursorsEnd(recorded)
	require.True(t, ok, "the drive staples its exec behind a close-cursors list")

	baseline := append([]byte(nil), recorded[end:]...)

	got, ok := execWideNoStatementCursor(baseline)
	require.True(t, ok)
	require.Equal(t, uint16(2), got)

	mutations := []struct {
		name string
		at   int
		to   byte
	}{
		{"a cursor id of zero is a parse", execWideCursorIDAt, 0x00},
		{"an id past sixteen bits is not a cursor", execWideCursorIDAt + 2, 0x01},
		{"a statement pointer sentinel means the frame carries SQL", execWideSentinelAt, 0xfe},
		{"a sentinel byte set anywhere is not a SQL-less frame", execWideSentinelAt + 7, 0x01},
		{"the pad byte must be the constant OCI writes", 3, 0x02},
		{"the sequence pad must be this header's own successor", 4, 0x00},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()

			frame := append([]byte(nil), baseline...)
			frame[m.at] = m.to

			_, ok := execWideNoStatementCursor(frame)
			assert.False(t, ok)
		})
	}

	t.Run("a truncated frame yields nothing", func(t *testing.T) {
		t.Parallel()

		for n := range execWideSQLLenAt {
			_, ok := execWideNoStatementCursor(baseline[:n])
			assert.Falsef(t, ok, "a %d-byte header must not resolve to a cursor", n)
		}
	})

	t.Run("and neither does a thin frame read this way", func(t *testing.T) {
		t.Parallel()

		thin, _ := recordedNoStatementExecs(t)
		require.NotEmpty(t, thin)

		_, ok := execWideNoStatementCursor(thin[0])
		assert.False(t, ok, "ojdbc6's thin header has its own walk")
	})
}

// TestDumpReplay_OCIDriveIsGatedAgainstItsCursorsStatement is the enforcement
// claim on the frame sqlplus actually sent: with the cursor tracked, the drive
// is resolved to the statement that cursor was parsed with and re-gated as a
// query of its own — refused under read_only and block_ddl when that statement
// is a write, through the dispatcher that decides whether bytes travel upstream.
//
// The cursor is planted rather than replayed, exactly as in
// TestDumpReplay_OJDBC6ReexecIsRefusedUnderStatementControls and for the same
// reason: under either control the statement would never have been parsed, and
// the exposure covered is a control that becomes relevant after the parse.
func TestDumpReplay_OCIDriveIsGatedAgainstItsCursorsStatement(t *testing.T) {
	t.Parallel()

	drive := extractTTCPayload(recordedFrames(t, ociRefCursorDrives)[0])
	cursorID := ociDriveCursorIDs(t)[0]

	tests := []struct {
		name    string
		control string
		sql     string
		wantErr error
	}{
		{
			name:    "read_only refuses the driven write",
			control: store.ControlReadOnly,
			sql:     "INSERT INTO dbbat_reexec_test VALUES (1)",
			wantErr: shared.ErrReadOnlyViolation,
		},
		{
			name:    "block_ddl refuses the driven DDL",
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
			s.tracker.cursors[cursorID] = &trackedCursor{
				cursorID: cursorID,
				sql:      tc.sql,
				parsedAt: time.Now(),
			}

			require.ErrorIs(t, s.handleJDBCExec(drive), tc.wantErr)
			assert.Nil(t, s.tracker.pendingQuery, "a refused re-execution must not be tracked as in flight")
		})
	}

	t.Run("a read is allowed and tracked", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientConn = drainedPipe(t)
		s.tracker.cursors[cursorID] = &trackedCursor{
			cursorID: cursorID,
			sql:      "SELECT * FROM dual",
			parsedAt: time.Now(),
		}

		require.NoError(t, s.handleJDBCExec(drive))
	})
}

// TestDumpReplay_OCIDriveOfAnUntrackedCursorFailsClosed is the symmetry claim:
// the wide SQL-less execute answers an untracked cursor exactly like the other
// three re-execution frames, through the same refuseUnknownCursor the thin path
// uses. The wire encoding a client picks cannot change the answer.
func TestDumpReplay_OCIDriveOfAnUntrackedCursorFailsClosed(t *testing.T) {
	t.Parallel()

	drive := extractTTCPayload(recordedFrames(t, ociRefCursorDrives)[0])

	t.Run("refused under a statement-shaped control", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientConn = drainedPipe(t)

		require.ErrorIs(t, s.handleJDBCExec(drive), ErrUnknownCursor)
		assert.Nil(t, s.tracker.pendingQuery, "a refused execution must not be tracked as in flight")
	})

	t.Run("forwarded without one", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})

		require.NoError(t, s.handleJDBCExec(drive),
			"a grant with no statement-shaped control must not be broken by an unidentified execution")
		assert.Nil(t, s.tracker.pendingQuery, "a forwarded but unidentified execution is not tracked")
	})
}

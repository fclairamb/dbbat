package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// refCursorBindOutput is the go-ora recording's second call, byte for byte: a
// bind-output message carrying one `SYS_REFCURSOR` descriptor whose two columns
// are a NUMBER named "N" and a VARCHAR named "LABEL", ending on cursor id 7 and
// then the summary object. See testdata/go_ora_refcursor.pcapng and
// refcursor_bind_test.go, which pins the same bytes against the id the client
// then drives.
var refCursorBindOutput = []byte{
	0x07,
	0x4c, 0x01, 0x42, 0x01, 0x02, 0x82,
	0x02, 0x00, 0x00, 0x00, 0x01, 0x16, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x01, 0x01, 0x01, 0x01, 0x4e, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x80, 0x00, 0x00, 0x01, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x02, 0x03, 0x69, 0x01,
	0x01, 0x2c, 0x02, 0x3f, 0xfe, 0x01, 0x05, 0x01, 0x05, 0x05, 0x4c, 0x41, 0x42, 0x45, 0x4c,
	0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x01, 0x07, 0x07, 0x78, 0x7e, 0x09, 0x13, 0x10, 0x31, 0x1f,
	0x00, 0x02, 0x1f, 0xe8, 0x00, 0x00, 0x00,
	0x01, 0x07,
	0x00, 0x04, 0x03, 0x01, 0x00, 0x05,
}

const refCursorCall = "BEGIN dbbat_learn_refcur(:1); END;"

// sessionMidCall returns a session with the REF-cursor call in flight, as it is
// while the server's response to that statement is being read.
func sessionMidCall(t *testing.T, controls ...string) *session {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{Controls: controls}})
	cursor := &trackedCursor{cursorID: 6, sql: refCursorCall, parsedAt: time.Now()}
	s.tracker.cursors[6] = cursor
	s.tracker.pendingQuery = &pendingOracleQuery{cursor: cursor, startTime: time.Now()}

	return s
}

// TestLearnRefCursorIDs_TracksTheCursorAgainstTheCall is the end the whole spec
// is about: the id the server reports in a call's bind output lands in the
// tracker, so the fetch the client drives on it resolves instead of meeting
// refuseUnknownCursor.
//
// It also pins *what* the entry says. The `SELECT` the cursor runs was opened
// inside the procedure body and never crossed the wire, so the only statement
// dbbat can honestly attribute this execution to is the call — and the entry
// says so in a SQL comment rather than letting /queries imply otherwise.
func TestLearnRefCursorIDs_TracksTheCursorAgainstTheCall(t *testing.T) {
	t.Parallel()

	s := sessionMidCall(t)

	s.learnRefCursorIDs(refCursorBindOutput)

	learned, ok := s.tracker.cursors[7]
	require.True(t, ok, "the REF cursor id from the bind output must be tracked")
	assert.Equal(t, uint16(7), learned.cursorID)
	assert.True(t, learned.fromRefCursorBind)

	assert.Equal(t, refCursorCall+refCursorNote, learned.sql,
		"the entry carries the call — the statement the grant already gated — annotated as such")
	assert.Contains(t, learned.sql, "never crossed the wire")

	// The call's own cursor is untouched: the two ids are different cursors and
	// learnCursorID owns the other one.
	assert.Equal(t, refCursorCall, s.tracker.cursors[6].sql)
}

// TestLearnRefCursorIDs_ResolvesTheDriveThatUsedToBeRefused closes the loop from
// the user's side. Under a `read_only` grant — a statement-shaped control — the
// drive of a cursor dbbat was never shown is ORA-01031 (ErrUnknownCursor); once
// the id has been learned from the bind output, the very same frame is gated
// against the call and allowed through.
func TestLearnRefCursorIDs_ResolvesTheDriveThatUsedToBeRefused(t *testing.T) {
	t.Parallel()

	drive := buildPiggybackReexec(7)

	refused := sessionMidCall(t, store.ControlReadOnly)
	require.ErrorIs(t, refused.handlePiggybackReexec(drive), ErrUnknownCursor,
		"without the bind output the drive is still refused — the fail-closed rule is unchanged")

	s := sessionMidCall(t, store.ControlReadOnly)
	s.learnRefCursorIDs(refCursorBindOutput)

	require.NoError(t, s.handlePiggybackReexec(drive))
	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, refCursorCall+refCursorNote, s.tracker.pendingQuery.cursor.sql,
		"the drive is charged to the call, which is the only statement text this execution has")
}

// TestLearnRefCursorIDs_LeavesAnUnlearnedCursorRefused is the other half of that
// claim, and the one that matters for security: learning a REF cursor id must
// not soften refuseUnknownCursor for any id that was never learned. An id the
// bind output did not name is still refused under a restrictive grant.
func TestLearnRefCursorIDs_LeavesAnUnlearnedCursorRefused(t *testing.T) {
	t.Parallel()

	s := sessionMidCall(t, store.ControlReadOnly)
	s.learnRefCursorIDs(refCursorBindOutput)

	require.ErrorIs(t, s.handlePiggybackReexec(buildPiggybackReexec(31)), ErrUnknownCursor)
	require.ErrorIs(t, s.handleCursorReexec(31), ErrUnknownCursor)
}

// TestLearnRefCursorIDs_OnlyRunsWhereABindOutputCanExist pins the two gates that
// keep the locator away from payloads it has no business walking. Both matter:
// a `0x07` message is also what carries ordinary row data, so a locator offered
// every response would be walking rows — and the annotated entry a REF cursor
// drive runs under itself starts with the call's `BEGIN`, so without the flag it
// would pass the statement gate while its own rows stream back.
func TestLearnRefCursorIDs_OnlyRunsWhereABindOutputCanExist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*session)
	}{
		{
			name:    "no call is in flight",
			prepare: func(s *session) { s.tracker.pendingQuery = nil },
		},
		{
			name: "the statement in flight is an ordinary query",
			prepare: func(s *session) {
				s.tracker.pendingQuery.cursor.sql = "SELECT 1 AS n FROM dual"
			},
		},
		{
			name: "the statement in flight is itself a learned REF cursor",
			prepare: func(s *session) {
				s.tracker.pendingQuery.cursor.fromRefCursorBind = true
			},
		},
		{
			name: "this execution already yielded its ids",
			prepare: func(s *session) {
				s.tracker.pendingQuery.refCursorsLearned = true
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := sessionMidCall(t)
			tc.prepare(s)

			s.learnRefCursorIDs(refCursorBindOutput)

			assert.NotContains(t, s.tracker.cursors, uint16(7))
		})
	}
}

// TestStatementIsAPLSQLCall covers the gate on its own, including the shapes the
// three recorded clients actually send — go-ora and JDBC upper-case `BEGIN`,
// python-oracledb's callproc lower-cases it — and the annotated text dbbat
// itself writes, which must still be recognized so a second call learns again.
func TestStatementIsAPLSQLCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sql  string
		want bool
	}{
		{sql: "BEGIN dbbat_learn_refcur(:1); END;", want: true},
		{sql: "begin dbbat_py_refcur(:1); end;", want: true},
		{sql: "  \n DECLARE x NUMBER; BEGIN NULL; END;", want: true},
		{sql: "CALL dbbat_learn_refcur(:1)", want: true},
		{sql: "/* a leading hint */ BEGIN p(:1); END;", want: true},
		{sql: "-- a leading line comment\nBEGIN p(:1); END;", want: true},
		{sql: refCursorCall + refCursorNote, want: true},
		{sql: "SELECT 1 AS n FROM dual", want: false},
		{sql: "INSERT INTO t VALUES (1)", want: false},
		{sql: "BEGINNING_OF_TIME(:1)", want: false},
		{sql: "CALLBACK(:1)", want: false},
		{sql: "/* never closed BEGIN p; END;", want: false},
		{sql: "-- only a comment", want: false},
		{sql: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, statementIsAPLSQLCall(tc.sql))
		})
	}
}

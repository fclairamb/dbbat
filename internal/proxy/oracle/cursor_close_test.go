package oracle

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestDecodeCloseCursors pins the close-cursors piggyback byte-for-byte, on
// frames lifted verbatim out of the recordings in testdata/.
//
// The layout was not obvious and the first reading of it was wrong: dbbat used
// to take `03 09 <byte>` for "close cursor <byte>", which is the session logoff
// with its TTC sequence number. Getting this wrong deletes tracker entries the
// client never closed, and an entry deleted early turns a correctly-gated
// re-execution into an ORA-01031 — so the bytes are pinned here rather than
// inferred anywhere.
func TestDecodeCloseCursors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
		want    []uint16
	}{
		{
			// testdata/go_ora_cursor_reexec.pcapng, client frame 9. The trailing
			// `03 93 0a 00` is the next TTC message in the same packet, which is
			// why the list is not required to consume the frame.
			name:    "go-ora, 23ai token byte present",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x01, 0x01, 0x01, 0x01, 0x02, 0x03, 0x93, 0x0a, 0x00},
			want:    []uint16{2},
		},
		{
			// testdata/go_ora.pcapng, client frame 9: same client, older
			// capture, no token byte — the pointer flag sits right after the
			// sequence number.
			name:    "go-ora, no token byte",
			payload: []byte{0x11, 0x69, 0x00, 0x01, 0x01, 0x01, 0x01, 0x03, 0x03, 0x93, 0x00},
			want:    []uint16{3},
		},
		{
			// testdata/jdbc_thin_cursor_reexec.pcapng, client frame 7. Two more
			// messages follow the list in the same packet.
			name: "JDBC thin",
			payload: []byte{
				0x11, 0x69, 0x07, 0x00, 0x01, 0x01, 0x01, 0x01, 0x02,
				0x11, 0xbf, 0x08, 0x00, 0x01, 0x01, 0x09, 0x00,
			},
			want: []uint16{2},
		},
		{
			// testdata/dbeaver.pcapng, client frame 70 — the batched close the
			// whole fix is about: count 3, then ids 5, 3 and 4, then the
			// execute the client stapled behind it. Reading one id here is
			// exactly how two stale entries used to survive.
			name: "DBeaver closes three cursors at once",
			payload: []byte{
				0x11, 0x69, 0x77, 0x01, 0x01, 0x03, 0x01, 0x05, 0x01, 0x03, 0x01, 0x04,
				0x03, 0x5e, 0x78, 0x02, 0x80, 0x29,
			},
			want: []uint16{5, 3, 4},
		},
		{
			// A two-byte id, to show the walk is a compressed-int walk and not
			// a fixed stride.
			name:    "wide cursor ids",
			payload: []byte{0x11, 0x69, 0x04, 0x00, 0x01, 0x01, 0x02, 0x02, 0x01, 0x2c, 0x01, 0x07},
			want:    []uint16{300, 7},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := decodeCloseCursors(tc.payload)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDecodeCloseCursorsRejectsWhatItCannotRead is the other half, and the more
// important one: every frame here must delete **nothing**. A half-read list
// evicts cursors the client still holds, and the failure mode of an early
// eviction is a refusal of ordinary work.
func TestDecodeCloseCursorsRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: nil},
		{name: "message type only", payload: []byte{0x11}},
		{
			name:    "the session logoff, which carries no cursor id at all",
			payload: []byte{0x03, 0x09, 0x0b, 0x00},
		},
		{
			name:    "a piggyback re-execution",
			payload: []byte{0x03, 0x4e, 0x06, 0x00, 0x01, 0x02, 0x01, 0x19, 0x01, 0x20, 0x01, 0x01},
		},
		{
			name:    "the other func-0x11 exec sub-op",
			payload: []byte{0x11, 0x98, 0x04, 0x00, 0x01, 0x01, 0x01, 0x01, 0x02},
		},
		{
			name:    "truncated right after the header",
			payload: []byte{0x11, 0x69, 0x09, 0x00},
		},
		{
			name:    "no pointer flag",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x02, 0x01, 0x01, 0x01, 0x02},
		},
		{
			// testdata/sqlplus_cursor_reexec.pcapng, client frame 9: the OCI
			// thick client sends the same op in the wide encoding — an 8-byte
			// pointer sentinel and little-endian 32-bit fields. Guessing at it
			// would delete ids read out of the middle of a sentinel.
			name: "the OCI wide encoding",
			payload: []byte{
				0x11, 0x69, 0x08, 0x01, 0x09, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
				0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00,
			},
		},
		{
			name:    "count says two, only one id present",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x01, 0x01, 0x02, 0x01, 0x02},
		},
		{
			name:    "an implausible count",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x01, 0x03, 0xff, 0xff, 0xff, 0x01, 0x02},
		},
		{
			name:    "cursor id zero means allocate, not close",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x01, 0x01, 0x01, 0x01, 0x00},
		},
		{
			name:    "a cursor id past 16 bits",
			payload: []byte{0x11, 0x69, 0x09, 0x00, 0x01, 0x01, 0x01, 0x03, 0x01, 0x00, 0x00},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := decodeCloseCursors(tc.payload)
			require.ErrorIs(t, err, ErrNotCloseCursors)
			assert.Empty(t, got, "a frame that did not decode must name no cursor")
		})
	}
}

// TestDecodeCloseCursorsNeverPanics feeds every prefix of a real close frame,
// plus a run of hostile shapes, through the decoder. This runs on the client
// leg of a live session, so a panic here is a dropped connection.
func TestDecodeCloseCursorsNeverPanics(t *testing.T) {
	t.Parallel()

	recorded := []byte{
		0x11, 0x69, 0x77, 0x01, 0x01, 0x03, 0x01, 0x05, 0x01, 0x03, 0x01, 0x04,
		0x03, 0x5e, 0x78, 0x02, 0x80, 0x29,
	}

	for i := 0; i <= len(recorded); i++ {
		_, _ = decodeCloseCursors(recorded[:i])
	}

	hostile := [][]byte{
		{0x11, 0x69, 0x00, 0x01, 0xff},
		{0x11, 0x69, 0x00, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x11, 0x69, 0x00, 0x01, 0x01, 0x0a, 0x08},
	}
	for _, payload := range hostile {
		_, _ = decodeCloseCursors(payload)
	}
}

// TestDumpReplay_CloseListsAreRealAndBatched reads the close lists out of the
// recordings themselves rather than out of hand-copied bytes, so a decoder that
// stopped recognizing what real clients send fails here.
//
// It also settles the question the spec left open — do clients really batch? —
// with the answer from the wire: DBeaver does, in `dbeaver.pcapng`.
func TestDumpReplay_CloseListsAreRealAndBatched(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		dump  string
		wants [][]uint16
	}{
		{name: "go-ora SELECT", dump: goOraReexecDump, wants: [][]uint16{{2}}},
		{
			name: "go-ora INSERT", dump: goOraDMLReexecDump,
			wants: [][]uint16{{4}, {5}, {6}, {8}},
		},
		{name: "JDBC thin", dump: jdbcReexecDump, wants: [][]uint16{{2}}},
		{
			// python-oracledb held its cursor open for the whole recording and
			// let the logoff take it. Nothing to close, and nothing that the
			// old one-id read could have got right either.
			name: "python-oracledb thin closes nothing", dump: pythonReexecDump, wants: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wants, recordedCloseLists(t, tc.dump))
		})
	}

	t.Run("DBeaver batches its closes", func(t *testing.T) {
		t.Parallel()

		lists := recordedCloseLists(t, "dbeaver.pcapng")
		require.NotEmpty(t, lists)

		var batched [][]uint16

		for _, ids := range lists {
			if len(ids) > 1 {
				batched = append(batched, ids)
			}
		}

		assert.Equal(t, [][]uint16{{5, 3, 4}}, batched,
			"reading one id per frame is what left the other two behind as stale entries")
	})
}

// recordedCloseLists returns the cursor ids of every close-cursors piggyback in
// a recording, one slice per frame.
func recordedCloseLists(t *testing.T, name string) [][]uint16 {
	t.Helper()

	var out [][]uint16

	for _, ttc := range clientTTCPayloads(t, name) {
		if !IsCloseCursorsPiggyback(ttc) {
			continue
		}

		cursorIDs, err := decodeCloseCursors(ttc)
		require.NoErrorf(t, err, "recorded close list must decode: % x", ttc)

		out = append(out, cursorIDs)
	}

	return out
}

// TestDumpReplay_ClosedCursorsLeaveTheTracker is the end-to-end claim on real
// frames: run a recording through the proxy's own intercept paths and the
// cursors the client closed are gone from the tracker afterwards.
//
// This is the whole spec in one assertion. The tracker used to keep them —
// `handleOCLOSE` was fed the logoff's sequence number — and because Oracle
// recycles cursor ids, a leftover entry is what makes a later re-execution
// resolve to a statement the client is not running.
func TestDumpReplay_ClosedCursorsLeaveTheTracker(t *testing.T) {
	t.Parallel()

	for _, dump := range []string{goOraReexecDump, goOraDMLReexecDump, jdbcReexecDump} {
		t.Run(dump, func(t *testing.T) {
			t.Parallel()

			closed := recordedCloseLists(t, dump)
			require.NotEmpty(t, closed, "this recording must close something")

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.Zero(t, replaySession(t, s, dump), "a full-write grant blocks nothing")

			for _, ids := range closed {
				for _, id := range ids {
					assert.NotContainsf(t, s.tracker.cursors, id,
						"cursor %d was closed by the client and must not still be tracked", id)
				}
			}
		})
	}
}

// TestHandleCloseCursorsReportsTheTrackerSize guards the counter
// TestIntegration_CursorIDLearningMissRate reads to assert the tracker stays
// bounded. That assertion is what makes "no cap is needed" a measured claim,
// and it would go silently vacuous if this record stopped carrying `tracked`.
func TestHandleCloseCursorsReportsTheTrackerSize(t *testing.T) {
	t.Parallel()

	logs := newCountingHandler()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.logger = slog.New(logs)

	for id := uint16(1); id <= 5; id++ {
		s.tracker.cursors[id] = &trackedCursor{cursorID: id, sql: "SELECT 1 FROM dual"}
	}

	s.handleCloseCursors([]uint16{1, 2, 3})

	assert.Len(t, s.tracker.cursors, 2)
	assert.Equal(t, 1, logs.count(logMsgCursorsClosed))
	assert.Equal(t, []int64{3}, logs.intsFor(logMsgCursorsClosed, "closed"))
	assert.Equal(t, []int64{2}, logs.intsFor(logMsgCursorsClosed, "tracked"))

	peak, seen := logs.maxIntFor(logMsgCursorsClosed, "tracked")
	assert.True(t, seen, "the measurement's bound must have something to read")
	assert.Equal(t, int64(2), peak)

	_, seen = logs.maxIntFor(logMsgCursorsClosed, "no_such_attr")
	assert.False(t, seen, "an unrecorded attribute must not report a bound of zero")
}

// TestRememberCursorWarnsOnARecycledID covers the signal the spec asked for.
//
// Oracle really does hand the same cursor id to a different statement, so the
// overwrite is correct and must never become a refusal. What was missing is any
// trace of it: the original wrong-SQL gate went unnoticed for a whole spec
// because the moment a stale entry was replaced — or, worse, was *not* replaced
// and kept answering for a statement long gone — looked exactly like normal
// operation in the logs.
func TestRememberCursorWarnsOnARecycledID(t *testing.T) {
	t.Parallel()

	t.Run("different SQL on the same id warns, and still overwrites", func(t *testing.T) {
		t.Parallel()

		logs := newCountingHandler()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		s.logger = slog.New(logs)
		s.tracker.cursors[7] = &trackedCursor{cursorID: 7, sql: "SELECT 35 AS churn FROM dual"}

		fresh := &trackedCursor{cursorID: 7, sql: "SELECT 1 AS n FROM dual"}
		s.rememberCursor(7, fresh)

		assert.Equal(t, 1, logs.count(logMsgRecycledCursorID))
		assert.Equal(t, []string{"SELECT 1 AS n FROM dual"}, logs.sqlsFor(logMsgRecycledCursorID))
		assert.Same(t, fresh, s.tracker.cursors[7],
			"the warning is a signal, never a refusal — the server owns the id")
	})

	t.Run("a fresh id is silent", func(t *testing.T) {
		t.Parallel()

		logs := newCountingHandler()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		s.logger = slog.New(logs)

		s.rememberCursor(7, &trackedCursor{cursorID: 7, sql: "SELECT 1 AS n FROM dual"})

		assert.Zero(t, logs.count(logMsgRecycledCursorID))
	})

	t.Run("re-recording the same statement is silent", func(t *testing.T) {
		t.Parallel()

		logs := newCountingHandler()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		s.logger = slog.New(logs)
		s.tracker.cursors[7] = &trackedCursor{cursorID: 7, sql: "SELECT 1 AS n FROM dual"}

		s.rememberCursor(7, &trackedCursor{cursorID: 7, sql: "SELECT 1 AS n FROM dual"})

		assert.Zero(t, logs.count(logMsgRecycledCursorID),
			"a client re-parsing the same statement onto the same id is not a recycle")
	})
}

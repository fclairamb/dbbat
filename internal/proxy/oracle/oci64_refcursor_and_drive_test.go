package oracle

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// The 64-bit OCI dialect's half of the REF-cursor evidence, and it is the half
// CI actually exercises: `.github/workflows/integration.yml` reaches the
// sqlplus bundled in the Oracle image back over host.docker.internal, and that
// client writes 8-byte integers after a 17-byte op header where the Instant
// Client writes 4-byte ones after a 5-byte one (isCloseCursorsWide8Header).
//
// Until this landed, neither half of the feature held there: no REF cursor id
// was read out of a call's bind output, so no drive could resolve, so every
// drive — REF cursor or ordinary cursor — was forwarded **ungated**, with
// read_only, block_ddl, the approval patterns, the `queries` row and the quota
// applying to the parse alone.
//
// Every fixture below comes from one live sqlplus 23.26 session through dbbat
// (capture_oci_fixtures_integration_test.go), which is what the bytes have to
// be: recorded off a bare relay, this client's op headers carry a different
// sequence pad and nothing downstream decodes. See ociFixtureProvenance.
const (
	oci64RefCursorBindOutputs = "testdata/oci64_refcursor_bind_output.hex"
	oci64RefCursorDrives      = "testdata/oci64_refcursor_drives.hex"
	oci64ScalarOutBinds       = "testdata/oci64_scalar_outbind_bind_output.hex"
	oci64ParseExecs           = "testdata/oci64_parse_execs.hex"
)

// oci64RefCursorIDs are the ids the server handed back over that session, in
// order. They are spelled out rather than derived so a regenerated capture that
// shifts them fails loudly instead of agreeing with itself — and they are not
// all the same number on purpose: the script takes a cursor out of circulation
// between calls, so a locator that latched onto the first id would be caught.
var oci64RefCursorIDs = []uint16{3, 2, 3}

// oci64OERShape is a session that has learned its upstream speaks the 64-bit
// fixed-width OCI encoding — what an sqlplus session of that flavor learns off
// the AUTH exchange, long before any statement runs (learnOERShape, and
// usesWide64OpHeader on the client's own Phase 1).
func oci64OERShape() oerShape {
	shape := ociOERShape()
	shape.fixedWidth64 = true

	return shape
}

// oci64RefCursorBindOutputIDs runs the locator over the recorded call responses
// as a 64-bit OCI session sees them, and returns the ids it learned.
func oci64RefCursorBindOutputIDs(t *testing.T) []uint16 {
	t.Helper()

	ids := make([]uint16, 0, len(oci64RefCursorIDs))

	for i, payload := range recordedFrames(t, oci64RefCursorBindOutputs) {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)
		require.Equalf(t, byte(ttcMsgIOVector), ttc[0],
			"frame %d must be a call's bind-output response", i)

		ids = append(ids, refCursorIDsInBindOutput(oci64OERShape(), ttc)...)
	}

	return ids
}

// oci64DrivenCursorID reads the cursor id out of one recorded drive by hand.
//
// It is deliberately written out here rather than taken from a decoder: this is
// the independent witness the walks under test are checked against, so it must
// not share code with them. The offsets are the ones
// execWide64NoStatementCursor documents — past the 17-byte op header, past the
// four option bytes — and the reading is only meaningful because the same field
// is zero on every recorded parse (TestOCI64ExecCarryingAStatementIsNeverAReexecution).
func oci64DrivenCursorID(t *testing.T, ttc []byte) uint16 {
	t.Helper()

	body := ttc

	if end, ok := closeCursorsEnd(ttc); ok && end < len(ttc) {
		body = ttc[end:]
	}

	require.True(t, isPiggybackExecHeader(body), "the drive must staple an execute op behind its closes")
	require.GreaterOrEqual(t, len(body), 25, "the 64-bit exec header is 25 bytes before its statement fields")
	require.Equalf(t, []byte{0, 0}, body[3:5], "the 64-bit exec header's pad")

	// The field is four bytes; a cursor id is sixteen. Insisting on the top half
	// being zero is what keeps the comparison honest rather than truncating a
	// number that would not have matched.
	require.Equalf(t, []byte{0, 0}, body[23:25], "the drive's cursor id must fit sixteen bits")

	return uint16(body[21]) | uint16(body[22])<<8
}

// oci64DriveCursorIDs runs the gate's own reading over the recorded drives.
func oci64DriveCursorIDs(t *testing.T) []uint16 {
	t.Helper()

	frames := recordedFrames(t, oci64RefCursorDrives)
	ids := make([]uint16, 0, len(frames))

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		cursorID, ok := execNoStatementCursor(ttc, true)
		require.Truef(t, ok, "drive %d declares no statement, so it must read as a re-execution", i)

		ids = append(ids, cursorID)
	}

	return ids
}

// TestDumpReplay_OCI64RefCursorIDsMatchTheCursorsTheClientDrives is what makes
// the bind-output walk trustworthy on this dialect rather than merely
// self-consistent.
//
// The walk reads the ids out of the *server's* call responses; the client's own
// next frame then drives those cursors, in a part of the wire the walk never
// touches. Agreement in order is the falsifiable claim — and it is falsifiable,
// because the three ids are not the same number.
func TestDumpReplay_OCI64RefCursorIDsMatchTheCursorsTheClientDrives(t *testing.T) {
	t.Parallel()

	learned := oci64RefCursorBindOutputIDs(t)

	drives := recordedFrames(t, oci64RefCursorDrives)
	require.Len(t, learned, len(drives),
		"one id per recorded call response, or the pairing below compares different things")

	driven := make([]uint16, 0, len(drives))
	for _, payload := range drives {
		driven = append(driven, oci64DrivenCursorID(t, extractTTCPayload(payload)))
	}

	assert.Equal(t, oci64RefCursorIDs, learned, "the ids read out of the 64-bit sqlplus bind-output")
	assert.Equal(t, learned, driven,
		"every learned id must be the one sqlplus then drives — that agreement is what says the "+
			"field was located correctly rather than merely decoded consistently")
	assert.Greater(t, len(uniqueIDs(learned)), 1,
		"the recording must hand out more than one id, or a locator latching onto the first would pass")
}

// uniqueIDs is the distinct set of ids, for the bound above.
func uniqueIDs(ids []uint16) []uint16 {
	seen := map[uint16]struct{}{}

	var out []uint16

	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}

		seen[id] = struct{}{}

		out = append(out, id)
	}

	return out
}

// TestDumpReplay_OCI64DriveReadsTheCursorTheClientIsDriving is the same
// cross-check for the gate's half: the id the re-execution reading pulls out of
// a drive must be the id the hand-walk reads out of the same frame, and the id
// the bind-output walk read out of the call before it.
func TestDumpReplay_OCI64DriveReadsTheCursorTheClientIsDriving(t *testing.T) {
	t.Parallel()

	read := oci64DriveCursorIDs(t)

	assert.Equal(t, oci64RefCursorIDs, read, "the ids dbbat reads out of the 64-bit sqlplus drives")
	assert.Equal(t, oci64RefCursorBindOutputIDs(t), read,
		"every id the gate reads must be the id the server handed back in the call's bind output, in order")

	byHand := make([]uint16, 0, len(read))
	for _, payload := range recordedFrames(t, oci64RefCursorDrives) {
		byHand = append(byHand, oci64DrivenCursorID(t, extractTTCPayload(payload)))
	}

	assert.Equal(t, byHand, read, "and the id the independent hand-walk reads out of the same frame")
}

// TestOCI64ExecCarryingAStatementIsNeverAReexecution is the negative half of the
// gate, and it matters as much as the positive one: a parse must keep reading as
// a parse. A false positive here does not merely mis-log — it gates the parse
// against whatever cursor those four bytes happened to hold, and the visible
// result is an ORA-01031 on work that used to run.
//
// The fixture is the same session's own statement-carrying frames, picked out of
// the recording by searching for the statement's text rather than by decoding
// anything. Ordinary queries on this dialect are covered live instead, by
// TestIntegration_RepeatedStatementFromSQLPlusUnderReadOnly: a repeated SELECT
// must not be refused, which is the same claim from the other end.
func TestOCI64ExecCarryingAStatementIsNeverAReexecution(t *testing.T) {
	t.Parallel()

	frames := recordedFrames(t, oci64ParseExecs)
	require.NotEmpty(t, frames, "the session must have parsed something, or this proves nothing")

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		_, reexec := execNoStatementCursor(ttc, true)
		assert.Falsef(t, reexec, "parse %d carries a statement and must stay a parse", i)

		assert.Truef(t, frameCarriesStatement(ttc, true),
			"parse %d must still be a statement frame for the gate and the rewriter", i)
	}
}

// TestOCI64ReadingsAreOfferedToTheirOwnDialectOnly holds the gate from both
// sides, for both walks.
//
// Which reading a session gets is asked of its learned shape and never of the
// payload, so each dialect's frames must decode under their own reading and
// yield nothing under the other. That is not tidiness: the three exec headers
// are each other's near-misses, and a payload offered two layouts is a payload
// with two chances to produce a plausible number — which, planted in the
// tracker, lets a fetch resolve against another statement's grant and text.
func TestOCI64ReadingsAreOfferedToTheirOwnDialectOnly(t *testing.T) {
	t.Parallel()

	t.Run("the bind-output walk", func(t *testing.T) {
		t.Parallel()

		for i, payload := range recordedFrames(t, oci64RefCursorBindOutputs) {
			ttc := extractTTCPayload(payload)

			assert.NotEmptyf(t, refCursorIDsInBindOutput(oci64OERShape(), ttc),
				"frame %d must decode under the shape it was recorded from", i)
			assert.Emptyf(t, refCursorIDsInBindOutput(ociOERShape(), ttc),
				"frame %d must yield nothing to the 4-byte reading", i)
			assert.Emptyf(t, refCursorIDsInBindOutput(thinOERShape(), ttc),
				"frame %d must yield nothing to the compressed reading", i)
		}

		for i, payload := range recordedFrames(t, ociRefCursorBindOutputs) {
			ttc := extractTTCPayload(payload)

			assert.Emptyf(t, refCursorIDsInBindOutput(oci64OERShape(), ttc),
				"the 4-byte dialect's frame %d must yield nothing to the 64-bit reading", i)
		}
	})

	t.Run("the re-execution reading", func(t *testing.T) {
		t.Parallel()

		for i, payload := range recordedFrames(t, oci64RefCursorDrives) {
			ttc := extractTTCPayload(payload)

			_, ok := execNoStatementCursor(ttc, false)
			assert.Falsef(t, ok, "the 64-bit drive %d must yield nothing without the 64-bit reading", i)
		}

		for i, payload := range recordedFrames(t, ociRefCursorDrives) {
			ttc := extractTTCPayload(payload)

			_, ok := execNoStatementCursor(ttc, true)
			assert.Falsef(t, ok, "the 4-byte drive %d must yield nothing to the 64-bit reading", i)
		}
	})
}

// TestOCI64ReadingsFindNothingInTheRestOfTheCorpus is the sweep: every other
// recording is a thin or 4-byte client, and neither 64-bit reading may find
// anything in any of them. A recording added to testdata/ is opted into this
// automatically rather than being silently exempt.
func TestOCI64ReadingsFindNothingInTheRestOfTheCorpus(t *testing.T) {
	t.Parallel()

	reexecs := 0

	for _, name := range surveyCorpus(t) {
		dump := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, dump) {
			if _, ok := execNoStatementCursor(ttc, true); ok {
				reexecs++

				assert.Failf(t, "a non-64-bit frame read as a 64-bit re-execution",
					"%s: % x", name, ttc[:min(32, len(ttc))])
			}
		}

		for _, ttc := range serverTTCPayloads(t, name) {
			assert.Emptyf(t, refCursorIDsInBindOutput(oci64OERShape(), ttc),
				"%s: a non-64-bit server payload must yield no REF cursor id", name)
		}
	}

	assert.Zero(t, reexecs)
}

// TestOCI64ScalarOutBindsYieldNoRefCursorID is the false-positive bound on the
// bind-output walk, and it carries more weight on this dialect than on either
// other: the walk does not parse the column records at all (see
// refcursor_bind_wide64.go), so what stands between it and a wrong id is the
// descriptor header, the trailing block's own signature and the landing check.
//
// `BEGIN dbbat_cap_scalarout(:n, :s, :m); END;` through the same client puts a
// real bind-output block on the wire — the check below insists on it — carrying
// nothing but scalar values. Not one id may come out of it: an id learned here
// would be planted against the call, and rememberCursor **overwrites**, so a
// collision with a tracked cursor would replace that statement's text with an
// anonymous PL/SQL block's, which passes `read_only`.
func TestOCI64ScalarOutBindsYieldNoRefCursorID(t *testing.T) {
	t.Parallel()

	for i, payload := range recordedFrames(t, oci64ScalarOutBinds) {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		_, ok := bindOutputBodyStartWide64(ttc)
		require.Truef(t, ok, "frame %d must be a walkable bind-output response, or this proves nothing", i)

		assert.Emptyf(t, refCursorIDsInBindOutput(oci64OERShape(), ttc),
			"a call with only scalar OUT parameters must yield no REF cursor id: % x", ttc)
	}
}

// TestExecWide64NoStatementCursorRefusesWhatItCannotRead pins the reading's
// edges. The cost of a false positive is a re-execution refused on a client that
// was working, so each mutation below is one byte away from the recorded frame.
func TestExecWide64NoStatementCursorRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	recorded := extractTTCPayload(recordedFrames(t, oci64RefCursorDrives)[0])

	end, ok := closeCursorsEnd(recorded)
	require.True(t, ok, "the drive staples its exec behind a close-cursors list")

	baseline := append([]byte(nil), recorded[end:]...)

	got, ok := execWide64NoStatementCursor(baseline)
	require.True(t, ok)
	require.Equal(t, oci64RefCursorIDs[0], got)

	mutations := []struct {
		name string
		at   int
		to   byte
	}{
		{"a cursor id of zero is a parse", execWide64CursorIDAt, 0x00},
		{"an id past sixteen bits is not a cursor", execWide64CursorIDAt + 2, 0x01},
		{"a statement pointer sentinel means the frame carries SQL", execWide64SentinelAt, 0xfe},
		{"a sentinel byte set anywhere is not a SQL-less frame", execWide64SentinelAt + 7, 0x01},
		{"a declared statement length means the frame carries SQL", execWide64SQLLenAt, 0x21},
		{"the pad must be the two zeros this dialect writes", 3, 0x01},
		{"and both of them", 4, 0x01},
		{"the sequence pad must be this header's own successor", execWide64SeqPadAt, 0x00},
		{"all eight bytes of it", execWide64SeqPadAt + 7, 0x01},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()

			frame := append([]byte(nil), baseline...)
			frame[m.at] = m.to

			_, ok := execWide64NoStatementCursor(frame)
			assert.False(t, ok)
		})
	}

	t.Run("a truncated frame yields nothing", func(t *testing.T) {
		t.Parallel()

		for n := range execWide64MinLen {
			_, ok := execWide64NoStatementCursor(baseline[:n])
			assert.Falsef(t, ok, "a %d-byte header must not resolve to a cursor", n)
		}
	})
}

// TestDumpReplay_OCI64DriveIsGatedAgainstItsCursorsStatement is the enforcement
// claim on the frame sqlplus actually sent: with the cursor tracked, the drive
// resolves to the statement that cursor was parsed with and is re-gated as a
// query of its own — refused under read_only and block_ddl when that statement
// is a write, through the dispatcher that decides whether bytes travel upstream.
//
// The cursor is planted rather than replayed, exactly as in the 4-byte twin and
// for the same reason: under either control the statement would never have been
// parsed, and the exposure covered is a control that becomes relevant after the
// parse.
func TestDumpReplay_OCI64DriveIsGatedAgainstItsCursorsStatement(t *testing.T) {
	t.Parallel()

	drive := extractTTCPayload(recordedFrames(t, oci64RefCursorDrives)[0])
	cursorID := oci64RefCursorIDs[0]

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
			s.clientWide64Encoding = true
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
		s.clientWide64Encoding = true
		s.clientConn = drainedPipe(t)
		s.tracker.cursors[cursorID] = &trackedCursor{
			cursorID: cursorID,
			sql:      "SELECT * FROM dual",
			parsedAt: time.Now(),
		}

		require.NoError(t, s.handleJDBCExec(drive))
	})

	// The whole point of keying on the learned dialect: a session that never
	// learned it speaks this one must not read the frame at all, which is the
	// behavior every 4-byte and thin session keeps.
	t.Run("a session that did not learn the dialect does not read it", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientConn = drainedPipe(t)
		s.tracker.cursors[cursorID] = &trackedCursor{
			cursorID: cursorID,
			sql:      "INSERT INTO dbbat_reexec_test VALUES (1)",
			parsedAt: time.Now(),
		}

		assert.NotErrorIs(t, s.handleJDBCExec(drive), shared.ErrReadOnlyViolation,
			"the 64-bit reading is offered to 64-bit sessions and to nothing else")
	})
}

// TestDumpReplay_OCI64DriveOfAnUntrackedCursorFailsClosed is the symmetry claim:
// the 64-bit SQL-less execute answers an untracked cursor exactly like the other
// re-execution frames, through the same refuseUnknownCursor. The wire encoding a
// client picks cannot change the answer.
func TestDumpReplay_OCI64DriveOfAnUntrackedCursorFailsClosed(t *testing.T) {
	t.Parallel()

	drive := extractTTCPayload(recordedFrames(t, oci64RefCursorDrives)[0])

	t.Run("refused under a statement-shaped control", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{
			Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}},
		})
		s.clientWide64Encoding = true
		s.clientConn = drainedPipe(t)

		require.ErrorIs(t, s.handleJDBCExec(drive), ErrUnknownCursor)
		assert.Nil(t, s.tracker.pendingQuery, "a refused execution must not be tracked as in flight")
	})

	t.Run("forwarded without one", func(t *testing.T) {
		t.Parallel()

		s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
		s.clientWide64Encoding = true

		require.NoError(t, s.handleJDBCExec(drive),
			"a grant with no statement-shaped control must not be broken by an unidentified execution")
		assert.Nil(t, s.tracker.pendingQuery, "a forwarded but unidentified execution is not tracked")
	})
}

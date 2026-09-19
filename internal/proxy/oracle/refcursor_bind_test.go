package oracle

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// The recordings replayed here are the evidence refCursorIDsInBindOutput was
// written against, and the reason it is a walk rather than a scan. Each one
// calls the same procedure three times:
//
//	PROCEDURE dbbat_cap_refcur(p OUT SYS_REFCURSOR) AS
//	BEGIN
//	  OPEN p FOR SELECT LEVEL AS n, 'row-' || LEVEL AS label FROM dual CONNECT BY LEVEL <= 5;
//	END;
//
// so the server hands out a **fresh** id per OPEN. Regenerate with
// `go test -tags capture -run 'TestCapture_.*RefCursor' ./internal/proxy/oracle/`.
const (
	goOraRefCursorDump      = "go_ora_refcursor.pcapng"
	pythonThinRefCursorDump = "python_thin_refcursor.pcapng"
	jdbcThinRefCursorDump   = "jdbc_thin_refcursor.pcapng"
	sqlplusRefCursorDump    = "sqlplus_refcursor.pcapng"
)

// serverTTCPayloads returns the TTC payloads of every server→client Data packet
// in a recording, in order.
func serverTTCPayloads(t *testing.T, name string) [][]byte {
	t.Helper()

	var out [][]byte

	for _, pkt := range loadTestDump(t, name).Packets {
		if pkt.Direction != dump.DirServerToClient {
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

// recordedRefCursorIDs runs the locator over every server payload in a recording
// and returns the ids it learned, in order.
func recordedRefCursorIDs(t *testing.T, name string) []uint16 {
	t.Helper()

	var ids []uint16

	for _, ttc := range serverTTCPayloads(t, name) {
		ids = append(ids, refCursorIDsInBindOutput(ttc)...)
	}

	return ids
}

// drivenCursorIDs returns the cursor id of every client frame that executes a
// cursor without carrying a statement — which, in a REF-cursor recording, is
// exactly the drives of the cursors the procedure handed back.
func drivenCursorIDs(t *testing.T, name string) []uint16 {
	t.Helper()

	var ids []uint16

	for _, ttc := range clientTTCPayloads(t, name) {
		if TTCFunctionCode(ttc[0]) != TTCFuncPiggyback {
			continue
		}

		if IsPiggybackCursorReexec(ttc) {
			continue // a re-execution of the *call*, not a drive of its REF cursor
		}

		_, err := decodePiggybackExecSQL(ttc)

		var noSQL *PiggybackExecNoSQLError
		if errors.As(err, &noSQL) {
			ids = append(ids, noSQL.CursorID)
		}
	}

	return ids
}

// TestDumpReplay_RefCursorIDsMatchTheCursorsTheClientDrives is the measurement
// that makes the locator trustworthy: for three thin clients, every id read out
// of a call's bind-output is the very id the client then puts on the wire to
// drive that cursor — in order, with nothing extra.
//
// It is the whole proof against a mis-located field. A decoder reading one field
// too early or too late would still produce *a* number; only agreeing with the
// client's own next frame, three times over, on three independent driver
// implementations, says the field is the right one. The expected ids are spelled
// out rather than derived, so a regenerated capture that shifts them fails loudly
// instead of agreeing with itself.
func TestDumpReplay_RefCursorIDsMatchTheCursorsTheClientDrives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dump string
		want []uint16
	}{
		{name: "go-ora", dump: goOraRefCursorDump, want: []uint16{2, 7, 5}},
		{name: "python-oracledb thin", dump: pythonThinRefCursorDump, want: []uint16{4, 2, 4}},
		{name: "JDBC thin", dump: jdbcThinRefCursorDump, want: []uint16{4, 4, 4}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			learned := recordedRefCursorIDs(t, tc.dump)
			driven := drivenCursorIDs(t, tc.dump)

			require.Lenf(t, learned, refCursorDrivesInFixtures,
				"the recording drives %d REF cursors, so the locator must read %d ids: got %v",
				refCursorDrivesInFixtures, refCursorDrivesInFixtures, learned)

			assert.Equal(t, tc.want, learned, "the ids read out of the bind-output")
			assert.Equal(t, learned, driven,
				"every learned id must be the one the client then drives — that agreement is what "+
					"says the field was located correctly rather than merely decoded consistently")
		})
	}
}

// refCursorDrivesInFixtures is the loop count baked into every REF-cursor
// capture (refCursorDrives in capture_refcursor_test.go, which is behind the
// capture build tag).
const refCursorDrivesInFixtures = 3

// TestDumpReplay_RefCursorLocatorRefusesTheOCIEncoding pins the documented gap
// rather than leaving it to be discovered.
//
// sqlplus marshals the identical field list in the wide/fixed-width OCI encoding
// — four-byte little-endian integers where a thin client sends compressed ones —
// and the compressed walk refuses it at its first field rather than reading a
// number out of it. That refusal is the safe outcome: an OCI session keeps the
// behaviour it had before this existed (the drive stays an untracked cursor),
// whereas a number read out of the wrong encoding would gate a fetch against the
// wrong statement. See docs/oracle.md, "Learning a REF cursor's id".
func TestDumpReplay_RefCursorLocatorRefusesTheOCIEncoding(t *testing.T) {
	t.Parallel()

	assert.Empty(t, recordedRefCursorIDs(t, sqlplusRefCursorDump),
		"the OCI/wide encoding must yield no id at all rather than a mis-decoded one")
}

// TestDumpReplay_RefCursorLocatorIsSilentOnOrdinaryTraffic is the false-positive
// half. The locator runs on server payloads, and a `0x07` message is also what
// carries ordinary *row* data — so every recording in the corpus that has
// nothing to do with REF cursors is swept for an id, and none may appear.
//
// The session-level gate (only while a PL/SQL call is in flight) sits on top of
// this; this asserts the decoder does not need it to stay quiet.
func TestDumpReplay_RefCursorLocatorIsSilentOnOrdinaryTraffic(t *testing.T) {
	t.Parallel()

	quiet := []string{
		goOraReexecDump, goOraDMLReexecDump, pythonReexecDump, jdbcReexecDump, sqlplusReexecDump,
		"go_ora.pcapng", "go_ora_binds.pcapng", "go_ora_largeresult.pcapng", "go_ora_mixed.pcapng",
		"go_ora_numbers.pcapng", "go_ora_temporal.pcapng", "go_ora_failed_stmt.pcapng",
		"python_thin.pcapng", "python_thin_failed_stmt.pcapng", "python_thin_midfetch_fail.pcapng",
		"dbeaver.pcapng", "ojdbc6_legacy.pcapng", "sqlplus_midfetch_fail.pcapng",
	}

	for _, name := range quiet {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Empty(t, recordedRefCursorIDs(t, name),
				"a recording with no REF cursor in it must yield no REF cursor id")
		})
	}
}

// TestRefCursorIDsInBindOutput_RefusesWhatItCannotAccountFor holds the locator to
// its own bounds on synthetic input. Each case is one way a walk can go wrong,
// and every one of them must return nothing rather than a number.
func TestRefCursorIDsInBindOutput_RefusesWhatItCannotAccountFor(t *testing.T) {
	t.Parallel()

	// The go-ora recording's second call, byte for byte: a bind-output message
	// leading the payload, one descriptor, two columns (NUMBER "N", VARCHAR
	// "LABEL"), cursor id 7, then the OER that ends the call.
	good := []byte{
		0x07,
		0x4c, 0x01, 0x42, 0x01, 0x02, 0x82,
		// column 1: NUMBER, name "N"
		0x02, 0x00, 0x00, 0x00, 0x01, 0x16, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x01, 0x01, 0x01, 0x01, 0x4e, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// column 2: VARCHAR, name "LABEL"
		0x01, 0x80, 0x00, 0x00, 0x01, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x02, 0x03, 0x69, 0x01,
		0x01, 0x2c, 0x02, 0x3f, 0xfe, 0x01, 0x05, 0x01, 0x05, 0x05, 0x4c, 0x41, 0x42, 0x45, 0x4c,
		0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		// descriptor tail, then cursor id 7
		0x01, 0x07, 0x07, 0x78, 0x7e, 0x09, 0x13, 0x10, 0x31, 0x1f,
		0x00, 0x02, 0x1f, 0xe8, 0x00, 0x00, 0x00,
		0x01, 0x07,
		// the PL/SQL trailing int, then the summary object
		0x00, 0x04, 0x03, 0x01, 0x00, 0x05,
	}

	require.Equal(t, []uint16{7}, refCursorIDsInBindOutput(good),
		"the fixture this table mutates must itself decode")

	truncated := func(n int) []byte { return good[:len(good)-n] }

	// endingWith replaces the summary object the fixture ends on.
	endingWith := func(tail ...byte) []byte {
		return append(append([]byte(nil), good[:len(good)-5]...), tail...)
	}

	mutate := func(at int, to byte) []byte {
		out := make([]byte, len(good))
		copy(out, good)
		out[at] = to

		return out
	}

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty payload", payload: nil},
		{name: "not a bind-output or IO-vector message", payload: mutate(0, 0x08)},
		{name: "a column type no TNSType defines", payload: mutate(7, 0x37)},
		{name: "a column count the block cannot hold", payload: mutate(5, 0xfe)},
		{name: "the walk lands on a message the block never ends with", payload: endingWith(0x10, 0x07)},
		{name: "the walk lands on bytes that are no message at all", payload: endingWith(0x11, 0x69)},
		{name: "the cursor id runs off the end", payload: truncated(7)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Empty(t, refCursorIDsInBindOutput(tc.payload))
		})
	}
}

// TestRefCursorIDsInBindOutput_RefusesAZeroCursorID is its own case because a
// zero is what the field holds when the server allotted nothing, and go-ora
// treats it as ORA-01001 (invalid cursor) rather than as an id. Planting a 0 in
// the tracker would make every unrelated decode failure that yields 0 resolve to
// this call.
func TestRefCursorIDsInBindOutput_RefusesAZeroCursorID(t *testing.T) {
	t.Parallel()

	payload := []byte{
		0x07,
		0x4c, 0x01, 0x42, 0x00, // no columns at all
		0x00,                   // dlc
		0x00, 0x00, 0x00, 0x00, // the four version ints
		0x00, // dlc
		0x00, // cursor id: zero
		0x04, // the summary object
	}

	assert.Empty(t, refCursorIDsInBindOutput(payload))
}

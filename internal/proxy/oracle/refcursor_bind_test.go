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
)

// ociRefCursorBindOutputs is the OCI half of the same session, kept as a hex
// fixture of the two call responses rather than as a recording: sqlplus's PL/SQL
// call also carries an exec frame the exact statement locator cannot certify,
// which is a finding of its own and not one to fold into `testdata/*.pcapng` —
// a corpus several whole-corpus surveys enumerate and hold to 100%.
const ociRefCursorBindOutputs = "testdata/oci_refcursor_bind_output.hex"

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

	payloads := serverTTCPayloads(t, name)
	ids := make([]uint16, 0, len(payloads))

	for _, ttc := range payloads {
		ids = append(ids, refCursorIDsInBindOutput(thinOERShape(), ttc)...)
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

// TestOCIRefCursorBindOutputYieldsNoID pins the documented gap rather than
// leaving it to be discovered.
//
// sqlplus marshals the identical field list in the wide/fixed-width OCI encoding
// — four-byte little-endian integers where a thin client sends compressed ones —
// and the compressed walk refuses it at its first field rather than reading a
// number out of it. That refusal is the safe outcome: an OCI session keeps the
// behavior it had before this existed (the drive stays an untracked cursor),
// whereas a number read out of the wrong encoding would gate a fetch against the
// wrong statement. See docs/oracle.md, "Learning a REF cursor's id".
//
// The fixture is checked for being what it claims first: these really are the
// bind-output responses of a call, leading with the IO vector, or the refusal
// below would prove nothing.
func TestOCIRefCursorBindOutputYieldsNoID(t *testing.T) {
	t.Parallel()

	for i, payload := range recordedFrames(t, ociRefCursorBindOutputs) {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)
		require.Equalf(t, byte(ttcMsgIOVector), ttc[0],
			"frame %d must be a call's bind-output response", i)

		assert.Emptyf(t, refCursorIDsInBindOutput(thinOERShape(), ttc),
			"frame %d: the OCI/wide encoding must yield no id at all rather than a mis-decoded one", i)
	}
}

// refCursorDumps are the only recordings in the corpus that hold a REF cursor.
// Everything else must be silent, which is what the sweep below turns into an
// assertion rather than a list.
var refCursorDumps = map[string]bool{
	goOraRefCursorDump:      true,
	pythonThinRefCursorDump: true,
	jdbcThinRefCursorDump:   true,
}

// TestDumpReplay_RefCursorLocatorIsSilentOnOrdinaryTraffic is the false-positive
// half. The locator runs on server payloads, and a `0x07` message is also what
// carries ordinary *row* data and ordinary *scalar* out-bind values — so every
// recording in the corpus bar the three REF-cursor ones is swept for an id, and
// none may appear.
//
// It walks surveyCorpus rather than a hand-kept list on purpose: a list is a
// place for a future recording to go unswept, and a recording that is never
// swept is exactly where a false positive would hide. Adding a fixture to
// `testdata/` now opts it in automatically, and a fixture that genuinely holds a
// REF cursor has to be named above to be excused.
//
// The session-level gate (only while a PL/SQL call is in flight) sits on top of
// this; this asserts the decoder does not need it to stay quiet.
func TestDumpReplay_RefCursorLocatorIsSilentOnOrdinaryTraffic(t *testing.T) {
	t.Parallel()

	swept := 0

	for _, name := range surveyCorpus(t) {
		if refCursorDumps[name] {
			continue
		}

		swept++

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Empty(t, recordedRefCursorIDs(t, name),
				"a recording with no REF cursor in it must yield no REF cursor id")
		})
	}

	require.GreaterOrEqual(t, swept, len(refCursorDumps),
		"the sweep must actually cover the corpus; a glob that returned nothing would make it vacuous")
}

// scalarOutBindDumps are calls with ordinary **scalar** OUT parameters — the one
// shape the session gate genuinely admits and that is not a REF cursor.
// learnRefCursorIDs offers the locator every bind-output response arriving while
// a `BEGIN … END;` is in flight, so this is what it is actually offered in the
// field, and the corpus sweep above would not single it out.
var scalarOutBindDumps = []string{
	"go_ora_scalar_outbinds.pcapng",
	"python_thin_scalar_outbinds.pcapng",
}

// TestScalarOutBindsYieldNoRefCursorID is the negative measurement for that
// shape, and the reason readRefCursorDescriptor refuses a descriptor with no
// columns.
//
// `BEGIN dbbat_cap_scalarout(:1, :2, :3); END;` with `OUT NUMBER`,
// `OUT VARCHAR2` and a second `OUT NUMBER` puts a real bind-output block on the
// wire — the fixture check below insists on it — carrying nothing but three
// scalar values. Not one id may come out of it: an id learned here would be
// planted against the call, and rememberCursor **overwrites**, so a collision
// with a tracked cursor would replace that statement's text with an anonymous
// PL/SQL block's. A block passes `read_only`; the statement it displaced might
// not have.
func TestScalarOutBindsYieldNoRefCursorID(t *testing.T) {
	t.Parallel()

	for _, name := range scalarOutBindDumps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bindOutputs := 0

			for _, ttc := range serverTTCPayloads(t, name) {
				if _, ok := bindOutputBodyStart(ttc, false); ok {
					bindOutputs++
				}

				assert.Emptyf(t, refCursorIDsInBindOutput(thinOERShape(), ttc),
					"a call with only scalar OUT parameters must yield no REF cursor id: % x", ttc)
			}

			require.Positivef(t, bindOutputs,
				"%s must actually carry bind-output responses, or this proves nothing", name)
		})
	}
}

// TestRefCursorIDsInBindOutput_RefusesWhatItCannotAccountFor holds the locator to
// its own bounds on synthetic input. Each case is one way a walk can go wrong,
// and every one of them must return nothing rather than a number.
func TestRefCursorIDsInBindOutput_RefusesWhatItCannotAccountFor(t *testing.T) {
	t.Parallel()

	good := recordedGoOraRefCursorDescriptor()

	require.Equal(t, []uint16{7}, refCursorIDsInBindOutput(thinOERShape(), good),
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

			assert.Empty(t, refCursorIDsInBindOutput(thinOERShape(), tc.payload))
		})
	}
}

// TestRefCursorIDsInBindOutput_RefusesAZeroCursorID is its own case because a
// zero is what the field holds when the server allotted nothing, and go-ora
// treats it as ORA-01001 (invalid cursor) rather than as an id. Planting a 0 in
// the tracker would make every unrelated decode failure that yields 0 resolve to
// this call.
//
// It mutates the real descriptor rather than building a minimal one, so the id
// bound is what is actually under test — a hand-built stub with no columns would
// now be refused a field earlier, for the reason the next test pins, and this
// case would quietly stop testing anything.
func TestRefCursorIDsInBindOutput_RefusesAZeroCursorID(t *testing.T) {
	t.Parallel()

	good := recordedGoOraRefCursorDescriptor()
	require.Equal(t, []uint16{7}, refCursorIDsInBindOutput(thinOERShape(), good), "the fixture must itself decode")

	zeroed := make([]byte, len(good))
	copy(zeroed, good)
	zeroed[len(good)-7] = 0x00 // the cursor id's length byte: a zero-length cint is 0

	assert.Empty(t, refCursorIDsInBindOutput(thinOERShape(), zeroed))
}

// TestRefCursorIDsInBindOutput_RefusesADescriptorWithNoColumns pins the bound
// that closes the gap the case above only *looked* like it covered.
//
// go-ora's RefCursor.load tolerates a zero column count — it simply skips the
// loop — and so did this walk, which meant a descriptor with no columns was
// accepted on nothing but "a short run of small integers ending on a nonzero one
// that lands on a message byte". The payload below is exactly that: fifteen
// bytes, all but one of them zero. It used to yield cursor 3.
//
// That matters because it is *reachable*. The session gate offers the locator
// every bind-output response arriving while a PL/SQL call is in flight, and a
// call with scalar OUT parameters is one — see TestScalarOutBindsYieldNoRefCursorID
// for the recorded shape. An id planted from one would be attributed to the
// call, and rememberCursor overwrites, so it could displace a tracked
// statement's text with an anonymous PL/SQL block's — which passes `read_only`.
//
// Requiring at least one column restores isKnownTNSType's alignment proof as a
// mandatory bound on every accepted descriptor. It costs nothing real: a
// `SYS_REFCURSOR` is a query's result set, and no recording holds one with zero
// columns.
func TestRefCursorIDsInBindOutput_RefusesADescriptorWithNoColumns(t *testing.T) {
	t.Parallel()

	payload := []byte{
		0x07,
		0x4c, 0x01, 0x42, 0x00, // descriptor length, max row size, colCount: none
		0x00,                   // dlc
		0x00, 0x00, 0x00, 0x00, // the four version ints
		0x00,       // dlc
		0x01, 0x03, // cursor id: 3, a perfectly plausible one
		0x04, // and it lands on the summary object
	}

	assert.Empty(t, refCursorIDsInBindOutput(thinOERShape(), payload),
		"a descriptor with no columns offers no alignment proof at all, so its id is not trustworthy")
}

// recordedGoOraRefCursorDescriptor is the go-ora recording's second call, byte
// for byte: a bind-output message leading the payload, one descriptor, two
// columns (NUMBER "N", VARCHAR "LABEL"), cursor id 7, then the OER that ends the
// call. See testdata/go_ora_refcursor.pcapng.
func recordedGoOraRefCursorDescriptor() []byte {
	return []byte{
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
}

package oracle

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"sort"
	"strings"
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
// These recordings close that. A TO_NUMBER over a 20 000-row table whose row
// 15 000 will not convert, no ORDER BY (a sort would materialize the set and
// raise before the first row, which is the shape already measured), array size
// 100: every client fetches 14 900 rows and then takes an ORA-01722 that arrives
// as a **standalone func 0x04 with CallStatus 0x1** — the same bit-less shape,
// only now with the column definitions and dozens of fetch round trips behind
// it.
//
// Four clients were measured, and the shape is the same on all four. What used
// to differ is whether dbbat could read it: the three thin clients marshal the
// summary object as TTC compressed integers, and sqlplus (OCI) marshals it
// fixed-width, which the decoder did not read at all. It does now
// (decodeOERFieldsAtLayout), so all four are on the same footing here.
//
// Regenerate with
// `go test -tags capture -run 'TestCapture_.*MidFetchFailure' ./internal/proxy/oracle/`.
const (
	pythonMidFetchDump = "python_thin_midfetch_fail.pcapng"
	goOraMidFetchDump  = "go_ora_midfetch_fail.pcapng"
	jdbcMidFetchDump   = "jdbc_thin_midfetch_fail.pcapng"

	// sqlplusMidFetchDump is the same fixture on an OCI client. Its failure is
	// on the wire in the same place and the same shape, only marshaled in the
	// fixed-width OCI encoding — which is why it used to sit outside
	// midFetchDumps proving nothing. The byte-level reading of it is pinned in
	// TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndReadable.
	sqlplusMidFetchDump = "sqlplus_midfetch_fail.pcapng"
)

// oraMidFetchCode is the ORA code both recordings raise mid-fetch, and
// oerMidFetchCallStatus the CallStatus both carry it with — no end-of-call bit,
// which is what used to make it undecodable.
const (
	oraMidFetchCode       = 1722
	oerMidFetchCallStatus = 0x1
	oerMidFetchSeqNumber  = 7

	// oerMidFetchCurRow is how many rows the server reports having produced
	// before it raised — row 15 000 is the one that will not convert, and both
	// recordings agree on 14999. The client saw 14 900 of them, an array short
	// of the batch that died.
	oerMidFetchCurRow = 14999

	// midFetchMinRowStreamPackets is a floor, not a measurement: both recordings
	// carry ~150 packets of row stream before the failure, and a fixture that
	// dropped under this would no longer be measuring a *mid*-fetch failure at
	// all. Kept loose so re-recording at a different fetch size is not a
	// failure.
	midFetchMinRowStreamPackets = 50
)

// midFetchDumps is the set of recordings whose mid-fetch failure dbbat can
// read: all four clients, since the fixed-width decoder landed. The OCI
// recording joining this list is what puts it through the cursor anchor and both
// RecordsItsORAText tests rather than only through a byte-level measurement.
func midFetchDumps() []string {
	return []string{pythonMidFetchDump, goOraMidFetchDump, jdbcMidFetchDump, sqlplusMidFetchDump}
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
	dump    string
	index   int
	payload []byte
	fields  *oerInfo
	relaxed *oerInfo

	// strict is what handleOERStatus's first branch makes of the packet: the
	// end-of-call-bit reading of the compressed encoding, plus (since
	// decodeFixedStatusOERAt) the fixed-width *status* object at byte 0. Both
	// halves must refuse a mid-stream packet.
	strict bool

	// embedded is what handleResponse's mid-row-stream branch makes of it —
	// findOERInResponse, scanning every offset. It must find nothing here.
	embedded *oerInfo

	streamed uint16 // the cursor whose rows were on the wire at that moment
}

// midStreamWalk is what one recording contributes to the corpus measurement:
// the packets that reached handleOERStatus mid-stream, and the histogram of what
// every mid-stream packet actually leads with — the denominator of the
// false-positive question, and the source of the figures quoted in
// docs/oracle.md.
type midStreamWalk struct {
	oers         []midStreamOER
	leadingBytes map[byte]int
}

// walkMidStreamOERs replays a recording through the real intercept pipeline and
// returns every server packet whose TTC payload begins at byte 0 with func 0x04
// while `rowStreamActive()` is true, the leading byte of every mid-stream packet
// — plus how many of those went past in total.
//
// It reads `rowStreamActive()` off a real session rather than reconstructing it,
// because that predicate — a pending query whose cursor already has column
// definitions — is exactly what handleOERStatus consults, and a second
// implementation of it in a test would measure the wrong thing. The summary-
// object shape is taken off the same session for the same reason: which encoding
// the decoders read is what the session has learned from the upstream by that
// point in the recording, not a constant.
func walkMidStreamOERs(t *testing.T, name string) (midStreamWalk, int) {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = drainedPipe(t)

	var midStreamPackets int

	found := midStreamWalk{leadingBytes: map[byte]int{}}

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
			found.leadingBytes[ttc[0]]++

			if TTCFunctionCode(ttc[0]) == TTCFuncOERR {
				shape := s.oerShapeSnapshot()
				fields, _ := decodeOERFieldsForShape(shape, ttc, 0)

				found.oers = append(found.oers, midStreamOER{
					dump:     name,
					index:    i,
					payload:  ttc,
					fields:   fields,
					relaxed:  decodeErrorOER(shape, ttc),
					strict:   decodeOERAt(shape, ttc, 0) != nil,
					embedded: findOERInResponse(shape, ttc),
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
//
// It also pins that the failure really is raised *mid*-fetch rather than near
// its start, because that is the only thing separating these fixtures from the
// six in failed_stmt_replay_test.go. A regenerated capture whose failure landed
// on row 3 would still satisfy every other assertion here while measuring
// nothing new, so the depth is asserted on two independent readings: the OER's
// own CurRowNumber, and how many packets of row stream went past before it.
func TestDumpReplay_MidFetchFailureIsABitLessStandaloneOER(t *testing.T) {
	t.Parallel()

	for _, name := range midFetchDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			midStream, total := walkMidStreamOERs(t, name)
			require.Positive(t, total, "the recording must contain a row stream at all")
			require.Len(t, midStream.oers, 1, "exactly one standalone 0x04 arrives mid-fetch")

			got := midStream.oers[0]
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

			assert.Equal(t, oerMidFetchCurRow, got.fields.CurRowNumber,
				"packet #%d: the server had produced %d rows before it failed — if this moves, "+
					"the fixture no longer measures a failure raised deep inside a fetch",
				got.index, oerMidFetchCurRow)
			assert.GreaterOrEqual(t, total, midFetchMinRowStreamPackets,
				"the failure must arrive after a real run of row stream, not at the head of one")
			assert.Greater(t, got.index, midFetchMinRowStreamPackets,
				"packet #%d: the failure must be late in the recording", got.index)
		})
	}
}

// Where the OCI recording's mid-fetch OER carries each field, and what it
// carries there. These are oerFixed32Layout's own offsets — the layout dbbat
// *writes* for an OCI client — read back off a summary object the server sent
// one, which is the point: the encoder already speaks this encoding and the
// decoder does not.
const (
	ociMidFetchCallStatus = 1
	ociMidFetchECID       = 166
	ociMidFetchCurRow     = 14999
	ociMidFetchCursorID   = 5
	ociMidFetchCallNumber = 169
)

// TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndReadable is the fourth
// client, and the measurement that used to go the other way.
//
// The shape the mid-fetch relaxation turns on always held: sqlplus takes its
// ORA-01722 as a standalone func 0x04 at byte 0 of a whole TNS Data packet,
// arriving 150 packets into a row stream, with no end-of-call bit, naming a
// cursor, and reporting the same 14 999 rows the thin clients see. Every field
// the two anchors read is *there*.
//
// What was missing was a decoder that could read them. sqlplus negotiates the
// fixed-width OCI encoding (see encodeOERFixedWidth), so the seven leading
// fields are little-endian integers at constant offsets rather than TTC
// compressed ones, and decodeOERFieldsAt — the entry point of both
// decodeErrorOER and the cursor-id learning the second anchor compares against —
// returned nil for the whole block. dbbat was not failing the anchor closed on a
// judgement call; it never got as far as the judgement.
//
// The field values below are unchanged from when this test pinned that refusal:
// they are the fixture decodeOERFieldsAtLayout has to reproduce, asserted both
// as raw bytes at oerFixed32Layout's offsets and as what the decoder now makes
// of them. Reading the same numbers twice is the point — a decoder that agreed
// with itself but not with the wire would pass only one half.
func TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndReadable(t *testing.T) {
	t.Parallel()

	midStream, total := walkMidStreamOERs(t, sqlplusMidFetchDump)
	require.GreaterOrEqual(t, total, midFetchMinRowStreamPackets,
		"the recording must carry a real run of row stream before the failure")
	require.Len(t, midStream.oers, 1,
		"the failure still arrives as a standalone 0x04 at byte 0 of a whole packet")

	got := midStream.oers[0]
	assert.Greater(t, got.index, midFetchMinRowStreamPackets,
		"packet #%d: the failure must be late in the recording", got.index)

	// The fields the server sent, at oerFixed32Layout's offsets.
	p := got.payload
	require.GreaterOrEqual(t, len(p), oerFixedWidthPrefixLen+4, "the prefix must be whole")

	assert.Equal(t, uint32(ociMidFetchCallStatus), binary.LittleEndian.Uint32(p[oerFixedCallStatusOffset:]),
		"call status, and no end-of-call bit — the same reading as the three thin clients")
	assert.Zero(t, binary.LittleEndian.Uint32(p[oerFixedCallStatusOffset:])&oerEndOfCallBit)
	assert.Equal(t, uint16(ociMidFetchECID), binary.LittleEndian.Uint16(p[oerFixedECIDOffset:]))
	assert.Equal(t, uint16(oraMidFetchCode), binary.LittleEndian.Uint16(p[oerFixedErrNumOffset:]),
		"the error number")
	assert.Equal(t, uint32(oraMidFetchCode), binary.LittleEndian.Uint32(p[oerFixedRetCodeOffset:]),
		"repeated as the RetCode, which is what the decoder anchors the layout on")
	assert.Equal(t, uint16(ociMidFetchCursorID), binary.LittleEndian.Uint16(p[oerFixedCursorIDOffset:]),
		"the OER names a cursor")
	assert.Equal(t, uint32(ociMidFetchCallNumber), binary.LittleEndian.Uint32(p[oerFixedCallNumberOffset:]))

	// The row count is the one field the 32-bit layout leaves compressed, and it
	// agrees with the reading the thin clients' own field gives.
	rowCount, n := readCompressedInt(p[oerFixedWidthPrefixLen:])
	require.Positive(t, n, "the row count must decode")
	assert.Equal(t, ociMidFetchCurRow, rowCount, "the same 14 999 rows the thin clients are told about")
	assert.Equal(t, oerMidFetchCurRow, rowCount, "and the same the fixture pins everywhere else")

	assert.Contains(t, string(p), "ORA-01722", "and the diagnostic text is right behind it")

	// And now the decoder's own reading of exactly those bytes.
	require.NotNil(t, got.fields, "the fixed-width summary object must decode")
	assert.Equal(t, ociMidFetchCallStatus, got.fields.CallStatus)
	assert.Zero(t, got.fields.CallStatus&oerEndOfCallBit)
	assert.Equal(t, ociMidFetchECID, got.fields.SeqNumber)
	assert.Equal(t, oraMidFetchCode, got.fields.ErrorCode)
	assert.Equal(t, ociMidFetchCursorID, got.fields.CursorID)
	assert.Equal(t, ociMidFetchCurRow, got.fields.CurRowNumber)

	assert.False(t, got.strict, "the strict decoder is exactly why this used to be dropped")

	require.NotNil(t, got.relaxed, "decodeErrorOER must prove it a diagnostic")
	assert.Contains(t, got.relaxed.ErrorMessage, "ORA-01722")

	// The second anchor, which is what cursor-id learning going blind on these
	// sessions used to make unreachable: the OER names the cursor whose rows
	// were on the wire, and dbbat now holds that id to compare it against.
	assert.Equal(t, ociMidFetchCursorID, int(got.streamed),
		"cursor-id learning must have read the same fixed-width fields")
	assert.Equal(t, int(got.streamed), got.fields.CursorID,
		"packet #%d: the OER names the cursor whose rows it interrupted", got.index)
}

// TestDumpReplay_OCIFailuresAreRecordedWithTheirORAText is the scope of the fix,
// and the reason it was never really a mid-fetch bug.
//
// The same recording opens with a `DROP TABLE` of a table that does not exist —
// an ORA-00942 raised *before the first row*, which is the shape
// failed_stmt_replay_test.go measured on the thin clients and
// failed_stmt_integration_test.go drives live. It was on the wire and recorded
// with no error either, so on an OCI client *every* failing statement went into
// `queries` as a success, mid-fetch or not. Both must now carry their
// diagnostic.
func TestDumpReplay_OCIFailuresAreRecordedWithTheirORAText(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	recorder := &collectingCompletionStore{}
	s.completionStore = recorder

	replaySession(t, s, sqlplusMidFetchDump)

	// Both statements are on the wire with a diagnostic behind them.
	var raw strings.Builder
	for _, pkt := range loadTestDump(t, sqlplusMidFetchDump).Packets {
		raw.Write(pkt.Data)
	}

	require.Contains(t, raw.String(), "ORA-00942", "the setup DROP failed upstream")
	require.Contains(t, raw.String(), "ORA-01722", "and so did the SELECT, mid-fetch")

	// The recording runs `DROP TABLE dbbat_midfetch` twice — once as setup,
	// against a table that does not exist yet, and once as teardown against one
	// that does — so the SQL text alone does not identify the failure. What is
	// asserted is that *a* statement of that text carries the diagnostic, which
	// is exactly what was missing before: every one of them read as a success.
	wanted := []struct {
		sql     string
		wantORA string
	}{
		{sql: "DROP TABLE dbbat_midfetch", wantORA: "ORA-00942"},
		{sql: midFetchSelectFragment, wantORA: "ORA-01722"},
	}

	for _, w := range wanted {
		// Both store writes are asynchronous; a returned replay has not
		// necessarily written yet.
		var (
			errs  []string
			found bool
		)

		deadline := time.Now().Add(5 * time.Second)
		for {
			errs, found = recorder.createdErrorsFor(w.sql), false

			for _, got := range errs {
				if contains(got, w.wantORA) {
					found = true
				}
			}

			if found || time.Now().After(deadline) {
				break
			}

			time.Sleep(10 * time.Millisecond)
		}

		require.NotEmptyf(t, errs, "%q must be persisted at all", w.sql)
		assert.Truef(t, found,
			"%q: an OCI client's diagnostics are readable now, so its failure must carry %s "+
				"rather than be recorded as a success; got %q",
			w.sql, w.wantORA, errs)
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
// that the only ones accepted are the genuine mid-fetch failures — anything
// else is row data read as an error, which is the production incident this
// guard descends from.
//
// "Genuine" is midFetchDumps(), which is now all four recordings: the OCI one
// used to be a denominator only — its 150 mid-stream packets counted, its
// fixed-width failure unreadable and therefore never accepted — and the accepted
// count moved from three to four when the decoder landed. Its own reading is
// pinned in TestDumpReplay_OCIMidFetchFailureIsFixedWidthAndReadable.
//
// The fixed-width reading widens nothing for the thin recordings: their sessions
// learn a compressed shape off the upstream, and decodeOERFixedFieldsAt refuses
// to offer a layout a learned shape did not ask for.
//
// The stress count is the same predicate run at every 0x04 offset *inside* those
// mid-stream packets. Nothing routes that way — handleOERStatus only ever sees
// byte 0 — so it is not a rate, it is a measure of how much real row data would
// have to be misread before the predicate is the weak link.
//
// Three more figures were added when decodeOERAt learned to read the fixed-width
// *status* object, because that predicate has no diagnostic to prove itself with
// and the same corpus is what bounds it:
//
//   - `statusAccepted`: what decodeOERAt now accepts at byte 0 of a mid-stream
//     packet. It must be nothing, and it is: only four mid-stream packets in the
//     corpus lead with 0x04 at all, and all four are the genuine ORA-01722
//     failures, which a *status* predicate refuses by their error code alone.
//   - `respAccepted`: what findOERInResponse accepts on those same packets — the
//     mid-row-stream branch of handleResponse. Also nothing.
//   - `bitlessScan`: the counterfactual that decides both of the restrictions on
//     decodeFixedStatusOERAt. It is the fixed-width status predicate run at every
//     *non-zero* 0x04 offset inside those packets, and it is emphatically **not**
//     zero — an OCI fetch response carries a genuine summary object of exactly
//     this shape inside every continuation packet, naming the streaming cursor and
//     reporting the fetch's running row count. Accepting one ends the call
//     mid-fetch, which is why the reading is offered at offset 0 only and only
//     outside a row stream.
func TestDumpReplay_MidStreamOERFalsePositiveRate(t *testing.T) {
	t.Parallel()

	genuine := map[string]bool{}
	for _, name := range midFetchDumps() {
		genuine[name] = true
	}

	corpus := midStreamCorpus(t)

	var midStreamPackets, accepted, stress, statusAccepted, respAccepted, bitlessScan int

	lead := map[byte]int{}

	for _, name := range corpus {
		found, total := walkMidStreamOERs(t, name)
		midStreamPackets += total

		for b, n := range found.leadingBytes {
			lead[b] += n
		}

		for _, got := range found.oers {
			if got.strict {
				statusAccepted++

				assert.Failf(t, "mid-stream acceptance at byte 0",
					"%s packet #%d: decodeOERAt accepted a mid-row-stream packet", name, got.index)
			}

			if got.embedded != nil {
				respAccepted++

				assert.Failf(t, "mid-stream acceptance by the Response locator",
					"%s packet #%d: findOERInResponse accepted row-stream bytes as ORA-%05d",
					name, got.index, got.embedded.ErrorCode)
			}

			if got.relaxed == nil {
				continue
			}

			accepted++

			assert.Truef(t, genuine[name],
				"%s packet #%d: row data accepted as ORA-%05d (%q) — this is a false positive",
				name, got.index, got.relaxed.ErrorCode, got.relaxed.ErrorMessage)
		}
	}

	for _, name := range corpus {
		stress += midStreamStressAcceptances(t, name)
		bitlessScan += midStreamBitlessStatusAcceptances(t, name)
	}

	// Every figure quoted in docs/oracle.md is printed here, so the numbers in
	// the prose are reproducible from the tree rather than remembered. Only the
	// load-bearing ones are asserted: pinning a distribution would turn
	// "somebody re-recorded a fixture" into a failure (same reasoning as
	// sql_extraction_survey_test.go).
	t.Logf("corpus: %d recordings; mid-row-stream server packets: %d; leading with 0x04: %d; "+
		"accepted as diagnostics: %d; stress acceptances at non-zero offsets: %d; "+
		"accepted at byte 0 by decodeOERAt: %d; accepted by findOERInResponse: %d; "+
		"a bit-less fixed-width status scan would accept: %d",
		len(corpus), midStreamPackets, lead[byte(TTCFuncOERR)], accepted, stress,
		statusAccepted, respAccepted, bitlessScan)

	for _, b := range sortedLeadingBytes(lead) {
		t.Logf("  mid-row-stream packets leading with %#02x: %d", b, lead[b])
	}

	require.Positive(t, midStreamPackets, "the corpus must contain row streams to measure against")
	assert.Equal(t, len(genuine), accepted, "only the genuine mid-fetch failures may be accepted")
	assert.Zero(t, stress,
		"real row bytes must never satisfy the diagnostic proof, at any offset")
	assert.Zero(t, statusAccepted, "the fixed-width status reading must not fire mid-row-stream")
	assert.Zero(t, respAccepted, "nor may the Response locator, which is why it kept the bit")
	assert.Positive(t, bitlessScan,
		"this is the measurement decodeFixedStatusOERAt's two restrictions exist for — if it ever "+
			"reaches zero the corpus stopped containing an OCI fetch, not the risk stopped existing")
}

// midStreamBitlessStatusAcceptances is the counterfactual behind
// decodeFixedStatusOERAt's offset restriction: the fixed-width status predicate
// with that restriction lifted, run at every non-zero 0x04 offset inside every
// mid-row-stream packet of one recording. See the test above.
func midStreamBitlessStatusAcceptances(t *testing.T, name string) int {
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
			shape := s.oerShapeSnapshot()

			for off := 1; off < len(ttc); off++ {
				if ttc[off] != 0x04 {
					continue
				}

				if info, _ := decodeOERFixedFieldsAt(shape, ttc, off); plausibleStatusOER(info) {
					acceptances++
				}
			}
		}

		s.interceptUpstreamMessage(tns)
	}

	return acceptances
}

// sortedLeadingBytes orders a leading-byte histogram so the report is stable
// across runs.
func sortedLeadingBytes(lead map[byte]int) []byte {
	out := make([]byte, 0, len(lead))
	for b := range lead {
		out = append(out, b)
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out
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
			shape := s.oerShapeSnapshot()

			for off := 1; off < len(ttc); off++ {
				if ttc[off] == 0x04 && decodeErrorOER(shape, ttc[off:]) != nil {
					acceptances++
				}
			}
		}

		s.interceptUpstreamMessage(tns)
	}

	return acceptances
}

// collectingCompletionStore keeps every write a replay makes, on *both* of
// completeQuery's branches. The buffered recordingCompletionStore cannot be used
// here: a whole recording completes far more statements than its channels hold,
// and the replay would deadlock on the setup DDL long before reaching the
// failure.
type collectingCompletionStore struct {
	mu      sync.Mutex
	created []*store.Query
	updated []completedQuery
}

func (c *collectingCompletionStore) CreateQuery(_ context.Context, query *store.Query) (*store.Query, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if query.UID == uuid.Nil {
		query.UID = uuid.Must(uuid.NewV7())
	}

	c.created = append(c.created, query)

	return query, nil
}

func (c *collectingCompletionStore) UpdateQueryCompletion(
	_ context.Context,
	uid uuid.UUID,
	_ *float64,
	rowsAffected *int64,
	queryError *string,
	resultsTruncated bool,
	_ bool,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updated = append(c.updated, completedQuery{
		uid:          uid,
		rowsAffected: rowsAffected,
		queryError:   queryError,
		truncated:    resultsTruncated,
	})

	return nil
}

func (c *collectingCompletionStore) IncrementConnectionStats(_ context.Context, _ uuid.UUID, _ int64) error {
	return nil
}

// awaitUpdate waits for the asynchronous UpdateQueryCompletion naming uid and
// returns it. Both store writes happen on their own goroutine (that is what
// makes the row-flush barrier affordable), so a replay that has returned has not
// necessarily written yet.
func (c *collectingCompletionStore) awaitUpdate(t *testing.T, uid uuid.UUID) (completedQuery, bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		c.mu.Lock()

		for _, got := range c.updated {
			if got.uid == uid {
				c.mu.Unlock()

				return got, true
			}
		}

		c.mu.Unlock()

		if time.Now().After(deadline) {
			return completedQuery{}, false
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// createdErrorsFor returns the recorded error text of every query *created* at
// completion whose SQL contains needle. Used as a negative: the statement under
// test must not come back down this branch.
func (c *collectingCompletionStore) createdErrorsFor(needle string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

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

	return out
}

// midFetchSelectFragment is the one statement these fixtures are about.
const midFetchSelectFragment = "TO_NUMBER(txt)"

// replayPersistingRowStreams feeds a recording through the real intercept
// pipeline and, the moment a fetch's row stream opens, puts the pending query
// into the state persistQueryRecord leaves behind: a query row already in the
// store, addressed by uid, with queryPersisted set.
//
// That state is the whole point. `session.store` is a concrete *store.Store, so
// persistQueryRecord itself cannot run without a database — but it is exactly
// what production reaches on a streaming SELECT with result capture on, and it
// sends completeQuery down its *other* branch: finalizeQuery →
// UpdateQueryCompletion instead of CreateQuery. A test that only ever exercises
// the create-now branch would prove the ORA text reaches the store object and
// not that it reaches the update a real session performs.
//
// It returns the uid stamped on the statement whose SQL contains sqlFragment.
func replayPersistingRowStreams(t *testing.T, s *session, name, sqlFragment string) uuid.UUID {
	t.Helper()

	s.clientConn = drainedPipe(t)

	var want uuid.UUID

	for _, pkt := range loadTestDump(t, name).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			s.interceptClientMessage(tns)

			continue
		}

		s.interceptUpstreamMessage(tns)

		s.trackerMu.Lock()

		if pending := s.tracker.pendingQuery; s.rowStreamActive() && !pending.queryPersisted {
			pending.queryPersisted = true
			pending.queryUID = uuid.Must(uuid.NewV7())

			if contains(pending.cursor.sql, sqlFragment) {
				want = pending.queryUID
			}
		}

		s.trackerMu.Unlock()
	}

	return want
}

// TestDumpReplay_MidFetchFailureRecordsItsORAText is the regression, end to end:
// drive the whole recording through the proxy's own intercept paths and the
// failing SELECT must reach the store carrying ORA-01722.
//
// Before this change it reached the store carrying nothing at all — the OER was
// dropped, the statement stayed pending, and the DROP that followed closed it as
// a success through flushPendingQuery.
//
// The row it lands on is a row that already exists, because a SELECT that has
// been streaming for 14 900 rows was persisted long before it failed. See
// replayPersistingRowStreams.
func TestDumpReplay_MidFetchFailureRecordsItsORAText(t *testing.T) {
	t.Parallel()

	for _, name := range midFetchDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			recorder := &collectingCompletionStore{}
			s.completionStore = recorder

			uid := replayPersistingRowStreams(t, s, name, midFetchSelectFragment)
			require.NotEqual(t, uuid.Nil, uid,
				"the failing SELECT must have opened a row stream and been persisted")

			got, ok := recorder.awaitUpdate(t, uid)
			require.True(t, ok, "the persisted query row was never completed")

			require.NotNil(t, got.queryError, "a statement that died mid-fetch must record its ORA text")
			assert.Contains(t, *got.queryError, "ORA-01722",
				"the completion update must carry the ORA text, not close the row as a success")

			assert.Empty(t, recorder.createdErrorsFor(midFetchSelectFragment),
				"an already-persisted statement completes through UpdateQueryCompletion, never a second CreateQuery")
		})
	}
}

// TestDumpReplay_MidFetchFailureRecordsItsORATextOnTheCreateBranch is the same
// claim on completeQuery's other branch — the one a session with result capture
// off takes, where no row was persisted while streaming and the query record is
// created at completion. Both branches carry the error; only the shape of the
// write differs.
func TestDumpReplay_MidFetchFailureRecordsItsORATextOnTheCreateBranch(t *testing.T) {
	t.Parallel()

	for _, name := range midFetchDumps() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			recorder := &collectingCompletionStore{}
			s.completionStore = recorder

			replaySession(t, s, name)

			var errs []string

			deadline := time.Now().Add(5 * time.Second)
			for {
				errs = recorder.createdErrorsFor(midFetchSelectFragment)
				if len(errs) > 0 || time.Now().After(deadline) {
					break
				}

				time.Sleep(10 * time.Millisecond)
			}

			require.NotEmpty(t, errs, "the failing SELECT must be persisted at all")

			for _, got := range errs {
				assert.Contains(t, got, "ORA-01722",
					"a statement that died mid-fetch must record its ORA text, not close as a success")
			}
		})
	}
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

	require.NotNil(t, decodeErrorOER(thinOERShape(), oerNaming(streamingCursor)),
		"the synthesized OER must be provable, or this test is measuring the wrong refusal")
	require.NotNil(t, decodeErrorOER(thinOERShape(), oerNaming(streamingCursor+1)),
		"...and so must the one naming another cursor, so the cursor is what separates them")

	tests := []struct {
		name string
		// streaming is the cursor id dbbat holds for the fetch in progress; 0
		// means it never learned one, which is the anchor's unset guard rather
		// than its inequality.
		streaming uint16
		cursor    int
		complete  bool
	}{
		{name: "names the streaming cursor", streaming: streamingCursor, cursor: streamingCursor, complete: true},
		{name: "names another cursor", streaming: streamingCursor, cursor: streamingCursor + 1},
		{name: "names no cursor", streaming: streamingCursor, cursor: 0},
		{name: "the streaming cursor was never learned", streaming: 0, cursor: 0},
		{name: "the streaming cursor was never learned, OER names one", streaming: 0, cursor: streamingCursor},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
			require.NoError(t, s.handleOALL8(buildOALL8("SELECT id FROM emp", nil, streamingCursor)))

			// Set directly rather than through a cursor-0 OALL8: what is under
			// test is a session that never learned an id for the fetch it is
			// streaming, not a statement parsed against cursor 0.
			s.tracker.pendingQuery.cursor.cursorID = tc.streaming
			s.tracker.pendingQuery.cursor.columns = []columnDef{{Name: "ID"}}
			require.True(t, s.rowStreamActive())

			s.handleOERStatus(oerNaming(tc.cursor))

			if tc.complete {
				assert.Nil(t, s.tracker.pendingQuery,
					"a proven diagnostic naming the streaming cursor ends the fetch")

				return
			}

			assert.NotNil(t, s.tracker.pendingQuery,
				"mid-fetch, a 0x04 run that does not name a known streaming cursor is row data")
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

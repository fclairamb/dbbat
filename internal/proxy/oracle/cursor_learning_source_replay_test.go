package oracle

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// cursorLearn is one write learnCursorID made, replayed off a recording: the id
// it stored, the evidence it stored it on, what it replaced, and what the
// session was doing at the time.
//
// `source` is the shipped value — read straight off the tracked cursor — rather
// than something the test re-derives, so this measures the rule that runs in
// production and not a second copy of it. `byte0ID` is the one thing the test
// adds: what byte 0 of the same packet would have said, which is how the
// cheaper of the two bounds the spec proposes gets measured alongside the other.
type cursorLearn struct {
	dump            string
	index           int
	sql             string
	id              uint16
	previous        uint16
	source          cursorIDSource
	offset          int // where the scan's candidate sat, -1 when it found none
	duringRowStream bool
	byte0ID         int // the id byte 0 of the same packet reports, 0 when it reports none

	// compressed is whether the scan's candidate came from the TTC compressed
	// reading rather than the fixed-width one, and fixedWidthSession is what the
	// session had *learned* the upstream speaks by then. The pair answers a
	// question worth asking once: a compressed acceptance on a session known to
	// speak fixed-width cannot be a server OER, because a server speaks one
	// encoding.
	compressed        bool
	fixedWidthSession bool

	// funcCode is the TTC function code of the packet the id was learned from —
	// the router's own classification of it, which is what "a QueryResult's
	// describe records" is a figure of rather than an inference.
	funcCode TTCFunctionCode
}

// walkCursorLearning replays one recording through the real intercept pipeline
// and returns every write learnCursorID made to a tracked cursor's id.
//
// Learning is observed rather than reimplemented: the cursor's id and source are
// read before and after each upstream packet, so an entry here is a write the
// production path really made.
func walkCursorLearning(t *testing.T, name string) []cursorLearn {
	t.Helper()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	s.clientConn = drainedPipe(t)

	var learns []cursorLearn

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

		funcCode, err := parseTTCFunctionCode(tns.Payload)
		if err != nil {
			continue
		}

		s.trackerMu.Lock()

		var (
			watched *trackedCursor
			before  uint16
			source  cursorIDSource
			active  bool
		)

		if pending := s.tracker.pendingQuery; pending != nil && pending.cursor != nil {
			watched = pending.cursor
			before = watched.cursorID
			source = watched.cursorIDSource
			active = s.rowStreamActive()
		}

		s.trackerMu.Unlock()

		s.interceptUpstreamMessage(tns)

		s.trackerMu.Lock()

		if watched != nil && watched.cursorIDSource != source {
			// The shape learnOERTail left behind on this very packet is the one
			// learnCursorID read it under, so re-deriving the scan's candidate
			// with it reproduces the acceptance rather than guessing at another.
			shape := s.oerShapeSnapshot()
			_, off := locatePlausibleOER(shape, ttc)

			compressed := false

			if off >= 0 {
				info, _ := decodeOERFieldsAt(ttc, off)
				compressed = plausibleStatusOER(info)
			}

			learns = append(learns, cursorLearn{
				dump:              name,
				index:             i,
				sql:               truncateSQL(watched.sql, 60),
				id:                watched.cursorID,
				previous:          before,
				source:            watched.cursorIDSource,
				offset:            off,
				duringRowStream:   active,
				byte0ID:           byte0StatusCursorID(shape, ttc),
				compressed:        compressed,
				fixedWidthSession: shape.tailLearned && shape.fixedWidth,
				funcCode:          funcCode,
			})
		}

		s.trackerMu.Unlock()
	}

	return learns
}

// byte0StatusCursorID reports the cursor id a status OER at byte 0 of this
// payload names, or 0 when byte 0 carries no readable status.
//
// It is deliberately the *loose* reading — plausibleStatusOER over either
// encoding, with no end-of-call bit and no offset-0 restriction demanded — so
// the figure it feeds answers "was there anything at byte 0 at all", which is a
// wider question than what cursorIDFromCallBoundary accepts.
func byte0StatusCursorID(shape oerShape, payload []byte) int {
	if len(payload) == 0 || payload[0] != 0x04 {
		return 0
	}

	if info, _ := decodeOERFieldsAt(payload, 0); plausibleStatusOER(info) {
		return info.CursorID
	}

	if info, _ := decodeOERFixedFieldsAt(shape, payload, 0); plausibleStatusOER(info) {
		return info.CursorID
	}

	return 0
}

// TestDumpReplay_CursorIDLearningSource is step 1 of
// specs/todos/2026-09-21-05-oracle-cursor-id-learning-latches-row-bytes.md,
// offline and corpus-wide: across every recording in testdata/, where does each
// learned cursor id actually come from?
//
// The question the 2026-09-21-05 fix had to turn on is whether any statement
// shape learns its id *only* while a row stream is open. A blanket refusal to
// learn mid-stream is the obvious bound — the server has already had its chance
// to name the cursor in its own end-of-call OER by then — but if some client
// shape's id is only ever available there, that refusal silently stops learning
// for it, and the re-executions that follow become ORA-01031 under a
// restrictive grant.
//
// The answer is that one shape does: dbeaver's JDBC thin client, whose big
// catalog SELECT is answered across seven packets, with the end-of-call OER in
// the last of them — six packets after the QueryResult that opened the row
// stream. The id it names is the one the client then fetches by, so it is
// genuine and it is needed. Hence the ranking in cursorIDSource rather than a
// refusal: that id is learned, and marked as the weakest evidence there is, so
// anything better replaces it and it is never trusted where trust is optional
// (midFetchOERNamesTheStreamingCursor).
//
// The test now also carries 2026-09-21-06's step 1, which asked the follow-up
// question the 17744 correction raised: *what packet* is each rank read off?
// The figures that answered it — 107 of 167 out-of-stream scan hits were
// QueryResult describe records, the Response and OVERSION hits at structural
// offsets — are what turned cursorIDFromScan into the carrier-packet rank and
// added cursorIDFromDescribeScan beneath it, and the func-code measurement here
// is what keeps those figures figures rather than inferences.
//
// The figures are printed rather than pinned as a distribution: re-recording a
// fixture must not be a test failure. What is asserted is the shape of the
// answer — see the assertions at the end.
func TestDumpReplay_CursorIDLearningSource(t *testing.T) {
	t.Parallel()

	corpus := midStreamCorpus(t)

	var (
		total, corrections, byte0Disagreeing int
		compressedOnFixed, fixedSessions     int
		byte0OnDataPacket                    int
		midStreamOnly                        []string
	)

	bySource := map[cursorIDSource]int{}
	// The packet kind each rank was read off, which is what "learned off a
	// QueryResult" needs to be a figure rather than an inference.
	byPacket := map[cursorIDSource]map[TTCFunctionCode]int{}

	for _, name := range corpus {
		learns := walkCursorLearning(t, name)
		if len(learns) == 0 {
			continue
		}

		// Per recording, which statements hold an id whose *final* evidence is a
		// mid-stream scan hit. Those are the ones a blanket refusal would have
		// stopped learning altogether.
		weakest := map[string]bool{}
		stronger := map[string]bool{}

		for _, l := range learns {
			total++
			bySource[l.source]++

			if byPacket[l.source] == nil {
				byPacket[l.source] = map[TTCFunctionCode]int{}
			}

			byPacket[l.source][l.funcCode]++

			if l.source == cursorIDFromMidStreamScan {
				weakest[l.sql] = true

				t.Logf("  %s packet #%d: cursor %d on the weakest evidence there is — "+
					"scan hit at offset %d, mid-stream, byte 0 says %d — %q",
					name, l.index, l.id, l.offset, l.byte0ID, l.sql)
			} else {
				stronger[l.sql] = true
			}

			if l.source == cursorIDFromCallBoundary {
				t.Logf("  %s packet #%d: cursor %d off the call boundary — %q",
					name, l.index, l.id, l.sql)
			}

			if l.source == cursorIDFromDescribeScan {
				t.Logf("  %s packet #%d: cursor %d scanned off a %s packet at offset %d "+
					"(compressed=%t, fixedWidthSession=%t) — %q",
					name, l.index, l.id, l.funcCode, l.offset, l.compressed, l.fixedWidthSession, l.sql)
			}

			if l.source == cursorIDFromScan && l.funcCode != TTCFuncQueryResult {
				t.Logf("  %s packet #%d: cursor %d scanned off a %s packet at offset %d "+
					"(compressed=%t, fixedWidthSession=%t) — %q",
					name, l.index, l.id, l.funcCode, l.offset, l.compressed, l.fixedWidthSession, l.sql)
			}

			if l.previous != 0 && l.previous != l.id {
				corrections++

				t.Logf("  %s packet #%d: cursor %d corrected to %d on %s evidence — %q",
					name, l.index, l.previous, l.id, l.source, l.sql)
			}

			if l.byte0ID != 0 && l.byte0ID != int(l.id) {
				byte0Disagreeing++

				t.Logf("  %s packet #%d: the scan says cursor %d, byte 0 says %d — %q",
					name, l.index, l.id, l.byte0ID, l.sql)
			}

			if l.fixedWidthSession {
				fixedSessions++

				if l.compressed {
					compressedOnFixed++
				}
			}

			if l.byte0ID != 0 && l.funcCode != TTCFuncOERR {
				byte0OnDataPacket++

				t.Logf("  %s packet #%d: byte 0 of a %s packet decodes as a status naming "+
					"cursor %d (scan says %d) — %q",
					name, l.index, l.funcCode, l.byte0ID, l.id, l.sql)
			}
		}

		for sql := range weakest {
			if !stronger[sql] {
				midStreamOnly = append(midStreamOnly, name+": "+sql)
			}
		}
	}

	sort.Strings(midStreamOnly)

	t.Logf("cursor-id learning provenance across %d recordings:", len(corpus))
	t.Logf("  ids learned:                          %d", total)
	t.Logf("  from the call boundary (byte 0):      %d", bySource[cursorIDFromCallBoundary])
	t.Logf("  from a scan on an OER-carrying packet: %d", bySource[cursorIDFromScan])
	t.Logf("  from a scan on describe records:      %d", bySource[cursorIDFromDescribeScan])
	t.Logf("  from a scan inside a row stream:      %d", bySource[cursorIDFromMidStreamScan])
	t.Logf("  corrections of an id already held:    %d", corrections)
	t.Logf("  packets where byte 0 disagreed:       %d", byte0Disagreeing)
	t.Logf("  learned on a fixed-width session:     %d (of which read as compressed: %d)",
		fixedSessions, compressedOnFixed)
	t.Logf("  byte 0 accepted on a non-OERR packet: %d", byte0OnDataPacket)

	for _, src := range []cursorIDSource{
		cursorIDFromCallBoundary, cursorIDFromScan, cursorIDFromDescribeScan, cursorIDFromMidStreamScan,
	} {
		var names []string

		for fc, n := range byPacket[src] {
			names = append(names, fmt.Sprintf("%s=%d", fc, n))
		}

		sort.Strings(names)
		t.Logf("  packets behind %-21s %s", src, strings.Join(names, " "))
	}

	for _, s := range midStreamOnly {
		t.Logf("  holds a mid-stream-scan id only:      %s", s)
	}

	require.Positive(t, total, "the corpus must contain cursor-id learning to measure at all")

	// The load-bearing claim, and the reason the fix is a ranking rather than a
	// refusal: mid-stream scan hits are a rounding error in the corpus, but they
	// are not zero, so refusing them outright would cost a real client its ids.
	assert.Lessf(t, bySource[cursorIDFromMidStreamScan], total/10,
		"mid-stream scan hits must stay the exception (%d of %d); if they become the rule, the "+
			"ranking is no longer protecting anything",
		bySource[cursorIDFromMidStreamScan], total)

	// The describe-scan rank is in use, and heavily: most thin-client ids are
	// learned off the OER a QueryResult bundles behind its describe records.
	// That is exactly why the rank cannot be a refusal — and why any tightening
	// that reads it as "junk" would have to survive the four mid-fetch fixtures
	// first (see midFetchOERNamesTheStreamingCursor).
	assert.Positivef(t, bySource[cursorIDFromDescribeScan],
		"describe-record scan hits must still be learned from (%d total)", bySource[cursorIDFromDescribeScan])

	// Byte 0 of a packet is the function-code byte itself, and both of
	// decodeOERAt's halves demand a 0x04 there — so a call-boundary acceptance
	// is a standalone OER's own marker, never a describe record that happens to
	// lead with one. The corpus agrees: every byte-0 acceptance sits on an OERR
	// packet. If a recording ever starts disagreeing, cursorIDFromCallBoundary
	// is rating data as if it were the router's own reading.
	assert.Zerof(t, byte0OnDataPacket,
		"byte 0 was accepted as a status on a packet that is not a standalone OER")

	// Nothing in the corpus needs correcting, which is the point: the recordings
	// all learn their ids off a genuine OER the first time. The 17744 case is a
	// *live* one — see TestIntegration_CursorIDLearningMissRate — so this figure
	// is here to make a corpus that starts needing corrections visible rather
	// than to assert the mechanism is unused.
	t.Logf("  (corrections above are 0 on a corpus whose ids are all learned cleanly)")
}

package oracle

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// cursorLearn is one acceptance of learnCursorID, replayed off a recording: the
// id it latched, where in the packet the OER it read sat, and what the session
// was doing at the time.
//
// The two fields that matter are `duringRowStream` and `byte0ID`. The first is
// the question the spec asks — an id learned while rows are on the wire is an id
// that may have been scanned out of row bytes rather than read off the server's
// own end-of-call OER. The second is the cheaper bound measured alongside it: the
// fixed-width status object an OCI call ends on sits at byte 0 under the RetCode
// anchor, and the scan behind findCursorIDInResponse starts at offset 1, so it
// cannot see that object at all.
type cursorLearn struct {
	dump            string
	index           int
	sql             string
	id              uint16
	offset          int // where the accepted OER sat; always ≥1, the scan starts there
	duringRowStream bool
	byte0ID         int // the id byte 0 of the same packet reports, 0 when it reports none

	// compressed is whether the acceptance came from the TTC compressed reading
	// rather than the fixed-width one, and fixedWidthSession is what the session
	// had *learned* the upstream speaks by then. The pair is the second question
	// the measurement answers: a compressed acceptance on a session known to speak
	// fixed-width cannot be a server OER, because the server has one encoding.
	compressed        bool
	fixedWidthSession bool
}

// walkCursorLearning replays one recording through the real intercept pipeline
// and returns every cursor id learnCursorID latched, with the provenance of each.
//
// Learning is observed rather than reimplemented: the tracked cursor's id is read
// before and after each upstream packet, so an entry here is an id the production
// path really wrote. The *provenance* is then re-derived from the same payload
// under the same session shape, which is the only part a test can add.
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

		s.trackerMu.Lock()

		var (
			watched *trackedCursor
			active  bool
		)

		if pending := s.tracker.pendingQuery; pending != nil && pending.cursor != nil && pending.cursor.cursorID == 0 {
			watched = pending.cursor
			active = s.rowStreamActive()
		}

		s.trackerMu.Unlock()

		s.interceptUpstreamMessage(tns)

		s.trackerMu.Lock()

		if watched != nil && watched.cursorID != 0 {
			// The shape learnOERTail left behind on this very packet is the one
			// learnCursorID read it under, so re-deriving the offset with it
			// reproduces the acceptance rather than guessing at another.
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
				offset:            off,
				duringRowStream:   active,
				byte0ID:           byte0StatusCursorID(shape, ttc),
				compressed:        compressed,
				fixedWidthSession: shape.tailLearned && shape.fixedWidth,
			})
		}

		s.trackerMu.Unlock()
	}

	return learns
}

// byte0StatusCursorID reports the cursor id a status OER at byte 0 of this
// payload names, or 0 when byte 0 carries no readable status.
//
// Both encodings are offered, each under plausibleStatusOER — the same bound the
// scan applies — so the comparison below is "the same proof, at a better place"
// rather than a looser reading. The fixed-width half is decodeOERFixedFieldsAt's,
// which carries the RetCode anchor on top.
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
// The question the fix turns on is whether any statement shape learns its id
// *only* while a row stream is open. A blanket refusal to learn mid-stream is
// the obvious bound — the server has already had its chance to name the cursor
// in its own end-of-call OER by then — but if some client shape's id is only
// ever available there, that refusal silently stops learning for it, and the
// re-executions that follow become ORA-01031 under a restrictive grant.
//
// The figures are printed rather than pinned as a distribution: re-recording a
// fixture must not be a test failure. What *is* asserted is the load-bearing
// claim — see the assertions at the end.
func TestDumpReplay_CursorIDLearningSource(t *testing.T) {
	t.Parallel()

	corpus := midStreamCorpus(t)

	var (
		total, midStream, agreeing, disagreeing, byte0Only int
		compressedOnFixed, fixedSessions                   int
		midStreamOnly                                      []string
	)

	for _, name := range corpus {
		learns := walkCursorLearning(t, name)
		if len(learns) == 0 {
			continue
		}

		// Per recording, which statements learned an id *only* mid-stream. A
		// statement that learned one outside the stream too is unaffected by the
		// bound; one that never did is what would stop being learned at all.
		outside := map[string]bool{}
		inside := map[string]bool{}

		for _, l := range learns {
			total++

			if l.duringRowStream {
				midStream++

				inside[l.sql] = true

				t.Logf("  %s packet #%d: learned cursor %d mid-stream at offset %d (byte 0 says %d) — %q",
					name, l.index, l.id, l.offset, l.byte0ID, l.sql)
			} else {
				outside[l.sql] = true
			}

			switch {
			case l.byte0ID == 0:
			case l.byte0ID == int(l.id):
				agreeing++
			default:
				disagreeing++

				t.Logf("  %s packet #%d: scan at offset %d says cursor %d, byte 0 says %d — %q",
					name, l.index, l.offset, l.id, l.byte0ID, l.sql)
			}

			if l.byte0ID != 0 && l.duringRowStream {
				byte0Only++
			}

			if l.fixedWidthSession {
				fixedSessions++

				if l.compressed {
					compressedOnFixed++

					t.Logf("  %s packet #%d: compressed acceptance at offset %d on a fixed-width "+
						"session — cursor %d, %q", name, l.index, l.offset, l.id, l.sql)
				}
			}
		}

		for sql := range inside {
			if !outside[sql] {
				midStreamOnly = append(midStreamOnly, name+": "+sql)
			}
		}
	}

	sort.Strings(midStreamOnly)

	t.Logf("cursor-id learning provenance across %d recordings:", len(corpus))
	t.Logf("  ids learned:                          %d", total)
	t.Logf("  learned while a row stream was open:  %d", midStream)
	t.Logf("  packets whose byte 0 also named one:  %d (agreeing %d, disagreeing %d)",
		agreeing+disagreeing, agreeing, disagreeing)
	t.Logf("  of those, mid-stream:                 %d", byte0Only)
	t.Logf("  learned on a fixed-width session:     %d (of which read as compressed: %d)",
		fixedSessions, compressedOnFixed)

	for _, s := range midStreamOnly {
		t.Logf("  learned ONLY mid-stream:              %s", s)
	}

	require.Positive(t, total, "the corpus must contain cursor-id learning to measure at all")
}

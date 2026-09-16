package oracle

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// taggingTagPrefix is what shared.NewUserQueryTagger produces for the fixture
// below — no `conn=`, which is the whole point of the Oracle variant.
const taggingTagPrefix = "/*dbbat='0.28.1',user='florent',grant='diag-paris'*/ "

// taggedSession builds a session with the per-user tag on and a negotiated
// session data unit, ready to rewrite.
func taggedSession(t *testing.T, sdu int) *session {
	t.Helper()

	s := newTestSession(&store.Grant{
		UID:        uuid.New(),
		Definition: &store.GrantDefinition{Slug: "diag-paris"},
	})
	s.tagging.tagger = shared.NewUserQueryTagger("0.28.1", "florent", "diag-paris")
	s.tagging.sdu = sdu

	require.Equal(t, taggingTagPrefix, s.tagging.tagger.Prefix(),
		"the bytes are pinned by internal/proxy/shared; this fixture must use them")

	return s
}

// messageOf wraps a TTC body as the single-packet client message
// clientToUpstream hands to the rewriter.
func messageOf(ttc []byte) *statementFragments {
	payload := append([]byte{0x00, 0x00}, ttc...)
	pkt := &TNSPacket{Type: TNSPacketTypeData, Payload: payload, Raw: encodeV315DataPacket(payload)}

	return &statementFragments{packets: []*TNSPacket{pkt}, gate: pkt, complete: true}
}

// upstreamTTC reassembles what the rewriter would put on the upstream socket
// back into one TTC body, the way the server's own reader would.
func upstreamTTC(t *testing.T, frames [][]byte) []byte {
	t.Helper()

	var out []byte

	for _, frame := range frames {
		require.Greater(t, len(frame), tnsHeaderSize+ttcDataFlagsSize)
		out = append(out, frame[tnsHeaderSize+ttcDataFlagsSize:]...)
	}

	return out
}

// firstCorpusStatementFrame returns the first statement-carrying TTC body of a
// recording.
func firstCorpusStatementFrame(t *testing.T, name string) []byte {
	t.Helper()

	td := loadTestDump(t, name)

	for _, ttc := range surveyClientTTC(t, td) {
		if frameCarriesStatement(ttc) {
			return ttc
		}
	}

	t.Fatalf("%s carries no statement frame", name)

	return nil
}

// TestStatementTaggingOffChangesNotOneByte is the default, and the one an
// upgrade must not change: with DBB_QUERY_TAGGING_ORACLE unset the rewriter is
// never consulted at all and the client's own packets go upstream.
func TestStatementTaggingOffChangesNotOneByte(t *testing.T) {
	t.Parallel()

	s := newTestSession(&store.Grant{Definition: &store.GrantDefinition{}})
	ttc := firstCorpusStatementFrame(t, "go_ora.pcapng")

	_, ok := s.rewriteStatementMessage(messageOf(ttc))
	assert.False(t, ok, "an untagged session must never rewrite")

	// And a session whose Accept yielded no session data unit does not tag
	// either, however configured: a rewritten message has to be cut into packets
	// the upstream accepts.
	s.tagging.tagger = shared.NewUserQueryTagger("0.28.1", "florent", "diag-paris")

	_, ok = s.rewriteStatementMessage(messageOf(ttc))
	assert.False(t, ok, "no negotiated SDU, no rewriting")
}

// TestStatementTaggingTagsEveryRecordedClient walks one recording per client
// shape and checks the tag reaches the wire in each — which is the claim the
// per-client table in docs/oracle.md makes.
func TestStatementTaggingTagsEveryRecordedClient(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"go_ora.pcapng",
		"python_thin.pcapng",
		"jdbc_thin_midfetch_fail.pcapng",
		"dbeaver.pcapng",
		"sqlplus_cursor_reexec.pcapng",
	} {
		ttc := firstCorpusStatementFrame(t, name)

		before, ok := decodeExecStatement(ttc)
		require.True(t, ok, "%s", name)

		s := taggedSession(t, 8192)

		frames, ok := s.rewriteStatementMessage(messageOf(ttc))
		require.True(t, ok, "%s: the session must certify and tag", name)
		require.True(t, s.tagging.certified, "%s", name)

		after, ok := decodeExecStatement(upstreamTTC(t, frames))
		require.True(t, ok, "%s: the rewritten frame must still decode", name)

		assert.Equal(t, taggingTagPrefix+strings.TrimSuffix(before, "\x00"),
			strings.TrimSuffix(after, "\x00"),
			"%s: the wire carries the tag in front of the client's own statement", name)
	}
}

// TestStatementTaggingKeepsTheClientsBytesForTheRecord is the storage invariant
// the other three protocols assert, in the form Oracle needs it: the buffers the
// gate, the `queries` row, the audit chain and the .pcapng capture all read from
// are the client's, and the rewriter does not touch them.
//
// The ordering is structural — clientToUpstream writes the capture and runs
// interceptClientMessage before it ever calls in here — so what is checked is
// the other half: that the rewrite leaves those buffers alone rather than
// editing the message in place.
func TestStatementTaggingKeepsTheClientsBytesForTheRecord(t *testing.T) {
	t.Parallel()

	ttc := firstCorpusStatementFrame(t, "go_ora.pcapng")

	s := taggedSession(t, 8192)
	msg := messageOf(ttc)

	// What the gate sees, and what the tracker therefore records.
	require.False(t, s.interceptClientMessage(msg.gate), "the fixture statement is not blocked")

	require.NotNil(t, s.tracker.pendingQuery)
	recorded := s.tracker.pendingQuery.cursor.sql
	require.NotEmpty(t, recorded)
	assert.NotContains(t, recorded, "dbbat=", "the recorded statement is the client's own")

	gateBefore := string(msg.gate.Payload)
	rawBefore := string(msg.packets[0].Raw)

	frames, ok := s.rewriteStatementMessage(msg)
	require.True(t, ok)

	assert.Equal(t, gateBefore, string(msg.gate.Payload),
		"the buffer the gate and the recorder read must not be edited in place")
	assert.Equal(t, rawBefore, string(msg.packets[0].Raw),
		"nor the client's own packet, which is what the capture wrote")

	wire, ok := decodeExecStatement(upstreamTTC(t, frames))
	require.True(t, ok)
	assert.Equal(t, taggingTagPrefix+recorded, wire,
		"the tag exists on the upstream wire and nowhere else")
}

// TestStatementTaggingDecidesOncePerSession is the rule the spec is most
// insistent about: a client whose first statement cannot be relocated exactly
// runs untagged start to finish, rather than tagging the frames that happen to
// parse. A statement tagged on some executions and not others gets both a tagged
// and an untagged SQL_ID, which doubles the cursor count the whole feature
// exists to bound.
func TestStatementTaggingDecidesOncePerSession(t *testing.T) {
	t.Parallel()

	// A frame the exact locator refuses: a 0xFC short-form CLR prefix, which is
	// a length this encoder will not write.
	unrewritable := thinExecCLR("SELECT " + strings.Repeat("f", 235) + " FROM dual")
	require.True(t, frameCarriesStatement(unrewritable))

	_, ok := locateStatementRewrite(unrewritable, false)
	require.False(t, ok, "the fixture must actually be refused")

	// An ordinary one, which on its own would tag.
	ordinary := firstCorpusStatementFrame(t, "go_ora.pcapng")

	s := taggedSession(t, 8192)

	_, ok = s.rewriteStatementMessage(messageOf(unrewritable))
	require.False(t, ok)
	require.True(t, s.tagging.decided)
	require.False(t, s.tagging.certified)

	_, ok = s.rewriteStatementMessage(messageOf(ordinary))
	assert.False(t, ok,
		"once a session is decided against, every later statement forwards untagged too")

	// The verdict really is per session and not per frame: the same ordinary
	// frame on a fresh session tags.
	fresh := taggedSession(t, 8192)
	_, ok = fresh.rewriteStatementMessage(messageOf(ordinary))
	assert.True(t, ok)
}

// TestStatementTaggingIgnoresNonStatementFrames keeps a fetch, a close list or a
// logoff from taking the session's verdict — the decision belongs to the first
// frame that was actually supposed to carry a statement.
func TestStatementTaggingIgnoresNonStatementFrames(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora.pcapng")

	s := taggedSession(t, 8192)
	skipped := 0

	for _, ttc := range surveyClientTTC(t, td) {
		if frameCarriesStatement(ttc) {
			break
		}

		_, ok := s.rewriteStatementMessage(messageOf(ttc))
		require.False(t, ok, "a frame carrying no statement is forwarded untouched")
		require.False(t, s.tagging.decided, "and does not decide anything")

		skipped++
	}

	assert.Positive(t, skipped, "the recording opens with non-statement frames")
}

// TestStatementTaggingLeavesCursorReexecutionAlone settles the spec's "check
// rather than assume": a re-execution frame carries a cursor id and no SQL, so
// there is nothing to rewrite, and the upstream cursor keeps whichever text the
// parse installed.
//
// The consequence to be sure of is the one that could bite silently — dbbat's
// tracker holds the *client's* text for that cursor while the upstream holds the
// tagged one, and nothing in the re-execution path compares the two.
func TestStatementTaggingLeavesCursorReexecutionAlone(t *testing.T) {
	t.Parallel()

	reexec := 0

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		s := taggedSession(t, 8192)

		for _, ttc := range surveyClientTTC(t, td) {
			if !IsPiggybackCursorReexec(ttc) && !surveyIsCursorReexec(ttc) {
				continue
			}

			reexec++

			assert.False(t, frameCarriesStatement(ttc),
				"%s: a re-execution carries a cursor id, not a statement", name)

			_, ok := s.rewriteStatementMessage(messageOf(ttc))
			assert.False(t, ok, "%s: so there is nothing for the rewriter to do", name)
		}
	}

	t.Logf("cursor re-execution frames in the corpus: %d", reexec)
	require.Positive(t, reexec, "the corpus must carry re-executions for this to mean anything")
}

// TestStatementTaggingReexecutionKeepsTheClientsTextInTheTracker is the other
// half of the same question, at the tracker rather than on the wire: the cursor
// dbbat re-gates is the one it learned from the client's own text, and a tagged
// parse upstream does not put it out of step.
//
// The parse here is an OALL8, which is simply the one op that seeds a cursor id
// without a server round trip. That op is *not* rewritten (oall8RewriteEnabled),
// and the assertion below says so rather than working around it — what is being
// checked is the tracker, and the tracker holds the client's text whether the
// wire carried a tag or not.
func TestStatementTaggingReexecutionKeepsTheClientsTextInTheTracker(t *testing.T) {
	t.Parallel()

	const sql = "SELECT 1 FROM DUAL"

	s := taggedSession(t, 8192)

	// The parse: gated and tracked on the client's text.
	require.NoError(t, s.handleOALL8(buildOALL8(sql, nil, 7)))
	require.Equal(t, sql, s.tracker.cursors[7].sql)

	_, ok := s.rewriteStatementMessage(messageOf(buildOALL8(sql, nil, 7)))
	require.False(t, ok, "OALL8 is forwarded byte for byte; see TestOALL8RewriteIsDisabled")

	// The re-execution names cursor 7 and nothing else, and re-gates against the
	// text the tracker holds — the client's.
	s.tracker.pendingQuery = nil

	require.NoError(t, s.handlePiggybackReexec(buildPiggybackReexec(7)))
	require.NotNil(t, s.tracker.pendingQuery)
	assert.Equal(t, sql, s.tracker.pendingQuery.cursor.sql,
		"the re-execution is gated on the statement the client sent, untagged")
}

// TestStatementTaggingTagsTheParseButNotTheReexecution is the same invariant on
// the wire, on the op that is actually rewritten: the parse carries the tag
// upstream, the re-execution that follows carries a cursor id and nothing else,
// and dbbat leaves it exactly as it arrived.
func TestStatementTaggingTagsTheParseButNotTheReexecution(t *testing.T) {
	t.Parallel()

	parse := firstCorpusStatementFrame(t, "go_ora.pcapng")

	client, ok := decodeExecStatement(parse)
	require.True(t, ok)

	s := taggedSession(t, 8192)

	frames, ok := s.rewriteStatementMessage(messageOf(parse))
	require.True(t, ok)

	wire, ok := decodeExecStatement(upstreamTTC(t, frames))
	require.True(t, ok)
	require.Equal(t, taggingTagPrefix+client, wire)

	// The upstream cursor now holds the tagged text and dbbat's tracker holds the
	// client's. Nothing compares the two, which is the thing to be sure of.
	_, ok = s.rewriteStatementMessage(messageOf(buildPiggybackReexec(7)))
	assert.False(t, ok, "a re-execution carries no statement, so there is nothing to rewrite")
}

// TestStatementTaggingCertifiedSessionSkipsOneFrameAndKeepsGoing is the converse
// of TestStatementTaggingDecidesOncePerSession, and it pins a choice rather than
// an accident.
//
// A session that certified and then meets a frame the locator refuses forwards
// *that* frame untagged and carries on tagging the rest. The tempting
// alternative — demote the session on the first refusal — is what must not
// happen: it would leave the statements before the demotion tagged and the same
// statements after it untagged, which is precisely the two-SQL_IDs-for-one-
// statement bug the per-session gate exists to prevent. Forwarding the one frame
// untagged does not have that property, because the locator is a pure function
// of the frame: the same statement always gets the same verdict.
//
// Without this test a refactor that dropped the `!located` guard, or that
// demoted the session, would pass the whole suite while reintroducing the bug.
func TestStatementTaggingCertifiedSessionSkipsOneFrameAndKeepsGoing(t *testing.T) {
	t.Parallel()

	ordinary := firstCorpusStatementFrame(t, "go_ora.pcapng")

	// Statement-carrying, and refused: a 0xFC short-form CLR prefix.
	unrewritable := thinExecCLR("SELECT " + strings.Repeat("f", 235) + " FROM dual")
	require.True(t, frameCarriesStatement(unrewritable))

	s := taggedSession(t, 8192)

	// Frame 1 certifies the session and is tagged.
	first, ok := s.rewriteStatementMessage(messageOf(ordinary))
	require.True(t, ok)
	require.True(t, s.tagging.certified)

	wire, ok := decodeExecStatement(upstreamTTC(t, first))
	require.True(t, ok)
	require.Contains(t, wire, "/*dbbat='")

	// Frame 2 is one the locator refuses: it forwards untagged, and says so once.
	_, ok = s.rewriteStatementMessage(messageOf(unrewritable))
	assert.False(t, ok, "a frame that cannot be relocated exactly is forwarded as it arrived")
	assert.True(t, s.tagging.warnedFrame, "and the anomaly is logged")
	assert.True(t, s.tagging.certified,
		"but the session is NOT demoted: demoting it would untag statements that were "+
			"tagged a moment ago, which is the two-SQL_IDs bug this design prevents")

	// Frame 3 is ordinary again, and is tagged — the same verdict frame 1 got, so
	// that statement has exactly one text upstream for the life of the session.
	third, ok := s.rewriteStatementMessage(messageOf(ordinary))
	require.True(t, ok, "the session keeps tagging")

	again, ok := decodeExecStatement(upstreamTTC(t, third))
	require.True(t, ok)
	assert.Equal(t, wire, again,
		"the same statement must go upstream as the same bytes every time, or it costs "+
			"two shared-pool cursors instead of one")
}

// TestStatementTaggingRecutsPastTheNegotiatedUnit exercises the growth axis the
// SDU imposes: sqlplus negotiates 2048, so a statement that already filled its
// packets cannot absorb the tag in place and the outgoing message has to be cut
// into more packets than arrived.
func TestStatementTaggingRecutsPastTheNegotiatedUnit(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("r", 6000) + " FROM dual"
	ttc := thinExecFrame(sql, encodeChunkedCLR([]byte(sql), len(sql), true))

	s := taggedSession(t, 2048)
	s.clientBigClrChunks = true

	frames, ok := s.rewriteStatementMessage(messageOf(ttc))
	require.True(t, ok)
	require.GreaterOrEqual(t, len(frames), 3, "a 6KB statement cannot fit 2048-byte packets")

	for _, frame := range frames {
		assert.LessOrEqual(t, len(frame), 2048, "no packet may exceed the negotiated unit")
	}

	stmt, ok := decodeExecStatementText(upstreamTTC(t, frames))
	require.True(t, ok, "the fragments must reassemble to a readable message")
	assert.Equal(t, taggingTagPrefix+sql, stmt.Text)
}

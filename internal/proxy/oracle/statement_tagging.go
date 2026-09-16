package oracle

import (
	"log/slog"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/version"
)

// The per-user statement tag on the Oracle leg.
//
// `V$SQL` keys on statement text, so the tag's cardinality *is* its cost: every
// distinct tag is a distinct SQL_ID holding its own child cursor in the shared
// pool. Measured on Oracle 23ai Free (docs/oracle.md, "Statement tagging"), a
// tag constant per dbbat **user** costs one cursor, one hard parse and ~48KB per
// identity and then plateaus; the same traffic under a per-connection tag cost
// 200 cursors and 9.6MB with no ceiling. So the tag dbbat prepends here is
// `shared.NewUserQueryTagger`'s — version, user and grant, no `conn=` — and the
// switch that turns it on is Oracle's own (`DBB_QUERY_TAGGING_ORACLE=user`),
// deliberately not `DBB_QUERY_TAGGING`: an operator who accepted the trade-off
// on PostgreSQL did not accept this one.
//
// **The decision is per session, and it is taken once.** Partial coverage is its
// own bug, and a worse one than no tagging: a statement tagged on some
// executions and not others gets *both* a tagged and an untagged SQL_ID, which
// doubles exactly the cursor count the measurement was about. So the first
// statement-carrying frame of a session decides for the whole session — if this
// client's shape cannot be rewritten with certainty, the session runs untagged
// start to finish and says so once, rather than tagging the frames that happen
// to parse.
//
// What the decision rests on is `locateStatementRewrite`, whose certainty is a
// round-trip identity: it re-encodes what it read and refuses unless the result
// is the client's own bytes. That check is cheap enough to run on *every* frame
// rather than only on the first, and it does — the per-session flag says whether
// tagging is attempted at all, the per-frame check is what makes attempting it
// safe. The two are not the same thing, and the second is not a fallback: the
// locator is a pure function of the frame, so the same statement from the same
// client always gets the same verdict, which is the property the SQL_ID argument
// actually needs.
//
// Nothing here reaches the store. `interceptClientMessage` has already gated,
// recorded and (if configured) parked the statement on the client's own text by
// the time `clientToUpstream` calls in here, and the `.pcapng` capture is
// written by the reader. The tag exists on the upstream wire and nowhere else.

// statementTagging is one session's tagging state.
type statementTagging struct {
	// tagger is inert (zero value) unless the per-user tag is configured, in
	// which case Apply/Prefix carry the session's identity.
	tagger shared.QueryTagger

	// sdu is the negotiated session data unit, read off the Accept packet. Zero
	// means it could not be read, which is itself a reason not to tag: a
	// rewritten message has to be cut into packets the upstream will accept.
	sdu int

	// decided/certified are the once-per-session verdict. certified is only
	// meaningful once decided is set.
	decided   bool
	certified bool

	// warnedFrame records that a post-decision frame was refused, so the
	// anomaly is logged once rather than once per statement.
	warnedFrame bool
}

// active reports whether this session might tag anything at all.
func (t *statementTagging) active() bool {
	return t.tagger.Active() && t.sdu > 0
}

// configureStatementTagging installs the per-user tagger once the grant is
// known. It is the Oracle counterpart of the one line MySQL and PostgreSQL need
// (`shared.NewQueryTagger` at auth), minus the `conn=` field.
func (s *session) configureStatementTagging() {
	if !s.statementTaggingEnabled || s.user == nil || s.grant == nil {
		return
	}

	s.tagging.tagger = shared.NewUserQueryTagger(version.Version, s.user.Username, s.grant.DefinitionSlug())
}

// rewriteStatementMessage returns the packets to write upstream in place of the
// client's own, when this session tags and this message can be rewritten with
// certainty. It reports false for everything else, which is every message on a
// session with tagging off — the disabled path costs one boolean.
func (s *session) rewriteStatementMessage(msg *statementFragments) ([][]byte, bool) {
	if msg == nil || msg.gate == nil || len(msg.packets) == 0 || !s.tagging.active() {
		return nil, false
	}

	ttc := extractTTCPayload(msg.gate.Payload)
	if ttc == nil || !frameCarriesStatement(ttc) {
		return nil, false
	}

	rw, located := locateStatementRewrite(ttc, s.clientBigClrChunks)

	if !s.tagging.decided {
		s.decideStatementTagging(ttc, located)
	}

	if !s.tagging.certified {
		return nil, false
	}

	if !located {
		s.warnStatementFrameSkipped(ttc)

		return nil, false
	}

	prefix := s.tagging.tagger.Prefix()

	tagged := make([]byte, 0, len(prefix)+len(rw.run))
	tagged = append(tagged, prefix...)
	tagged = append(tagged, rw.run...)

	body := rw.apply(ttc, tagged, s.clientBigClrChunks)

	frames, ok := refragmentStatementMessage(msg.packets[0], body, s.tagging.sdu)
	if !ok {
		s.warnStatementFrameSkipped(ttc)

		return nil, false
	}

	s.logger.DebugContext(s.ctx, "oracle: tagged statement forwarded upstream",
		slog.String("sql", truncateSQL(rw.text(), 80)),
		slog.Int("packets_in", len(msg.packets)),
		slog.Int("packets_out", len(frames)),
		slog.Int("ttc_bytes", len(body)))

	return frames, true
}

// decideStatementTagging takes the once-per-session verdict and records the
// reason, whichever way it goes. It is deliberately not silent: an operator who
// turned the setting on and sees no tags in V$SQL has to be able to find out
// why from the logs of the session that did not carry one.
func (s *session) decideStatementTagging(ttc []byte, located bool) {
	s.tagging.decided = true
	s.tagging.certified = located

	if located {
		s.logger.InfoContext(s.ctx, "oracle: per-user statement tag enabled for this session",
			slog.String("tag", s.tagging.tagger.Tag()),
			slog.Int("sdu", s.tagging.sdu))

		return
	}

	s.logger.InfoContext(s.ctx, "oracle: per-user statement tag disabled for this session: "+
		"the first statement's frame could not be located exactly, and a session tagged in part "+
		"would give one statement two SQL_IDs",
		slog.String("op", ttcOpFunction(ttc)),
		slog.String("func", TTCFunctionCode(ttc[0]).String()))
}

// warnStatementFrameSkipped reports the anomaly of a certified session meeting a
// frame it cannot rewrite, once per session.
func (s *session) warnStatementFrameSkipped(ttc []byte) {
	if s.tagging.warnedFrame {
		return
	}

	s.tagging.warnedFrame = true

	s.logger.WarnContext(s.ctx, "oracle: forwarding a statement untagged on a session that tags; "+
		"its shape is not one the exact locator covers",
		slog.String("op", ttcOpFunction(ttc)),
		slog.String("func", TTCFunctionCode(ttc[0]).String()))
}

// frameCarriesStatement reports whether a client TTC frame is one of the three
// statement-carrying ops interceptClientMessage dispatches on.
//
// It reads the op *header*, not the statement, and deliberately so: this is what
// decides whether a frame takes part in the per-session verdict, so it has to
// say "this was supposed to carry a statement" even when the exact locator
// cannot find it — that case is precisely the one that must leave the session
// untagged rather than tag around it.
func frameCarriesStatement(ttcPayload []byte) bool {
	if len(ttcPayload) == 0 {
		return false
	}

	switch TTCFunctionCode(ttcPayload[0]) { //nolint:exhaustive // only the statement-carrying ops matter here
	case TTCFuncPiggyback:
		return IsPiggybackExecSQL(ttcPayload)

	case TTCFuncOFETCH:
		// 0x11/0x69 is Oracle's close-cursors piggyback; it carries a statement
		// only when a client staples an execute behind the close list.
		if !IsExecSQL(ttcPayload) {
			return false
		}

		end, ok := closeCursorsEnd(ttcPayload)

		return ok && end < len(ttcPayload) && isPiggybackExecHeader(ttcPayload[end:])

	case TTCFuncOALL8:
		_, err := decodeOALL8(ttcPayload)

		return err == nil

	default:
		return false
	}
}

package postgresql

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/approval"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

const querytagConnUID = "0192f3a1-7c4d-7e2a-9b11-3f9a1c7b2e4d"

// taggedSession is newTestSession plus an active statement tagger, i.e. a
// session running with DBB_QUERY_TAGGING=true.
func taggedSession(t *testing.T) *Session {
	t.Helper()

	s := newTestSession("write")
	s.queryTagging = true
	s.queryTag = shared.NewQueryTagger("0.28.1", "florent", uuid.MustParse(querytagConnUID), "diag-paris")

	return s
}

func wantPrefix() string {
	return "/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris'*/ "
}

// TestQueryTag_SimplePathTagsTheWireAndNotTheRecord is the central invariant of
// the whole feature: the message that goes upstream carries the tag, and the
// pendingQuery that feeds the queries table and the audit chain carries the
// client's text.
func TestQueryTag_SimplePathTagsTheWireAndNotTheRecord(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	msg := &pgproto3.Query{String: "SELECT 1"}

	require.NoError(t, s.handleQuery(msg))

	assert.Equal(t, wantPrefix()+"SELECT 1", msg.String,
		"the Query forwarded upstream must carry the tag")
	require.NotNil(t, s.currentQuery)
	assert.Equal(t, "SELECT 1", s.currentQuery.sql,
		"what dbbat records and MACs is the client's statement, never the tagged one")
}

// TestQueryTag_ExtendedPathTagsParseOnce — a prepared statement is parsed once
// and executed many times, so the tag goes on the Parse. Every Bind/Execute
// inherits it from the upstream's own parsed statement, and the text dbbat
// records for each Execute is still the client's.
func TestQueryTag_ExtendedPathTagsParseOnce(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	parse := &pgproto3.Parse{Name: "s1", Query: "SELECT * FROM t WHERE id = $1", ParameterOIDs: []uint32{23}}

	require.NoError(t, s.handleParse(parse))

	assert.Equal(t, wantPrefix()+"SELECT * FROM t WHERE id = $1", parse.Query,
		"the Parse forwarded upstream must carry the tag")

	stmt := s.extendedState.preparedStatements["s1"]
	require.NotNil(t, stmt)
	assert.Equal(t, "SELECT * FROM t WHERE id = $1", stmt.sql,
		"the prepared statement dbbat remembers is the client's text")

	// Bind + Execute: the recorded statement is still the client's.
	s.handleBind(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	require.NoError(t, s.handleExecute(&pgproto3.Execute{Portal: "p1"}))

	require.Len(t, s.extendedState.pendingQueries, 1)
	assert.Equal(t, "SELECT * FROM t WHERE id = $1", s.extendedState.pendingQueries[0].sql)
}

// TestQueryTag_DisabledChangesNothing — with the feature off, the bytes on the
// wire must be exactly what they were before this feature existed.
func TestQueryTag_DisabledChangesNothing(t *testing.T) {
	t.Parallel()

	s := newTestSession("write")

	query := &pgproto3.Query{String: "SELECT 1"}
	require.NoError(t, s.handleQuery(query))
	assert.Equal(t, "SELECT 1", query.String)
	assert.NotContains(t, query.String, "/*dbbat=")

	parse := &pgproto3.Parse{Name: "s1", Query: "SELECT 2"}
	require.NoError(t, s.handleParse(parse))
	assert.Equal(t, "SELECT 2", parse.Query)
	assert.NotContains(t, parse.Query, "/*dbbat=")
}

// TestQueryTag_ControlsRunOnTheClientText — the refusals are identical with
// tagging on. They have to be: every control matched the client's text before
// the tag existed, so a pattern author never has to account for it, and a
// refused statement never reaches the tagging step at all.
func TestQueryTag_ControlsRunOnTheClientText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		controls []string
		sql      string
		wantErr  error
	}{
		{"read_only write", []string{store.ControlReadOnly}, "DELETE FROM t", ErrWriteNotPermitted},
		{"read_only bypass", []string{store.ControlReadOnly}, "SET ROLE admin", ErrReadOnlyBypassAttempt},
		{"block_ddl", []string{store.ControlBlockDDL}, "CREATE TABLE t (a int)", ErrDDLNotPermitted},
		{"block_copy", []string{store.ControlBlockCopy}, "COPY t FROM STDIN", ErrCopyNotPermitted},
		{"password change", nil, "ALTER USER bob PASSWORD 'x'", ErrPasswordChangeNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, tagging := range []bool{false, true} {
				s := newTestSessionWithControls(tt.controls)
				if tagging {
					s.queryTagging = true
					s.queryTag = shared.NewQueryTagger(
						"0.28.1", "florent", uuid.MustParse(querytagConnUID), "diag-paris")
				}

				query := &pgproto3.Query{String: tt.sql}
				require.ErrorIs(t, s.handleQuery(query), tt.wantErr,
					"the refusal must not depend on whether tagging is on")
				assert.Equal(t, tt.sql, query.String,
					"a refused statement is never tagged — it never reaches the upstream")

				parse := &pgproto3.Parse{Query: tt.sql}
				require.ErrorIs(t, s.handleParse(parse), tt.wantErr)
				assert.Equal(t, tt.sql, parse.Query)
			}
		})
	}
}

// TestQueryTag_CopyIsTaggedLikeAnyStatement — COPY is a statement and gets the
// tag. Only the statement: the CopyData stream that follows is relayed through
// a different message type entirely and is never touched.
func TestQueryTag_CopyIsTaggedLikeAnyStatement(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	msg := &pgproto3.Query{String: "COPY t (a, b) FROM STDIN"}

	require.NoError(t, s.handleQuery(msg))

	assert.Equal(t, wantPrefix()+"COPY t (a, b) FROM STDIN", msg.String)
	assert.True(t, strings.HasSuffix(msg.String, "COPY t (a, b) FROM STDIN"))
}

// TestQueryTag_RepeatedExecutionsAreByteIdentical is what keeps
// pg_stat_statements aggregating: the same statement on the same session must
// produce the same bytes every time.
func TestQueryTag_RepeatedExecutionsAreByteIdentical(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)

	seen := make([]string, 0, 3)

	for range 3 {
		msg := &pgproto3.Query{String: "SELECT count(*) FROM big"}
		require.NoError(t, s.handleQuery(msg))
		seen = append(seen, msg.String)
	}

	assert.Equal(t, seen[0], seen[1])
	assert.Equal(t, seen[1], seen[2])
}

// TestQueryTag_EmptyStatementUntouched — PostgreSQL answers an empty query
// string with EmptyQueryResponse; dbbat must not turn it into a comment.
func TestQueryTag_EmptyStatementUntouched(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	msg := &pgproto3.Query{String: ""}

	require.NoError(t, s.handleQuery(msg))
	assert.Empty(t, msg.String)
}

// TestQueryTag_ApprovalHoldSeesTheClientText is the security-relevant half of
// the invariant: the pattern that decides whether a statement is parked, and
// the row an approver reads before releasing it, both carry the statement the
// *client* sent. A pattern author must never have to write `^/\*dbbat=` into
// their regex, and an approver must never be shown proxy-rewritten SQL.
//
// The statement is tagged only once the human has said yes.
func TestQueryTag_ApprovalHoldSeesTheClientText(t *testing.T) {
	t.Parallel()

	sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})
	sess.queryTagging = true
	sess.queryTag = shared.NewQueryTagger(
		"0.28.1", "florent", uuid.MustParse(querytagConnUID), "diag-paris")

	msg := &pgproto3.Query{String: "DELETE FROM users"}

	errc := make(chan error, 1)
	go func() { errc <- sess.handleQuery(msg) }()

	pending := st.waitPending(t)

	assert.Equal(t, "DELETE FROM users", pending.SQLText,
		"the parked row an approver reads must be the client's statement, untagged")

	by := uuid.New()
	reg.Resolve(approval.Decision{
		QueryUID: pending.UID, Status: store.ApprovalApproved, By: &by, ByName: "bob",
	})

	select {
	case err := <-errc:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("approved statement never resumed")
	}

	assert.Equal(t, wantPrefix()+"DELETE FROM users", msg.String,
		"the tag is applied after the hold resolved, on the way upstream")
	require.NotNil(t, sess.currentQuery)
	assert.Equal(t, "DELETE FROM users", sess.currentQuery.sql)
}

// TestQueryTag_ApprovalPatternStillMatchesAnchoredStart — the tag is
// *prepended*, so an anchored pattern like `^DELETE` would stop matching if the
// hold ran on the tagged text. It does not, and this pins that: the same
// pattern parks the same statement with tagging on as with it off.
func TestQueryTag_ApprovalPatternStillMatchesAnchoredStart(t *testing.T) {
	t.Parallel()

	for _, tagging := range []bool{false, true} {
		sess, _, st, reg := heldSession(t, []string{`(?i)^DELETE\s+FROM`})
		if tagging {
			sess.queryTagging = true
			sess.queryTag = shared.NewQueryTagger(
				"0.28.1", "florent", uuid.MustParse(querytagConnUID), "diag-paris")
		}

		errc := make(chan error, 1)
		go func() { errc <- sess.handleQuery(&pgproto3.Query{String: "DELETE FROM users"}) }()

		pending := st.waitPending(t)
		assert.Equal(t, "DELETE FROM users", pending.SQLText)

		by := uuid.New()
		reg.Resolve(approval.Decision{
			QueryUID: pending.UID, Status: store.ApprovalDenied, By: &by, ByName: "bob",
		})

		select {
		case err := <-errc:
			require.Error(t, err, "a denied statement must be refused, tagging or not")
		case <-time.After(3 * time.Second):
			t.Fatal("denied statement never resumed")
		}
	}
}

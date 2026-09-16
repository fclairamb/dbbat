package shared

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const querytagUID = "0192f3a1-7c4d-7e2a-9b11-3f9a1c7b2e4d"

func TestQueryTaggerFormat(t *testing.T) {
	t.Parallel()

	tagger := NewQueryTagger("0.28.1", "florent", uuid.MustParse(querytagUID), "diag-paris-habitat")

	assert.Equal(t,
		"/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris-habitat'*/ ",
		tagger.Prefix())

	assert.Equal(t,
		"/*dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris-habitat'*/ SELECT 1",
		tagger.Apply("SELECT 1"))
}

// TestQueryTaggerConnMatchesAppName is the consistency the whole "find the
// connection" story rests on: the 12 hex characters in the statement tag have
// to be the same ones the upstream application_name carries, or a DBA reading
// pg_stat_activity gets two different identifiers for one session.
func TestQueryTaggerConnMatchesAppName(t *testing.T) {
	t.Parallel()

	connUID := uuid.MustParse(querytagUID)

	appName := BuildUpstreamName("0.28.1", "florent", connUID, "", 128)
	tagger := NewQueryTagger("0.28.1", "florent", connUID, "diag")

	require.Contains(t, appName, "c=3f9a1c7b2e4d")
	assert.Contains(t, tagger.Prefix(), "conn='3f9a1c7b2e4d'")
}

// TestQueryTaggerIsStable is the property that keeps pg_stat_statements and the
// MySQL digest aggregating: two executions of the same statement on the same
// session must produce byte-identical text. A clock or a counter anywhere in
// the tag would turn one digest row into one row per execution.
func TestQueryTaggerIsStable(t *testing.T) {
	t.Parallel()

	connUID := uuid.MustParse(querytagUID)

	first := NewQueryTagger("0.28.1", "florent", connUID, "diag")
	second := NewQueryTagger("0.28.1", "florent", connUID, "diag")

	assert.Equal(t, first.Prefix(), second.Prefix())

	for range 5 {
		assert.Equal(t, first.Apply("SELECT 1"), second.Apply("SELECT 1"))
	}

	// Nothing that looks like a timestamp, a sequence or a uuid beyond the
	// connection suffix may appear.
	for _, banned := range []string{"time", "ts=", "seq", "id='", "20"} {
		assert.NotContains(t, first.Prefix(), banned)
	}
}

// TestQueryTaggerKeyOrder pins the emitted order. It is fixed rather than
// incidental: a tag whose keys could reorder between two runs would split one
// digest into several.
func TestQueryTaggerKeyOrder(t *testing.T) {
	t.Parallel()

	prefix := NewQueryTagger("v", "u", uuid.MustParse(querytagUID), "g").Prefix()

	iVersion := strings.Index(prefix, "dbbat=")
	iUser := strings.Index(prefix, "user=")
	iConn := strings.Index(prefix, "conn=")
	iGrant := strings.Index(prefix, "grant=")

	require.NotEqual(t, -1, iVersion)
	assert.Less(t, iVersion, iUser)
	assert.Less(t, iUser, iConn)
	assert.Less(t, iConn, iGrant)
}

// TestQueryTaggerEncodesValues is the structural-safety test: a value is
// percent-encoded, so nothing a username or a slug could contain can close the
// comment early or escape the quoting into the statement being forwarded.
func TestQueryTaggerEncodesValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		user  string
		grant string
		want  string
	}{
		{"single quote", "o'brien", "g", "user='o%27brien'"},
		{"comment close", "a*/DROP", "g", "user='a%2A%2FDROP'"},
		{"space", "jean pierre", "g", "user='jean+pierre'"},
		{"comma", "a,b", "g", "user='a%2Cb'"},
		{"newline", "a\nb", "g", "user='a%0Ab'"},
		{"slug with dashes survives", "u", "diag-paris-habitat", "grant='diag-paris-habitat'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefix := NewQueryTagger("0.1.0", tt.user, uuid.MustParse(querytagUID), tt.grant).Prefix()

			assert.Contains(t, prefix, tt.want)

			// Whatever the input, the comment closes exactly once, at the end.
			assert.Equal(t, 1, strings.Count(prefix, "*/"))
			assert.True(t, strings.HasSuffix(prefix, "*/ "))
			assert.True(t, strings.HasPrefix(prefix, "/*"))
		})
	}
}

// TestQueryTaggerOmitsEmptyFields — a shapeless grant or an unidentified
// connection shortens the tag rather than padding it with an empty-valued key, which would
// read like a value.
func TestQueryTaggerOmitsEmptyFields(t *testing.T) {
	t.Parallel()

	prefix := NewQueryTagger("0.28.1", "florent", uuid.Nil, "").Prefix()

	assert.Equal(t, "/*dbbat='0.28.1',user='florent'*/ ", prefix)
	assert.NotContains(t, prefix, "conn=")
	assert.NotContains(t, prefix, "grant=")
}

// TestQueryTaggerZeroValueIsInert is what the disabled feature relies on: with
// DBB_QUERY_TAGGING off a session holds the zero tagger, and not one byte of
// any statement may change.
func TestQueryTaggerZeroValueIsInert(t *testing.T) {
	t.Parallel()

	var tagger QueryTagger

	assert.False(t, tagger.Active())
	assert.Empty(t, tagger.Prefix())

	for _, sql := range []string{"SELECT 1", "", "   ", "COPY t FROM STDIN"} {
		assert.Equal(t, sql, tagger.Apply(sql))
	}

	// A tagger with nothing to say at all is the zero value too.
	assert.False(t, NewQueryTagger("", "", uuid.Nil, "").Active())
}

// TestQueryTaggerLeavesBlankStatementsAlone — PostgreSQL answers an empty query
// string with EmptyQueryResponse; turning it into a comment-only statement
// changes the wire bytes for no observability gain.
func TestQueryTaggerLeavesBlankStatementsAlone(t *testing.T) {
	t.Parallel()

	tagger := NewQueryTagger("0.28.1", "florent", uuid.MustParse(querytagUID), "diag")
	require.True(t, tagger.Active())

	for _, sql := range []string{"", " ", "\n\t "} {
		assert.Equal(t, sql, tagger.Apply(sql))
	}
}

// TestQueryTaggerPreservesTheStatementVerbatim — the tag is a prefix and
// nothing else. Whatever follows it is the client's bytes, untouched, which is
// what keeps optimizer hints, COPY and dollar-quoted bodies working.
func TestQueryTaggerPreservesTheStatementVerbatim(t *testing.T) {
	t.Parallel()

	tagger := NewQueryTagger("0.28.1", "florent", uuid.MustParse(querytagUID), "diag")

	for _, sql := range []string{
		"SELECT /*+ IndexScan(t) */ * FROM t",
		"COPY t (a, b) FROM STDIN",
		"DO $$ BEGIN PERFORM 1; END $$",
		"SELECT 'a''b' -- trailing comment",
	} {
		got := tagger.Apply(sql)

		assert.Equal(t, tagger.Prefix()+sql, got)
		assert.True(t, strings.HasSuffix(got, sql))
	}
}

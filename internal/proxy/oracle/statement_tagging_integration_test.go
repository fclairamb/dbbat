//go:build integration

package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// The per-user statement tag against a real Oracle.
//
// Every other test of this feature reasons about bytes. This one is the only
// thing that answers the question that actually matters: **does the server
// accept the frame dbbat rewrote?** The failure mode if it does not is
// `ORA-03146 invalid buffer length for TTC field` and a dead session, so the
// assertions below are deliberately blunt — run the statement, and read the tag
// back out of `V$SQL`, which is where Oracle's own tooling would see it.
//
// `V$SQL` is also what makes the cost argument checkable: it keys on statement
// text, so a tag whose cardinality is the number of dbbat *users* costs k
// cursors rather than one per session. See docs/oracle.md.

// vsqlTextsLike returns every V$SQL entry whose text contains marker, read
// through the proxy (so the lookup itself is a tagged statement, which is fine —
// it just does not match the marker).
func vsqlTextsLike(t *testing.T, db *sql.DB, marker string) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		"SELECT sql_text FROM v$sql WHERE sql_text LIKE :1 AND sql_text NOT LIKE :2",
		"%"+marker+"%", "%v$sql%")
	require.NoError(t, err)

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var text string

		require.NoError(t, rows.Scan(&text))

		out = append(out, text)
	}

	require.NoError(t, rows.Err())

	return out
}

// recordedStatements returns the sql_text of every `queries` row this fixture's
// user has, which is what the UI, the audit chain and the capture all agree on.
func recordedStatements(t *testing.T, env *oracleThroughProxy) []string {
	t.Helper()

	rows, err := env.store.ListQueries(context.Background(), store.QueryFilter{
		UserID: &env.user.UID,
		Limit:  500,
	})
	require.NoError(t, err)

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.SQLText)
	}

	return out
}

// TestIntegration_StatementTagReachesVSQL is the whole feature, end to end: a
// statement issued through a tagging session arrives at Oracle carrying the tag,
// the server parses and runs it, and what dbbat recorded is the client's own
// text.
func TestIntegration_StatementTagReachesVSQL(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_tag_probe_one"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
		"the rewritten frame must be one the server accepts (ORA-03146 is the failure this checks for)")
	assert.Equal(t, 1, n)

	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts, "the statement must be in V$SQL")

	for _, text := range texts {
		assert.True(t, strings.HasPrefix(text, "/*dbbat='"),
			"Oracle's own view of the statement carries the tag: %q", text)
		assert.Contains(t, text, "user='"+env.username+"'")
		assert.Contains(t, text, "grant='")
		assert.NotContains(t, text, "conn='",
			"the Oracle tag is per user, not per connection — that is the whole cost argument")
	}

	// And the storage invariant: the tag is on the wire and nowhere else.
	recorded := recordedStatements(t, env)
	require.NotEmpty(t, recorded)

	found := false

	for _, sqlText := range recorded {
		assert.NotContains(t, sqlText, "/*dbbat='",
			"no recorded statement may carry the tag")

		if sqlText == probe {
			found = true
		}
	}

	assert.True(t, found, "the probe must be recorded verbatim; got %v", recorded)
}

// TestIntegration_StatementTagCostsOneCursorPerUser is the measurement the whole
// feature rests on, asserted rather than remembered: the same statement executed
// many times over one identity is one SQL_ID, not one per execution.
func TestIntegration_StatementTagCostsOneCursorPerUser(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_tag_probe_cardinality"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	for range 25 {
		var n int

		require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n))
	}

	texts := vsqlTextsLike(t, env.db, marker)
	assert.Len(t, texts, 1,
		"25 executions of one statement under one identity must be one shared-pool entry, got %v", texts)
}

// TestIntegration_StatementTagCrossesTheCLRBoundary is the encoding case the
// spec singles out: a statement sitting just under the 252-byte CLR short-form
// limit, which the ~50-byte tag pushes into the 0xFE-chunked long form. Getting
// that wrong is the ORA-03146 case, so it gets a real server.
func TestIntegration_StatementTagCrossesTheCLRBoundary(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	for _, pad := range []int{150, 200, 210, 220, 240, 245} {
		marker := fmt.Sprintf("dbbat_clr_probe_%d", pad)
		probe := fmt.Sprintf("SELECT 1 AS %s /* %s */ FROM dual",
			marker, strings.Repeat("p", pad))

		var n int

		require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
			"pad=%d (statement %d bytes) must be accepted", pad, len(probe))
		assert.Equal(t, 1, n)

		texts := vsqlTextsLike(t, env.db, marker)
		require.NotEmpty(t, texts, "pad=%d", pad)
		assert.True(t, strings.HasPrefix(texts[0], "/*dbbat='"), "pad=%d: %q", pad, texts[0])
	}
}

// TestIntegration_StatementTagPastTheSDU covers the other growth axis: a message
// already at the negotiated session data unit cannot absorb the tag in place, so
// the outgoing packets are re-cut. The statement is deliberately larger than the
// 8192-byte default.
func TestIntegration_StatementTagPastTheSDU(t *testing.T) {
	env := startOracleThroughProxyWith(t, oracleFixtureOptions{statementTagging: true})

	const marker = "dbbat_sdu_probe"

	probe := fmt.Sprintf("SELECT 1 AS %s /* %s */ FROM dual", marker, strings.Repeat("s", 20000))
	require.Greater(t, len(probe), 16384)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n),
		"a statement spanning several TNS packets must survive being re-cut")
	assert.Equal(t, 1, n)

	// V$SQL truncates SQL_TEXT at 1000 characters, so the tag — which is at the
	// front, deliberately, precisely because every such view truncates — is
	// still visible.
	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts)
	assert.True(t, strings.HasPrefix(texts[0], "/*dbbat='"), "%q", texts[0])
}

// TestIntegration_StatementTaggingOffLeavesVSQLAlone is the default path, on a
// real server: with the setting off not one byte changes, so Oracle sees exactly
// what the client sent.
func TestIntegration_StatementTaggingOffLeavesVSQLAlone(t *testing.T) {
	env := startOracleThroughProxy(t, nil)

	const marker = "dbbat_untagged_probe"

	probe := fmt.Sprintf("SELECT 1 AS %s FROM dual", marker)

	var n int

	require.NoError(t, env.db.QueryRowContext(context.Background(), probe).Scan(&n))

	texts := vsqlTextsLike(t, env.db, marker)
	require.NotEmpty(t, texts)

	for _, text := range texts {
		assert.NotContains(t, text, "dbbat=",
			"with tagging off the server must see the client's own statement: %q", text)
	}
}

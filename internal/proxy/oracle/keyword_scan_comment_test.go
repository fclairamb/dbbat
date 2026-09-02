package oracle

import (
	"strings"
	"testing"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// unreadableExecFrame wraps text in a frame whose exec header dbbat cannot
// read, which is the only way the last-resort keyword scan is reached.
func unreadableExecFrame(text string) []byte {
	payload := append([]byte{byte(ttcPiggybackMessageType), subOpCloseCursors, 0x00, 0x01}, text...)

	return append(payload, 0x00, 0x01)
}

// TestKeywordScanDoesNotOpenAStatementInsideAComment pins the 2026-09-01
// production reading: a SQL verb that is a *word in a French comment* was read
// as the statement's verb, the `--` in front of it was dropped, and the
// apostrophe two words later opened a quoted run that never closed — so a
// statement that arrived whole was refused as unreadable.
//
// The header-anchored decode learned to step over leading comments in 0.26.1;
// this is the same defect on the path that has no header to anchor on.
func TestKeywordScanDoesNotOpenAStatementInsideAComment(t *testing.T) {
	t.Parallel()

	// Every one of these is a comment line taken from the Abyla migration
	// package that hit this, or its shape.
	cases := []struct{ name, sql string }{
		{
			"line comment leading the statement",
			"-- MERGE s'execute (pas la date de generation).\nSELECT 1 FROM dual",
		},
		{
			"accented French comment",
			"-- UPDATE d’instances ne pouvait faire le travail\n" +
				"SELECT COUNT(*) FROM VALOPHIS_SYNCHRO.COMPOSANT",
		},
		{
			"comment naming a verb the gate refuses",
			"-- TRUNCATE : la table peut porter le mapping d'une migration précédente.\n" +
				"DELETE FROM VALOPHIS_SYNCHRO.ABY_CORR_BIB WHERE 1 = 0",
		},
		{
			"block comment",
			"/* MERGE s'execute (pas la date) */ SELECT 1 FROM dual",
		},
		{
			"trailing comment on the verb's own line",
			"INSERT INTO VALOPHIS_SYNCHRO.ABY_CORR_BIB (A) -- HC0333 Traitement d'air divers\n" +
				"SELECT 1 FROM dual",
		},
		{
			"comment between two statements' worth of text",
			"WITH x AS (SELECT 1 c FROM dual)\n-- DELETE n'est pas fait ici\nSELECT c FROM x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, cut := findSQLInPayload(unreadableExecFrame(tc.sql))
			if got != tc.sql {
				t.Fatalf("scan returned a fragment\n want %q\n got  %q", tc.sql, got)
			}

			if cut {
				t.Fatalf("statement reported as cut by the payload end: %q", got)
			}

			// The point of reading it whole: the gate can now check it. Before
			// the fix each of these was refused with ErrStatementNotFullyRead
			// for a statement that had arrived intact.
			if err := shared.ValidateOracleQuery(got, &store.Grant{Definition: &store.GrantDefinition{}}); err != nil {
				if strings.Contains(err.Error(), "could not read this statement to its end") {
					t.Fatalf("still refused as unreadable: %v", err)
				}
			}
		})
	}
}

// TestKeywordScanStillRefusesARunThatIsOnlyAComment keeps the re-anchoring from
// becoming a way past the gate. A run whose only verb sits in a comment carries
// no statement, so re-anchoring on the comment would hand the gate text with no
// verb in it and the frame would be forwarded unexamined. The scan falls back
// to its old reading instead, which is refused — the scan vouches for nothing
// here, and fail-closed is the direction that costs a retry rather than a hole.
func TestKeywordScanStillRefusesARunThatIsOnlyAComment(t *testing.T) {
	t.Parallel()

	got, _ := findSQLInPayload(unreadableExecFrame("-- rien qu'un commentaire nommant MERGE"))
	if got == "" {
		t.Fatal("comment-only run yielded no reading at all; it must stay fail-closed")
	}

	if strings.HasPrefix(got, "--") {
		t.Fatalf("comment-only run was re-anchored and would reach the gate verbless: %q", got)
	}
}

// TestCommentOpenerBefore covers the enclosure rules directly, including the
// two that stop it re-anchoring on something that is not a comment.
func TestCommentOpenerBefore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"inside a line comment", "-- MERGE", true},
		{"inside a block comment", "/* MERGE", true},
		{"after the line comment's newline", "-- x\nMERGE", false},
		{"after the block comment closed", "/* x */ MERGE", false},
		{"no comment at all", "SELECT 1 FROM t WHERE a = MERGE", false},
		// Not comment openers: a double minus continuing an identifier, and a
		// slash-star behind a word byte.
		{"double minus inside an expression", "SELECT a--MERGE", false},
		{"slash star behind a word byte", "SELECT a/*MERGE", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := []byte(tc.payload)

			idx := indexOfAnyKeywordCI(payload, findSQLKeywords)
			if idx < 0 {
				t.Fatalf("test payload carries no keyword: %q", tc.payload)
			}

			// The first keyword match may be an earlier verb (SELECT); the
			// enclosure question is about the MERGE this fixture ends on.
			idx = strings.Index(tc.payload, "MERGE")

			if got := commentOpenerBefore(payload, idx) >= 0; got != tc.want {
				t.Fatalf("commentOpenerBefore(%q) enclosed=%v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// TestCommentOpenerBeforeStaysInsideThePrintableRun is the bound that keeps this
// from becoming the backward extension the extraction survey rejected: a `--`
// on the far side of TTC framing bytes is not a comment over this text.
func TestCommentOpenerBeforeStaysInsideThePrintableRun(t *testing.T) {
	t.Parallel()

	payload := append([]byte("-- une autre requête"), 0x00, 0x01)
	idx := len(payload)
	payload = append(payload, "MERGE INTO t USING dual ON (1=1)"...)

	if got := commentOpenerBefore(payload, idx); got >= 0 {
		t.Fatalf("re-anchored across framing bytes at %d; the run before it is not a comment", got)
	}
}

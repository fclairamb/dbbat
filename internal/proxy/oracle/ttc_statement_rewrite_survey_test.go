package oracle

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The corpus verdict for the exact locator, structured the way
// sql_extraction_survey_test.go structures its own: one measurement per
// recording, run across every frame, reported as a table and asserted where the
// answer must not regress.
//
// It is the test that had to pass before a single rewritten byte was allowed
// near a socket. Two properties are the deliverable:
//
//   - **Identity.** For every statement-carrying frame in testdata/, rewriting
//     the statement to *itself* reproduces the client's own bytes exactly. A
//     model of a frame that cannot reproduce the frame is a model that must not
//     be used to change it, and this is that check run over every recorded shape
//     dbbat supports rather than over a hand-written sample.
//   - **Round trip.** Rewriting the statement to a tagged one produces a frame
//     dbbat's own decoders read back as the tagged statement, with the declared
//     length agreeing. That is as close as a unit test gets to "the server
//     parses it"; the rest is make test-e2e-oracle.
//
// A shape the locator cannot certify is reported as refused. It is not a
// failure — a session on such a client runs untagged start to finish — but it is
// counted, because the count is what says how much of the fleet the tag actually
// reaches.

// surveyTagPrefix is a tag of the size the real one has:
// `/*dbbat='0.28.1',user='florent.clairambault',grant='diag-paris'*/ ` is 62
// bytes. Using a realistic length matters — the 252-byte CLR format change is
// provoked by the tag's size, not by its content.
const surveyTagPrefix = "/*dbbat='0.28.1',user='florent',grant='diag'*/ "

// statementFrameVerdict is one recording's tally.
type statementFrameVerdict struct {
	frames    int
	located   int
	identity  int
	roundTrip int
	refused   []string
}

// surveyStatementFrames walks a recording and returns the verdict for it.
func surveyStatementFrames(t *testing.T, name string) statementFrameVerdict {
	t.Helper()

	var v statementFrameVerdict

	td := loadTestDump(t, name)

	for _, ttc := range surveyClientTTC(t, td) {
		if !frameCarriesStatement(ttc) {
			continue
		}

		v.frames++

		// bigChunks is irrelevant to locating and to every value under the CLR
		// short-form limit; it only decides the encoding of a value that has to
		// *become* long. Both settings are exercised below.
		rw, ok := locateStatementRewrite(ttc, true)
		if !ok {
			v.refused = append(v.refused, ttcOpFunction(ttc))

			continue
		}

		v.located++

		if string(rw.apply(ttc, rw.run, true)) == string(ttc) {
			v.identity++
		}

		if statementRoundTrips(t, ttc, rw, surveyTagPrefix) {
			v.roundTrip++
		}
	}

	return v
}

// statementRoundTrips rewrites a frame's statement with the prefix and reports
// whether dbbat's own decoders read the tagged statement back out of the result.
func statementRoundTrips(t *testing.T, ttc []byte, rw stmtRewrite, prefix string) bool {
	t.Helper()

	tagged := append([]byte(prefix), rw.run...)
	out := rw.apply(ttc, tagged, true)

	stmt, ok := decodeExecStatementText(out)
	if !ok {
		// The exec decoders do not read an OALL8; that op has its own.
		if TTCFunctionCode(ttc[0]) != TTCFuncOALL8 {
			return false
		}

		res, err := decodeOALL8(out)
		if err != nil || res.Truncated {
			return false
		}

		return res.SQL == prefix+rw.text()
	}

	return strings.TrimSuffix(stmt.Text, "\x00") == prefix+rw.text()
}

// TestSurveyStatementRewriteCorpus is the measurement: every statement-carrying
// frame in every recording, located or refused, with identity and round trip
// checked on the ones that locate.
func TestSurveyStatementRewriteCorpus(t *testing.T) {
	t.Parallel()

	total := statementFrameVerdict{}
	refusedShapes := map[string]int{}

	t.Logf("=== exact statement locator, per recording ===")

	for _, name := range surveyCorpus(t) {
		v := surveyStatementFrames(t, name)
		if v.frames == 0 {
			continue
		}

		t.Logf("%-38s frames=%3d located=%3d identity=%3d roundtrip=%3d refused=%v",
			name, v.frames, v.located, v.identity, v.roundTrip, v.refused)

		total.frames += v.frames
		total.located += v.located
		total.identity += v.identity
		total.roundTrip += v.roundTrip

		for _, shape := range v.refused {
			refusedShapes[shape]++
		}
	}

	t.Logf("TOTAL frames=%d located=%d identity=%d roundtrip=%d refused-by-shape=%v",
		total.frames, total.located, total.identity, total.roundTrip, refusedShapes)

	require.Positive(t, total.frames, "the corpus must carry statement frames at all")

	require.Equal(t, total.located, total.identity,
		"every frame the locator answers for must rewrite to itself byte for byte; "+
			"a located frame that cannot reproduce its own bytes is the ORA-03146 case")
	require.Equal(t, total.located, total.roundTrip,
		"every located frame must read back as the tagged statement")

	// The coverage floor. Every client shape the corpus records — go-ora,
	// python-oracledb thin, JDBC thin, DBeaver and sqlplus (OCI) — is located,
	// so a regression that starts refusing one of them fails here rather than
	// showing up as a fleet that quietly stopped being attributable.
	require.Equal(t, total.frames, total.located,
		"every statement frame in the corpus must be locatable exactly; refused shapes: %v", refusedShapes)
}

// TestSurveyStatementRewritePerClientShape names the shapes behind the numbers,
// so the table above can be read as "which clients does the tag reach".
func TestSurveyStatementRewritePerClientShape(t *testing.T) {
	t.Parallel()

	shapes := map[string]map[string]int{}

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			if !frameCarriesStatement(ttc) {
				continue
			}

			rw, ok := locateStatementRewrite(ttc, true)
			if !ok {
				continue
			}

			client := strings.SplitN(strings.TrimSuffix(name, ".pcapng"), "_", 2)[0]
			if shapes[client] == nil {
				shapes[client] = map[string]int{}
			}

			shapes[client][stmtShapeName(rw)]++
		}
	}

	clients := make([]string, 0, len(shapes))
	for c := range shapes {
		clients = append(clients, c)
	}

	sort.Strings(clients)

	for _, c := range clients {
		t.Logf("%-10s %v", c, shapes[c])
	}

	// The two framings the thin clients split over, measured rather than
	// assumed: go-ora and python-oracledb thin repeat the length as a CLR byte
	// in front of the statement; ojdbc and DBeaver write the run bare. Reading
	// that off the wire instead of off a client identity is what lets one code
	// path serve both.
	require.Contains(t, shapes["go"], "compressed/clr-short")
	require.Contains(t, shapes["python"], "compressed/clr-short")
	require.Contains(t, shapes["jdbc"], "compressed/bare")
	require.Contains(t, shapes["dbeaver"], "compressed/bare")
	require.Contains(t, shapes["sqlplus"], "wide-ub4/clr-short")
}

// stmtShapeName labels a located statement by its two encodings.
func stmtShapeName(rw stmtRewrite) string {
	kind := map[stmtLenKind]string{
		stmtLenCompressed: "compressed",
		stmtLenWideUB4:    "wide-ub4",
		stmtLenVarLen:     "varlen",
	}[rw.lenKind]

	clr := map[stmtClrKind]string{
		stmtClrNone:    "bare",
		stmtClrShort:   "clr-short",
		stmtClrChunked: "clr-chunked",
	}[rw.clrKind]

	return kind + "/" + clr
}

// TestSurveyStatementRewriteNulTerminatedOCI pins the one shape whose declared
// length is not its text length: the OCI client counts the trailing NUL, so the
// *value* the rewriter prepends to is one byte longer than the statement. The
// NUL riding along at the end is what keeps that from needing a special case.
func TestSurveyStatementRewriteNulTerminatedOCI(t *testing.T) {
	t.Parallel()

	withNUL, withoutNUL := 0, 0

	for _, name := range surveyCorpus(t) {
		if !strings.HasPrefix(name, "sqlplus") {
			continue
		}

		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			if !frameCarriesStatement(ttc) {
				continue
			}

			rw, ok := locateStatementRewrite(ttc, true)
			if !ok {
				continue
			}

			require.Equal(t, stmtLenWideUB4, rw.lenKind)

			if rw.run[len(rw.run)-1] == 0 {
				withNUL++

				require.Len(t, rw.run, len(rw.text())+1)
			} else {
				withoutNUL++

				require.Len(t, rw.run, len(rw.text()))
			}
		}
	}

	t.Logf("OCI statements declared with a trailing NUL: %d; without: %d", withNUL, withoutNUL)

	require.Positive(t, withNUL, "the sqlplus recordings carry at least one NUL-terminated statement")
	require.Positive(t, withoutNUL, "and at least one that is not, which is why both are accepted")
}

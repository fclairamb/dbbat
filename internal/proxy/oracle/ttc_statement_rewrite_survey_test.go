package oracle

import (
	"encoding/binary"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
		if !frameCarriesStatement(ttc, false) {
			continue
		}

		v.frames++

		// bigChunks is irrelevant to locating and to every value under the CLR
		// short-form limit; it only decides the encoding of a value that has to
		// *become* long. Both settings are exercised below.
		rw, ok := locateStatementRewrite(ttc, true, false)
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
			if !frameCarriesStatement(ttc, false) {
				continue
			}

			rw, ok := locateStatementRewrite(ttc, true, false)
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
		stmtLenWide64UB8:  "wide64-ub8",
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
			if !frameCarriesStatement(ttc, false) {
				continue
			}

			rw, ok := locateStatementRewrite(ttc, true, false)
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

// --- the 64-bit OCI dialect ---------------------------------------------------
//
// The corpus above is pcapng recordings, and none of them is a 64-bit OCI
// session: that client's frames live as hex fixtures instead, recorded through
// dbbat because a bare relay never decodes them (ociFixtureProvenance). So the
// fourth length encoding gets the same two properties, measured the same way,
// against testdata/oci64_parse_execs.hex.

// wide64StatementFrames returns the recorded 64-bit OCI parses as TTC payloads.
func wide64StatementFrames(t *testing.T) [][]byte {
	t.Helper()

	frames := recordedFrames(t, oci64ParseExecs)
	out := make([][]byte, 0, len(frames))

	for i, payload := range frames {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		out = append(out, ttc)
	}

	require.Len(t, out, 3, "the fixture holds three recorded parses")

	return out
}

// TestSurveyStatementRewriteWide64OCI is the survey's two properties on the
// dialect CI runs: every recorded parse locates, rewriting it to itself
// reproduces the client's own bytes, and rewriting it to a tagged statement
// produces a frame the same reading walks back to the tagged text.
//
// The length field is checked by hand as well as by round trip, because its one
// difference from the 4-byte header is invisible to a round trip that only ever
// compares dbbat to itself: **the value is the plain byte count**, so a reading
// that kept the `sqlLen * 3` convention would declare three times the statement
// and re-encode it consistently while the server read a third of it.
func TestSurveyStatementRewriteWide64OCI(t *testing.T) {
	t.Parallel()

	const statement = "BEGIN dbbat_cap_refcur(:rc); END;"

	for i, ttc := range wide64StatementFrames(t) {
		require.Truef(t, frameCarriesStatement(ttc, true), "frame %d must be seen as statement-carrying", i)

		rw, ok := locateStatementRewrite(ttc, true, true)
		require.Truef(t, ok, "frame %d must locate exactly on a 64-bit OCI session", i)

		assert.Equal(t, stmtLenWide64UB8, rw.lenKind, "frame %d", i)
		assert.Equal(t, stmtClrShort, rw.clrKind, "frame %d: the text carries a CLR short prefix", i)
		assert.Equal(t, execWide64SQLLenWidth, rw.lenWidth, "frame %d", i)
		assert.Equal(t, statement, rw.text(), "frame %d", i)

		// The plain byte count, at the offset execWide64SQLLenAt names, in the
		// frame's own bytes — not through any of the code under test.
		base := rw.lenAt - execWide64SQLLenAt
		assert.Equalf(t, uint64(len(statement)),
			binary.LittleEndian.Uint64(ttc[rw.lenAt:rw.lenAt+execWide64SQLLenWidth]),
			"frame %d declares the plain byte count, not %d×it", i, wideCharWidth)
		assert.Equalf(t, byte(len(statement)), ttc[rw.valueAt],
			"frame %d repeats it as the CLR short prefix", i)
		assert.Equalf(t, closeCursorsWideSentinel,
			ttc[base+execWide64SentinelAt:base+execWide64SentinelAt+len(closeCursorsWideSentinel)],
			"frame %d must carry the pointer sentinel in front of the length", i)

		// Identity: the model reproduces the frame.
		assert.Equalf(t, ttc, rw.apply(ttc, rw.run, true),
			"frame %d must rewrite to itself byte for byte", i)

		// Round trip: the tagged frame reads back as the tagged statement, with
		// the declared length agreeing.
		tagged := append([]byte(surveyTagPrefix), rw.run...)
		out := rw.apply(ttc, tagged, true)

		back, ok := locateStatementRewrite(out, true, true)
		require.Truef(t, ok, "frame %d: the tagged frame must locate again", i)
		assert.Equalf(t, surveyTagPrefix+statement, back.text(), "frame %d", i)
		assert.Lenf(t, back.run, len(tagged), "frame %d: the located run is the tagged one", i)
		assert.Equalf(t, uint64(len(tagged)),
			binary.LittleEndian.Uint64(out[back.lenAt:back.lenAt+execWide64SQLLenWidth]),
			"frame %d: the rewritten header must declare the tagged byte count", i)

		// The field is fixed-width, so nothing behind it moved by more than the
		// statement itself grew.
		assert.Lenf(t, out, len(ttc)+len(surveyTagPrefix), "frame %d", i)
	}
}

// TestWide64StatementRewriteIsSelectedByTheSessionNotByTheBytes pins the rule
// step 3 of the spec that added this is about: the dialect comes from what the
// session learned about its client, and each reading answers only for its own.
//
// Both directions are asserted, because both are failure modes. A 64-bit session
// whose frames were offered the 4-byte or thin walk is how a run of zeros becomes
// a length; a 4-byte or thin session whose frames were offered the 64-bit walk
// would have dbbat overwrite eight bytes of somebody else's header.
func TestWide64StatementRewriteIsSelectedByTheSessionNotByTheBytes(t *testing.T) {
	t.Parallel()

	for i, ttc := range wide64StatementFrames(t) {
		_, ok := locateStatementRewrite(ttc, true, false)
		assert.Falsef(t, ok,
			"frame %d: a 64-bit parse must not be locatable by the other dialects' readings", i)
	}

	refusedAsWide64 := 0

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			if !frameCarriesStatement(ttc, false) {
				continue
			}

			if _, ok := locateStatementRewrite(ttc, true, false); !ok {
				continue
			}

			_, ok := locateStatementRewrite(ttc, true, true)
			require.Falsef(t, ok,
				"%s: a frame of another dialect must be refused outright when the session is flagged "+
					"64-bit, never rewritten through the wrong header", name)

			refusedAsWide64++
		}
	}

	t.Logf("frames of the other dialects refused under the 64-bit reading: %d", refusedAsWide64)
	require.Positive(t, refusedAsWide64, "the corpus must carry frames to check this against")
}

// TestWide64DrivesCarryNoStatementToTag is the negative half of the per-session
// verdict on this dialect: a `PRINT rc` drive declares no statement, so it must
// not take part in the decision at all. Reading one as a statement frame the
// locator cannot certify is what would leave a whole sqlplus session untagged
// because of a frame that never had a statement in it.
func TestWide64DrivesCarryNoStatementToTag(t *testing.T) {
	t.Parallel()

	for i, payload := range recordedFrames(t, oci64RefCursorDrives) {
		ttc := extractTTCPayload(payload)
		require.NotEmptyf(t, ttc, "frame %d must carry a TTC message", i)

		assert.Falsef(t, frameCarriesStatement(ttc, true),
			"drive %d declares no statement, so it is not a frame to tag", i)
	}
}

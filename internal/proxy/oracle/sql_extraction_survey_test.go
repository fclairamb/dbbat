package oracle

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// This file is the measurement specs/todos/2026-08-13-05 asks for, kept as a
// test so it can be re-run against a regenerated corpus. It mostly *logs*
// rather than asserts: the numbers are the deliverable, and pinning a
// distribution would turn "somebody re-recorded a fixture" into a failure. The
// two properties that must not regress are asserted, and say so.
//
// Run it with -v to read the report:
//
//	go test ./internal/proxy/oracle/ -run TestSurvey -v

// --- corpus plumbing --------------------------------------------------------

func surveyCorpus(t *testing.T) []string {
	t.Helper()

	entries, err := filepath.Glob(filepath.Join("testdata", "*.pcapng"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Base(e))
	}

	sort.Strings(out)

	return out
}

// surveyClientTTC returns the TTC payload of every client->server Data packet.
func surveyClientTTC(t *testing.T, td *testDump) [][]byte {
	t.Helper()

	var out [][]byte

	for _, dpkt := range td.Packets {
		if dpkt.Direction != dump.DirClientToServer {
			continue
		}

		pkt, err := parseTNSFromDumpPacket(dpkt.Data)
		if err != nil || pkt == nil {
			continue
		}

		if pkt.Type != TNSPacketTypeData || len(pkt.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		ttc := extractTTCPayload(pkt.Payload)
		if ttc == nil {
			continue
		}

		out = append(out, ttc)
	}

	return out
}

// surveyExecOps returns every piggyback-exec op body in a frame: the frame
// itself when it opens with one, plus the one stapled behind a close list.
func surveyExecOps(ttc []byte) [][]byte {
	var out [][]byte

	if isPiggybackExecHeader(ttc) {
		out = append(out, ttc)
	}

	if end, ok := closeCursorsEnd(ttc); ok && isPiggybackExecHeader(ttc[end:]) {
		out = append(out, ttc[end:])
	}

	return out
}

// --- the report -------------------------------------------------------------

// TestSurveyStatementOpShapes answers the spec's first question: which op
// shapes actually carry SQL, and where does that SQL sit relative to the op
// header.
func TestSurveyStatementOpShapes(t *testing.T) {
	t.Parallel()

	kinds := map[string]int{}
	withSQL := map[string]int{}
	stapledBehindClose := 0

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			for _, at := range statementOpOffsets(ttc) {
				kind := fmt.Sprintf("%02x/%02x", ttc[at], ttc[at+1])
				kinds[kind]++

				if sql, ok := decodeExecStatement(ttc[at:]); ok && sql != "" {
					withSQL[kind]++
				}
			}

			if end, ok := closeCursorsEnd(ttc); ok && isPiggybackExecHeader(ttc[end:]) {
				stapledBehindClose++
			}
		}
	}

	t.Logf("statement-carrying op headers seen, by kind: %v", kinds)
	t.Logf("  ... of which a precise decode reads a statement out of: %v", withSQL)
	t.Logf("frames that staple an exec behind a close list: %d", stapledBehindClose)

	// The python exec sub-op is in dbbat's anchor list but no recording carries
	// it: python-oracledb thin sends the piggyback exec like everyone else.
	require.Zero(t, kinds["11/98"], "no recording should carry an 11/98 exec")
}

// TestSurveyPreciseDecodeCoverage is the measurement that decided the fix: for
// every exec op in the corpus, does the header-anchored length-prefixed decode
// find the statement, and does the old scan agree with it.
func TestSurveyPreciseDecodeCoverage(t *testing.T) {
	t.Parallel()

	var (
		ops, precise, legacyOnly, neither  int
		agree, legacyFragment, legacyOther int
	)

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			for _, body := range surveyExecOps(ttc) {
				ops++

				exact, ok := decodeExecStatement(body)
				legacy := legacyExecScan(body)

				switch {
				case ok:
					precise++
				case legacy != "":
					legacyOnly++

					t.Logf("PRECISE MISS %s: legacy scan says %q", name, truncateSQL(legacy, 60))
				default:
					neither++
				}

				if !ok || legacy == "" {
					continue
				}

				switch {
				case strings.TrimSpace(legacy) == strings.TrimSpace(exact):
					agree++
				case surveyIsFragmentOf(legacy, exact):
					legacyFragment++
				default:
					legacyOther++

					t.Logf("NEITHER %s\n    legacy = %q\n    exact  = %q",
						name, truncateSQL(legacy, 60), truncateSQL(exact, 60))
				}
			}
		}
	}

	t.Logf("=== precise decode vs the legacy window+keyword scan ===")
	t.Logf("exec ops in the corpus:              %d", ops)
	t.Logf("  precise (header length) decode:    %d", precise)
	t.Logf("  legacy scan only:                  %d", legacyOnly)
	t.Logf("  neither:                           %d", neither)
	t.Logf("where both spoke, the legacy scan:")
	t.Logf("  agreed:                            %d", agree)
	t.Logf("  returned a mid-statement FRAGMENT: %d", legacyFragment)
	t.Logf("  returned something else entirely:  %d", legacyOther)

	require.Zero(t, neither, "every exec op in the corpus must yield a statement")
	require.Zero(t, legacyOnly, "the precise decode must cover every exec op the legacy scan finds")
}

// surveyIsFragmentOf reports whether legacy is a slice taken out of the middle
// of exact. Compared on the leading printable prefix, because the legacy scan
// routinely ran past the statement into the TTC framing behind it, so an exact
// substring test would call an obvious fragment "something else".
func surveyIsFragmentOf(legacy, exact string) bool {
	head := strings.TrimSpace(legacy)
	for i := range len(head) {
		if head[i] < 0x20 || head[i] > 0x7e {
			head = head[:i]

			break
		}
	}

	if len(head) < 8 {
		return false
	}

	return strings.Contains(exact, head)
}

// legacyExecScan is decodeExecSQL's pre-fix strategy, kept here so the report
// can quantify what it used to return. It is not called by production code.
func legacyExecScan(body []byte) string {
	for offset := 50; offset <= 75 && offset < len(body)-1; offset++ {
		sqlLen, read, err := decodeVarLen(body[offset:])
		if err != nil || sqlLen == 0 || sqlLen > 32768 {
			continue
		}

		start, end := offset+read, offset+read+int(sqlLen)
		if end > len(body) {
			continue
		}

		if text := string(body[start:end]); legacyLooksLikeSQL(text) {
			return text
		}
	}

	return legacyFindSQLInPayload(body)
}

// legacyLooksLikeSQL is looksLikeSQL as it was before the word-boundary fix:
// a bare prefix match, which is what made GRANTED_ROLE read as GRANT.
func legacyLooksLikeSQL(s string) bool {
	if len(s) < 2 {
		return false
	}

	upper := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range sqlStatementVerbs {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}

	return false
}

// legacyFindSQLInPayload is findSQLInPayload's pre-fix keyword list, without
// TRUNCATE/GRANT/REVOKE and without the word-boundary requirement.
func legacyFindSQLInPayload(payload []byte) string {
	keywords := [][]byte{
		[]byte("SELECT"), []byte("INSERT"), []byte("UPDATE"), []byte("DELETE"),
		[]byte("CREATE"), []byte("DROP"), []byte("ALTER"), []byte("BEGIN"),
		[]byte("DECLARE"), []byte("WITH"), []byte("MERGE"), []byte("CALL"),
	}

	idx := -1

	for i := range payload {
		for _, kw := range keywords {
			if i+len(kw) <= len(payload) && equalFoldASCIIBytes(payload[i:i+len(kw)], kw) {
				idx = i

				break
			}
		}

		if idx >= 0 {
			break
		}
	}

	if idx < 0 {
		return ""
	}

	end := idx
	for end < len(payload) && payload[end] >= 0x0A && payload[end] <= 0x7E {
		end++
	}

	if end > idx+2 {
		return strings.TrimSpace(string(payload[idx:end]))
	}

	return ""
}

// TestSurveyUnnameableReexecution counts the shape the spec's resolved open
// question turns on: a frame dbbat cannot name that is also a cursor
// re-execution. A measured zero is the deliverable — see docs/oracle.md.
func TestSurveyUnnameableReexecution(t *testing.T) {
	t.Parallel()

	var frames, unnameable, reexec, both int

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			frames++

			_, named := clientCallNumber(ttc)
			isReexec := surveyIsCursorReexec(ttc)

			if !named {
				unnameable++
			}

			if isReexec {
				reexec++
			}

			if !named && isReexec {
				both++

				t.Logf("%s: unnameable re-execution: % x", name, ttc[:min(24, len(ttc))])
			}
		}
	}

	t.Logf("client TTC frames=%d unnameable=%d cursor-re-executions=%d BOTH=%d",
		frames, unnameable, reexec, both)

	require.Zero(t, both,
		"no recording carries a frame that is both unnameable and a cursor re-execution; "+
			"a non-zero count reopens item 3 of specs/todos/2026-08-13-05 and needs an owner decision")
}

func surveyIsCursorReexec(ttc []byte) bool {
	if _, err := decodeCursorReexec(ttc); err == nil {
		return true
	}

	if len(ttc) > 0 && TTCFunctionCode(ttc[0]) == TTCFuncOALL8 {
		if _, err := decodeOALL8(ttc); err != nil {
			if _, isNoSQL := err.(*OALL8NoSQLError); isNoSQL { //nolint:errorlint // the exact type is the question
				return true
			}
		}
	}

	return false
}

// TestSurveyStapledOALL8 settles item 4: is a legacy OALL8 (message type 0x0e)
// ever stapled behind a piggyback in a position where it decodes as one?
func TestSurveyStapledOALL8(t *testing.T) {
	t.Parallel()

	var byteHits, decodable int

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			if len(ttc) == 0 || TTCFunctionCode(ttc[0]) != TTCFuncOFETCH {
				continue
			}

			for i := 1; i < len(ttc); i++ {
				if TTCFunctionCode(ttc[i]) != TTCFuncOALL8 {
					continue
				}

				byteHits++

				result, err := decodeOALL8(ttc[i:])
				if err == nil && result != nil && looksLikeSQL(result.SQL) {
					decodable++

					t.Logf("%s: 0x0e at offset %d decodes to %q", name, i, truncateSQL(result.SQL, 60))
				}
			}
		}
	}

	t.Logf("0x0e bytes at a non-zero offset inside a 0x11 piggyback: %d", byteHits)
	t.Logf("  ... of which decode as an OALL8 carrying plausible SQL: %d", decodable)

	require.Zero(t, decodable,
		"no recorded piggyback staples a decodable legacy OALL8; adding 0x0e to the anchor "+
			"list would buy nothing and cost the false positives anchoring removed")
}

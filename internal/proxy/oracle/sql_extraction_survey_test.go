package oracle

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// This file is the measurement the 2026-08-13-05 spec asks for, kept as a
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

				if sql, ok := decodeExecStatement(ttc[at:], false); ok && sql != "" {
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
		reexecs                            int
	)

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			for _, body := range surveyExecOps(ttc) {
				// An exec op that declares no statement carries none to find:
				// it re-runs a cursor already parsed (ojdbc6, packet #20 of
				// ojdbc6_legacy.pcapng). Counting it here would demand a
				// statement out of a frame that has none, which is exactly the
				// misreading that let it travel upstream ungated.
				if _, reexec := execNoStatementCursor(body, false); reexec {
					reexecs++

					continue
				}

				ops++

				exact, ok := decodeExecStatement(body, false)
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
	t.Logf("  (plus SQL-less re-executions:      %d, no statement to find)", reexecs)
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

// TestSurveyAlterSessionMisreadAsSet computes the sub-count the docs quote, so
// the figure is measured rather than remembered: how many recorded executes
// carry an ALTER SESSION that the pre-fix scan handed to the gate as a bare
// SET — a statement read_only and block_ddl refuse, arriving as one they do not.
func TestSurveyAlterSessionMisreadAsSet(t *testing.T) {
	t.Parallel()

	misread := 0
	byFile := map[string]int{}
	distinct := map[string]struct{}{}

	for _, name := range surveyCorpus(t) {
		td := loadTestDump(t, name)

		for _, ttc := range surveyClientTTC(t, td) {
			for _, body := range surveyExecOps(ttc) {
				exact, ok := decodeExecStatement(body, false)
				if !ok || !strings.HasPrefix(strings.ToUpper(exact), "ALTER SESSION") {
					continue
				}

				distinct[exact] = struct{}{}

				if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(legacyExecScan(body))), "SET") {
					misread++
					byFile[name]++
				}
			}
		}
	}

	t.Logf("ALTER SESSION executes the old scan handed over as a bare SET: %d", misread)
	t.Logf("  by recording: %v", byFile)
	t.Logf("  distinct ALTER SESSION statements in the corpus: %d", len(distinct))

	require.Equal(t, 9, misread,
		"the figure docs/oracle.md quotes; recompute and update both if the corpus is re-recorded")
	require.Len(t, distinct, 5, "and the count of distinct statements behind it")
	require.Equal(t, map[string]int{"dbeaver.pcapng": 5, "dbeaver_init.pcapng": 4}, byFile,
		"every one of them is DBeaver's connection setup; no other client in the corpus sends an "+
			"ALTER SESSION *as an execute op* — the string is in every recording as the "+
			"AUTH_ALTER_SESSION key/value of the phase-2 AUTH message (func 0x03 sub-op 0x73), "+
			"which is not a statement-carrying op and never reaches the gate")

	// The five, spelled out, and each one checked against the very allowlist the
	// gate consults. This is the floor of shared.IsAllowedAlterSession: if a
	// re-recording adds an ALTER SESSION parameter that is not on the list, that
	// is a client which can no longer *connect* under a read_only or block_ddl
	// grant, and it fails here rather than in support.
	require.Equal(t, []string{
		`ALTER SESSION SET "_optimizer_cost_based_transformation" = 'OFF'`,
		`ALTER SESSION SET "_optimizer_push_pred_cost_based" = FALSE`,
		`ALTER SESSION SET "_optimizer_squ_bottomup" = FALSE`,
		"ALTER SESSION SET CURRENT_SCHEMA=TESTADM",
		"ALTER SESSION SET OPTIMIZER_FEATURES_ENABLE='10.2.0.5'",
	}, sortedKeys(distinct), "the distinct ALTER SESSION statements the corpus carries")

	for sql := range distinct {
		require.True(t, shared.IsAllowedAlterSession(sql),
			"every ALTER SESSION a real client sends at connection setup must be on the allowlist "+
				"(internal/proxy/shared/validation.go), or a read_only grant refuses the connection "+
				"itself: %s", sql)
	}
}

// sortedKeys returns a set's members in a stable order.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
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
			"a non-zero count reopens item 3 of the 2026-08-13-05 spec and needs an owner decision")
}

func surveyIsCursorReexec(ttc []byte) bool {
	if _, err := decodeCursorReexec(ttc); err == nil {
		return true
	}

	// The third shape: an execute op declaring a zero-length statement, which is
	// how ojdbc6 re-runs a PreparedStatement. See execNoStatementCursor.
	if _, ok := execNoStatementCursor(ttc, false); ok {
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

// --- the 64-bit OCI dialect ---------------------------------------------------
//
// The survey above is the pcapng corpus, none of which is a 64-bit OCI session.
// That dialect's exec header had its own reading (execSQLLengthWide64Field) for
// as long as the statement tag existed, but only the *rewriter* used it: the
// gate's own decode took no dialect, refused the header, and fell through to the
// offset window and the keyword scan — the mechanism this whole file exists to
// measure the cost of. This half is the measurement that decided whether closing
// that was worth a hot-path change, and the answer is in
// TestSurveyWide64ScanReadsAPrefixPastTheShortCLRForm.
//
// Its long statements are encoded by dbbat's own rewriter rather than by a
// client, for the reason wide64DerivedFrames spells out. The live version, where
// sqlplus writes every byte itself, is
// TestIntegration_OCILongStatementIsRecordedWhole.

// wide64ScanReading is decodePiggybackExecSQL's fallback, and only its fallback:
// the 40-70 offset window followed by the keyword scan. It is what a 64-bit OCI
// session's statements were read by before the decode learned the dialect, kept
// here for the same reason legacyExecScan is — the report has to be able to say
// what the old mechanism returned.
func wide64ScanReading(ttcPayload []byte) (string, bool) {
	var stmt execStatement

	for offset := 40; stmt.Text == "" && offset < 70 && offset < len(ttcPayload)-1; offset++ {
		if found, err := extractSQLAtOffsetText(ttcPayload, offset); err == nil && found.Text != "" {
			stmt = found
		}
	}

	if stmt.Text != "" {
		// The window has no length to check its run against, so it never reports
		// a prefix as one.
		return stmt.Text, false
	}

	if found, cut := findSQLInPayload(ttcPayload); found != "" {
		return found, cut
	}

	return "", false
}

// wide64SurveyStatements is the statement set the derived corpus carries. It is
// chosen around the one boundary the scan cannot see — **253 bytes**, measured
// by bisection: at or below it the scan reads the statement whole, from it on
// the scan keeps exactly 252 bytes and says nothing — plus the three shapes
// whose misreading on the 4-byte corpus is what made this decode exist at all
// (TestSurveyAlterSessionMisreadAsSet).
func wide64SurveyStatements() []string {
	return []string{
		"BEGIN dbbat_cap_refcur(:rc); END;",
		"SELECT 1 FROM dual",
		"ALTER SESSION SET CURRENT_SCHEMA=TESTADM",
		"UPDATE emp SET name = 'x' WHERE id = 1",
		"SELECT * FROM dba_role_privs WHERE GRANTED_ROLE='DBA'",
		"SELECT COUNT(*) FROM user_tables",
		// 252 bytes: the last size the scan reads whole.
		"SELECT " + strings.Repeat("a", 235) + " FROM dual",
		// 253: the first it does not.
		"SELECT " + strings.Repeat("b", 236) + " FROM dual",
		"UPDATE emp SET " + strings.Repeat("x", 400) + " = 1",
		// Past one chunk, so the statement does not even sit contiguously.
		"SELECT " + strings.Repeat("c", 40000) + " FROM dual",
	}
}

// wide64DerivedFrames returns the recorded 64-bit parses with their statement
// rewritten to sql.
//
// Deriving rather than recording is sound here for exactly one reason, and it is
// measured rather than assumed: TestSurveyStatementRewriteWide64OCI shows this
// rewriter reproduces each recorded frame's own bytes byte for byte when it
// writes the statement back unchanged. A model that reproduces the client's
// frame is a model that can stand in for the client on the same header with
// different text. What it cannot vouch for is the CLR *long* form on this
// dialect — no recording carries a 64-bit statement past 251 bytes — so the
// finding below is about what the scan does with a length prefix it cannot read
// as a single byte, whichever long form the client picks.
func wide64DerivedFrames(t *testing.T, sql string) [][]byte {
	t.Helper()

	bases := wide64StatementFrames(t)
	out := make([][]byte, 0, len(bases))

	for i, base := range bases {
		rw, ok := locateStatementRewrite(base, true, true)
		require.Truef(t, ok, "frame %d must locate", i)

		out = append(out, rw.apply(base, []byte(sql), true))
	}

	return out
}

// TestSurveyWide64RecordedFramesAreReadWhole is the first half of the
// before/after, on the bytes a real 64-bit sqlplus session actually wrote.
//
// It is a **negative** result and it is reported as one: all three recorded
// parses carry the same 33-byte `BEGIN dbbat_cap_refcur(:rc); END;`, and the
// scan reads every one of them whole. On this corpus alone the answer would be
// "no gap, do not ship a hot-path change" — which is why the corpus is widened
// below rather than concluded from.
func TestSurveyWide64RecordedFramesAreReadWhole(t *testing.T) {
	t.Parallel()

	const recorded = "BEGIN dbbat_cap_refcur(:rc); END;"

	var scanWhole, headerWhole, headerBefore int

	frames := wide64StatementFrames(t)

	for i, ttc := range frames {
		// What the decode did before it was told the dialect: nothing. The
		// 4-byte and thin walks both refuse this header, which is the whole
		// reason the scan was running.
		if _, ok := decodeExecStatementText(ttc, false); ok {
			headerBefore++
		}

		if stmt, ok := decodeExecStatementText(ttc, true); ok && stmt.Text == recorded {
			headerWhole++
		}

		scanned, _ := wide64ScanReading(ttc)
		if scanned == recorded {
			scanWhole++
		} else {
			t.Logf("frame %d: the scan read %q", i, truncateSQL(scanned, 60))
		}
	}

	t.Logf("=== testdata/%s: %d recorded parses, one distinct statement ===",
		filepath.Base(oci64ParseExecs), len(frames))
	t.Logf("  read whole by the window+keyword scan:      %d", scanWhole)
	t.Logf("  read whole by the header-anchored decode:   %d", headerWhole)
	t.Logf("  read at all by the decode before it knew the dialect: %d", headerBefore)

	require.Equal(t, len(frames), scanWhole,
		"the recorded 64-bit corpus is read whole by the scan; the gap this spec closes is not "+
			"visible on it, and TestSurveyWide64ScanReadsAPrefixPastTheShortCLRForm is where it is")
	require.Equal(t, len(frames), headerWhole, "and read whole by the decode that knows the dialect")
	require.Zero(t, headerBefore,
		"before the dialect was threaded down, the decode refused every one of these frames — "+
			"which is what left the scan in charge of the gate on this client family")
}

// TestSurveyWide64ScanReadsAPrefixPastTheShortCLRForm is the measurement that
// decided the change, and the gap it finds is silent rather than noisy.
//
// On this dialect the statement's length prefix sits at a fixed offset the 40-70
// window happens to cover, so as long as that prefix reads as one byte — the CLR
// short form — the scan lands on it and takes the run it names. That holds up to
// 252 bytes. From **253** on it reads the long form's first bytes as a length
// instead and hands the gate exactly 252 bytes, with `Truncated` unset, because
// a window scan has no declared length to check its run against and so cannot
// tell a prefix from a statement. The boundary is measured by bisection, not
// derived. The header-anchored decode reads every one of them whole.
//
// A prefix is not a cosmetic misreading. It is what read_only, block_ddl, the
// approval patterns and ValidateOracleQuery are evaluated against, and it is
// what the `queries` row stores — so a `MERGE` whose write clause sits past byte
// 252 was gated on its first 252 bytes and recorded as them.
func TestSurveyWide64ScanReadsAPrefixPastTheShortCLRForm(t *testing.T) {
	t.Parallel()

	var (
		frames                          int
		scanWhole, scanPrefix, scanMiss int
		headerWhole, headerMiss         int
		silentPrefixes                  int
	)

	for _, sql := range wide64SurveyStatements() {
		for i, frame := range wide64DerivedFrames(t, sql) {
			frames++

			stmt, ok := decodeExecStatementText(frame, true)
			if ok && stmt.Text == sql {
				headerWhole++
			} else {
				headerMiss++

				t.Logf("HEADER MISS (%d bytes, frame %d): ok=%v got %q", len(sql), i, ok, truncateSQL(stmt.Text, 60))
			}

			scanned, flagged := wide64ScanReading(frame)

			switch {
			case scanned == sql:
				scanWhole++
			case scanned != "" && strings.HasPrefix(sql, scanned):
				scanPrefix++

				if !flagged {
					silentPrefixes++
				}

				if i == 0 {
					t.Logf("SCAN PREFIX (%d bytes): %d bytes kept, truncation flagged=%v",
						len(sql), len(scanned), flagged)
				}
			default:
				scanMiss++

				if i == 0 {
					t.Logf("SCAN OTHER (%d bytes): %q", len(sql), truncateSQL(scanned, 60))
				}
			}
		}
	}

	t.Logf("=== 64-bit OCI exec frames, recorded headers with %d statements each ===",
		len(wide64SurveyStatements()))
	t.Logf("frames measured:                         %d", frames)
	t.Logf("the window+keyword scan (before) read:")
	t.Logf("  the whole statement:                   %d", scanWhole)
	t.Logf("  a prefix of it:                        %d", scanPrefix)
	t.Logf("    ... of which silently, Truncated unset: %d", silentPrefixes)
	t.Logf("  something else / nothing:              %d", scanMiss)
	t.Logf("the header-anchored decode (after) read:")
	t.Logf("  the whole statement:                   %d", headerWhole)
	t.Logf("  anything short of it:                  %d", headerMiss)

	require.Zero(t, headerMiss,
		"the header-anchored decode must read every one of these frames whole; that is the "+
			"property the change buys")
	require.Positive(t, scanPrefix,
		"the gap this spec closes: the scan hands the gate a prefix once the statement outgrows "+
			"the CLR short form. A zero here means the corpus stopped exercising the boundary, "+
			"not that the scan became precise")
	require.Equal(t, scanPrefix, silentPrefixes,
		"and every one of those prefixes is silent — the scan reports no truncation, so nothing "+
			"downstream can tell a gated prefix from a gated statement")
}

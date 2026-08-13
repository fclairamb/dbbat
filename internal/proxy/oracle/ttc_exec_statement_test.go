package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

// recordedExec finds the first client frame in a recording whose extracted
// statement contains want, and returns what the production path extracts.
func recordedExec(t *testing.T, file, want string) string {
	t.Helper()

	td := loadTestDump(t, file)

	for _, ttc := range surveyClientTTC(t, td) {
		if !strings.Contains(strings.ToUpper(string(ttc)), strings.ToUpper(want)) {
			continue
		}

		for _, at := range statementOpOffsets(ttc) {
			result, err := decodeExecSQL(ttc[at:])
			if err == nil && result != nil && strings.Contains(strings.ToUpper(result.SQL), strings.ToUpper(want)) {
				return result.SQL
			}
		}
	}

	t.Fatalf("no recorded frame in %s carries %q", file, want)

	return ""
}

// TestRecordedStatementsAreNotFragments pins the three readings that made this
// a security defect rather than a cosmetic one. Each is a real frame from a
// real client, and each used to reach the gate as a fragment that the controls
// it should have tripped do not recognise.
func TestRecordedStatementsAreNotFragments(t *testing.T) {
	t.Parallel()

	t.Run("ALTER SESSION is not read as SET", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "dbeaver.pcapng", "CURRENT_SCHEMA")

		assert.Equal(t, "ALTER SESSION SET CURRENT_SCHEMA=TESTADM", sql)
		assert.True(t, shared.IsWriteQuery(sql), "read_only has to see the ALTER")
		assert.True(t, shared.IsDDLQuery(sql), "and so does block_ddl")
	})

	t.Run("UPDATE is not read as SET", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "go_ora_dml.pcapng", "dbbat_dml_test SET")

		assert.Equal(t, "UPDATE dbbat_dml_test SET name = 'updated' WHERE id <= 3", sql)
		assert.True(t, shared.IsWriteQuery(sql), "read_only has to see the UPDATE")
	})

	t.Run("GRANTED_ROLE is not read as GRANT", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "dbeaver_init.pcapng", "GRANTED_ROLE")

		assert.Equal(t, "SELECT 'YES' FROM USER_ROLE_PRIVS WHERE GRANTED_ROLE='DBA'", sql)
		assert.False(t, shared.IsWriteQuery(sql), "a SELECT is not a GRANT")
	})
}

// TestExecStatementLengthSources pins both header encodings the decode reads
// the statement length out of, against the recordings they were measured on.
func TestExecStatementLengthSources(t *testing.T) {
	t.Parallel()

	t.Run("thin", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "python_thin.pcapng", "COUNT(*)")
		assert.Equal(t, "SELECT COUNT(*) FROM user_tables", sql,
			"a statement whose length byte is itself printable must not slide by one")
	})

	t.Run("OCI wide", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "sqlplus_cursor_reexec.pcapng", "SELECT 1 AS n")
		assert.Equal(t, "SELECT 1 AS n FROM dual", sql)
	})

	t.Run("OCI wide with the trailing NUL counted in the length", func(t *testing.T) {
		t.Parallel()

		sql := recordedExec(t, "sqlplus_cursor_reexec.pcapng", "XS_SYS_CONTEXT")
		assert.True(t, strings.HasSuffix(sql, "FROM SYS.DUAL"),
			"the terminator is not statement text: %q", sql)
		assert.NotContains(t, sql, "\x00")
	})
}

// TestFindSQLInPayloadWordBoundary pins the half of the fix that *narrows* the
// last-resort keyword scan. Widening its verb list (TRUNCATE/GRANT/REVOKE, the
// three the controls refuse but the scan could not see) is only safe because
// this landed with it.
func TestFindSQLInPayloadWordBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{"GRANTED_ROLE is an identifier", "\x00\x00GRANTED_ROLE = 'DBA'\x00", ""},
		{"DELETE_RULE is an identifier", "\x00\x00DELETE_RULE, POSITION\x00", ""},
		{"TRUNCATE is now seen", "\x00\x00TRUNCATE TABLE payroll\x00", "TRUNCATE TABLE payroll"},
		{"GRANT is now seen", "\x00\x00GRANT DBA TO scott\x00", "GRANT DBA TO scott"},
		{"REVOKE is now seen", "\x00\x00REVOKE DBA FROM scott\x00", "REVOKE DBA FROM scott"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, findSQLInPayload([]byte(tc.payload)))
		})
	}
}

// TestBundledOCIFixturesCarryNoStatement pins the false-positive behaviour on
// the binary fixtures, which is the cost side of every keyword the scan learns.
// None of these frames is an execute, so none of them may hand the gate a
// statement — on the unnameable path that would end a live session.
func TestBundledOCIFixturesCarryNoStatement(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"testdata/oci_bundled_first_call.hex",
		"testdata/oci_bundled_close_cursors.hex",
		"testdata/oci_bundled_auth_phase1.hex",
		"testdata/oci_bundled_oer.hex",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			for i, frame := range recordedFrames(t, path) {
				ttc := extractTTCPayload(frame)
				if ttc == nil {
					continue
				}

				require.Empty(t, stapledStatements(ttc),
					"%s frame %d must not read as a statement", path, i)
			}
		})
	}
}

// TestStapledStatementsGatesEveryExec pins the "gate every one of them"
// resolution: a frame that staples two executes must surrender both.
func TestStapledStatementsGatesEveryExec(t *testing.T) {
	t.Parallel()

	frame := append(buildPiggybackExec("SELECT 1 FROM dual"), buildPiggybackExec("DROP TABLE emp")...)

	assert.Equal(t, []string{"SELECT 1 FROM dual", "DROP TABLE emp"}, stapledStatements(frame))
}

// TestStapledStatementsDedupes keeps the recorded `11 69 <closes> 03 5e <exec>`
// shape from being gated twice for the one statement it runs.
func TestStapledStatementsDedupes(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "dbeaver.pcapng")

	for _, ttc := range surveyClientTTC(t, td) {
		if len(statementOpOffsets(ttc)) < 2 {
			continue
		}

		if got := stapledStatements(ttc); len(got) > 0 {
			assert.Len(t, got, 1, "one stapled execute is one statement: %v", got)

			return
		}
	}

	t.Fatal("no recorded frame carries two statement-op anchors")
}

// buildPiggybackExec assembles a thin-encoding piggyback execute carrying sql,
// in the layout execSQLLength walks.
func buildPiggybackExec(sql string) []byte {
	out := []byte{byte(TTCFuncPiggyback), PiggybackSubExecSQL, 0x00}
	out = append(out, 0x02, 0x81, 0x21) // options
	out = append(out, 0x00)             // cursor id 0
	out = append(out, 0x01)             // the cursor-id-is-zero flag
	out = append(out, 0x01, byte(len(sql)))
	out = append(out, 0x01, 0x01, 0x0d)
	out = append(out, make([]byte, 24)...)
	out = append(out, sql...)
	out = append(out, 0x00, 0x00)

	return out
}

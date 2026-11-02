package mssql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// TestLaterStatementOfBatchIsGated is the session-level regression test: the
// read_only and block_ddl controls must reach every statement of a SQLBatch,
// and of the batch an EXEC('…') runs, not just the leading one.
func TestLaterStatementOfBatchIsGated(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sql     string
		control string
		want    error
	}{
		{"SELECT 1; DELETE FROM t", store.ControlReadOnly, shared.ErrReadOnlyViolation},
		{"SELECT 1\nDELETE FROM t", store.ControlReadOnly, shared.ErrReadOnlyViolation},
		{"EXEC('SELECT 1 UPDATE t SET a = 1')", store.ControlReadOnly, shared.ErrReadOnlyViolation},
		{"SELECT 1\nDROP TABLE t", store.ControlBlockDDL, shared.ErrDDLBlocked},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			s := switchSession(t)
			s.grant = grantWithControls(tc.control)

			require.ErrorIs(t, s.validate(statement{text: tc.sql, enforce: []string{tc.sql}}), tc.want)
		})
	}
}

// TestMultiStatementReadBatchStillRuns is the positive control.
func TestMultiStatementReadBatchStillRuns(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"SET NOCOUNT ON; SELECT 1\nSELECT name FROM sys.tables",
		"SELECT a FROM t INNER MERGE JOIN u ON t.id = u.id; SELECT 'DROP TABLE t'",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			s := switchSession(t)
			s.grant = grantWithControls(store.ControlReadOnly, store.ControlBlockDDL)

			require.NoError(t, s.validate(statement{text: sql, enforce: []string{sql}}))
		})
	}
}

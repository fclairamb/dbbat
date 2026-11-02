package shared

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestValidateMSSQLBatch_RefusesLaterStatements is the regression test: a T-SQL
// batch needs no separator, so a write or DDL statement behind a read is part of
// the same SQLBatch, and the leading-keyword check never reads it.
func TestValidateMSSQLBatch_RefusesLaterStatements(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sql     string
		control string
		want    error
	}{
		{"SELECT 1; DELETE FROM t", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT 1\nUPDATE t SET a = 1", store.ControlReadOnly, ErrReadOnlyViolation},
		{"DECLARE @n int = 1 INSERT INTO t VALUES (@n)", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT 1 /* c */ MERGE INTO t USING s ON 1 = 1 WHEN MATCHED THEN DELETE;", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT 1;DROP TABLE t", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT 1 GRANT SELECT ON t TO public", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT 1; DROP TABLE t", store.ControlBlockDDL, ErrDDLBlocked},
		{"SELECT 1\ncreate table t (a int)", store.ControlBlockDDL, ErrDDLBlocked},
		{"SELECT 1; TRUNCATE TABLE t", store.ControlBlockDDL, ErrDDLBlocked},
		// A literal left open fails closed rather than hiding the rest.
		{"SELECT 'open", store.ControlReadOnly, ErrReadOnlyViolation},
		{"SELECT [open", store.ControlBlockDDL, ErrDDLBlocked},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, ValidateMSSQLBatch(tc.sql, controlGrant(tc.control)), tc.want)
		})
	}
}

// TestValidateMSSQLBatch_AllowsReads is the positive control: the keywords are
// reserved words, so every place a read can carry one is stepped over.
func TestValidateMSSQLBatch_AllowsReads(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"SELECT 1",
		"SELECT 'DELETE FROM t'",
		"SELECT N'x; DROP TABLE t'",
		`SELECT "delete" FROM t`,
		"SELECT [update], [drop table] FROM t",
		"SELECT t.delete FROM t",
		"DECLARE @update int = 1; SELECT @update",
		"SELECT * FROM #insert",
		"SELECT last_user_update, user_updates FROM sys.dm_db_index_usage_stats",
		"SELECT REPLACE(name, 'a', 'b') FROM t",
		"SELECT a FROM t INNER MERGE JOIN u ON t.id = u.id",
		"SELECT a FROM t JOIN u ON t.id = u.id OPTION (MERGE JOIN)",
		"SELECT a FROM t UNION SELECT a FROM u OPTION (MERGE UNION)",
		"SELECT 1 -- then DELETE FROM t",
		"SELECT 1 /* DROP TABLE t */",
		"SELECT inserted.a, deleted.a FROM t",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, ValidateMSSQLBatch(sql, controlGrant(store.ControlReadOnly, store.ControlBlockDDL)))
		})
	}
}

// TestValidateMSSQLBatch_FullWriteGrantUntouched: no control, no scan.
func TestValidateMSSQLBatch_FullWriteGrantUntouched(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateMSSQLBatch("SELECT 1; DELETE FROM t; DROP TABLE t", controlGrant()))
	require.NoError(t, ValidateMSSQLBatch("SELECT 1; DELETE FROM t", controlGrant(store.ControlBlockDDL)))
}

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

// TestValidateMSSQLBatch_BlocksPasswordChange pins SQL Server's password-change
// spellings, which carry no ALTER USER/ROLE prefix the shared classifier knows:
// refused whatever the grant, because every dbbat session shares the one
// upstream login. A full-write grant was the hole.
func TestValidateMSSQLBatch_BlocksPasswordChange(t *testing.T) {
	t.Parallel()

	grants := map[string]*store.Grant{
		"full write": controlGrant(),
		"read_only":  controlGrant(store.ControlReadOnly),
		"block_ddl":  controlGrant(store.ControlBlockDDL),
	}
	refused := []string{
		"ALTER LOGIN l WITH PASSWORD = 'x'",
		"ALTER LOGIN l WITH PASSWORD = 'x' OLD_PASSWORD = 'y'",
		"ALTER LOGIN [my login] WITH PASSWORD = 0x0200AB HASHED",
		"alter login l with password = 'x'",
		"ALTER LOGIN l WITH NAME = l2, PASSWORD = 'x'",
		"CREATE LOGIN l2 WITH PASSWORD = 'x'",
		"ALTER USER u WITH PASSWORD = 'x'",
		"CREATE USER u2 WITH PASSWORD = 'x'",
		"ALTER/**/LOGIN l WITH PASSWORD = 'x'",
		"ALTER LOGIN l /* c */ WITH -- c\n PASSWORD = 'x'",
		"ALTER\nLOGIN l\nWITH\nPASSWORD = 'x'",
		// The second statement of a batch, with and without a separator.
		"SELECT 1; ALTER LOGIN l WITH PASSWORD = 'x'",
		"SELECT 1\nALTER LOGIN l WITH PASSWORD = 'x'",
		"DECLARE @p sysname = N'x' ALTER LOGIN l WITH PASSWORD = @p",
		// A name left open while armed fails closed.
		"ALTER LOGIN [open WITH PASSWORD = 'x'",
	}

	for name, grant := range grants {
		for _, sql := range refused {
			require.ErrorIs(t, ValidateMSSQLBatch(sql, grant), ErrPasswordChangeBlocked, "%s: %s", name, sql)
		}
	}

	full := grants["full write"]
	for _, sql := range []string{
		"ALTER LOGIN l DISABLE",
		"ALTER LOGIN l ENABLE; SELECT 1",
		"ALTER USER u WITH DEFAULT_SCHEMA = s",
		"CREATE LOGIN l2 FROM WINDOWS",
		"CREATE USER u2 FROM LOGIN l2",
		"SELECT password FROM t",
		"SELECT [password] FROM users WHERE name = 'ALTER LOGIN'",
		"ALTER TABLE t ADD password nvarchar(100)",
		"ALTER LOGIN l DISABLE; SELECT password FROM t",
		"SELECT 'ALTER LOGIN l WITH PASSWORD = x'",
		"UPDATE t SET old_password = 'x'",
	} {
		require.NoError(t, ValidateMSSQLBatch(sql, full), "should allow: %s", sql)
	}

	// ALTER LOGIN without a password is still DDL: refused by the controls
	// alone, exactly as before.
	require.ErrorIs(t, ValidateMSSQLBatch("ALTER LOGIN l DISABLE", grants["read_only"]), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateMSSQLBatch("ALTER LOGIN l DISABLE", grants["block_ddl"]), ErrDDLBlocked)
}

// TestValidateMSSQLBatch_BlocksPasswordChangeInDynamicSQL pins the unwrapped
// payload: the SQL Server proxy runs ValidateMSSQLBatch over every statement
// MSSQLDynamicSQL lifts out of the batch, so sp_executesql cannot carry one.
func TestValidateMSSQLBatch_BlocksPasswordChangeInDynamicSQL(t *testing.T) {
	t.Parallel()

	full := controlGrant()

	for _, sql := range []string{
		"EXEC sp_executesql N'ALTER LOGIN l WITH PASSWORD = ''x'''",
		"EXEC (N'ALTER LOGIN l WITH PASSWORD = ''x''')",
		"SELECT 1; EXECUTE sys.sp_executesql N'CREATE LOGIN l2 WITH PASSWORD = ''x'''",
	} {
		// The batch's own text is a call with a literal: it reads clean.
		require.NoError(t, ValidateMSSQLBatch(sql, full), sql)

		dyn, readable := MSSQLDynamicSQL(sql)
		err := ValidateDynamicSQL(dyn, readable, full, func(text string) error {
			return ValidateMSSQLBatch(text, full)
		})
		require.ErrorIs(t, err, ErrPasswordChangeBlocked, sql)
	}
}

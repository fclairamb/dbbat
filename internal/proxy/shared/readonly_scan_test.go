package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

func readOnlyGrant() *store.Grant {
	return &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
}

// A statement led by WITH used to read as a read whatever it did, because the
// write classifier only looks at the first keyword.
func TestValidateQuery_ReadOnly_BlocksWritingCTEs(t *testing.T) {
	t.Parallel()

	grant := readOnlyGrant()

	writes := []string{
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"with u as (update t set a = 1 returning id) select count(*) from u",
		"WITH i AS (INSERT INTO t VALUES (1) RETURNING id) SELECT id FROM i",
		"WITH c AS NOT MATERIALIZED (SELECT 1) DELETE FROM t",
		"WITH c AS (SELECT 1) UPDATE t SET a = 1",
		"WITH RECURSIVE c AS (SELECT 1) MERGE INTO t USING c ON (true) WHEN MATCHED THEN DELETE",
		"/* x */ WITH/**/d AS (DELETE FROM t) SELECT 1",
		// An unterminated literal is read as a write: nothing vouches for it.
		"WITH c AS (SELECT 'open) SELECT 1",
	}
	for _, sql := range writes {
		require.ErrorIs(t, ValidateQuery(sql, grant), ErrReadOnlyViolation, "should refuse: %s", sql)
	}

	reads := []string{
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"WITH c AS (SELECT * FROM audit WHERE action = 'DELETE') SELECT * FROM c",
		`WITH c AS (SELECT "update" FROM t) SELECT * FROM c`,
		"WITH c AS (SELECT t.delete FROM t) SELECT * FROM c",
		"WITH c AS (SELECT deleted_at, updated_by FROM t) SELECT * FROM c",
		"WITHOUT_A_CTE",
	}
	for _, sql := range reads {
		require.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
	}
}

// MySQL literals take backslash escapes, so the CTE scan has to skip them
// under MySQL's rules or `\'` would end the literal early.
func TestValidateMySQLQuery_ReadOnly_BlocksWritingCTEs(t *testing.T) {
	t.Parallel()

	grant := readOnlyGrant()

	require.ErrorIs(t, ValidateMySQLQuery("WITH c AS (SELECT 1) DELETE FROM t", grant), ErrReadOnlyViolation)
	require.NoError(t, ValidateMySQLQuery(`WITH c AS (SELECT 'it\'s DELETE') SELECT * FROM c`, grant))
}

// A PL/SQL block starts with BEGIN or DECLARE, which neither control reads as
// a write, and the dynamic-SQL scan only unwraps EXECUTE IMMEDIATE.
func TestValidateOracleQuery_ReadOnly_BlocksPLSQLWrites(t *testing.T) {
	t.Parallel()

	grant := readOnlyGrant()

	writes := []string{
		"BEGIN DELETE FROM t; END;",
		"begin update t set a = 1; commit; end;",
		"DECLARE v NUMBER; BEGIN INSERT INTO t VALUES (1); END;",
		"<<blk>> BEGIN MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET a = 1; END;",
		"BEGIN FOR r IN (SELECT id FROM s) LOOP DELETE FROM t WHERE id = r.id; END LOOP; END;",
		"BEGIN/**/DELETE FROM t; END;",
		// Oracle 12c: a function declared in WITH, with an autonomous transaction.
		"WITH FUNCTION f RETURN NUMBER IS PRAGMA AUTONOMOUS_TRANSACTION; " +
			"BEGIN DELETE FROM t; COMMIT; RETURN 1; END; SELECT f FROM dual",
	}
	for _, sql := range writes {
		require.ErrorIs(t, ValidateOracleQuery(sql, grant), ErrReadOnlyViolation, "should refuse: %s", sql)
	}

	require.ErrorIs(t, ValidateOracleQuery("CALL pkg.purge()", grant), ErrReadOnlyProcedureCall)
	require.ErrorIs(t, ValidateOracleQuery("  call purge(1)", grant), ErrReadOnlyProcedureCall)

	reads := []string{
		"BEGIN DBMS_OUTPUT.ENABLE(NULL); END;",
		"BEGIN DBMS_APPLICATION_INFO.SET_MODULE('x', 'DELETE'); END;",
		"DECLARE v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM t; END;",
		"BEGIN :x := t_pkg.updated_rows; END;",
		"SELECT * FROM callers",
	}
	for _, sql := range reads {
		require.NoError(t, ValidateOracleQuery(sql, grant), "should allow: %s", sql)
	}
}

// Without read_only, none of the new refusals apply.
func TestValidateOracleQuery_PLSQLWritesAllowedWithoutReadOnly(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}}

	require.NoError(t, ValidateOracleQuery("BEGIN DELETE FROM t; END;", grant))
	require.NoError(t, ValidateOracleQuery("CALL pkg.purge()", grant))
	require.NoError(t, ValidateQuery("WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", grant))
}

func TestPostgresSetConfigReaches(t *testing.T) {
	t.Parallel()

	names := []string{"default_transaction_read_only", "transaction_read_only", "role", "session_authorization"}

	tests := []struct {
		sql  string
		want bool
	}{
		{"SELECT set_config('default_transaction_read_only', 'off', false)", true},
		{"SELECT pg_catalog.set_config('transaction_read_only', 'off', true)", true},
		{"SELECT SET_CONFIG('Default_Transaction_Read_Only', 'off', false)", true},
		{`SELECT "set_config"('role', 'postgres', false)`, true},
		{`SELECT pg_catalog."set_config"('session_authorization', 'postgres', false)`, true},
		{"SELECT set_config ( 'role' , 'postgres', false)", true},
		{"SELECT set_config/**/('role', 'postgres', false)", true},
		// A name dbbat cannot read is refused.
		{"SELECT set_config('default_transaction_' || 'read_only', 'off', false)", true},
		{"SELECT set_config(E'role', 'x', false)", true},
		{"SELECT set_config(name, 'off', false) FROM v", true},
		{"SELECT set_config($$role$$, 'x', false)", true},
		// set_config run from inside a string.
		{"SELECT query_to_xml('select set_config(''role'',''x'',false)', true, false, '')", true},
		// One safe call does not clear a second one.
		{"SELECT set_config('search_path', '', false), set_config('role', 'x', false)", true},

		{"SELECT pg_catalog.set_config('search_path', '', false)", false},
		{"SELECT set_config('application_name', 'x', false)", false},
		{"SELECT current_setting('default_transaction_read_only')", false},
		{"SELECT 1", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, PostgresSetConfigReaches(tt.sql, names...), tt.sql)
	}
}

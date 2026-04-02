package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

func TestValidateQuery_ReadOnly_BlocksWrites(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	blocked := []string{
		"INSERT INTO t VALUES (1)", "UPDATE t SET x = 1", "DELETE FROM t WHERE id = 1",
		"MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
		"DROP TABLE t", "TRUNCATE TABLE t", "CREATE TABLE t (id NUMBER)",
		"ALTER TABLE t ADD (col VARCHAR2(100))", "GRANT SELECT ON t TO u", "REVOKE SELECT ON t FROM u",
	}
	for _, sql := range blocked {
		require.ErrorIs(t, ValidateQuery(sql, grant), ErrReadOnlyViolation, "should block: %s", sql)
	}
}

func TestValidateQuery_ReadOnly_AllowsReads(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	allowed := []string{
		"SELECT * FROM t", "SELECT 1 FROM DUAL",
		"WITH cte AS (SELECT 1 FROM DUAL) SELECT * FROM cte",
		"EXPLAIN PLAN FOR SELECT * FROM t", "  select * from t  ",
	}
	for _, sql := range allowed {
		require.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
	}
}

func TestValidateQuery_BlockDDL(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlBlockDDL}}
	blocked := []string{
		"CREATE TABLE t (id NUMBER)", "ALTER TABLE t ADD (col NUMBER)", "DROP TABLE t",
		"CREATE INDEX idx ON t(col)", "CREATE OR REPLACE VIEW v AS SELECT 1 FROM DUAL",
		"CREATE SEQUENCE s", "ALTER INDEX idx REBUILD",
	}
	allowed := []string{"INSERT INTO t VALUES (1)", "SELECT * FROM t", "UPDATE t SET x = 1"}

	for _, sql := range blocked {
		require.ErrorIs(t, ValidateQuery(sql, grant), ErrDDLBlocked, "should block: %s", sql)
	}
	for _, sql := range allowed {
		require.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
	}
}

func TestValidateQuery_CaseInsensitive(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	assert.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
	assert.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_CommentBypass(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	assert.NoError(t, ValidateQuery("/* harmless */ INSERT INTO t VALUES (1)", grant))
}

func TestValidateQuery_PasswordChange(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // No restrictions
	assert.ErrorIs(t, ValidateQuery("ALTER USER bob PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
	assert.ErrorIs(t, ValidateQuery("ALTER ROLE admin PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
	assert.NoError(t, ValidateQuery("ALTER TABLE t ADD (col NUMBER)", grant))
}

func TestValidateOracleQuery_BlocksDangerousPatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // No restrictions — patterns always blocked
	blocked := []struct{ sql, reason string }{
		{"ALTER SYSTEM SET open_cursors=1000", "system config"},
		{"ALTER SYSTEM KILL SESSION '123,456'", "kill session"},
		{"CREATE DATABASE LINK remote CONNECT TO u IDENTIFIED BY p USING 'tns'", "network escape"},
		{"BEGIN DBMS_SCHEDULER.CREATE_JOB('job1'); END;", "async execution"},
		{"SELECT UTL_HTTP.REQUEST('http://evil.com') FROM DUAL", "network access"},
		{"SELECT UTL_FILE.FOPEN('/etc/passwd','r') FROM DUAL", "file system access"},
		{"BEGIN DBMS_PIPE.SEND_MESSAGE('pipe'); END;", "IPC escape"},
		{"BEGIN UTL_TCP.OPEN_CONNECTION('evil.com', 80); END;", "network escape"},
		{"BEGIN DBMS_JOB.SUBMIT(1, 'BEGIN NULL; END;'); END;", "async execution"},
	}
	for _, tt := range blocked {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidateOracleQuery(tt.sql, grant), ErrOraclePatternBlocked)
		})
	}
}

func TestValidateOracleQuery_AllowsSafePLSQL(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // No restrictions
	allowed := []string{
		"BEGIN my_pkg.read_data(:1, :2); END;",
		"DECLARE v NUMBER; BEGIN SELECT COUNT(*) INTO v FROM t; END;",
		"BEGIN NULL; END;",
		"SELECT * FROM employees",
	}
	for _, sql := range allowed {
		require.NoError(t, ValidateOracleQuery(sql, grant), "should allow: %s", sql)
	}
}

func TestValidateOracleQuery_CombinesSharedAndOracleChecks(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	assert.ErrorIs(t, ValidateOracleQuery("INSERT INTO t VALUES (1)", grant), ErrReadOnlyViolation)
	assert.ErrorIs(t, ValidateOracleQuery("SELECT UTL_HTTP.REQUEST('x') FROM DUAL", grant), ErrOraclePatternBlocked)
}

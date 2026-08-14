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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}
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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	require.Error(t, ValidateQuery("insert INTO t VALUES (1)", grant))
	require.Error(t, ValidateQuery("  INSERT INTO t VALUES (1)  ", grant))
}

func TestValidateQuery_CommentBypass(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	assert.NoError(t, ValidateQuery("/* harmless */ INSERT INTO t VALUES (1)", grant))
}

func TestValidateQuery_PasswordChange(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}} // No restrictions
	require.ErrorIs(t, ValidateQuery("ALTER USER bob PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
	require.ErrorIs(t, ValidateQuery("ALTER ROLE admin PASSWORD 'secret'", grant), ErrPasswordChangeBlocked)
	assert.NoError(t, ValidateQuery("ALTER TABLE t ADD (col NUMBER)", grant))
}

func TestValidateOracleQuery_BlocksDangerousPatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}} // No restrictions — patterns always blocked
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
		{"ALTER SESSION SET CONTAINER = PDB2", "pluggable database switch"},
		{"alter session set container=pdb2", "same, lower case and unspaced"},
		{`ALTER SESSION SET "CONTAINER" = PDB2`, "same, quoted parameter name"},
		{"ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=PDB2", "CONTAINER not the first parameter"},
		{"ALTER\tSESSION\nSET\n  CONTAINER\n=\nPDB2", "same, folded across whitespace"},
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

	grant := &store.Grant{Definition: &store.GrantDefinition{}} // No restrictions
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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	require.ErrorIs(t, ValidateOracleQuery("INSERT INTO t VALUES (1)", grant), ErrReadOnlyViolation)
	require.ErrorIs(t, ValidateOracleQuery("SELECT UTL_HTTP.REQUEST('x') FROM DUAL", grant), ErrOraclePatternBlocked)
}

func TestValidateMySQLQuery_BlocksDangerousPatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}} // no grant restrictions — patterns always blocked
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

	grant := &store.Grant{Definition: &store.GrantDefinition{}} // no restrictions
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

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
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

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}
	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	noDDL := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}

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
	// listDatabases is allowed at validation time (the reply is filtered to the
	// grant's database by the MongoDB proxy's result layer instead of denied).
	require.NoError(t, ValidateMongoCommand("listDatabases", "admin", mongoBody(t, bson.D{{Key: "listDatabases", Value: 1}}), db, full))
	require.NoError(t, ValidateMongoCommand("listDatabases", "", mongoBody(t, bson.D{{Key: "listDatabases", Value: 1}}), db, readOnly))
	require.ErrorIs(t, ValidateMongoCommand("frobnicate", "app", mongoBody(t, bson.D{{Key: "frobnicate", Value: 1}}), db, full), ErrMongoUnknownCommand)
}

func TestValidateMongoCommand_DBEnforcement(t *testing.T) {
	t.Parallel()

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}

	// admin allowed for diagnostics only.
	require.NoError(t, ValidateMongoCommand("ping", "admin", mongoBody(t, bson.D{{Key: "ping", Value: 1}}), db, full))
	require.ErrorIs(t,
		ValidateMongoCommand("find", "admin", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full),
		ErrMongoDatabaseBlocked)

	// local / config / other databases denied.
	require.ErrorIs(t, ValidateMongoCommand("find", "local", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full), ErrMongoDatabaseBlocked)
	require.ErrorIs(t, ValidateMongoCommand("find", "other", mongoBody(t, bson.D{{Key: "find", Value: "c"}}), db, full), ErrMongoDatabaseBlocked)
}

// --- ALTER SESSION carve-out ------------------------------------------------
//
// Both sides are pinned: an allowlisted parameter passes under read_only *and*
// block_ddl, and CONTAINER — plus anything not enumerated — is still refused.

// alterSessionCorpusStatements are the five ALTER SESSION statements the Oracle
// recordings in internal/proxy/oracle/testdata/ actually carry as execute ops
// (DBeaver's connection setup, measured by TestSurveyAlterSessionMisreadAsSet).
// They are the floor the allowlist must cover: a read-only grant that refuses
// any of them is a DBeaver that cannot connect.
var alterSessionCorpusStatements = []string{
	"ALTER SESSION SET CURRENT_SCHEMA=TESTADM",
	"ALTER SESSION SET OPTIMIZER_FEATURES_ENABLE='10.2.0.5'",
	`ALTER SESSION SET "_optimizer_cost_based_transformation" = 'OFF'`,
	`ALTER SESSION SET "_optimizer_push_pred_cost_based" = FALSE`,
	`ALTER SESSION SET "_optimizer_squ_bottomup" = FALSE`,
}

func TestIsAllowedAlterSession_Allows(t *testing.T) {
	t.Parallel()

	allowed := append([]string{
		// Whitespace, casing and quoting variants of the same thing.
		"alter session set current_schema = TESTADM",
		"  ALTER   SESSION   SET   CURRENT_SCHEMA   =   TESTADM  ",
		`ALTER SESSION SET "CURRENT_SCHEMA"=TESTADM`,
		// Multi-parameter, every parameter allowlisted — the shape dbbat itself
		// sends on the upstream leg (upstream_auth_client_wide.go).
		"ALTER SESSION SET NLS_LANGUAGE='AMERICAN' NLS_TERRITORY='AMERICA'  TIME_ZONE='+02:00'",
		"ALTER SESSION SET NLS_DATE_FORMAT='DD-MON-YYYY HH24:MI:SS'",
		"ALTER SESSION SET NLS_NUMERIC_CHARACTERS=',.'",
		// The _OPTIMIZER_ family rule, on a hint no recording carries.
		`ALTER SESSION SET "_optimizer_use_feedback" = FALSE`,
	}, alterSessionCorpusStatements...)

	for _, sql := range allowed {
		assert.True(t, IsAllowedAlterSession(sql), "should be allowlisted: %s", sql)
		assert.False(t, IsWriteQuery(sql), "should not be a write: %s", sql)
		assert.False(t, IsDDLQuery(sql), "should not be DDL: %s", sql)
	}
}

func TestIsAllowedAlterSession_Refuses(t *testing.T) {
	t.Parallel()

	refused := []struct{ sql, reason string }{
		{"ALTER SESSION SET CONTAINER = PDB2", "CONTAINER switches PDB, outside the grant's database"},
		{`ALTER SESSION SET "CONTAINER"=PDB2`, "quoted CONTAINER is the same parameter"},
		{"ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=PDB2", "one bad parameter refuses the whole statement"},
		{"ALTER SESSION SET CONTAINER=PDB2 CURRENT_SCHEMA=X", "order does not matter either"},
		{"ALTER SESSION SET SQL_TRACE = TRUE", "unenumerated parameter"},
		{"ALTER SESSION SET EVENTS '10046 trace name context forever'", "no = at all: unparseable, fail closed"},
		{"ALTER SESSION SET PLSQL_DEBUG=TRUE", "unenumerated parameter"},
		{"ALTER SESSION ENABLE PARALLEL DML", "not a SET"},
		{"ALTER SESSION CLOSE DATABASE LINK remote", "not a SET"},
		{"ALTER SESSION SET", "no parameter"},
		{"ALTER SESSION SET   ", "no parameter"},
		{"ALTER SESSION SETTINGS = 1", "SET must end at a word boundary"},
		{"ALTER SESSION SET CURRENT_SCHEMA", "missing = and value"},
		{"ALTER SESSION SET CURRENT_SCHEMA=", "missing value"},
		{"ALTER SESSION SET CURRENT_SCHEMA='unterminated", "unterminated quote"},
		{"ALTER SESSION SET CURRENT_SCHEMA=X; DROP TABLE t", "a second statement stapled on"},
		{"ALTER SESSION SET CURRENT_SCHEMA=X;DROP TABLE t", "same, without the space"},
		{"ALTER SESSION SET CURRENT_SCHEMA=X -- and then", "trailing comment is not parsed with confidence"},
		{"ALTER SYSTEM SET open_cursors=1000", "ALTER SYSTEM is a different statement"},
		{"ALTER TABLE t ADD (col NUMBER)", "ordinary DDL"},
		{"ALTER USER bob IDENTIFIED BY hunter2", "ordinary DDL"},
		{"SELECT 'ALTER SESSION SET CURRENT_SCHEMA=X' FROM DUAL", "not a leading ALTER SESSION"},
	}

	for _, tt := range refused {
		assert.False(t, IsAllowedAlterSession(tt.sql), "%s: %s", tt.reason, tt.sql)
	}
}

func TestValidateQuery_AlterSessionCarveOut(t *testing.T) {
	t.Parallel()

	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	blockDDL := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}

	// The carve-out applies to both controls, on the shared and the Oracle path.
	for _, sql := range alterSessionCorpusStatements {
		require.NoError(t, ValidateQuery(sql, readOnly), "read_only should allow: %s", sql)
		require.NoError(t, ValidateQuery(sql, blockDDL), "block_ddl should allow: %s", sql)
		require.NoError(t, ValidateOracleQuery(sql, readOnly), "Oracle read_only should allow: %s", sql)
		require.NoError(t, ValidateOracleQuery(sql, blockDDL), "Oracle block_ddl should allow: %s", sql)
	}

	// And nothing outside it moved.
	stillRefused := []string{
		"ALTER SESSION SET CONTAINER = PDB2",
		"ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=PDB2",
		"ALTER SESSION SET SQL_TRACE = TRUE",
		"ALTER SESSION ENABLE PARALLEL DML",
	}
	for _, sql := range stillRefused {
		require.ErrorIs(t, ValidateQuery(sql, readOnly), ErrReadOnlyViolation, "read_only should refuse: %s", sql)
		require.ErrorIs(t, ValidateQuery(sql, blockDDL), ErrDDLBlocked, "block_ddl should refuse: %s", sql)
	}

	// ALTER SYSTEM stays blocked outright, grant controls or not.
	require.ErrorIs(t,
		ValidateOracleQuery("ALTER SYSTEM SET open_cursors=1000", &store.Grant{Definition: &store.GrantDefinition{}}),
		ErrOraclePatternBlocked)
}

// TestValidateOracleQuery_BlocksAlterSessionContainer pins that `ALTER SESSION
// SET CONTAINER = <pdb>` is refused whatever the grant says, and that the two
// mechanisms that can refuse it agree.
//
// They have to be checked together because they are ordered: oracleBlockedPatterns
// runs *after* ValidateQuery, so a future edit that put CONTAINER on
// alterSessionAllowedParams would hand a read_only session the statement before
// the pattern was ever consulted, and nothing else in this file would notice.
func TestValidateOracleQuery_BlocksAlterSessionContainer(t *testing.T) {
	t.Parallel()

	fullWrite := &store.Grant{Definition: &store.GrantDefinition{}}
	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	blockDDL := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}

	containerSwitches := []string{
		"ALTER SESSION SET CONTAINER = PDB2",
		"alter session set container=pdb2",
		`ALTER SESSION SET "CONTAINER"=PDB2`,
		"ALTER SESSION SET CONTAINER = 'PDB2'",
		"ALTER SESSION SET CONTAINER=CDB$ROOT",
		// Multi-parameter: CONTAINER is not what SET is followed by here, so a
		// `SET\s+CONTAINER` anchor would miss it while the switch still happens.
		"ALTER SESSION SET CURRENT_SCHEMA=X CONTAINER=PDB2",
		"ALTER SESSION SET NLS_SORT=BINARY CONTAINER=PDB2",
		"ALTER\tSESSION\nSET\n  CONTAINER\n=\nPDB2",
	}

	for _, sql := range containerSwitches {
		// Mechanism 1 — the allowlist never admits it, so the statement keeps the
		// write/DDL classification its leading ALTER gives it.
		assert.False(t, IsAllowedAlterSession(sql), "must not be allowlisted: %s", sql)
		assert.True(t, IsWriteQuery(sql), "still a write: %s", sql)
		assert.True(t, IsDDLQuery(sql), "still DDL: %s", sql)

		// Mechanism 2 — blocked outright, which is what closes the full-write hole
		// (a grant with neither control used to allow the switch).
		require.ErrorIs(t, ValidateOracleQuery(sql, fullWrite), ErrOraclePatternBlocked, "full write: %s", sql)

		// The restricted grants refuse it first, on the shared path: a different
		// error, never a pass.
		require.ErrorIs(t, ValidateOracleQuery(sql, readOnly), ErrReadOnlyViolation, "read_only: %s", sql)
		require.ErrorIs(t, ValidateOracleQuery(sql, blockDDL), ErrDDLBlocked, "block_ddl: %s", sql)
	}
}

// TestValidateOracleQuery_AllowsAlterSessionCurrentSchema is the other half of
// the CONTAINER decision. CURRENT_SCHEMA changes unqualified name resolution
// only — Oracle still evaluates privileges as the connected user and a dbbat
// grant is scoped to a database rather than a schema — so it stays allowed, and
// the new CONTAINER pattern must not swallow it (nor a value that merely ends in
// the word).
func TestValidateOracleQuery_AllowsAlterSessionCurrentSchema(t *testing.T) {
	t.Parallel()

	grants := map[string]*store.Grant{
		"full_write": {Definition: &store.GrantDefinition{}},
		"read_only":  {Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}},
		"block_ddl":  {Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}},
	}

	allowed := []string{
		"ALTER SESSION SET CURRENT_SCHEMA=TESTADM",
		"ALTER SESSION SET CURRENT_SCHEMA = TESTADM",
		`ALTER SESSION SET "CURRENT_SCHEMA" = "TestAdm"`,
		"ALTER SESSION SET CURRENT_SCHEMA=MY_CONTAINER", // a schema that ends in the word
		"ALTER SESSION SET NLS_DATE_FORMAT='YYYY-MM-DD'",
		"SELECT container_id FROM v$containers", // reads about containers are reads
	}

	for _, sql := range allowed {
		for name, grant := range grants {
			require.NoError(t, ValidateOracleQuery(sql, grant), "%s should allow: %s", name, sql)
		}
	}
}

// TestAlterSessionCarveOut_LeavesOtherDialectsAlone pins that the carve-out is
// Oracle-shaped: IsWriteQuery/IsDDLQuery are shared by all five proxies, and no
// statement other than an allowlisted ALTER SESSION changed classification.
func TestAlterSessionCarveOut_LeavesOtherDialectsAlone(t *testing.T) {
	t.Parallel()

	writes := []string{
		"ALTER TABLE t ADD (col NUMBER)",          // Oracle / every dialect
		"ALTER USER bob IDENTIFIED BY hunter2",    // Oracle
		"ALTER ROLE app SET search_path = x",      // PostgreSQL
		"ALTER DATABASE db CHARACTER SET utf8mb4", // MySQL
		"ALTER LOGIN sa WITH PASSWORD = 'x'",      // SQL Server
		"ALTER SCHEMA s TRANSFER dbo.t",           // SQL Server
		"ALTER SYSTEM SET open_cursors=1000",      // Oracle
		"ALTER SEQUENCE s RESTART",                // PostgreSQL
		"ALTER INDEX idx REBUILD",                 // Oracle
		"CREATE TABLE t (id NUMBER)",
		"DROP TABLE t",
		"INSERT INTO t VALUES (1)",
	}
	for _, sql := range writes {
		assert.True(t, IsWriteQuery(sql), "still a write: %s", sql)
	}

	ddl := []string{
		"ALTER TABLE t ADD (col NUMBER)",
		"ALTER SYSTEM SET open_cursors=1000",
		"ALTER LOGIN sa WITH PASSWORD = 'x'",
		"CREATE INDEX idx ON t(col)",
		"TRUNCATE TABLE t",
	}
	for _, sql := range ddl {
		assert.True(t, IsDDLQuery(sql), "still DDL: %s", sql)
	}

	// Reads stay reads; SET (PG/MySQL session settings) was never a write.
	for _, sql := range []string{"SELECT 1", "SET search_path = x", "SET NAMES utf8mb4"} {
		assert.False(t, IsWriteQuery(sql), "still not a write: %s", sql)
		assert.False(t, IsDDLQuery(sql), "still not DDL: %s", sql)
	}
}

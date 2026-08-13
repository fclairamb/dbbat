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

// TestValidateQuery_CommentBypass used to pin the *bug*: a leading comment made
// the prefix classification read the statement as something other than an
// INSERT, while the database read it as an INSERT. It now pins the fix.
func TestValidateQuery_CommentBypass(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	assert.ErrorIs(t,
		ValidateQuery("/* harmless */ INSERT INTO t VALUES (1)", grant),
		ErrReadOnlyViolation)
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

// aggWithPipeline builds an `aggregate` command body on the granted database
// whose pipeline is the given stages.
func aggWithPipeline(t *testing.T, stages bson.A) bson.Raw {
	t.Helper()

	return mongoBody(t, bson.D{
		{Key: "aggregate", Value: "src"},
		{Key: "pipeline", Value: stages},
		{Key: "$db", Value: "app"},
	})
}

// A $out/$merge stage carries its own target database, which the message's $db
// never reveals. It is held to the same policy — otherwise any non-read_only
// grant writes wherever it likes while $db passes the check honestly.
func TestValidateMongoCommand_PipelineTargetDatabase(t *testing.T) {
	t.Parallel()

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}

	// Same-database forms stay allowed — this is normal, common traffic.
	allowed := map[string]bson.A{
		"$out string":            {bson.D{{Key: "$out", Value: "dest"}}},
		"$out same db":           {bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "app"}, {Key: "coll", Value: "dest"}}}}},
		"$merge string":          {bson.D{{Key: "$merge", Value: "dest"}}},
		"$merge into string":     {bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: "dest"}}}}},
		"$merge into same db":    {bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: bson.D{{Key: "db", Value: "app"}, {Key: "coll", Value: "dest"}}}}}}},
		"$lookup sub-pipeline":   {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "o"}, {Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}}}}}},
		"plain read pipeline":    {bson.D{{Key: "$match", Value: bson.D{}}}},
		"$facet without a write": {bson.D{{Key: "$facet", Value: bson.D{{Key: "a", Value: bson.A{bson.D{{Key: "$count", Value: "n"}}}}}}}},
	}
	for name, stages := range allowed {
		require.NoError(t, ValidateMongoCommand("aggregate", "app", aggWithPipeline(t, stages), db, full), name)
	}

	// A foreign target database is refused with the same error a foreign $db gets.
	refused := map[string]bson.A{
		"$out other db": {bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "dest"}}}}},
		"$merge other db": {
			bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "dest"}}}}}},
		},
		"$out admin":  {bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "admin"}, {Key: "coll", Value: "dest"}}}}},
		"$out config": {bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "config"}, {Key: "coll", Value: "dest"}}}}},
		// Database names are case-sensitive in MongoDB: match exactly, never fold.
		"$out wrong case": {bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "App"}, {Key: "coll", Value: "dest"}}}}},
		// Nested pipelines are walked too, even though MongoDB itself forbids
		// $out/$merge inside them.
		"$out inside $facet": {
			bson.D{{Key: "$facet", Value: bson.D{{Key: "a", Value: bson.A{
				bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "dest"}}}},
			}}}}},
		},
		"$merge inside $lookup": {
			bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "o"}, {Key: "pipeline", Value: bson.A{
				bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "d"}}}}}},
			}}}}},
		},
		"$out later in the pipeline": {
			bson.D{{Key: "$match", Value: bson.D{}}},
			bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "dest"}}}},
		},
	}
	for name, stages := range refused {
		require.ErrorIs(t,
			ValidateMongoCommand("aggregate", "app", aggWithPipeline(t, stages), db, full),
			ErrMongoDatabaseBlocked, name)
	}

	// The refusal is independent of grant controls: a grant with every control
	// off (full write) does not buy a cross-database write.
	crossDB := aggWithPipeline(t, bson.A{
		bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: bson.D{{Key: "db", Value: "other"}, {Key: "coll", Value: "d"}}}}}},
	})
	for _, grant := range []*store.Grant{
		full,
		{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}},
		{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockCopy}}},
	} {
		require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", crossDB, db, grant), ErrMongoDatabaseBlocked)
	}

	// The server row's dbbat Name and its upstream DatabaseName are both accepted
	// as the target, exactly as the $db check accepts either.
	renamed := &store.Server{Name: "app-prod", DatabaseName: "appdb"}
	for _, target := range []string{"app-prod", "appdb"} {
		body := mongoBody(t, bson.D{
			{Key: "aggregate", Value: "src"},
			{Key: "pipeline", Value: bson.A{bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: target}, {Key: "coll", Value: "dest"}}}}}},
			{Key: "$db", Value: "appdb"},
		})
		require.NoError(t, ValidateMongoCommand("aggregate", "appdb", body, renamed, full), target)
	}
}

// nestedLookupPipeline wraps innermost in `levels` layers of $lookup
// sub-pipeline, so the innermost stage is examined at walk depth == levels.
func nestedLookupPipeline(levels int, innermost bson.D) bson.A {
	stages := bson.A{innermost}

	for range levels {
		stages = bson.A{bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "joined"},
			{Key: "as", Value: "j"},
			{Key: "pipeline", Value: stages},
		}}}}
	}

	return stages
}

// A pipeline dbbat cannot read all of is refused, never waved through. The walk
// that finds $out/$merge is the same walk that classifies the command as a
// write, so a scan that gave up quietly would drop a nested $merge to *read* —
// which read_only does not stop. Fail closed on both the depth cap and a
// pipeline that will not parse.
func TestValidateMongoCommand_UncheckablePipelineRefused(t *testing.T) {
	t.Parallel()

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}
	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	sameDBMerge := bson.D{{Key: "$merge", Value: "dest"}}
	crossDBMerge := bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: bson.D{
		{Key: "db", Value: "other"}, {Key: "coll", Value: "d"},
	}}}}}

	// Past the cap: refused outright, under every grant. Before this was
	// inverted, a $merge at depth 9 was classified as a read and ran under a
	// read_only grant.
	for _, levels := range []int{9, 10, 12, 40} {
		for _, grant := range []*store.Grant{full, readOnly} {
			body := aggWithPipeline(t, nestedLookupPipeline(levels, crossDBMerge))
			require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", body, db, grant),
				ErrMongoPipelineNotCheckable, "depth %d", levels)
		}

		// Even a benign stage past the cap is refused: the point is that dbbat
		// could not see the pipeline, not what happened to be in it.
		benign := aggWithPipeline(t, nestedLookupPipeline(levels, bson.D{{Key: "$match", Value: bson.D{}}}))
		require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", benign, db, full), ErrMongoPipelineNotCheckable)
	}

	// Up to the cap, everything still works exactly as it does at the top level.
	for levels := range mongoPipelineMaxDepth + 1 {
		benign := aggWithPipeline(t, nestedLookupPipeline(levels, bson.D{{Key: "$match", Value: bson.D{}}}))
		require.NoError(t, ValidateMongoCommand("aggregate", "app", benign, db, full), "benign depth %d", levels)

		crossDB := aggWithPipeline(t, nestedLookupPipeline(levels, crossDBMerge))
		require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", crossDB, db, full),
			ErrMongoDatabaseBlocked, "cross-db depth %d", levels)

		nested := aggWithPipeline(t, nestedLookupPipeline(levels, sameDBMerge))
		require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", nested, db, readOnly),
			ErrMongoReadOnly, "nested write depth %d", levels)
	}

	// Malformed shapes are unreadable, not empty.
	malformed := []bson.D{
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: "not-a-pipeline"}, {Key: "$db", Value: "app"}},
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{42}}, {Key: "$db", Value: "app"}},
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{
			bson.D{{Key: "$facet", Value: bson.D{{Key: "a", Value: "not-a-pipeline"}}}},
		}}, {Key: "$db", Value: "app"}},
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{
			bson.D{{Key: "$lookup", Value: bson.D{{Key: "pipeline", Value: 7}}}},
		}}, {Key: "$db", Value: "app"}},
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{
			bson.D{{Key: "$out", Value: bson.D{{Key: "db", Value: 7}, {Key: "coll", Value: "x"}}}},
		}}, {Key: "$db", Value: "app"}},
		{{Key: "aggregate", Value: "c"}, {Key: "pipeline", Value: bson.A{
			bson.D{{Key: "$merge", Value: 7}},
		}}, {Key: "$db", Value: "app"}},
	}
	for i, d := range malformed {
		require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", mongoBody(t, d), db, full),
			ErrMongoPipelineNotCheckable, "malformed case %d", i)
	}
}

// $lookup / $graphLookup `from` and $unionWith `coll` take the same
// {db, coll} shape as $out/$merge — the read-side twin. read_only does not stop
// a read, so nothing but this check stands between a grant and another
// database's data.
func TestValidateMongoCommand_PipelineReadTargetDatabase(t *testing.T) {
	t.Parallel()

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}
	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	ns := func(dbName string) bson.D {
		return bson.D{{Key: "db", Value: dbName}, {Key: "coll", Value: "secret"}}
	}

	allowed := map[string]bson.A{
		"$lookup from string":       {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: "other_coll"}, {Key: "as", Value: "j"}}}}},
		"$lookup from same db":      {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: ns("app")}, {Key: "as", Value: "j"}}}}},
		"$lookup without from":      {bson.D{{Key: "$lookup", Value: bson.D{{Key: "pipeline", Value: bson.A{bson.D{{Key: "$documents", Value: bson.A{}}}}}, {Key: "as", Value: "j"}}}}},
		"$unionWith string":         {bson.D{{Key: "$unionWith", Value: "other_coll"}}},
		"$unionWith coll string":    {bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: "other_coll"}}}}},
		"$unionWith coll same db":   {bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: ns("app")}}}}},
		"$graphLookup from string":  {bson.D{{Key: "$graphLookup", Value: bson.D{{Key: "from", Value: "other_coll"}, {Key: "as", Value: "j"}}}}},
		"$graphLookup from same db": {bson.D{{Key: "$graphLookup", Value: bson.D{{Key: "from", Value: ns("app")}, {Key: "as", Value: "j"}}}}},
	}
	for name, stages := range allowed {
		require.NoError(t, ValidateMongoCommand("aggregate", "app", aggWithPipeline(t, stages), db, full), name)
	}

	refused := map[string]bson.A{
		"$lookup from other db":      {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: ns("other")}, {Key: "as", Value: "j"}}}}},
		"$lookup from admin":         {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: ns("admin")}, {Key: "as", Value: "j"}}}}},
		"$lookup from config":        {bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: ns("config")}, {Key: "as", Value: "j"}}}}},
		"$unionWith coll other db":   {bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: ns("other")}}}}},
		"$graphLookup from other db": {bson.D{{Key: "$graphLookup", Value: bson.D{{Key: "from", Value: ns("other")}, {Key: "as", Value: "j"}}}}},
		"nested in a sub-pipeline": {bson.D{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "local_coll"},
			{Key: "as", Value: "j"},
			{Key: "pipeline", Value: bson.A{bson.D{{Key: "$unionWith", Value: bson.D{{Key: "coll", Value: ns("other")}}}}}},
		}}}},
	}
	for name, stages := range refused {
		// A cross-database read is refused under a read-only grant too — being a
		// read is exactly why read_only never sees it.
		for _, grant := range []*store.Grant{full, readOnly} {
			require.ErrorIs(t, ValidateMongoCommand("aggregate", "app", aggWithPipeline(t, stages), db, grant),
				ErrMongoDatabaseBlocked, name)
		}
	}
}

// `explain` wraps a whole command — pipeline included — in a nested document,
// which hides it from a scan that only reads the top-level `pipeline`.
//
// Verified against mongod 7.0.40 and 8.2.12: an explain of an aggregate whose
// $out names another database is *accepted* (ok: 1), so the server is no
// backstop for the classification; it does not execute the $out, so this was a
// classification gap rather than a live write escape.
func TestValidateMongoCommand_ExplainWrappedPipeline(t *testing.T) {
	t.Parallel()

	db := &store.Server{Name: "app", DatabaseName: "app"}
	full := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{}}}
	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	explainOf := func(inner bson.D) bson.Raw {
		return mongoBody(t, bson.D{
			{Key: "explain", Value: inner},
			{Key: "verbosity", Value: "queryPlanner"},
			{Key: "$db", Value: "app"},
		})
	}

	aggregateWith := func(stage bson.D) bson.D {
		return bson.D{
			{Key: "aggregate", Value: "src"},
			{Key: "pipeline", Value: bson.A{stage}},
			{Key: "cursor", Value: bson.D{}},
		}
	}

	// A cross-database target inside the explained command is refused.
	crossDB := explainOf(aggregateWith(bson.D{{Key: "$out", Value: bson.D{
		{Key: "db", Value: "other"}, {Key: "coll", Value: "x"},
	}}}))
	require.ErrorIs(t, ValidateMongoCommand("explain", "app", crossDB, db, full), ErrMongoDatabaseBlocked)

	// And the explained pipeline still classifies the command as a write.
	sameDB := explainOf(aggregateWith(bson.D{{Key: "$out", Value: "dest"}}))
	require.ErrorIs(t, ValidateMongoCommand("explain", "app", sameDB, db, readOnly), ErrMongoReadOnly)
	require.NoError(t, ValidateMongoCommand("explain", "app", sameDB, db, full))

	// An ordinary explain is untouched.
	plain := explainOf(bson.D{{Key: "find", Value: "src"}, {Key: "filter", Value: bson.D{}}})
	require.NoError(t, ValidateMongoCommand("explain", "app", plain, db, readOnly))

	// `explain: true` as an option (not a wrapped command) is not a document and
	// must not be mistaken for one.
	option := mongoBody(t, bson.D{
		{Key: "aggregate", Value: "src"},
		{Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}},
		{Key: "explain", Value: true},
		{Key: "$db", Value: "app"},
	})
	require.NoError(t, ValidateMongoCommand("aggregate", "app", option, db, readOnly))
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

// TestValidateOracleQuery_CommentsDoNotEvadePatterns pins the class-wide hole
// the scratch-copy normalisation closes: every entry in oracleBlockedPatterns
// is multi-keyword, and Oracle ignores a comment wherever whitespace is
// allowed, so `ALTER/**/SESSION SET CONTAINER=PDB2` used to walk straight
// through an outright block.
//
// The grant carries no controls, so the only thing that can refuse these is the
// pattern list itself.
func TestValidateOracleQuery_CommentsDoNotEvadePatterns(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}}
	blocked := []struct{ sql, reason string }{
		{"ALTER SESSION /* x */ SET CONTAINER=PDB2", "comment between the keywords"},
		{"ALTER/**/SESSION SET CONTAINER=PDB2", "empty comment as the only separator"},
		{"ALTER/**/SYSTEM FLUSH SHARED_POOL", "ALTER SYSTEM, same trick"},
		{"CREATE/**/DATABASE/**/LINK l CONNECT TO u IDENTIFIED BY p USING 't'", "three keywords, two comments"},
		{"ALTER SESSION -- pick a pdb\n SET CONTAINER=PDB2", "line comment, not block"},
		{"ALTER SESSION SET/*x*/CONTAINER/*y*/=/*z*/PDB2", "comments around the parameter and its ="},
		{"alter/**/session/**/set/**/container=pdb2", "same, lower case"},
		// Fails closed: the stripper cannot read an unterminated literal, so the
		// matchers run against the raw text rather than against a statement
		// nothing checked.
		{"SELECT 'unterminated /* ALTER SYSTEM */", "un-normalisable input falls back to the raw text"},
	}

	for _, tt := range blocked {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidateOracleQuery(tt.sql, grant), ErrOraclePatternBlocked)
		})
	}
}

// TestValidateQuery_CommentsDoNotEvadeClassification is the other half: the
// read_only / block_ddl classification is prefix-shaped, so a leading comment
// changed what a statement looked like to dbbat but not to the database.
func TestValidateQuery_CommentsDoNotEvadeClassification(t *testing.T) {
	t.Parallel()

	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	blockDDL := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}
	noControls := &store.Grant{Definition: &store.GrantDefinition{}}

	writes := []string{
		"/*x*/INSERT INTO t VALUES (1)",
		"/* leading */ UPDATE t SET x = 1",
		"-- leading\nDELETE FROM t",
		"/*a*//*b*/ MERGE INTO t USING s ON (t.id = s.id) WHEN MATCHED THEN UPDATE SET t.x = s.x",
	}
	for _, sql := range writes {
		require.ErrorIs(t, ValidateQuery(sql, readOnly), ErrReadOnlyViolation, "write: %s", sql)
	}

	ddl := []string{
		"/*x*/DROP TABLE t",
		"-- comment\n\tCREATE TABLE t (id NUMBER)",
	}
	for _, sql := range ddl {
		require.ErrorIs(t, ValidateQuery(sql, blockDDL), ErrDDLBlocked, "ddl: %s", sql)
	}

	// Password changes are prefix-shaped too.
	require.ErrorIs(t,
		ValidateQuery("/*x*/ALTER USER bob PASSWORD 'secret'", noControls),
		ErrPasswordChangeBlocked)
	require.ErrorIs(t,
		ValidateQuery("ALTER/**/USER bob PASSWORD 'secret'", noControls),
		ErrPasswordChangeBlocked)
}

// TestValidateQuery_HintsStayAllowed is why the normalised form is a scratch
// copy and never what gets relayed: an optimizer hint *is* a comment. Stripping
// it from the matching copy must not turn a SELECT into anything else, and the
// caller's own string is what reaches the database.
func TestValidateQuery_HintsStayAllowed(t *testing.T) {
	t.Parallel()

	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	allowed := []string{
		"SELECT /*+ INDEX(t i) */ * FROM t",
		"/*+ ALL_ROWS */ SELECT * FROM t",
		"SELECT * FROM t -- trailing note",
		"SELECT * FROM t /* trailing note */",
	}

	for _, sql := range allowed {
		before := sql

		require.NoError(t, ValidateQuery(sql, readOnly), "should allow: %s", sql)
		require.NoError(t, ValidateOracleQuery(sql, readOnly), "should allow: %s", sql)
		// The validators only ever read: the bytes the caller goes on to relay are
		// the bytes it passed in.
		require.Equal(t, before, sql)
	}
}

// TestMatchableSQL_LiteralAware is the part the spec calls out as the one to
// test hardest: a `/*` or `--` inside a literal is not a comment, and mistaking
// one for a comment deletes real SQL from the copy the matchers see — a parser
// bug that becomes an authorization bug.
func TestMatchableSQL_LiteralAware(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		syntax sqlCommentSyntax
		in     string
		want   string
	}{
		{
			name:   "block comment inside a literal is not a comment",
			syntax: syntaxStandard,
			in:     `SELECT * FROM t WHERE c = '/*' AND d = 1`,
			want:   `SELECT * FROM t WHERE c = '/*' AND d = 1`,
		},
		{
			name:   "line comment inside a literal is not a comment",
			syntax: syntaxStandard,
			in:     `SELECT * FROM t WHERE c = '-- not a comment' AND d = 1`,
			want:   `SELECT * FROM t WHERE c = '-- not a comment' AND d = 1`,
		},
		{
			name:   "doubled quote does not end the literal",
			syntax: syntaxStandard,
			in:     `SELECT 'it''s /* fine' FROM dual`,
			want:   `SELECT 'it''s /* fine' FROM dual`,
		},
		{
			name:   "quoted identifier shields a comment introducer",
			syntax: syntaxStandard,
			in:     `SELECT "co/*l" FROM t`,
			want:   `SELECT "co/*l" FROM t`,
		},
		{
			name:   "q-quote operator, bracket form",
			syntax: syntaxStandard,
			in:     `SELECT q'[it's /* fine]' FROM dual`,
			want:   `SELECT q'[it's /* fine]' FROM dual`,
		},
		{
			name:   "q-quote operator, brace form",
			syntax: syntaxStandard,
			in:     `SELECT q'{a -- b}' FROM dual`,
			want:   `SELECT q'{a -- b}' FROM dual`,
		},
		{
			name:   "q-quote operator, paren form",
			syntax: syntaxStandard,
			in:     `SELECT Q'(a /* b)' FROM dual`,
			want:   `SELECT Q'(a /* b)' FROM dual`,
		},
		{
			name:   "q-quote operator, angle form",
			syntax: syntaxStandard,
			in:     `SELECT q'<a -- b>' FROM dual`,
			want:   `SELECT q'<a -- b>' FROM dual`,
		},
		{
			name:   "q-quote operator, arbitrary delimiter",
			syntax: syntaxStandard,
			in:     `SELECT q'!a /* b!' FROM dual`,
			want:   `SELECT q'!a /* b!' FROM dual`,
		},
		{
			name:   "nq-quote operator",
			syntax: syntaxStandard,
			in:     `SELECT nq'[a /* b]' FROM dual`,
			want:   `SELECT nq'[a /* b]' FROM dual`,
		},
		{
			name:   "a column ending in q is not a quote operator",
			syntax: syntaxStandard,
			in:     `SELECT abq'x' /*c*/ FROM t`,
			want:   `SELECT abq'x'   FROM t`,
		},
		{
			name:   "comments are replaced, not deleted",
			syntax: syntaxStandard,
			in:     `ALTER/**/SESSION`,
			want:   `ALTER SESSION`,
		},
		{
			name:   "unterminated block comment runs to end of input",
			syntax: syntaxStandard,
			in:     "SELECT 1 FROM dual /* and then",
			want:   "SELECT 1 FROM dual  ",
		},
		{
			name:   "line comment keeps its newline",
			syntax: syntaxStandard,
			in:     "SELECT 1 -- note\nFROM dual",
			want:   "SELECT 1  \nFROM dual",
		},
		{
			name:   "unterminated literal falls back to the raw text",
			syntax: syntaxStandard,
			in:     `SELECT '/*x*/`,
			want:   `SELECT '/*x*/`,
		},
		{
			name:   "no comment introducer at all is returned untouched",
			syntax: syntaxStandard,
			in:     `SELECT * FROM t WHERE a = 'b'`,
			want:   `SELECT * FROM t WHERE a = 'b'`,
		},
		{
			name:   "mysql hash comment",
			syntax: syntaxMySQL,
			in:     "SELECT 1 # note\nFROM t",
			want:   "SELECT 1  \nFROM t",
		},
		{
			name:   "mysql hash is not a comment for the other dialects",
			syntax: syntaxStandard,
			in:     "SELECT 1 # note\nFROM t",
			want:   "SELECT 1 # note\nFROM t",
		},
		{
			name:   "mysql requires whitespace after the double dash",
			syntax: syntaxMySQL,
			in:     "SELECT 1--2 FROM t",
			want:   "SELECT 1--2 FROM t",
		},
		{
			name:   "the other dialects do not",
			syntax: syntaxStandard,
			in:     "SELECT 1--2 FROM t",
			want:   "SELECT 1 ",
		},
		{
			name:   "mysql backslash escape does not end the literal",
			syntax: syntaxMySQL,
			in:     `SELECT '\'' /*c*/ FROM t`,
			want:   `SELECT '\''   FROM t`,
		},
		{
			name:   "mysql backtick identifier shields a comment introducer",
			syntax: syntaxMySQL,
			in:     "SELECT `co/*l` /*c*/ FROM t",
			want:   "SELECT `co/*l`   FROM t",
		},
		// The executable-comment family. MySQL strips the introducer, its optional
		// version number *and* the closing `*/` before running the body, so the
		// scratch copy has to strip exactly the same three things: a version number
		// left behind sits between two keywords the patterns expect adjacent, which
		// is an evasion rather than a leftover.
		{
			name:   "mysql executable comment, 5-digit version",
			syntax: syntaxMySQL,
			in:     "/*!40101 SET GLOBAL x=1 */",
			want:   "  SET GLOBAL x=1  ",
		},
		{
			name:   "mysql executable comment, 6-digit version",
			syntax: syntaxMySQL,
			in:     "/*!100101 SET GLOBAL x=1 */",
			want:   "  SET GLOBAL x=1  ",
		},
		{
			name:   "mysql executable comment, no version",
			syntax: syntaxMySQL,
			in:     "/*!SET GLOBAL x=1*/",
			want:   " SET GLOBAL x=1 ",
		},
		{
			name:   "mysql executable comment wrapping only a keyword",
			syntax: syntaxMySQL,
			in:     "SET /*!32302 GLOBAL */ max_connections=1",
			want:   "SET   GLOBAL   max_connections=1",
		},
		{
			name:   "mariadb executable comment, 6-digit version",
			syntax: syntaxMySQL,
			in:     "/*M!100001 SET GLOBAL x=1 */",
			want:   "  SET GLOBAL x=1  ",
		},
		{
			name:   "mariadb executable comment, no version",
			syntax: syntaxMySQL,
			in:     "/*M!SET GLOBAL x=1*/",
			want:   " SET GLOBAL x=1 ",
		},
		{
			name:   "unterminated executable comment falls back to the raw text",
			syntax: syntaxMySQL,
			in:     "/*!40101 SET GLOBAL x=1",
			want:   "/*!40101 SET GLOBAL x=1",
		},
		{
			name:   "nested executable comment falls back to the raw text",
			syntax: syntaxMySQL,
			in:     "/*!40101 /*!40102 SET GLOBAL x=1 */ */",
			want:   "/*!40101 /*!40102 SET GLOBAL x=1 */ */",
		},
		{
			name:   "plain comment inside an executable one falls back to the raw text",
			syntax: syntaxMySQL,
			in:     "/*!40101 /*x*/ SET GLOBAL x=1 */",
			want:   "/*!40101 /*x*/ SET GLOBAL x=1 */",
		},
		{
			name:   "an executable comment is a plain comment for the other dialects",
			syntax: syntaxStandard,
			in:     "SELECT /*!40101 1 */ FROM t",
			want:   "SELECT   FROM t",
		},
		{
			name:   "a mysql optimizer hint is a plain comment",
			syntax: syntaxMySQL,
			in:     "SELECT /*+ MAX_EXECUTION_TIME(1) */ 1",
			want:   "SELECT   1",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, matchableSQL(tt.in, tt.syntax))
		})
	}
}

// TestValidateOracleQuery_LiteralsKeepLaterCodeVisible is the literal-awareness
// property stated as the thing that actually matters: a naive stripper reading
// the `/*` inside `'/*'` as a comment would delete the rest of the statement
// from the matching copy, and with it the UTL_HTTP call that must be blocked.
func TestValidateOracleQuery_LiteralsKeepLaterCodeVisible(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}}
	blocked := []string{
		`SELECT * FROM t WHERE c = '/*' AND d = UTL_HTTP.REQUEST('http://evil')`,
		`SELECT * FROM t WHERE c = '--' AND d = UTL_HTTP.REQUEST('http://evil')`,
		`SELECT q'[a /* b]', UTL_FILE.FOPEN('/etc/passwd','r') FROM dual`,
		`SELECT 'it''s /*', DBMS_PIPE.SEND_MESSAGE('p') FROM dual`,
	}

	for _, sql := range blocked {
		assert.ErrorIs(t, ValidateOracleQuery(sql, grant), ErrOraclePatternBlocked, "should block: %s", sql)
	}
}

// TestValidateMySQLQuery_Comments covers the MySQL comment syntax the shared
// validators have to know about: `#`, `--` only with whitespace behind it, and
// `/*! … */`, whose body MySQL *executes* — stripping that one would have made
// this change the evasion rather than the fix.
func TestValidateMySQLQuery_Comments(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{}}
	blocked := []struct{ sql, reason string }{
		{"SET/**/GLOBAL max_connections=1", "block comment between the keywords"},
		{"SET -- note\nGLOBAL max_connections=1", "line comment"},
		{"SET # note\nGLOBAL max_connections=1", "hash comment"},
		{"/*!40101 SET GLOBAL max_connections=1 */", "version-gated comment MySQL executes"},
		{"/*M!100001 SET GLOBAL max_connections=1 */", "MariaDB flavor of the same"},
		{"/*!SET GLOBAL max_connections=1*/", "version-gated comment with no version"},
		// The version number is part of the marker, not of the body. Left in the
		// scratch copy it lands between two keywords the pattern expects adjacent,
		// which is how `SET /*!32302 GLOBAL */ x=1` reached the server as
		// `SET GLOBAL x=1` while reading as neither to dbbat.
		{"SET /*!32302 GLOBAL */ max_connections=1", "version number splitting SET from GLOBAL"},
		{"LOAD /*!50000 DATA */ LOCAL INFILE '/etc/passwd' INTO TABLE t", "same, splitting LOAD from DATA"},
		{"SELECT * FROM t INTO /*!50000 OUTFILE */ '/tmp/x'", "same, splitting INTO from OUTFILE"},
		// Unterminated and nested executable comments fail closed onto the raw
		// text, which still carries the keywords adjacent.
		{"/*!40101 SET GLOBAL max_connections=1", "unterminated, matched against the raw text"},
		{"LOAD/**/DATA LOCAL INFILE '/etc/passwd' INTO TABLE t", "LOAD DATA INFILE"},
		{"SELECT * FROM t INTO/**/OUTFILE '/tmp/x'", "INTO OUTFILE"},
		{"SET/**/PASSWORD = PASSWORD('x')", "SET PASSWORD"},
		// A `#` inside a literal is not a comment: reading it as one would delete
		// the rest of the statement from the matching copy, INTO OUTFILE included.
		{"SELECT * FROM t WHERE c = '#' INTO OUTFILE '/tmp/x'", "hash inside a literal"},
		{`SELECT * FROM t WHERE c = '-- ' INTO OUTFILE '/tmp/x'`, "double dash inside a literal"},
	}

	for _, tt := range blocked {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, ValidateMySQLQuery(tt.sql, grant), ErrMySQLPatternBlocked)
		})
	}

	// A hint is still just a comment, and `--` without whitespace behind it is
	// not one for MySQL (`1--2` is `1 - (-2)`).
	allowed := []string{
		"SELECT /*+ MAX_EXECUTION_TIME(1) */ * FROM t",
		"SELECT 1--2 FROM t",
		"SELECT * FROM t # trailing note",
	}
	for _, sql := range allowed {
		assert.NoError(t, ValidateMySQLQuery(sql, grant), "should allow: %s", sql)
	}
}

// TestValidateMySQLQuery_ExecutableCommentIsNotAWriteEscape is the general case
// behind the pattern list: MySQL runs the body of `/*!… */`, so wrapping *any*
// write in one must not make it read as a non-write. Nothing in
// mysqlBlockedPatterns is involved — this is the read_only control itself.
func TestValidateMySQLQuery_ExecutableCommentIsNotAWriteEscape(t *testing.T) {
	t.Parallel()

	readOnly := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}
	blockDDL := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}}

	writes := []string{
		"/*!00000 INSERT INTO t VALUES (1)*/",
		"/*!40101 UPDATE t SET x = 1 */",
		"/*M!100001 DELETE FROM t */",
		"/*! REPLACE INTO t VALUES (1) */",
	}
	for _, sql := range writes {
		require.ErrorIs(t, ValidateMySQLQuery(sql, readOnly), ErrReadOnlyViolation, "write: %s", sql)
	}

	require.ErrorIs(t,
		ValidateMySQLQuery("/*!00000 DROP TABLE t*/", blockDDL),
		ErrDDLBlocked)
}

// BenchmarkValidateOracleQuery guards the hot path: this runs on every
// statement on all five protocols, so the overwhelmingly common comment-free
// statement must cost a single ContainsAny scan and no allocation.
func BenchmarkValidateOracleQuery(b *testing.B) {
	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}}

	b.Run("no_comment", func(b *testing.B) {
		sql := "SELECT id, name, created_at FROM customers WHERE tenant_id = :1 ORDER BY created_at DESC"

		b.ReportAllocs()

		for range b.N {
			_ = ValidateOracleQuery(sql, grant)
		}
	})

	b.Run("with_comment", func(b *testing.B) {
		sql := "SELECT /*+ INDEX(customers ix_tenant) */ id, name FROM customers WHERE tenant_id = :1"

		b.ReportAllocs()

		for range b.N {
			_ = ValidateOracleQuery(sql, grant)
		}
	})
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

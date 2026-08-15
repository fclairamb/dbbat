package shared

import "testing"

// dynamicCase is one statement and everything a static read of it must conclude:
// which payloads come back, whether a payload dbbat could not read is *there*
// (opaque), and whether the read succeeded at all. The three are separate
// answers, and conflating any two of them is how a control silently stops
// applying.
type dynamicCase struct {
	name     string
	sql      string
	texts    []string
	opaque   bool
	readable bool
}

func (tc dynamicCase) check(t *testing.T, label string, dyn DynamicSQL, readable bool) {
	t.Helper()

	if readable != tc.readable {
		t.Fatalf("%s(%q) readable = %v, want %v", label, tc.sql, readable, tc.readable)
	}

	if dyn.Opaque != tc.opaque {
		t.Fatalf("%s(%q) opaque = %v, want %v", label, tc.sql, dyn.Opaque, tc.opaque)
	}

	if len(dyn.Payloads) != len(tc.texts) {
		t.Fatalf("%s(%q) = %q, want %q", label, tc.sql, dyn.Payloads, tc.texts)
	}

	for i := range dyn.Payloads {
		if dyn.Payloads[i] != tc.texts[i] {
			t.Fatalf("%s(%q) = %q, want %q", label, tc.sql, dyn.Payloads, tc.texts)
		}
	}
}

// TestMSSQLDynamicSQL pins what is extracted, what is reported as opaque, and
// what fails closed. The extraction is what makes `read_only`, `block_ddl` and
// the database-switch refusal apply to `EXEC('…')`, so a shape read wrong here is
// a control that silently stops applying.
func TestMSSQLDynamicSQL(t *testing.T) {
	t.Parallel()

	tests := []dynamicCase{
		{name: "exec", sql: "EXEC('DELETE FROM t')", texts: []string{"DELETE FROM t"}, readable: true},
		{name: "execute", sql: "EXECUTE('DROP TABLE t')", texts: []string{"DROP TABLE t"}, readable: true},
		{name: "lower case", sql: "exec('delete from t')", texts: []string{"delete from t"}, readable: true},
		{name: "space before paren", sql: "EXEC ('DELETE FROM t')", texts: []string{"DELETE FROM t"}, readable: true},
		{name: "space inside parens", sql: "EXEC(  'DELETE FROM t'  )", texts: []string{"DELETE FROM t"}, readable: true},
		{name: "unicode literal", sql: "EXEC(N'USE otherdb')", texts: []string{"USE otherdb"}, readable: true},
		{name: "quote doubling", sql: "EXEC('SELECT ''a'' FROM t')", texts: []string{"SELECT 'a' FROM t"}, readable: true},
		{
			name: "switch and read", sql: "EXEC('USE otherdb; SELECT * FROM secret')",
			texts: []string{"USE otherdb; SELECT * FROM secret"}, readable: true,
		},
		{name: "comment before", sql: "/* x */ EXEC('DELETE FROM t')", texts: []string{"DELETE FROM t"}, readable: true},
		{name: "mid batch", sql: "SELECT 1; EXEC('DELETE FROM t')", texts: []string{"DELETE FROM t"}, readable: true},
		{
			name: "two of them", sql: "EXEC('DELETE FROM a'); EXEC('DELETE FROM b')",
			texts: []string{"DELETE FROM a", "DELETE FROM b"}, readable: true,
		},

		// All-literal concatenation. The operands are written apart and the server
		// runs them joined, so reading only the first literal is reading half a
		// statement — and `EXEC('DELETE ' + 'FROM t')` was measurably allowed under
		// a read_only grant before the fold.
		{
			name: "literal concatenation", sql: "EXEC('DELETE ' + 'FROM t')",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "three literals", sql: "EXEC('DROP ' + 'TABLE ' + 'Foo')",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "unicode concatenation", sql: "EXEC(N'DELETE ' + N'FROM t')",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "named concatenation of literals", sql: "EXEC sp_executesql @stmt = N'DELETE ' + N'FROM t'",
			texts: []string{"DELETE FROM t"}, readable: true,
		},

		// sp_executesql sent as batch text rather than as an RPC.
		{name: "sp_executesql", sql: "EXEC sp_executesql N'DELETE FROM t'", texts: []string{"DELETE FROM t"}, readable: true},
		{
			name: "sp_executesql qualified", sql: "EXEC sys.sp_executesql N'DELETE FROM t'",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "sp_executesql with params", sql: "EXEC sp_executesql N'SELECT @a', N'@a int', @a=1",
			texts: []string{"SELECT @a"}, readable: true,
		},
		{name: "sp_executesql bare", sql: "sp_executesql N'DELETE FROM t'", texts: []string{"DELETE FROM t"}, readable: true},

		// The sp_prepare family, which carries its statement at a *positional*
		// index rather than first. The RPC path always enforced these; the
		// batch-text scanner did not implement positional indexing at all, so this
		// exact spelling was measurably unchecked. Both paths now read one table,
		// shared.MSSQLStatementParamIndex.
		{
			name: "sp_prepare", sql: "EXEC sp_prepare @handle OUT, NULL, N'DROP TABLE Foo', 1",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "sp_prepexec", sql: "EXEC sp_prepexec @handle OUT, NULL, N'DELETE FROM t'",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "sp_cursorprepare", sql: "EXEC sp_cursorprepare @handle OUTPUT, NULL, N'DROP TABLE Foo', 1",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "sp_cursoropen", sql: "EXEC sp_cursoropen @cursor OUT, N'DELETE FROM t', 1, 1",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "sp_cursorprepexec", sql: "EXEC sp_cursorprepexec @h OUT, @c OUT, NULL, N'DROP TABLE Foo', 1",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "sp_prepare named", sql: "EXEC sp_prepare @handle = @h OUTPUT, @params = NULL, @stmt = N'DROP TABLE Foo'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "sp_prepare qualified", sql: "EXEC sys.sp_prepare @handle OUT, NULL, N'DROP TABLE Foo', 1",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},

		// Passed by name. T-SQL takes any procedure's arguments by name in any
		// order, so recognizing only the positional slot let every one of these
		// walk past as an inert literal — clearing read_only, block_ddl,
		// block_copy and the switch refusal in one go. Which names can be the
		// statement is shared.IsMSSQLStatementParamName, the same set the RPC
		// path enforces on.
		{name: "named @stmt", sql: "EXEC sp_executesql @stmt = N'DROP TABLE Foo'", texts: []string{"DROP TABLE Foo"}, readable: true},
		{
			name: "named @statement", sql: "EXEC sp_executesql @statement = N'USE otherdb'",
			texts: []string{"USE otherdb"}, readable: true,
		},
		{
			name: "named @tsql, qualified", sql: "EXEC sys.sp_executesql @tsql = N'DROP TABLE Foo'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "params first", sql: "EXEC sp_executesql @params = N'@x int', @stmt = N'DROP TABLE Foo'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "stmt first", sql: "EXEC sp_executesql @stmt = N'DROP TABLE Foo', @params = N'@x int'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "no spaces around =", sql: "EXEC sp_executesql @stmt=N'DROP TABLE Foo',@params=N'@x int'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "comments around =", sql: "EXEC sp_executesql /*c*/@stmt/*c*/=/*c*/N'DROP TABLE Foo'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "named, case folded", sql: "exec sp_executesql @STMT = N'DROP TABLE Foo'",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "named with values after", sql: "EXEC sp_executesql @stmt = N'SELECT @a', @params = N'@a int', @a = 1",
			texts: []string{"SELECT @a"}, readable: true,
		},
		// An unparenthesized EXEC's argument list ends without punctuation, so
		// the next statement of the batch must not be mistaken for a
		// continuation of the value — that would hand the bypass straight back.
		{
			name: "positional, batch continues", sql: "EXEC sp_executesql N'DROP TABLE Foo'\nSELECT 2",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},
		{
			name: "named, batch continues", sql: "EXEC sp_executesql @stmt = N'DROP TABLE Foo'\nSELECT 2",
			texts: []string{"DROP TABLE Foo"}, readable: true,
		},

		// Not statically decidable: reported as opaque, which is a grant question
		// (ValidateDynamicSQL) and not a parse error.
		{name: "variable", sql: "EXEC(@sql)", opaque: true, readable: true},
		{name: "positional variable", sql: "EXEC sp_executesql @sql", opaque: true, readable: true},
		{name: "named variable", sql: "EXEC sp_executesql @stmt = @sql", opaque: true, readable: true},
		{name: "named concat with a variable", sql: "EXEC sp_executesql @stmt = N'USE ' + @db", opaque: true, readable: true},
		{name: "concatenation with a variable", sql: "EXEC('USE ' + @db)", opaque: true, readable: true},
		{name: "concatenation ending in a variable", sql: "EXEC('DROP ' + 'TABLE ' + @t)", opaque: true, readable: true},
		{name: "sp_prepare variable", sql: "EXEC sp_prepare @handle OUT, NULL, @sql, 1", opaque: true, readable: true},

		// Not dynamic SQL at all — and emphatically not opaque, which would drag
		// every stored-procedure call into the refusal.
		{name: "procedure call", sql: "EXEC dbo.some_proc", readable: true},
		{name: "procedure with args", sql: "EXEC dbo.p @a = 1", readable: true},
		{name: "plain", sql: "SELECT 1", readable: true},
		{name: "literal mentioning exec", sql: "INSERT INTO t VALUES ('EXEC(''DROP TABLE t'')')", readable: true},
		{name: "word prefix", sql: "SELECT execute_count FROM t", readable: true},
		{name: "proc name prefix", sql: "SELECT sp_prepare_log FROM t", readable: true},

		// Fail closed.
		{name: "nested exec", sql: "EXEC('EXEC(''DROP TABLE t'')')", readable: false},
		{name: "nested execute literal", sql: "EXEC('EXECUTE ''DROP TABLE t''')", readable: false},
		{name: "nested variable exec", sql: "EXEC('EXEC(@inner)')", readable: false},
		{name: "nested sp_executesql", sql: "EXEC('EXEC sp_executesql @inner')", readable: false},
		{name: "nested sp_prepare", sql: "EXEC('EXEC sp_prepare @h OUT, NULL, N''DROP TABLE Foo'', 1')", readable: false},
		{name: "unterminated literal", sql: "EXEC('DELETE FROM t", readable: false},
		// The keyword says a statement is being run and dbbat cannot find which
		// argument it is. "Nothing to check" is the one answer that must never
		// come out of that, so it fails closed.
		{name: "statement argument missing", sql: "EXEC sp_executesql @params = N'@x int'", readable: false},
		{name: "no arguments", sql: "EXEC sp_executesql", readable: false},
		{name: "sp_prepare too few arguments", sql: "EXEC sp_prepare @handle OUT, NULL", readable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dyn, readable := MSSQLDynamicSQL(tc.sql)
			tc.check(t, "MSSQLDynamicSQL", dyn, readable)
		})
	}
}

// TestMSSQLStatementProcTableIsComplete guards the property the sp_prepare work
// rests on: the batch-text scanner's keyword list and the positional-index table
// the RPC path reads are one table seen twice. A procedure in one and not the
// other is a spelling one path enforces and the other walks past, which is the
// drift that becomes an authorization bug.
func TestMSSQLStatementProcTableIsComplete(t *testing.T) {
	t.Parallel()

	scanned := map[string]bool{}

	for _, kw := range mssqlDynamicKeywords {
		if kw == kwExec || kw == kwExecute {
			continue
		}

		scanned[kw] = true

		if _, ok := MSSQLStatementParamIndex(kw); !ok {
			t.Errorf("scanner keyword %q has no statement index", kw)
		}
	}

	for proc := range mssqlStatementProcs {
		if !scanned[upperASCII(proc)] {
			t.Errorf("procedure %q has a statement index but the batch scanner never looks for it", proc)
		}
	}
}

func upperASCII(s string) string {
	out := []byte(s)

	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - ('a' - 'A')
		}
	}

	return string(out)
}

// TestDynamicSQLLeavesABareExecAlone guards the negative direction: a bare
// procedure call inside dynamic SQL is a procedure call, not a second level of
// it, and this change has no business turning it into a blanket refusal.
func TestDynamicSQLLeavesABareExecAlone(t *testing.T) {
	t.Parallel()

	dyn, readable := MSSQLDynamicSQL("EXEC('EXEC dbo.p')")
	if !readable {
		t.Fatal("a bare EXEC inside dynamic SQL must not fail closed")
	}

	if len(dyn.Payloads) != 1 || dyn.Payloads[0] != "EXEC dbo.p" {
		t.Fatalf("MSSQLDynamicSQL = %q, want [\"EXEC dbo.p\"]", dyn.Payloads)
	}
}

// TestOracleDynamicSQL pins the Oracle spelling: `EXECUTE IMMEDIATE '<literal>'`,
// which needs no statement of its own to be dangerous — a client wraps it in
// `BEGIN … END;` and the whole thing is classified on the keyword BEGIN, which is
// neither a write nor DDL.
func TestOracleDynamicSQL(t *testing.T) {
	t.Parallel()

	tests := []dynamicCase{
		{
			name: "bare execute immediate", sql: "EXECUTE IMMEDIATE 'DELETE FROM t'",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "inside a block", sql: "BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "lower case", sql: "begin execute immediate 'delete from t'; end;",
			texts: []string{"delete from t"}, readable: true,
		},
		{
			name: "extra whitespace", sql: "BEGIN\n\tEXECUTE\n\tIMMEDIATE\n\t'DELETE FROM t';\nEND;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "comment between", sql: "BEGIN EXECUTE/**/IMMEDIATE/**/'DELETE FROM t'; END;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "quote doubling", sql: "BEGIN EXECUTE IMMEDIATE 'UPDATE t SET a = ''x'''; END;",
			texts: []string{"UPDATE t SET a = 'x'"}, readable: true,
		},
		{
			name: "quote operator", sql: "BEGIN EXECUTE IMMEDIATE q'[DELETE FROM t WHERE a = 'x']'; END;",
			texts: []string{"DELETE FROM t WHERE a = 'x'"}, readable: true,
		},
		{
			name: "national literal", sql: "BEGIN EXECUTE IMMEDIATE n'DROP TABLE t'; END;",
			texts: []string{"DROP TABLE t"}, readable: true,
		},
		{
			name: "literal concatenation", sql: "BEGIN EXECUTE IMMEDIATE 'DELETE ' || 'FROM t'; END;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "using clause", sql: "BEGIN EXECUTE IMMEDIATE 'DELETE FROM t WHERE a = :1' USING v_a; END;",
			texts: []string{"DELETE FROM t WHERE a = :1"}, readable: true,
		},
		{
			name: "two of them",
			sql:  "BEGIN EXECUTE IMMEDIATE 'DELETE FROM a'; EXECUTE IMMEDIATE 'DELETE FROM b'; END;",
			texts: []string{
				"DELETE FROM a", "DELETE FROM b",
			}, readable: true,
		},
		{
			name: "container switch inside a literal",
			sql:  "BEGIN EXECUTE IMMEDIATE 'ALTER SESSION SET CONTAINER=PDB2'; END;",
			texts: []string{
				"ALTER SESSION SET CONTAINER=PDB2",
			}, readable: true,
		},
		{
			name: "dbms_sql.parse", sql: "BEGIN DBMS_SQL.PARSE(c, 'DELETE FROM t', DBMS_SQL.NATIVE); END;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},
		{
			name: "dbms_sql.parse qualified", sql: "BEGIN SYS.DBMS_SQL.PARSE(c, 'DROP TABLE t', DBMS_SQL.NATIVE); END;",
			texts: []string{"DROP TABLE t"}, readable: true,
		},
		{
			name: "dbms_sql.parse named", sql: "BEGIN DBMS_SQL.PARSE(c => cur, statement => 'DELETE FROM t', language_flag => 1); END;",
			texts: []string{"DELETE FROM t"}, readable: true,
		},

		// Built at run time: opaque, refused only under read_only/block_ddl.
		{name: "variable", sql: "BEGIN EXECUTE IMMEDIATE v_stmt; END;", opaque: true, readable: true},
		{name: "concat with a variable", sql: "BEGIN EXECUTE IMMEDIATE 'DELETE FROM ' || v_tab; END;", opaque: true, readable: true},
		{name: "dbms_sql.parse variable", sql: "BEGIN DBMS_SQL.PARSE(c, v_stmt, DBMS_SQL.NATIVE); END;", opaque: true, readable: true},

		// Not dynamic SQL.
		{name: "plain select", sql: "SELECT * FROM t", readable: true},
		{name: "plain block", sql: "BEGIN p(1); END;", readable: true},
		{name: "words in a literal", sql: "INSERT INTO t VALUES ('EXECUTE IMMEDIATE ''DROP TABLE t''')", readable: true},
		{name: "word prefix", sql: "SELECT execute_immediate_count FROM t", readable: true},
		{name: "qualified column", sql: "SELECT t.execute FROM t", readable: true},
		{name: "dbms_sql other call", sql: "BEGIN c := DBMS_SQL.OPEN_CURSOR; END;", readable: true},

		// Fail closed.
		{name: "nested execute immediate", sql: "BEGIN EXECUTE IMMEDIATE 'BEGIN EXECUTE IMMEDIATE ''DROP TABLE t''; END;'; END;", readable: false},
		{name: "nested dbms_sql.parse", sql: "BEGIN EXECUTE IMMEDIATE 'BEGIN DBMS_SQL.PARSE(c, v, 1); END;'; END;", readable: false},
		{name: "unterminated literal", sql: "BEGIN EXECUTE IMMEDIATE 'DELETE FROM t; END;", readable: false},
		{name: "dbms_sql.parse no parens", sql: "BEGIN DBMS_SQL.PARSE 'DELETE FROM t'; END;", readable: false},
		{name: "dbms_sql.parse one argument", sql: "BEGIN DBMS_SQL.PARSE(c); END;", readable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dyn, readable := OracleDynamicSQL(tc.sql)
			tc.check(t, "OracleDynamicSQL", dyn, readable)
		})
	}
}

// TestMySQLDynamicSQL pins the MySQL spelling of the same construct:
// `PREPARE s FROM '<literal>'` runs its payload a statement later, through text
// the checks around it step over. MariaDB's `EXECUTE IMMEDIATE` collapses the
// pair into one statement and is read the same way.
func TestMySQLDynamicSQL(t *testing.T) {
	t.Parallel()

	tests := []dynamicCase{
		{name: "single quoted", sql: "PREPARE s FROM 'USE otherdb'", texts: []string{"USE otherdb"}, readable: true},
		{name: "double quoted", sql: `PREPARE s FROM "USE otherdb"`, texts: []string{"USE otherdb"}, readable: true},
		{name: "mixed case", sql: "prepare S from 'use otherdb'", texts: []string{"use otherdb"}, readable: true},
		{name: "backticked name", sql: "PREPARE `my stmt` FROM 'USE otherdb'", texts: []string{"USE otherdb"}, readable: true},
		{
			name: "extra whitespace", sql: "  PREPARE\n\ts\n\tFROM\n\t'USE otherdb'  ",
			texts: []string{"USE otherdb"}, readable: true,
		},
		{name: "trailing semicolon", sql: "PREPARE s FROM 'USE otherdb';", texts: []string{"USE otherdb"}, readable: true},
		{name: "leading comment", sql: "/* x */ PREPARE s FROM 'USE otherdb'", texts: []string{"USE otherdb"}, readable: true},
		{name: "interior comment", sql: "PREPARE s FROM/**/'USE otherdb'", texts: []string{"USE otherdb"}, readable: true},
		{name: "quote doubling", sql: "PREPARE s FROM 'SELECT ''a'''", texts: []string{"SELECT 'a'"}, readable: true},
		{name: "backslash escape", sql: `PREPARE s FROM 'SELECT \'a\''`, texts: []string{"SELECT 'a'"}, readable: true},
		{name: "ordinary statement", sql: "PREPARE s FROM 'SELECT 1'", texts: []string{"SELECT 1"}, readable: true},
		{name: "write payload", sql: "PREPARE s FROM 'DELETE FROM t'", texts: []string{"DELETE FROM t"}, readable: true},

		// MariaDB's one-statement form.
		{name: "execute immediate", sql: "EXECUTE IMMEDIATE 'DELETE FROM t'", texts: []string{"DELETE FROM t"}, readable: true},
		{
			name: "execute immediate using", sql: "EXECUTE IMMEDIATE 'DELETE FROM t WHERE a = ?' USING @a",
			texts: []string{"DELETE FROM t WHERE a = ?"}, readable: true,
		},

		// Built at run time: opaque, refused only under read_only/block_ddl.
		{name: "variable", sql: "PREPARE s FROM @sql", opaque: true, readable: true},
		{name: "concat", sql: "PREPARE s FROM CONCAT('USE ', @db)", opaque: true, readable: true},
		{name: "execute immediate variable", sql: "EXECUTE IMMEDIATE @sql", opaque: true, readable: true},

		// Not dynamic SQL at all.
		{name: "not a prepare", sql: "SELECT 1", readable: true},
		{name: "xa prepare", sql: "XA PREPARE 'xid'", readable: true},
		{name: "execute by name", sql: "EXECUTE s", readable: true},
		{name: "execute by name using", sql: "EXECUTE s USING @a", readable: true},

		// Fail closed.
		{name: "unterminated literal", sql: "PREPARE s FROM 'USE otherdb", readable: false},
		{name: "adjacent literals", sql: "PREPARE s FROM 'USE ' 'otherdb'", readable: false},
		{name: "no FROM", sql: "PREPARE s 'USE otherdb'", readable: false},
		{name: "nested prepare", sql: "PREPARE s FROM 'PREPARE t FROM ''USE otherdb'''", readable: false},
		{name: "nested prepare from a variable", sql: "PREPARE s FROM 'PREPARE t FROM @x'", readable: false},
		{name: "nested execute", sql: "PREPARE s FROM 'EXECUTE t'", readable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dyn, readable := MySQLDynamicSQL(tc.sql)
			tc.check(t, "MySQLDynamicSQL", dyn, readable)
		})
	}
}

// TestMySQLPreparedStatementNames pins the half of the pair that carries no
// statement text. `EXECUTE <name>` can only be answered from what the matching
// PREPARE said, so the name — and which half it belongs to — has to come out of
// the read.
func TestMySQLPreparedStatementNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		kind MySQLPrepareKind
		stmt string
	}{
		{"prepare", "PREPARE s FROM 'SELECT 1'", MySQLPrepareDefine, "s"},
		{"prepare from a variable", "PREPARE s FROM @sql", MySQLPrepareDefine, "s"},
		{"prepare backticked", "PREPARE `my stmt` FROM 'SELECT 1'", MySQLPrepareDefine, "my stmt"},
		{"execute", "EXECUTE s", MySQLPrepareExecute, "s"},
		{"execute using", "EXECUTE s USING @a, @b", MySQLPrepareExecute, "s"},
		{"execute backticked", "EXECUTE `my stmt`", MySQLPrepareExecute, "my stmt"},
		// EXECUTE IMMEDIATE names nothing: it is dynamic SQL, not half a pair.
		{"execute immediate", "EXECUTE IMMEDIATE 'SELECT 1'", MySQLPrepareNone, ""},
		{"ordinary statement", "SELECT 1", MySQLPrepareNone, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			prepared, readable := MySQLPreparedStatement(tc.sql)
			if !readable {
				t.Fatalf("MySQLPreparedStatement(%q) failed closed", tc.sql)
			}

			if prepared.Kind != tc.kind || prepared.Name != tc.stmt {
				t.Fatalf("MySQLPreparedStatement(%q) = (%v, %q), want (%v, %q)",
					tc.sql, prepared.Kind, prepared.Name, tc.kind, tc.stmt)
			}
		})
	}
}

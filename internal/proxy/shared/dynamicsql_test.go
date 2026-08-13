package shared

import "testing"

// TestMSSQLDynamicSQL pins what is extracted, what is deliberately left alone,
// and what fails closed. The extraction is what makes `read_only`, `block_ddl`
// and the database-switch refusal apply to `EXEC('…')`, so a shape read wrong
// here is a control that silently stops applying.
func TestMSSQLDynamicSQL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		texts    []string
		readable bool
	}{
		{"exec", "EXEC('DELETE FROM t')", []string{"DELETE FROM t"}, true},
		{"execute", "EXECUTE('DROP TABLE t')", []string{"DROP TABLE t"}, true},
		{"lower case", "exec('delete from t')", []string{"delete from t"}, true},
		{"space before paren", "EXEC ('DELETE FROM t')", []string{"DELETE FROM t"}, true},
		{"space inside parens", "EXEC(  'DELETE FROM t'  )", []string{"DELETE FROM t"}, true},
		{"unicode literal", "EXEC(N'USE otherdb')", []string{"USE otherdb"}, true},
		{"quote doubling", "EXEC('SELECT ''a'' FROM t')", []string{"SELECT 'a' FROM t"}, true},
		{"switch and read", "EXEC('USE otherdb; SELECT * FROM secret')", []string{"USE otherdb; SELECT * FROM secret"}, true},
		{"comment before", "/* x */ EXEC('DELETE FROM t')", []string{"DELETE FROM t"}, true},
		{"mid batch", "SELECT 1; EXEC('DELETE FROM t')", []string{"DELETE FROM t"}, true},
		{"two of them", "EXEC('DELETE FROM a'); EXEC('DELETE FROM b')", []string{"DELETE FROM a", "DELETE FROM b"}, true},

		// sp_executesql sent as batch text rather than as an RPC.
		{"sp_executesql", "EXEC sp_executesql N'DELETE FROM t'", []string{"DELETE FROM t"}, true},
		{"sp_executesql qualified", "EXEC sys.sp_executesql N'DELETE FROM t'", []string{"DELETE FROM t"}, true},
		{"sp_executesql with params", "EXEC sp_executesql N'SELECT @a', N'@a int', @a=1", []string{"SELECT @a"}, true},
		{"sp_executesql bare", "sp_executesql N'DELETE FROM t'", []string{"DELETE FROM t"}, true},

		// Not decidable, and deliberately not refused: see docs/mssql.md.
		{"variable", "EXEC(@sql)", nil, true},
		{"concatenation", "EXEC('USE ' + @db)", nil, true},
		{"procedure call", "EXEC dbo.some_proc", nil, true},
		{"procedure with args", "EXEC dbo.p @a = 1", nil, true},

		// Not dynamic SQL at all.
		{"plain", "SELECT 1", nil, true},
		{"literal mentioning exec", "INSERT INTO t VALUES ('EXEC(''DROP TABLE t'')')", nil, true},
		{"word prefix", "SELECT execute_count FROM t", nil, true},

		// Fail closed.
		{"nested exec", "EXEC('EXEC(''DROP TABLE t'')')", nil, false},
		{"nested execute literal", "EXEC('EXECUTE ''DROP TABLE t''')", nil, false},
		{"nested variable exec", "EXEC('EXEC(@inner)')", nil, false},
		{"nested sp_executesql", "EXEC('EXEC sp_executesql @inner')", nil, false},
		{"unterminated literal", "EXEC('DELETE FROM t", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			texts, readable := MSSQLDynamicSQL(tc.sql)
			if readable != tc.readable {
				t.Fatalf("MSSQLDynamicSQL(%q) readable = %v, want %v", tc.sql, readable, tc.readable)
			}

			if len(texts) != len(tc.texts) {
				t.Fatalf("MSSQLDynamicSQL(%q) = %q, want %q", tc.sql, texts, tc.texts)
			}

			for i := range texts {
				if texts[i] != tc.texts[i] {
					t.Fatalf("MSSQLDynamicSQL(%q) = %q, want %q", tc.sql, texts, tc.texts)
				}
			}
		})
	}
}

// TestDynamicSQLLeavesABareExecAlone guards the negative direction: a bare
// procedure call inside dynamic SQL is a procedure call, not a second level of
// it, and this change has no business turning it into a blanket refusal.
func TestDynamicSQLLeavesABareExecAlone(t *testing.T) {
	t.Parallel()

	texts, readable := MSSQLDynamicSQL("EXEC('EXEC dbo.p')")
	if !readable {
		t.Fatal("a bare EXEC inside dynamic SQL must not fail closed")
	}

	if len(texts) != 1 || texts[0] != "EXEC dbo.p" {
		t.Fatalf("MSSQLDynamicSQL = %q, want [\"EXEC dbo.p\"]", texts)
	}
}

// TestMySQLPreparedText pins the MySQL spelling of the same construct:
// `PREPARE s FROM 'USE otherdb'` performs a switch a statement later, through
// text the checks around it step over.
func TestMySQLPreparedText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		text     string
		readable bool
	}{
		{"single quoted", "PREPARE s FROM 'USE otherdb'", "USE otherdb", true},
		{"double quoted", `PREPARE s FROM "USE otherdb"`, "USE otherdb", true},
		{"mixed case", "prepare S from 'use otherdb'", "use otherdb", true},
		{"backticked name", "PREPARE `my stmt` FROM 'USE otherdb'", "USE otherdb", true},
		{"extra whitespace", "  PREPARE\n\ts\n\tFROM\n\t'USE otherdb'  ", "USE otherdb", true},
		{"trailing semicolon", "PREPARE s FROM 'USE otherdb';", "USE otherdb", true},
		{"leading comment", "/* x */ PREPARE s FROM 'USE otherdb'", "USE otherdb", true},
		{"interior comment", "PREPARE s FROM/**/'USE otherdb'", "USE otherdb", true},
		{"quote doubling", "PREPARE s FROM 'SELECT ''a'''", "SELECT 'a'", true},
		{"backslash escape", `PREPARE s FROM 'SELECT \'a\''`, "SELECT 'a'", true},
		{"ordinary statement", "PREPARE s FROM 'SELECT 1'", "SELECT 1", true},

		// Nothing to check: not a PREPARE, or a text the server builds at run
		// time. Deliberately not refused — a blanket refusal of PREPARE would
		// break ordinary application code.
		{"variable", "PREPARE s FROM @sql", "", true},
		{"concat", "PREPARE s FROM CONCAT('USE ', @db)", "", true},
		{"not a prepare", "SELECT 1", "", true},
		{"xa prepare", "XA PREPARE 'xid'", "", true},
		{"execute", "EXECUTE s", "", true},

		// Fail closed.
		{"unterminated literal", "PREPARE s FROM 'USE otherdb", "", false},
		{"adjacent literals", "PREPARE s FROM 'USE ' 'otherdb'", "", false},
		{"no FROM", "PREPARE s 'USE otherdb'", "", false},
		{"nested prepare", "PREPARE s FROM 'PREPARE t FROM ''USE otherdb'''", "", false},
		{"nested prepare from a variable", "PREPARE s FROM 'PREPARE t FROM @x'", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			text, readable := MySQLPreparedText(tc.sql)
			if readable != tc.readable || text != tc.text {
				t.Fatalf("MySQLPreparedText(%q) = (%q, %v), want (%q, %v)",
					tc.sql, text, readable, tc.text, tc.readable)
			}
		})
	}
}

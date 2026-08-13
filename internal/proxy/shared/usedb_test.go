package shared

import "testing"

// TestMySQLUseTarget walks the shapes a client can spell a database switch in.
// The refusal these feed is independent of grant controls, so every one of them
// has to be read correctly on its own.
func TestMySQLUseTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sql    string
		target string
		isUse  bool
	}{
		{"plain", "USE otherdb", "otherdb", true},
		{"lower case", "use otherdb", "otherdb", true},
		{"mixed case", "UsE OtherDB", "OtherDB", true},
		{"leading whitespace", "   \n\tUSE otherdb", "otherdb", true},
		{"trailing semicolon", "USE otherdb;", "otherdb", true},
		{"semicolon and space", "USE otherdb ;  ", "otherdb", true},
		{"extra whitespace", "USE\n\t  otherdb", "otherdb", true},
		{"backticked", "USE `otherdb`", "otherdb", true},
		{"backticked, no space", "USE`otherdb`", "otherdb", true},
		{"backtick doubling", "USE `weird``name`", "weird`name", true},
		{"digits and dollar", "USE 9lives$db", "9lives$db", true},
		{"non-ascii", "USE café", "café", true},

		// Comments: the same evasion sqlcomments.go closed for the validators.
		{"leading block comment", "/* hi */ USE otherdb", "otherdb", true},
		{"interior block comment", "USE/**/otherdb", "otherdb", true},
		{"hash comment", "USE # nothing to see\notherdb", "otherdb", true},
		{"dash comment", "USE -- nothing\notherdb", "otherdb", true},
		{"executable comment", "USE/*!*/otherdb", "otherdb", true},
		{"versioned executable comment", "USE/*!50100*/otherdb", "otherdb", true},

		// A switch dbbat cannot read is still a switch: isUse true with no
		// target, which the caller refuses.
		{"trailing garbage", "USE otherdb SELECT 1", "", true},
		{"stapled statement", "USE otherdb; SELECT 1", "", true},
		{"unterminated backtick", "USE `otherdb", "", true},
		{"no target", "USE", "", true},
		{"no target, semicolon", "USE ;", "", true},

		// Not a switch at all.
		{"select", "SELECT 1", "", false},
		{"prefix only", "USEFUL", "", false},
		{"index hint", "SELECT * FROM t USE INDEX (i)", "", false},
		{"use in a literal", "SELECT 'USE otherdb'", "", false},
		{"empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target, isUse := MySQLUseTarget(tc.sql)
			if isUse != tc.isUse || target != tc.target {
				t.Fatalf("MySQLUseTarget(%q) = (%q, %v), want (%q, %v)",
					tc.sql, target, isUse, tc.target, tc.isUse)
			}
		})
	}
}

// TestMSSQLUseTargets pins the mid-batch scan. A TDS SQLBatch is genuinely
// multi-statement and T-SQL needs no separator, so anchoring at the start would
// leave `SELECT 1\nUSE otherdb` wide open.
func TestMSSQLUseTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		targets  []string
		readable bool
	}{
		{"plain", "USE otherdb", []string{"otherdb"}, true},
		{"case folded", "uSe OtherDB", []string{"OtherDB"}, true},
		{"bracketed", "USE [other db]", []string{"other db"}, true},
		{"bracket doubling", "USE [odd]]name]", []string{"odd]name"}, true},
		{"double quoted", `USE "otherdb"`, []string{"otherdb"}, true},
		{"semicolon", "USE otherdb;", []string{"otherdb"}, true},
		{"comment", "/* x */USE/**/otherdb", []string{"otherdb"}, true},
		{"dash comment", "USE -- x\notherdb", []string{"otherdb"}, true},
		{"temp-object sigil", "USE db#1", []string{"db#1"}, true},

		// The reason this scans rather than anchors.
		{"after a statement", "SELECT 1\nUSE otherdb", []string{"otherdb"}, true},
		{"after a semicolon", "SELECT 1; USE otherdb; SELECT 2", []string{"otherdb"}, true},
		{"several", "USE a; SELECT 1; USE b", []string{"a", "b"}, true},

		// What must not be mistaken for a switch.
		{"literal", "INSERT INTO t VALUES ('USE otherdb')", nil, true},
		{"unicode literal", "INSERT INTO t VALUES (N'USE otherdb')", nil, true},
		{"bracketed column", "SELECT [USE otherdb] FROM t", nil, true},
		{"quoted identifier", `SELECT "USE otherdb" FROM t`, nil, true},
		{"qualified name", "SELECT t.use FROM t", nil, true},
		{"variable", "SELECT @use", nil, true},
		{"word prefix", "SELECT used FROM t", nil, true},
		{"use plan hint", "SELECT 1 OPTION (USE PLAN N'<xml/>')", nil, true},
		{"use hint", "SELECT 1 OPTION (USE HINT ('FORCE_LEGACY_CARDINALITY_ESTIMATION'))", nil, true},
		{"nothing", "SELECT 1", nil, true},

		// Fail closed: a USE whose target cannot be read, or a run left open.
		{"unreadable target", "USE ", nil, false},
		{"unterminated bracket", "USE [otherdb", nil, false},
		{"unterminated literal", "SELECT 'x", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			targets, readable := MSSQLUseTargets(tc.sql)
			if readable != tc.readable {
				t.Fatalf("MSSQLUseTargets(%q) readable = %v, want %v", tc.sql, readable, tc.readable)
			}

			if len(targets) != len(tc.targets) {
				t.Fatalf("MSSQLUseTargets(%q) = %q, want %q", tc.sql, targets, tc.targets)
			}

			for i := range targets {
				if targets[i] != tc.targets[i] {
					t.Fatalf("MSSQLUseTargets(%q) = %q, want %q", tc.sql, targets, tc.targets)
				}
			}
		})
	}
}

package shared

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitPostgresStatements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		want []string
	}{
		{"single", "SELECT 1", []string{"SELECT 1"}},
		{"trailing semicolon", "SELECT 1;", []string{"SELECT 1"}},
		{"three", "SELECT f(); COMMIT; INSERT INTO t VALUES (1)", []string{"SELECT f()", "COMMIT", "INSERT INTO t VALUES (1)"}},
		{"single quoted", "SELECT 'a;b'; SELECT 2", []string{"SELECT 'a;b'", "SELECT 2"}},
		{"doubled quote", "SELECT 'a'';b'; SELECT 2", []string{"SELECT 'a'';b'", "SELECT 2"}},
		{"E string backslash", `SELECT E'a\';b'; SELECT 2`, []string{`SELECT E'a\';b'`, "SELECT 2"}},
		{"plain string backslash", `SELECT 'a\'; SELECT 2`, []string{`SELECT 'a\'`, "SELECT 2"}},
		{"quoted identifier", `SELECT "a;b"; SELECT 2`, []string{`SELECT "a;b"`, "SELECT 2"}},
		{"dollar quote", "SELECT $$a;b$$; SELECT 2", []string{"SELECT $$a;b$$", "SELECT 2"}},
		{"tagged dollar quote", "SELECT $x$a;$$b$x$; SELECT 2", []string{"SELECT $x$a;$$b$x$", "SELECT 2"}},
		{"parameter is not a dollar quote", "SELECT $1; SELECT $2", []string{"SELECT $1", "SELECT $2"}},
		{"comment hides nothing", "SELECT 1 /* ; */; /* x */ COMMIT -- ;\n; SELECT 2", []string{"SELECT 1", "COMMIT", "SELECT 2"}},
		{"blank", " ; ;", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, SplitPostgresStatements(tt.sql))
		})
	}
}

func TestIsPostgresTxEnd(t *testing.T) {
	t.Parallel()

	for sql, want := range map[string]bool{
		"COMMIT": true, "commit work": true, "END": true, "ABORT": true, "ROLLBACK": true,
		"/*x*/COMMIT AND CHAIN": true, "ROLLBACK TO SAVEPOINT a": false, "COMMIT PREPARED 'x'": false,
		"SELECT 1": false, "BEGIN": false, "": false,
	} {
		assert.Equal(t, want, IsPostgresTxEnd(sql), sql)
	}
}

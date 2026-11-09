package shared

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestBracketedExecProcedureFailsClosed pins the regression: a SQL-carrying
// system procedure invoked through a bracketed or quoted name used to slip past
// read_only/block_ddl because skipOpaque stepped over the delimited identifier
// before the procedure keyword could be matched, so the inner statement was
// never extracted and the batch keyword scan skipped the literal that held it.
//
// These must all fail closed under a restrictive grant. The matching bare
// spellings (TestBareExecStillExtracted) stay readable, so this is not a blanket
// refusal of every EXEC.
func TestBracketedExecProcedureFailsClosed(t *testing.T) {
	t.Parallel()

	grant := controlGrant(store.ControlReadOnly)

	refused := []string{
		"EXEC [sp_executesql] N'DROP TABLE t'",
		"EXEC [sys].[sp_executesql] N'DROP TABLE t'",
		`EXEC "sp_executesql" N'DROP TABLE t'`,
		"EXEC [sp_executesql] @stmt = N'DELETE FROM t'",
		"EXECUTE [sp_prepare] @h OUT, NULL, N'DROP TABLE t', 1",
	}

	for _, sql := range refused {
		err := validateMSSQL(sql, grant)
		require.Errorf(t, err, "expected a refusal for %q", sql)
	}
}

// TestBareExecStillExtracted is the positive control: the bare procedure
// spellings are still read and checked on their inner statement, so a benign
// dynamic SELECT stays allowed under read_only and a write is still refused on
// its extracted text (not on the wrapper).
func TestBareExecStillExtracted(t *testing.T) {
	t.Parallel()

	readOnly := controlGrant(store.ControlReadOnly)

	require.NoError(t, validateMSSQL("EXEC sp_executesql N'SELECT 1'", readOnly))
	require.NoError(t, validateMSSQL("EXEC sys.sp_executesql N'SELECT 1'", readOnly))
	require.ErrorIs(t, validateMSSQL("EXEC sp_executesql N'DROP TABLE t'", readOnly), ErrReadOnlyViolation)
}

// TestBracketedExecAllowedUnderFullWrite is the second positive control: the
// fail-closed behavior is scoped to grants that refuse opaque dynamic SQL
// (read_only/block_ddl). A full-write grant, which may run DDL anyway, is left
// untouched — the bracketed form is not refused there.
func TestBracketedExecAllowedUnderFullWrite(t *testing.T) {
	t.Parallel()

	fullWrite := controlGrant()

	require.NoError(t, validateMSSQL("EXEC [sp_executesql] N'DROP TABLE t'", fullWrite))
}

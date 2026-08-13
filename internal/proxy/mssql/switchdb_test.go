package mssql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// switchSession is sessionWithGrant plus the database the grant was issued on.
// The grant carries no controls: full write is the case that matters, because a
// full-write grant on one database is still not a grant on another.
func switchSession(t *testing.T) *session {
	t.Helper()

	s := sessionWithGrant(t, grantWithControls())
	s.database = &store.Server{Name: "prod-entry", DatabaseName: "AppDb"}

	return s
}

// TestUseIsRefusedOnEveryShape is the regression test for the hole: TDS pins
// nothing, so `USE otherdb` in an ordinary batch moved the session to a database
// the grant never covered — and every statement after it was still recorded
// against the granted one.
func TestUseIsRefusedOnEveryShape(t *testing.T) {
	t.Parallel()

	refused := []string{
		"USE otherdb",
		"use otherdb",
		"USE otherdb;",
		"USE [other db]",
		`USE "otherdb"`,
		"/* x */ USE otherdb",
		"USE/**/otherdb",
		"USE -- x\notherdb",
		// A T-SQL batch needs no separator, which is why the scan is not
		// anchored at the start of the batch.
		"SELECT 1\nUSE otherdb",
		"SELECT 1; USE otherdb; SELECT 2",
		"USE AppDb; USE otherdb",
		// Unreadable is refused, not forwarded.
		"USE [otherdb",
		"SELECT 1 USE ",
	}

	for _, sql := range refused {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			err := switchSession(t).validate(statement{text: sql, enforce: []string{sql}})
			require.ErrorIs(t, err, ErrDatabaseSwitchBlocked)
		})
	}
}

// TestUseOfSessionDatabaseIsAllowed pins the allowed case as hard as the
// refusal. A driver re-stating its own database must keep working, under the
// dbbat entry's name or the real one, in any casing SQL Server itself would
// accept.
func TestUseOfSessionDatabaseIsAllowed(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"USE AppDb",
		"USE appdb",
		"USE [AppDb]",
		"USE [prod-entry]",
		"use APPDB;",
		"USE AppDb; SELECT 1",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, switchSession(t).validate(statement{text: sql, enforce: []string{sql}}))
		})
	}
}

// TestUseLookalikesAreNotSwitches covers what must not be mistaken for one — the
// price of scanning mid-batch instead of anchoring.
func TestUseLookalikesAreNotSwitches(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"INSERT INTO t VALUES ('USE otherdb')",
		"INSERT INTO t VALUES (N'USE otherdb')",
		"SELECT [USE otherdb] FROM t",
		`SELECT "USE otherdb" FROM t`,
		"SELECT used FROM t",
		"SELECT 1 OPTION (USE PLAN N'<xml/>')",
		"SELECT 1 OPTION (USE HINT ('FORCE_LEGACY_CARDINALITY_ESTIMATION'))",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, switchSession(t).validate(statement{text: sql, enforce: []string{sql}}))
		})
	}
}

// TestUseRefusalIgnoresGrantControls states the binding decision: the refusal is
// not one of the grant's controls, so it fires under a full-write grant and its
// error is the switch refusal rather than a read-only one.
func TestUseRefusalIgnoresGrantControls(t *testing.T) {
	t.Parallel()

	for _, controls := range [][]string{
		{},
		{store.ControlReadOnly},
		{store.ControlBlockDDL},
		{store.ControlBlockCopy},
	} {
		s := switchSession(t)
		s.grant = grantWithControls(controls...)

		err := s.validate(statement{text: "USE otherdb", enforce: []string{"USE otherdb"}})
		require.ErrorIsf(t, err, ErrDatabaseSwitchBlocked, "controls %v", controls)
	}
}

// TestUseInsideSPExecuteSQLIsRefused: the same text wrapped in sp_executesql
// takes the identical path, so it must reach the identical decision.
func TestUseInsideSPExecuteSQLIsRefused(t *testing.T) {
	t.Parallel()

	s := switchSession(t)

	requests, err := parseRPC(rpcByID(spExecuteSQL, nvarcharMaxParam("", "USE otherdb")))
	require.NoError(t, err)

	st := s.describeRPC(requests)
	require.NoError(t, st.refusal)
	require.ErrorIs(t, s.validate(st), ErrDatabaseSwitchBlocked)
}

// TestDynamicSQLIsCheckedNotSteppedOver is the regression test for the second
// bypass: `EXEC(<literal>)` runs the literal's contents as a batch, so the USE
// scan stepped over it as an inert string and every prefix-shaped control
// classified the outer statement as an EXEC that writes nothing.
func TestDynamicSQLIsCheckedNotSteppedOver(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sql      string
		controls []string
		want     error
	}{
		{
			"USE inside EXEC, full-write grant",
			"EXEC('USE otherdb; SELECT * FROM secret_table')",
			nil, ErrDatabaseSwitchBlocked,
		},
		{
			"USE inside EXEC, every control set",
			"EXEC('USE otherdb')",
			[]string{store.ControlReadOnly, store.ControlBlockDDL, store.ControlBlockCopy},
			ErrDatabaseSwitchBlocked,
		},
		{
			"write inside EXEC under read_only",
			"EXEC('DELETE FROM t')",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"DDL inside EXECUTE under block_ddl",
			"EXECUTE('DROP TABLE t')",
			[]string{store.ControlBlockDDL},
			shared.ErrDDLBlocked,
		},
		{
			"unicode literal",
			"EXEC(N'DELETE FROM t')",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"doubled quotes inside the literal",
			"EXEC('DELETE FROM t WHERE name = ''O''''Brien''')",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"space before the paren",
			"EXEC ('DELETE FROM t')",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"mid-batch",
			"SELECT 1; EXEC('DELETE FROM t')",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"bulk copy inside EXEC under block_copy",
			"EXEC('BULK INSERT t FROM ''f.dat''')",
			[]string{store.ControlBlockCopy},
			ErrBulkCopyBlocked,
		},
		{
			// sp_executesql as batch text rather than as an RPC. The RPC form
			// was always enforced; this spelling was not.
			"sp_executesql as batch text",
			"EXEC sp_executesql N'DELETE FROM t'",
			[]string{store.ControlReadOnly},
			shared.ErrReadOnlyViolation,
		},
		{
			"sp_executesql as batch text, qualified",
			"EXEC sys.sp_executesql N'USE otherdb'",
			nil, ErrDatabaseSwitchBlocked,
		},
		{
			// One level is unwrapped; a second is refused rather than unwrapped
			// further, because stopping silently would be the same hole again.
			"nested dynamic SQL fails closed",
			"EXEC('EXEC(''DROP TABLE t'')')",
			nil, ErrDynamicSQLNotCheckable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := switchSession(t)
			s.grant = grantWithControls(tc.controls...)

			require.ErrorIs(t, s.validate(statement{text: tc.sql, enforce: []string{tc.sql}}), tc.want)
		})
	}
}

// TestBenignDynamicSQLStillRuns is the other half, and it matters as much: this
// must not become a blanket refusal of dynamic SQL. A read under a read-only
// grant is allowed whether it is spelled directly or through EXEC.
func TestBenignDynamicSQLStillRuns(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"EXEC('SELECT 1')",
		"EXECUTE('SELECT * FROM t WHERE name = ''O''''Brien''')",
		"EXEC sp_executesql N'SELECT @a', N'@a int', @a = 1",
		"EXEC('USE AppDb; SELECT 1')",
		// Undecidable, and deliberately not refused: dbbat cannot read what a
		// variable holds. See the limitation note in docs/mssql.md.
		"EXEC(@sql)",
		"EXEC('USE ' + @db)",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			s := switchSession(t)
			s.grant = grantWithControls(store.ControlReadOnly)

			require.NoError(t, s.validate(statement{text: sql, enforce: []string{sql}}))
		})
	}
}

// TestUseWithoutAResolvedDatabaseIsRefused: with no session database there is
// nothing to compare against, and "cannot tell" is refused, never allowed.
func TestUseWithoutAResolvedDatabaseIsRefused(t *testing.T) {
	t.Parallel()

	s := sessionWithGrant(t, grantWithControls())

	require.ErrorIs(t,
		s.validate(statement{text: "USE otherdb", enforce: []string{"USE otherdb"}}),
		ErrDatabaseSwitchBlocked)
}

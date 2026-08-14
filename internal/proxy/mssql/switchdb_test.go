package mssql

import (
	"testing"

	"github.com/stretchr/testify/require"

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

// TestUseWithoutAResolvedDatabaseIsRefused: with no session database there is
// nothing to compare against, and "cannot tell" is refused, never allowed.
func TestUseWithoutAResolvedDatabaseIsRefused(t *testing.T) {
	t.Parallel()

	s := sessionWithGrant(t, grantWithControls())

	require.ErrorIs(t,
		s.validate(statement{text: "USE otherdb", enforce: []string{"USE otherdb"}}),
		ErrDatabaseSwitchBlocked)
}

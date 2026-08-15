package shared

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// controlGrant is a grant carrying exactly the named controls.
func controlGrant(controls ...string) *store.Grant {
	return &store.Grant{Definition: &store.GrantDefinition{Controls: controls}}
}

// validateMSSQL is what the SQL Server proxy composes: the statement's own
// classification, plus the same classification over whatever it would run
// dynamically. The proxy adds the database-switch and bulk-copy checks to the
// per-statement half; those are pinned in internal/proxy/mssql.
func validateMSSQL(sql string, grant *store.Grant) error {
	if err := ValidateQuery(sql, grant); err != nil {
		return err
	}

	dyn, readable := MSSQLDynamicSQL(sql)

	return ValidateDynamicSQL(dyn, readable, grant, func(text string) error {
		return ValidateQuery(text, grant)
	})
}

// TestDynamicSQLRefusalTable pins the table the spec measured. Every row marked
// "allowed" there was a live grant escape: the classification behind read_only
// and block_ddl is prefix-shaped, so a statement whose first keyword is PREPARE,
// BEGIN or EXEC was classified on the wrapper and never on what runs.
//
// The grant carries **both** controls, as the measurement did.
func TestDynamicSQLRefusalTable(t *testing.T) {
	t.Parallel()

	grant := controlGrant(store.ControlReadOnly, store.ControlBlockDDL)

	tests := []struct {
		name     string
		sql      string
		validate func(string, *store.Grant) error
		want     error
	}{
		{"mysql direct", "DELETE FROM t", ValidateMySQLQuery, ErrReadOnlyViolation},
		{"mysql prepare", "PREPARE s FROM 'DELETE FROM t'", ValidateMySQLQuery, ErrReadOnlyViolation},
		{
			"oracle execute immediate in a block",
			"BEGIN EXECUTE IMMEDIATE 'DELETE FROM t'; END;",
			ValidateOracleQuery, ErrReadOnlyViolation,
		},
		{
			"mssql literal concatenation",
			"EXEC('DELETE ' + 'FROM t')",
			validateMSSQL, ErrReadOnlyViolation,
		},
		{
			// This one was already blocked before the change — but only by
			// accident of oracleBlockedPatterns being a *substring* match, which
			// reaches inside a literal by luck rather than by design. It stays
			// blocked, and is now blocked by design too:
			// TestOracleContainerSwitchIsBlockedByDesign below.
			"oracle container switch inside a literal",
			"BEGIN EXECUTE IMMEDIATE 'ALTER SESSION SET CONTAINER=PDB2'; END;",
			ValidateOracleQuery, ErrOraclePatternBlocked,
		},
		{
			"oracle container switch, q-quoted",
			"BEGIN EXECUTE IMMEDIATE q'[ALTER SESSION SET CONTAINER=PDB2]'; END;",
			ValidateOracleQuery, ErrOraclePatternBlocked,
		},
		{
			"oracle dbms_sql.parse",
			"BEGIN DBMS_SQL.PARSE(c, 'DROP TABLE t', DBMS_SQL.NATIVE); END;",
			ValidateOracleQuery, ErrReadOnlyViolation,
		},
		{
			"mariadb execute immediate",
			"EXECUTE IMMEDIATE 'DROP TABLE t'",
			ValidateMySQLQuery, ErrReadOnlyViolation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.validate(tc.sql, grant), tc.want)
		})
	}
}

// TestOracleContainerSwitchIsBlockedByDesign is the row of the spec's table that
// was already blocked, and the reason it was listed: it was blocked only because
// oracleBlockedPatterns is a *substring* match, so it reached inside the literal
// by accident of matching style. Now the payload is extracted and the whole
// Oracle validator runs over it as a statement of its own — the same answer, for
// a reason that does not depend on luck.
func TestOracleContainerSwitchIsBlockedByDesign(t *testing.T) {
	t.Parallel()

	dyn, readable := OracleDynamicSQL("BEGIN EXECUTE IMMEDIATE 'ALTER SESSION SET CONTAINER=PDB2'; END;")
	require.True(t, readable)
	require.Equal(t, []string{"ALTER SESSION SET CONTAINER=PDB2"}, dyn.Payloads)

	// The payload on its own, with no wrapper for a substring match to reach
	// through, and under a grant with no controls at all — the switch is refused
	// whatever the grant says, exactly as a direct one is.
	require.ErrorIs(t, ValidateOracleQuery(dyn.Payloads[0], controlGrant()), ErrOraclePatternBlocked)
}

// TestBenignDynamicSQLIsStillAllowed is the boundary, and it matters as much as
// the table above: this must not become a blanket refusal of dynamic SQL. A read
// is a read whether it is spelled directly or wrapped, under a grant carrying
// both controls.
func TestBenignDynamicSQLIsStillAllowed(t *testing.T) {
	t.Parallel()

	grant := controlGrant(store.ControlReadOnly, store.ControlBlockDDL)

	tests := []struct {
		sql      string
		validate func(string, *store.Grant) error
	}{
		{"SELECT 1", ValidateMySQLQuery},
		{"PREPARE s FROM 'SELECT 1'", ValidateMySQLQuery},
		{"EXECUTE IMMEDIATE 'SELECT 1'", ValidateMySQLQuery},
		{"EXEC('SELECT 1')", validateMSSQL},
		{"EXEC('SELECT ' + '1')", validateMSSQL},
		{"EXEC sp_prepare @handle OUT, NULL, N'SELECT 1', 1", validateMSSQL},
		{"BEGIN EXECUTE IMMEDIATE 'SELECT 1 FROM DUAL'; END;", ValidateOracleQuery},
		{"BEGIN EXECUTE IMMEDIATE 'SELECT ' || '1 FROM DUAL'; END;", ValidateOracleQuery},
		{"BEGIN DBMS_SQL.PARSE(c, 'SELECT 1 FROM DUAL', DBMS_SQL.NATIVE); END;", ValidateOracleQuery},
		{"BEGIN my_pkg.read_data(:1, :2); END;", ValidateOracleQuery},
	}

	for _, tc := range tests {
		t.Run(tc.sql, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, tc.validate(tc.sql, grant))
		})
	}
}

// opaqueByProtocol are the undecidable forms: the payload is assembled by the
// server from values dbbat never sees.
var opaqueByProtocol = []struct {
	sql      string
	validate func(string, *store.Grant) error
}{
	{"PREPARE s FROM @sql", ValidateMySQLQuery},
	{"PREPARE s FROM CONCAT('DELETE FROM ', @t)", ValidateMySQLQuery},
	{"EXECUTE IMMEDIATE @sql", ValidateMySQLQuery},
	{"BEGIN EXECUTE IMMEDIATE v_stmt; END;", ValidateOracleQuery},
	{"BEGIN EXECUTE IMMEDIATE 'DELETE FROM ' || v_tab; END;", ValidateOracleQuery},
	{"BEGIN DBMS_SQL.PARSE(c, v_stmt, DBMS_SQL.NATIVE); END;", ValidateOracleQuery},
	{"EXEC(@sql)", validateMSSQL},
	{"EXEC('DELETE FROM ' + @t)", validateMSSQL},
	{"EXEC sp_executesql @stmt = @sql", validateMSSQL},
}

// TestOpaqueDynamicSQLRefusedUnderTheTwoControls: "dbbat cannot tell what this
// runs" must not resolve to "allow" for a grant that says the session may not
// write or may not change schema. The refusal names its own cause so an operator
// can tell it from an ordinary blocked write.
func TestOpaqueDynamicSQLRefusedUnderTheTwoControls(t *testing.T) {
	t.Parallel()

	for _, control := range []string{store.ControlReadOnly, store.ControlBlockDDL} {
		for _, tc := range opaqueByProtocol {
			t.Run(control+" "+tc.sql, func(t *testing.T) {
				t.Parallel()

				require.ErrorIs(t, tc.validate(tc.sql, controlGrant(control)), ErrDynamicSQLOpaque)
			})
		}
	}
}

// TestOpaqueDynamicSQLAllowedWithoutThem is the owner's explicit boundary and the
// easiest thing to over-implement: the refusal is **not** "any control is set".
// `block_copy` alone and a grant with no controls at all are unaffected, because
// dynamic SQL defeats neither — ORMs and migration tools build SQL at run time
// constantly, so the traffic a wider refusal breaks is well-behaved traffic.
func TestOpaqueDynamicSQLAllowedWithoutThem(t *testing.T) {
	t.Parallel()

	for _, controls := range [][]string{nil, {store.ControlBlockCopy}} {
		label := "no controls"
		if len(controls) > 0 {
			label = controls[0]
		}

		for _, tc := range opaqueByProtocol {
			t.Run(label+" "+tc.sql, func(t *testing.T) {
				t.Parallel()

				require.NoError(t, tc.validate(tc.sql, controlGrant(controls...)))
			})
		}
	}
}

// TestRefusesOpaqueDynamicSQL states the predicate directly, including the
// fail-closed backstop: a grant with no definition attached reports every control
// (store.AccessGrant.Controls) and so refuses.
func TestRefusesOpaqueDynamicSQL(t *testing.T) {
	t.Parallel()

	require.True(t, RefusesOpaqueDynamicSQL(controlGrant(store.ControlReadOnly)))
	require.True(t, RefusesOpaqueDynamicSQL(controlGrant(store.ControlBlockDDL)))
	require.True(t, RefusesOpaqueDynamicSQL(controlGrant(store.ControlReadOnly, store.ControlBlockCopy)))
	require.False(t, RefusesOpaqueDynamicSQL(controlGrant()))
	require.False(t, RefusesOpaqueDynamicSQL(controlGrant(store.ControlBlockCopy)))
	require.True(t, RefusesOpaqueDynamicSQL(&store.Grant{}), "a shapeless grant fails closed")
	require.True(t, RefusesOpaqueDynamicSQL(nil), "so does no grant at all")
}

// TestUnreadableDynamicSQLIsRefusedWhateverTheGrantSays: one level is unwrapped,
// a second is refused. Unlike the opaque case this does not depend on the
// controls — dbbat could not read the statement at all, which is not a permission
// question.
func TestUnreadableDynamicSQLIsRefusedWhateverTheGrantSays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sql      string
		validate func(string, *store.Grant) error
	}{
		{"PREPARE s FROM 'PREPARE t FROM ''SELECT 1'''", ValidateMySQLQuery},
		{"PREPARE s FROM 'SELECT 1", ValidateMySQLQuery},
		{"BEGIN EXECUTE IMMEDIATE 'BEGIN EXECUTE IMMEDIATE ''SELECT 1''; END;'; END;", ValidateOracleQuery},
		{"BEGIN DBMS_SQL.PARSE(c); END;", ValidateOracleQuery},
		{"EXEC('EXEC(''SELECT 1'')')", validateMSSQL},
		{"EXEC sp_executesql @params = N'@x int'", validateMSSQL},
	}

	for _, controls := range [][]string{nil, {store.ControlBlockCopy}, {store.ControlReadOnly}} {
		for _, tc := range tests {
			t.Run(tc.sql, func(t *testing.T) {
				t.Parallel()

				require.ErrorIs(t,
					tc.validate(tc.sql, controlGrant(controls...)),
					ErrDynamicSQLNotCheckable)
			})
		}
	}
}

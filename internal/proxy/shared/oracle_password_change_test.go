package shared

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestValidateOracleQuery_BlocksIdentifiedPasswordChange pins Oracle's own
// password-change spelling, which carries no PASSWORD token: refused whatever
// the grant, because every dbbat session shares the one upstream account. A
// full-write grant was the hole.
func TestValidateOracleQuery_BlocksIdentifiedPasswordChange(t *testing.T) {
	t.Parallel()

	grants := map[string]*store.Grant{
		"full write": controlGrant(),
		"read_only":  controlGrant(store.ControlReadOnly),
		"block_ddl":  controlGrant(store.ControlBlockDDL),
	}
	refused := []string{
		"ALTER USER app IDENTIFIED BY x",
		"ALTER USER app IDENTIFIED BY x REPLACE old",
		`ALTER USER "app;x" IDENTIFIED BY "pw"`,
		"alter user app identified by x",
		"CREATE USER u2 IDENTIFIED BY x",
		"ALTER USER app IDENTIFIED EXTERNALLY",
		"ALTER USER app IDENTIFIED GLOBALLY AS 'CN=app'",
		"CREATE USER u2 IDENTIFIED EXTERNALLY",
		"ALTER/**/USER app IDENTIFIED BY x",
		"ALTER USER app /* c */ IDENTIFIED -- c\n BY x",
		"ALTER\nUSER app\nIDENTIFIED BY x",
		"BEGIN EXECUTE IMMEDIATE 'ALTER USER app IDENTIFIED BY x'; END;",
		"ALTER USER u PASSWORD EXPIRE", // the existing PASSWORD arm, unchanged
	}

	for name, grant := range grants {
		for _, sql := range refused {
			require.ErrorIs(t, ValidateOracleQuery(sql, grant), ErrPasswordChangeBlocked, "%s: %s", name, sql)
		}
	}

	full := grants["full write"]
	for _, sql := range []string{
		"ALTER TABLE t ADD (identified NUMBER)",
		"SELECT identified FROM t",
		"CREATE TABLE t (identified NUMBER)",
		"ALTER USER app ACCOUNT LOCK",
		"ALTER USER app DEFAULT TABLESPACE users",
		"SELECT 'ALTER USER app IDENTIFIED BY x' FROM dual",
	} {
		require.NoError(t, ValidateOracleQuery(sql, full), "should allow: %s", sql)
	}

	// The IDENTIFIED arm is Oracle-scoped: the standard path PostgreSQL uses
	// leaves it to the controls, as before.
	require.NoError(t, ValidateQuery("ALTER USER app IDENTIFIED BY x", full))
}

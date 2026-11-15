package shared

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

// TestValidateMySQLQuery_BlocksIdentifiedPasswordChange pins MySQL's own
// password-change spelling, which carries no PASSWORD token: refused whatever
// the grant, because every dbbat session shares the one upstream account.
func TestValidateMySQLQuery_BlocksIdentifiedPasswordChange(t *testing.T) {
	t.Parallel()

	grants := map[string]*store.Grant{
		"full write": {Definition: &store.GrantDefinition{Controls: []string{}}},
		"read_only":  {Definition: &store.GrantDefinition{Controls: []string{store.ControlReadOnly}}},
		"block_ddl":  {Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockDDL}}},
	}
	refused := []string{
		"ALTER USER CURRENT_USER() IDENTIFIED BY 'x'",
		"ALTER USER u IDENTIFIED BY 'x'",
		"ALTER USER u IDENTIFIED WITH caching_sha2_password BY 'x'",
		"ALTER USER u IDENTIFIED WITH mysql_native_password AS '*hash'",
		"ALTER USER IF EXISTS 'a;b'@'%' IDENTIFIED BY 'x'",
		"alter user u identified by 'x'",
		"CREATE USER a IDENTIFIED BY 'x'",
		"CREATE OR REPLACE USER a IDENTIFIED BY 'x'",
		"GRANT ALL ON *.* TO u IDENTIFIED BY 'x'",
		"ALTER/**/USER u IDENTIFIED BY 'x'",
		"ALTER USER u /* c */ IDENTIFIED BY 'x'",
		"ALTER USER u # c\n IDENTIFIED BY 'x'",
		"SELECT 1; ALTER USER u IDENTIFIED BY 'x'",
		"PREPARE s FROM 'ALTER USER u IDENTIFIED BY ''x'''",
		"ALTER USER u PASSWORD EXPIRE", // the existing PASSWORD arm, unchanged
	}

	for name, grant := range grants {
		for _, sql := range refused {
			require.ErrorIs(t, ValidateMySQLQuery(sql, grant), ErrPasswordChangeBlocked, "%s: %s", name, sql)
		}

		// MariaDB's SET PASSWORD spelling stays on mysqlBlockedPatterns.
		require.ErrorIs(t, ValidateMySQLQuery("SET PASSWORD FOR u = PASSWORD('x')", grant),
			ErrMySQLPatternBlocked, name)
	}

	full := grants["full write"]
	for _, sql := range []string{
		"SELECT identified FROM audit",
		"ALTER TABLE users ADD COLUMN identified bool",
		"CREATE TABLE t (identified bool)",
		"ALTER USER u ACCOUNT LOCK",
	} {
		require.NoError(t, ValidateMySQLQuery(sql, full), "should allow: %s", sql)
	}

	// The IDENTIFIED arm is MySQL-only: the standard path PostgreSQL uses is
	// unchanged.
	require.NoError(t, ValidateQuery("SELECT identified FROM audit", full))
	require.ErrorIs(t, ValidateQuery("ALTER ROLE app ENCRYPTED PASSWORD 'x'", full), ErrPasswordChangeBlocked)
}

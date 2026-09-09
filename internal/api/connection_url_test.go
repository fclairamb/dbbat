package api

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/store"
)

func makeEndpoints() store.ResolvedEndpoints {
	return store.ResolvedEndpoints{
		PGHost:    "db.example.com",
		PGPort:    5432,
		OraHost:   "db.example.com",
		OraPort:   1521,
		MySQLHost: "db.example.com",
		MySQLPort: 3306,
		MSSQLHost: "db.example.com",
		MSSQLPort: 1434,
	}
}

func makeUser() *store.User {
	return &store.User{UID: uuid.New(), Username: "alice"}
}

// makeDB builds a server row whose dbbat entry name and upstream database name
// are *different* on purpose: they are the two halves this builder has to keep
// straight (the entry name selects the dbbat server, the database name is what
// the client asks the target for), and a fixture that conflated them is exactly
// what hid the bug this file now guards.
func makeDB(protocol, name, databaseName, sslMode string) *store.Server {
	return &store.Server{
		UID:          uuid.New(),
		Name:         name,
		DatabaseName: databaseName,
		Username:     "target_user",
		Protocol:     protocol,
		SSLMode:      sslMode,
	}
}

func TestBuildConnectionURL_PostgreSQL(t *testing.T) {
	t.Parallel()

	endpoints := makeEndpoints()
	user := makeUser()

	t.Run("sslmode=require included in URL", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "require")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.Contains(t, info.URL, "sslmode=require")
		assert.Equal(t, "uri", info.Format)
		assert.Equal(t, store.ProtocolPostgreSQL, info.Protocol)
	})

	t.Run("sslmode=prefer omitted from URL", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.NotContains(t, info.URL, "sslmode")
	})

	t.Run("sslmode=disable included in URL", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "disable")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.Contains(t, info.URL, "sslmode=disable")
	})

	t.Run("URL contains host, port, dbname, username, key", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "analytics", "analytics_upstream", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "dbb_secret")
		require.True(t, ok)
		assert.Contains(t, info.URL, "db.example.com")
		assert.Contains(t, info.URL, "alice")
		assert.Contains(t, info.URL, "dbb_secret")
		assert.Contains(t, info.URL, "analytics")
		assert.NotEmpty(t, info.URL)
	})

	// The whole point of the spec: the path is the *real* upstream database,
	// and the dbbat entry is selected through the username instead. An IDE
	// rewrites the path per database as it walks the catalog; it never touches
	// the username.
	t.Run("path is the upstream database, entry selected in the username", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "demo_datalake_ro", "demo_datalake", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Equal(t,
			"postgresql://alice%23demo_datalake_ro:{DBBAT_KEY}@db.example.com:5432/demo_datalake",
			info.URL)
		// The response still names the dbbat entry, which is what the UI labels.
		assert.Equal(t, "demo_datalake_ro", info.DatabaseName)
	})

	t.Run("a server with no upstream database name falls back to the entry name", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "only_an_entry", "", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/only_an_entry")
	})

	t.Run("apiKey empty produces {DBBAT_KEY} placeholder", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Contains(t, info.URL, keyPlaceholder)
	})

	t.Run("disabled protocol (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.PGPort = 0
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "prefer")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})

	t.Run("username is dbbat user.Username not db.Username", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "mydb_upstream", "prefer")
		db.Username = "target_db_user"
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, user.Username)
		assert.NotContains(t, info.URL, "target_db_user")
	})
}

func TestBuildConnectionURL_MySQL(t *testing.T) {
	t.Parallel()

	endpoints := makeEndpoints()
	user := makeUser()

	t.Run("mysql protocol produces mysql:// scheme", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolMySQL, "shopdb", "shopdb_upstream", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "mysql://")
		assert.Equal(t, "uri", info.Format)
	})

	t.Run("mariadb protocol also produces mysql:// scheme", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolMariaDB, "shopdb", "shopdb_upstream", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "mysql://")
	})

	t.Run("disabled protocol (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.MySQLPort = 0
		db := makeDB(store.ProtocolMySQL, "shopdb", "shopdb_upstream", "")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})

	t.Run("path is the upstream database, entry selected in the username", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolMySQL, "shop_ro", "shopdb", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Equal(t,
			"mysql://alice%23shop_ro:{DBBAT_KEY}@db.example.com:3306/shopdb",
			info.URL)
	})
}

func TestBuildConnectionURL_MSSQL(t *testing.T) {
	t.Parallel()

	endpoints := makeEndpoints()
	user := makeUser()

	t.Run("keyword connection string naming the upstream database", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolMSSQL, "reporting_ro", "Reporting", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Equal(t,
			"Server=db.example.com,1434;Database=Reporting;User Id=alice#reporting_ro;"+
				"Password={DBBAT_KEY};Encrypt=true",
			info.URL)
		assert.Equal(t, "connection-string", info.Format)
		assert.Equal(t, store.ProtocolMSSQL, info.Protocol)
		assert.Equal(t, "reporting_ro", info.DatabaseName)
	})

	t.Run("disabled protocol (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.MSSQLPort = 0
		db := makeDB(store.ProtocolMSSQL, "reporting_ro", "Reporting", "")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})
}

func TestBuildConnectionURL_Oracle(t *testing.T) {
	t.Parallel()

	endpoints := makeEndpoints()
	user := makeUser()

	t.Run("oracle advertises dbbat logical name over shared service name", func(t *testing.T) {
		t.Parallel()
		// Several dbbat databases can share one upstream SERVICE_NAME (e.g.
		// MUTU01): the advertised connect string must carry the unambiguous
		// dbbat name, which the session resolver matches first (exact name).
		svc := "MUTU01"
		db := makeDB(store.ProtocolOracle, "abyla_i3f", "abyla_i3f_upstream", "")
		db.OracleServiceName = &svc
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/abyla_i3f")
		assert.NotContains(t, info.URL, "MUTU01")
	})

	t.Run("oracle falls back to OracleServiceName when dbbat name is not EZ-Connect safe", func(t *testing.T) {
		t.Parallel()
		svc := "MUTU02"
		db := makeDB(store.ProtocolOracle, "aby", "aby_upstream", "")
		db.Name = "abyla_abymutualise02 (Admin)"
		db.OracleServiceName = &svc
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/MUTU02")
		assert.NotContains(t, info.URL, "(Admin)")
	})

	t.Run("oracle falls back to DatabaseName when name unsafe and no service name", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "MYDB", "MYDB_upstream", "")
		db.Name = "my db (RO)"
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/MYDB")
	})

	t.Run("oracle URL uses EZ-Connect format (no ://)", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "ORCL", "ORCL_upstream", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.NotContains(t, info.URL, "://")
		assert.Equal(t, "ez-connect", info.Format)
	})

	t.Run("oracle disabled (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.OraPort = 0
		db := makeDB(store.ProtocolOracle, "ORCL", "ORCL_upstream", "")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})

	t.Run("oracle placeholder URL contains {DBBAT_KEY}", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "ORCL", "ORCL_upstream", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Contains(t, info.URL, keyPlaceholder)
	})
}

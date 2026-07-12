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
	}
}

func makeUser() *store.User {
	return &store.User{UID: uuid.New(), Username: "alice"}
}

func makeDB(protocol, dbName, sslMode string) *store.Database {
	return &store.Database{
		UID:          uuid.New(),
		Name:         dbName,
		DatabaseName: dbName,
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
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "require")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.Contains(t, info.URL, "sslmode=require")
		assert.Equal(t, "uri", info.Format)
		assert.Equal(t, store.ProtocolPostgreSQL, info.Protocol)
	})

	t.Run("sslmode=prefer omitted from URL", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.NotContains(t, info.URL, "sslmode")
	})

	t.Run("sslmode=disable included in URL", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "disable")
		info, ok := BuildConnectionURL(db, user, endpoints, "mykey")
		require.True(t, ok)
		assert.Contains(t, info.URL, "sslmode=disable")
	})

	t.Run("URL contains host, port, dbname, username, key", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "analytics", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "dbb_secret")
		require.True(t, ok)
		assert.Contains(t, info.URL, "db.example.com")
		assert.Contains(t, info.URL, "alice")
		assert.Contains(t, info.URL, "dbb_secret")
		assert.Contains(t, info.URL, "analytics")
		assert.NotEmpty(t, info.URL)
	})

	t.Run("apiKey empty produces {DBBAT_KEY} placeholder", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "prefer")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Contains(t, info.URL, keyPlaceholder)
	})

	t.Run("disabled protocol (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.PGPort = 0
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "prefer")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})

	t.Run("username is dbbat user.Username not db.Username", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolPostgreSQL, "mydb", "prefer")
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
		db := makeDB(store.ProtocolMySQL, "shopdb", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "mysql://")
		assert.Equal(t, "uri", info.Format)
	})

	t.Run("mariadb protocol also produces mysql:// scheme", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolMariaDB, "shopdb", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "mysql://")
	})

	t.Run("disabled protocol (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.MySQLPort = 0
		db := makeDB(store.ProtocolMySQL, "shopdb", "")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})
}

func TestBuildConnectionURL_Oracle(t *testing.T) {
	t.Parallel()

	endpoints := makeEndpoints()
	user := makeUser()

	t.Run("oracle uses OracleServiceName when set", func(t *testing.T) {
		t.Parallel()
		svc := "ORCLPDB"
		db := makeDB(store.ProtocolOracle, "ORCL", "")
		db.OracleServiceName = &svc
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/ORCLPDB")
		assert.NotContains(t, info.URL, "/ORCL\x00") // DatabaseName "ORCL" is not used as path when service name set
	})

	t.Run("oracle falls back to DatabaseName when OracleServiceName not set", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "MYDB", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.Contains(t, info.URL, "/MYDB")
	})

	t.Run("oracle URL uses EZ-Connect format (no ://)", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "ORCL", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "key")
		require.True(t, ok)
		assert.NotContains(t, info.URL, "://")
		assert.Equal(t, "ez-connect", info.Format)
	})

	t.Run("oracle disabled (port 0) returns false", func(t *testing.T) {
		t.Parallel()
		e := makeEndpoints()
		e.OraPort = 0
		db := makeDB(store.ProtocolOracle, "ORCL", "")
		_, ok := BuildConnectionURL(db, user, e, "key")
		assert.False(t, ok)
	})

	t.Run("oracle placeholder URL contains {DBBAT_KEY}", func(t *testing.T) {
		t.Parallel()
		db := makeDB(store.ProtocolOracle, "ORCL", "")
		info, ok := BuildConnectionURL(db, user, endpoints, "")
		require.True(t, ok)
		assert.Contains(t, info.URL, keyPlaceholder)
	})
}

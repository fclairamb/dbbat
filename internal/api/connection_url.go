package api

import (
	"fmt"
	"net"
	"net/url"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// ConnectionInfo describes a ready-to-paste connection URL for a single database.
type ConnectionInfo struct {
	DatabaseUID  uuid.UUID `json:"database_uid"`
	DatabaseName string    `json:"database_name"`
	Protocol     string    `json:"protocol"`
	Format       string    `json:"format"` // "uri", "ez-connect" or "connection-string"
	URL          string    `json:"url"`
}

const keyPlaceholder = "{DBBAT_KEY}"

// keyEncoder escapes the API key for the URL shape being built, passing the
// {DBBAT_KEY} placeholder through untouched so it stays copy-pasteable.
type keyEncoder func(string) string

// proxyUsername renders the login name a client must present: the dbbat user,
// then the `#` selector naming the dbbat server entry.
//
// The username is the one field every client, driver and IDE preserves verbatim
// on every connection it opens — the database field is not: DataGrip and
// DBeaver treat a data source as a *server* and reconnect per database with
// whatever name the catalog gave them. Carrying the selector in the username is
// what frees the database field to hold the real upstream name, which is what
// those catalogs hand back. See internal/proxy/shared.ResolveTarget.
func proxyUsername(user *store.User, db *store.Server) string {
	return user.Username + shared.UsernameServerSeparator + db.Name
}

// upstreamDatabase is the database name a client should ask for: the real one
// on the target. Servers registered without one (rare, and meaningless for the
// protocols that need a database) fall back to the dbbat entry name, which the
// resolver still accepts as an exact match.
func upstreamDatabase(db *store.Server) string {
	if db.DatabaseName != "" {
		return db.DatabaseName
	}

	return db.Name
}

// info builds the envelope every branch returns. DatabaseName is deliberately
// the *dbbat entry* name, not the upstream one: it is what the UI labels the
// entry with and what the rest of the API calls it.
func info(db *store.Server, format, rawURL string) (ConnectionInfo, bool) {
	return ConnectionInfo{
		DatabaseUID:  db.UID,
		DatabaseName: db.Name,
		Protocol:     db.Protocol,
		Format:       format,
		URL:          rawURL,
	}, true
}

// BuildConnectionURL builds a connection URL for the given database, user, and key.
// When apiKey is "", the placeholder "{DBBAT_KEY}" is substituted in the password slot.
// Returns (ConnectionInfo{}, false) when the protocol's resolved port is 0.
func BuildConnectionURL(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	apiKey string,
) (ConnectionInfo, bool) {
	isPlaceholder := apiKey == ""
	key := apiKey

	if isPlaceholder {
		key = keyPlaceholder
	}

	// encodeKey encodes the key for use in a URI, but passes the placeholder through unescaped.
	encodeKey := keyEncoder(func(k string) string {
		if isPlaceholder {
			return k
		}

		return url.PathEscape(k)
	})

	switch db.Protocol {
	case store.ProtocolPostgreSQL:
		return buildPostgreSQLURL(db, user, endpoints, key, encodeKey)

	case store.ProtocolMySQL, store.ProtocolMariaDB:
		return buildMySQLURL(db, user, endpoints, key, encodeKey)

	case store.ProtocolMongoDB:
		return buildMongoDBURL(db, user, endpoints, key, encodeKey)

	case store.ProtocolMSSQL:
		return buildMSSQLConnectionString(db, user, endpoints, key)

	case store.ProtocolOracle:
		return buildOracleEZConnect(db, user, endpoints, key)
	}

	return ConnectionInfo{}, false
}

// buildPostgreSQLURL emits the libpq URI form. The path carries the *real*
// upstream database and the dbbat entry is selected from the username, because
// a PostgreSQL client that walks `pg_database` reconnects per database with the
// name the catalog gave it and would otherwise lose the entry.
func buildPostgreSQLURL(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	key string,
	encodeKey keyEncoder,
) (ConnectionInfo, bool) {
	if endpoints.PGPort == 0 {
		return ConnectionInfo{}, false
	}

	// The '#' becomes %23 inside a URL userinfo; libpq and pgjdbc decode it,
	// and an IDE with a separate user field takes it typed as-is.
	rawURL := fmt.Sprintf("postgresql://%s:%s@%s/%s",
		url.PathEscape(proxyUsername(user, db)),
		encodeKey(key),
		net.JoinHostPort(endpoints.PGHost, fmt.Sprintf("%d", endpoints.PGPort)),
		url.PathEscape(upstreamDatabase(db)),
	)

	if db.SSLMode != "" && db.SSLMode != "prefer" {
		rawURL += "?sslmode=" + url.QueryEscape(db.SSLMode)
	}

	return info(db, "uri", rawURL)
}

// buildMySQLURL emits the same shape for MySQL and MariaDB.
func buildMySQLURL(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	key string,
	encodeKey keyEncoder,
) (ConnectionInfo, bool) {
	if endpoints.MySQLPort == 0 {
		return ConnectionInfo{}, false
	}

	rawURL := fmt.Sprintf("mysql://%s:%s@%s/%s",
		url.PathEscape(proxyUsername(user, db)),
		encodeKey(key),
		net.JoinHostPort(endpoints.MySQLHost, fmt.Sprintf("%d", endpoints.MySQLPort)),
		url.PathEscape(upstreamDatabase(db)),
	)

	return info(db, "uri", rawURL)
}

// buildMongoDBURL keeps MongoDB's own convention: authSource carries the dbbat
// database name — dbbat resolves the target from it (contract §5), terminates
// PLAIN, then re-authenticates upstream with SCRAM. The path selects the
// upstream database for commands. directConnection=true stops the driver from
// dialing the real host advertised by topology discovery (we present
// standalone).
func buildMongoDBURL(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	key string,
	encodeKey keyEncoder,
) (ConnectionInfo, bool) {
	if endpoints.MongoPort == 0 {
		return ConnectionInfo{}, false
	}

	rawURL := fmt.Sprintf(
		"mongodb://%s:%s@%s/%s?authMechanism=PLAIN&authSource=%s&tls=true&directConnection=true",
		url.PathEscape(user.Username),
		encodeKey(key),
		net.JoinHostPort(endpoints.MongoHost, fmt.Sprintf("%d", endpoints.MongoPort)),
		url.PathEscape(db.DatabaseName),
		url.PathEscape(db.Name),
	)

	return info(db, "uri", rawURL)
}

// buildMSSQLConnectionString emits the ADO.NET / ODBC keyword syntax rather
// than a URI: it is what SSMS, Azure Data Studio and sqlcmd take, and what
// every SQL Server driver documents. Host and port are comma-separated,
// TDS-style. No escaping is applied — a value containing ';' or '=' would need
// quoting, which dbbat entry names and usernames do not use.
func buildMSSQLConnectionString(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	key string,
) (ConnectionInfo, bool) {
	if endpoints.MSSQLPort == 0 {
		return ConnectionInfo{}, false
	}

	rawURL := fmt.Sprintf("Server=%s,%d;Database=%s;User Id=%s;Password=%s;Encrypt=true",
		endpoints.MSSQLHost,
		endpoints.MSSQLPort,
		upstreamDatabase(db),
		proxyUsername(user, db),
		key,
	)

	return info(db, "connection-string", rawURL)
}

// buildOracleEZConnect advertises the dbbat logical database name, not the raw
// upstream service name: the Oracle session resolver tries an exact
// GetServerByName lookup first, so the logical name is unambiguous even when
// several dbbat databases proxy the same upstream SERVICE_NAME (e.g. five
// databases sharing a mutualized MUTU01). A raw service name would resolve to
// an arbitrary one of them. Names that would break an EZ-Connect string
// (spaces, parens, …) fall back to the previous service-name behavior.
func buildOracleEZConnect(
	db *store.Server,
	user *store.User,
	endpoints store.ResolvedEndpoints,
	key string,
) (ConnectionInfo, bool) {
	if endpoints.OraPort == 0 {
		return ConnectionInfo{}, false
	}

	serviceOrDB := db.DatabaseName
	if db.OracleServiceName != nil && *db.OracleServiceName != "" {
		serviceOrDB = *db.OracleServiceName
	}

	if isEZConnectSafeName(db.Name) {
		serviceOrDB = db.Name
	}

	// Oracle EZ-Connect format: user/key@host:port/service
	rawURL := fmt.Sprintf("%s/%s@%s:%d/%s",
		user.Username,
		key,
		endpoints.OraHost,
		endpoints.OraPort,
		serviceOrDB,
	)

	return info(db, "ez-connect", rawURL)
}

// isEZConnectSafeName reports whether a dbbat database name can be embedded
// verbatim as the service part of an Oracle EZ-Connect string
// (user/key@host:port/name). Letters, digits, '_', '.' and '-' are safe;
// anything else (spaces, parentheses — e.g. "abyla_abymutualise02 (Admin)")
// would break the connect-string parse, so callers fall back to the raw
// upstream service name for those.
func isEZConnectSafeName(name string) bool {
	if name == "" {
		return false
	}

	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '-':
		default:
			return false
		}
	}

	return true
}

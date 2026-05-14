package api

import (
	"fmt"
	"net"
	"net/url"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// ConnectionInfo describes a ready-to-paste connection URL for a single database.
type ConnectionInfo struct {
	DatabaseUID  uuid.UUID `json:"database_uid"`
	DatabaseName string    `json:"database_name"`
	Protocol     string    `json:"protocol"`
	Format       string    `json:"format"` // "uri" or "ez-connect"
	URL          string    `json:"url"`
}

const keyPlaceholder = "{API_KEY}"

// BuildConnectionURL builds a connection URL for the given database, user, and key.
// When apiKey is "", the placeholder "{API_KEY}" is substituted in the password slot.
// Returns (ConnectionInfo{}, false) when the protocol's resolved port is 0.
func BuildConnectionURL(
	db *store.Database,
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
	encodeKey := func(k string) string {
		if isPlaceholder {
			return k
		}
		return url.PathEscape(k)
	}

	switch db.Protocol {
	case store.ProtocolPostgreSQL:
		if endpoints.PGPort == 0 {
			return ConnectionInfo{}, false
		}
		rawURL := fmt.Sprintf("postgresql://%s:%s@%s/%s",
			url.PathEscape(user.Username),
			encodeKey(key),
			net.JoinHostPort(endpoints.PGHost, fmt.Sprintf("%d", endpoints.PGPort)),
			url.PathEscape(db.DatabaseName),
		)
		if db.SSLMode != "" && db.SSLMode != "prefer" {
			rawURL += "?sslmode=" + url.QueryEscape(db.SSLMode)
		}
		return ConnectionInfo{
			DatabaseUID:  db.UID,
			DatabaseName: db.Name,
			Protocol:     db.Protocol,
			Format:       "uri",
			URL:          rawURL,
		}, true

	case store.ProtocolMySQL, store.ProtocolMariaDB:
		if endpoints.MySQLPort == 0 {
			return ConnectionInfo{}, false
		}
		rawURL := fmt.Sprintf("mysql://%s:%s@%s/%s",
			url.PathEscape(user.Username),
			encodeKey(key),
			net.JoinHostPort(endpoints.MySQLHost, fmt.Sprintf("%d", endpoints.MySQLPort)),
			url.PathEscape(db.DatabaseName),
		)
		return ConnectionInfo{
			DatabaseUID:  db.UID,
			DatabaseName: db.Name,
			Protocol:     db.Protocol,
			Format:       "uri",
			URL:          rawURL,
		}, true

	case store.ProtocolOracle:
		if endpoints.OraPort == 0 {
			return ConnectionInfo{}, false
		}
		serviceOrDB := db.DatabaseName
		if db.OracleServiceName != nil && *db.OracleServiceName != "" {
			serviceOrDB = *db.OracleServiceName
		}
		// Oracle EZ-Connect format: user/key@host:port/service
		rawURL := fmt.Sprintf("%s/%s@%s:%d/%s",
			user.Username,
			key,
			endpoints.OraHost,
			endpoints.OraPort,
			serviceOrDB,
		)
		return ConnectionInfo{
			DatabaseUID:  db.UID,
			DatabaseName: db.Name,
			Protocol:     db.Protocol,
			Format:       "ez-connect",
			URL:          rawURL,
		}, true
	}

	return ConnectionInfo{}, false
}

package conncheck

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/fclairamb/dbbat/internal/proxy/mongodb"
	"github.com/fclairamb/dbbat/internal/proxy/mssql"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// errMissingConfig marks a probe that could not even be attempted because the
// server row is incomplete. It is reported at the config stage: nothing was
// dialed, so calling it a handshake failure would point the admin at the
// network instead of at the field they left empty.
var errMissingConfig = errors.New("conncheck: incomplete server configuration")

// dialFunc opens the transport to the target — directly or through the SSH
// bastion chain. The probes never dial themselves: they inject this so the
// tunnel is exercised by the exact same code path the proxies use.
type dialFunc func(ctx context.Context) (net.Conn, error)

// probe performs a protocol-level login against the target over a connection
// obtained from dial. A nil error means the credentials were accepted.
type probe func(ctx context.Context, srv *store.Server, dial dialFunc) error

// probeFor returns the login probe for a protocol, or nil when dbbat has no
// standalone dial path for it.
func probeFor(protocol string) probe {
	switch protocol {
	case store.ProtocolPostgreSQL:
		return probePostgres
	case store.ProtocolMySQL, store.ProtocolMariaDB:
		return probeMySQL
	case store.ProtocolMongoDB:
		return probeMongo
	case store.ProtocolOracle:
		return probeOracle
	case store.ProtocolMSSQL:
		return probeMSSQL
	default:
		return nil
	}
}

// probeAppName identifies dbbat's connectivity checks upstream, so a DBA
// looking at pg_stat_activity / session_connect_attrs can tell a probe from a
// real proxied session.
func probeAppName() string {
	return "dbbat/" + version.Version + " connectivity-check"
}

// probePostgres runs the *proxy's* upstream connect — same dial, same ssl_mode
// negotiation, same login — and hangs up the moment it succeeds. It is not a
// reimplementation with a client library any more: a green check now means the
// exact code path a real session takes got in.
//
// The probe used to build a pgconn.Config here, and that second implementation
// is what drifted: it mapped ssl_mode=prefer to plaintext-only while the proxy
// sent an SSLRequest, so every target whose pg_hba.conf demands encryption
// reported "the database refused the stored credentials" for a server the proxy
// connects to fine.
func probePostgres(ctx context.Context, srv *store.Server, dial dialFunc) error {
	up, err := upstream.ConnectPostgres(ctx, upstream.DialFunc(dial), upstream.PostgresConfig{
		Host:            srv.Host,
		Username:        srv.Username,
		Password:        srv.Password,
		Database:        srv.DatabaseName,
		ApplicationName: probeAppName(),
		SSLMode:         srv.SSLMode,
	}, nil)
	if err != nil {
		return err
	}

	return up.Close()
}

// probeMySQL runs the MySQL proxy's own upstream connect and hangs up. Same
// TLS policy, same connection attributes, same capability flags — there is only
// one implementation left for them to be the same as.
func probeMySQL(ctx context.Context, srv *store.Server, dial dialFunc) error {
	conn, err := upstream.ConnectMySQL(ctx, upstream.DialFunc(dial), upstream.MySQLConfig{
		Host:        srv.Host,
		Port:        srv.Port,
		Username:    srv.Username,
		Password:    srv.Password,
		Database:    srv.DatabaseName,
		ProgramName: probeAppName(),
		SSLMode:     srv.SSLMode,
	})
	if err != nil {
		return err
	}

	return conn.Close()
}

// probeMongo runs the MongoDB proxy's own upstream connect — dial, ssl_mode
// policy, hello handshake, SCRAM-SHA-256 — and hangs up.
//
// It used to drive the mongo-driver instead, which meant a second TLS policy, a
// second SCRAM client and a topology layer that could wander off to a
// replica-set member the SSH tunnel does not cover. The proxy's connector talks
// to exactly the host in the row, because that is all it can do.
func probeMongo(ctx context.Context, srv *store.Server, dial dialFunc) error {
	conn, err := mongodb.ConnectUpstream(ctx, upstream.DialFunc(dial), mongodb.UpstreamConfig{
		Host:       srv.Host,
		Username:   srv.Username,
		Password:   srv.Password,
		AuthSource: srv.MongoAuthSourceOrDefault(),
		AppName:    probeAppName(),
		SSLMode:    srv.SSLMode,
	})
	if err != nil {
		return err
	}

	return conn.Close()
}

// probeMSSQL runs the SQL Server proxy's own upstream connect — dial, the
// ssl_mode policy expressed as a PRELOGIN ENCRYPTION offer, the encapsulated
// TLS handshake, LOGIN7 — and hangs up.
//
// It passes a nil LOGIN7 template because there is no client session to copy
// one from; the connector then sends a plain TDS 7.4 login, which is what the
// proxy would send for a client that asked for nothing special.
func probeMSSQL(ctx context.Context, srv *store.Server, dial dialFunc) error {
	conn, err := mssql.ConnectUpstream(ctx, upstream.DialFunc(dial), upstream.MSSQLConfig{
		Host:     srv.Host,
		Port:     srv.Port,
		Username: srv.Username,
		Password: srv.Password,
		Database: srv.DatabaseName,
		AppName:  probeAppName(),
		SSLMode:  srv.SSLMode,
	}, nil)
	if err != nil {
		return err
	}

	return conn.Close()
}

// probeOracle runs a real TNS Connect + TTC login against the target.
//
// This is the one protocol where the proxy's own code cannot be the probe: its
// TNS/TTC handshake only exists inside a live session with a downstream client
// attached (it relays the client's own Connect descriptor byte-for-byte). The
// standalone client half lives in upstream.ConnectOracle, which still shares
// this package's ssl_mode policy and the injected transport, so the probe goes
// through the exact SSH tunnel a real session would.
func probeOracle(ctx context.Context, srv *store.Server, dial dialFunc) error {
	service := oracleServiceName(srv)
	if service == "" {
		return fmt.Errorf("%w: set the Oracle service name (or a database name to use as one)", errMissingConfig)
	}

	conn, err := upstream.ConnectOracle(ctx, upstream.DialFunc(dial), upstream.OracleConfig{
		Host:        srv.Host,
		Port:        srv.Port,
		ServiceName: service,
		Username:    srv.Username,
		Password:    srv.Password,
		ProgramName: probeAppName(),
		SSLMode:     srv.SSLMode,
	})
	if err != nil {
		return err
	}

	return conn.Close()
}

// oracleServiceName resolves the service name to present upstream, mirroring
// the proxy's own SERVICE_NAME rewrite: the dedicated column when set, the
// database name otherwise.
func oracleServiceName(srv *store.Server) string {
	if srv.OracleServiceName != nil && *srv.OracleServiceName != "" {
		return *srv.OracleServiceName
	}

	return srv.DatabaseName
}

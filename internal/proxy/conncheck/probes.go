package conncheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	goora "github.com/sijms/go-ora/v3"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// errOracleConnectorShape guards against a go-ora upgrade changing what
// NewConnector returns: without the concrete type we cannot inject the
// transport, and a probe that silently dialed on its own would bypass the SSH
// tunnel entirely.
var errOracleConnectorShape = errors.New("go-ora connector does not expose a dialer hook")

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

// probeMySQL logs in with go-mysql's client — the same library and the same
// ConnectWithDialer entry point the MySQL proxy uses upstream.
func probeMySQL(ctx context.Context, srv *store.Server, dial dialFunc) error {
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

	dialer := func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dial(dialCtx)
	}

	conn, err := gomysqlclient.ConnectWithDialer(
		ctx, "tcp", addr,
		srv.Username, srv.Password, srv.DatabaseName,
		dialer,
		func(c *gomysqlclient.Conn) error {
			switch srv.SSLMode {
			case "require":
				c.UseSSL(true)
			case "verify-ca", "verify-full":
				c.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: srv.Host})
			}
			// Same defense-in-depth as the proxy: never let an upstream ask the
			// dbbat host to read local files.
			c.UnsetCapability(gomysql.CLIENT_LOCAL_FILES)
			c.SetAttributes(map[string]string{"program_name": probeAppName()})

			return nil
		},
	)
	if err != nil {
		return err
	}

	return conn.Close()
}

// probeMongo authenticates against the target with the mongo driver, pinned to
// a direct connection so it exercises exactly the host in the row rather than
// letting topology discovery wander off to a replica-set member the tunnel does
// not cover.
func probeMongo(ctx context.Context, srv *store.Server, dial dialFunc) error {
	opts := options.Client().
		SetHosts([]string{net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))}).
		SetDirect(true).
		SetAppName(probeAppName()).
		SetServerSelectionTimeout(DefaultTimeout).
		SetDialer(dialerFunc(dial)).
		SetAuth(options.Credential{
			Username:   srv.Username,
			Password:   srv.Password,
			AuthSource: srv.MongoAuthSourceOrDefault(),
		})

	client, err := mongo.Connect(opts)
	if err != nil {
		return err
	}

	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	// Connect is lazy — the ping is what actually performs the handshake and
	// SCRAM exchange, so it is the ping's error that classifies the outcome.
	return client.Ping(ctx, readpref.Primary())
}

// dialerFunc adapts our dialFunc to the mongo driver's ContextDialer.
type dialerFunc func(ctx context.Context) (net.Conn, error)

// DialContext satisfies options.ContextDialer.
func (d dialerFunc) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return d(ctx)
}

// probeOracle runs a real TNS Connect + TTC login against the target with
// go-ora, the same client library the Oracle proxy's own test suite drives.
//
// The proxy itself cannot be reused here: its TNS/TTC handshake only exists
// inside a live session with a downstream client attached (it relays the
// client's own Connect descriptor byte-for-byte). go-ora is the standalone
// client half of that same protocol, and it accepts an injected transport, so
// the probe still goes through the exact SSH tunnel a real session would.
func probeOracle(ctx context.Context, srv *store.Server, dial dialFunc) error {
	service := oracleServiceName(srv)
	if service == "" {
		return fmt.Errorf("%w: set the Oracle service name (or a database name to use as one)", errMissingConfig)
	}

	opts := map[string]string{"PROGRAM": probeAppName()}

	// Deliberately no TIMEOUT/READ TIMEOUT option: it makes go-ora arm real
	// socket deadlines, which an SSH-tunneled conn cannot honor. The conncheck
	// harness bounds the probe by closing the transport instead.
	switch srv.SSLMode {
	case "require":
		opts["SSL"] = "TRUE"
		opts["SSL VERIFY"] = "FALSE"
	case "verify-ca", "verify-full":
		opts["SSL"] = "TRUE"
		opts["SSL VERIFY"] = "TRUE"
	}

	dsn := goora.BuildUrl(srv.Host, srv.Port, service, srv.Username, srv.Password, opts)

	connector, ok := goora.NewConnector(dsn).(*goora.OracleConnector)
	if !ok {
		return errOracleConnectorShape
	}

	connector.Dialer(dialerFunc(func(dialCtx context.Context) (net.Conn, error) {
		conn, err := dial(dialCtx)
		if err != nil {
			return nil, err
		}

		return tolerantDeadlineConn{Conn: conn}, nil
	}))

	conn, err := connector.Connect(ctx)
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

// tolerantDeadlineConn makes a conn that cannot do deadlines usable by a client
// that sets them unconditionally. go-ora arms a deadline before every read and
// write — the zero time (meaning "no deadline") when no timeout is configured —
// while golang.org/x/crypto/ssh channels reject SetDeadline outright. Clearing
// a deadline that never existed is a no-op, so swallowing the error there is
// safe; a *real* deadline that the transport cannot honor is still reported,
// so a future caller cannot silently lose its timeout.
type tolerantDeadlineConn struct {
	net.Conn
}

func (c tolerantDeadlineConn) SetDeadline(t time.Time) error {
	return ignoreZeroDeadline(c.Conn.SetDeadline(t), t)
}

func (c tolerantDeadlineConn) SetReadDeadline(t time.Time) error {
	return ignoreZeroDeadline(c.Conn.SetReadDeadline(t), t)
}

func (c tolerantDeadlineConn) SetWriteDeadline(t time.Time) error {
	return ignoreZeroDeadline(c.Conn.SetWriteDeadline(t), t)
}

// ignoreZeroDeadline drops the error from clearing a deadline on a transport
// that does not support them.
func ignoreZeroDeadline(err error, t time.Time) error {
	if err != nil && t.IsZero() {
		return nil
	}

	return err
}

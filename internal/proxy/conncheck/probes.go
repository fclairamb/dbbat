package conncheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/fclairamb/dbbat/internal/store"
	"github.com/fclairamb/dbbat/internal/version"
)

// dialFunc opens the transport to the target — directly or through the SSH
// bastion chain. The probes never dial themselves: they inject this so the
// tunnel is exercised by the exact same code path the proxies use.
type dialFunc func(ctx context.Context) (net.Conn, error)

// probe performs a protocol-level login against the target over a connection
// obtained from dial. A nil error means the credentials were accepted.
type probe func(ctx context.Context, srv *store.Server, dial dialFunc) error

// probeFor returns the login probe for a protocol, or nil when dbbat has no
// standalone dial path for it (Oracle: its TTC handshake only exists inside a
// live proxy session, so a check there is reachability-only).
func probeFor(protocol string) probe {
	switch protocol {
	case store.ProtocolPostgreSQL:
		return probePostgres
	case store.ProtocolMySQL, store.ProtocolMariaDB:
		return probeMySQL
	case store.ProtocolMongoDB:
		return probeMongo
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

// probePostgres opens a real startup+auth exchange with the upstream using the
// same pgx stack the PostgreSQL proxy speaks, over the injected transport.
func probePostgres(ctx context.Context, srv *store.Server, dial dialFunc) error {
	// pgconn.Config must come from ParseConfig; every field that matters is
	// overridden below, so the environment cannot influence the probe.
	cfg, err := pgconn.ParseConfig("postgres://")
	if err != nil {
		return fmt.Errorf("build postgres probe config: %w", err)
	}

	cfg.Host = srv.Host
	cfg.Port = uint16(srv.Port) //nolint:gosec // port is a validated 1..65535 column
	cfg.User = srv.Username
	cfg.Password = srv.Password
	cfg.Database = srv.DatabaseName
	cfg.ConnectTimeout = DefaultTimeout
	cfg.RuntimeParams = map[string]string{"application_name": probeAppName()}
	cfg.Fallbacks = nil
	cfg.DialFunc = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dial(dialCtx)
	}
	cfg.TLSConfig = postgresTLSConfig(srv)

	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}

	return conn.Close(ctx)
}

// postgresTLSConfig mirrors libpq ssl_mode semantics for the probe. "prefer"
// deliberately maps to nil (plaintext): pgconn cannot express opportunistic TLS
// on a single config, and a probe that silently fell back would report a
// misleading stage.
func postgresTLSConfig(srv *store.Server) *tls.Config {
	switch srv.SSLMode {
	case "require":
		return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // "require" is encryption without verification, by definition
	case "verify-ca", "verify-full":
		return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: srv.Host}
	default:
		return nil
	}
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
			// Same defence-in-depth as the proxy: never let an upstream ask the
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

package upstream

import (
	"context"
	"net"
	"strconv"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// MySQLConfig is everything a MySQL/MariaDB login needs from a server row.
type MySQLConfig struct {
	// Host and Port name the target. They build the address go-mysql reports
	// in its errors and the TLS server name; the transport itself comes from
	// the injected DialFunc.
	Host string
	Port int
	// Username, Password and Database are the stored upstream credentials.
	Username string
	Password string
	Database string
	// ProgramName is the "program_name" connection attribute, so a DBA reading
	// performance_schema.session_connect_attrs can tell who is connected.
	ProgramName string
	// SSLMode is the row's ssl_mode; interpreted by PlanFor.
	SSLMode string
}

// ConnectMySQL opens an authenticated MySQL/MariaDB connection over the
// injected transport. It is the one implementation both the proxy and the
// connectivity check use, so a green check exercises the proxy's exact login.
//
// go-mysql handles auth-plugin negotiation (caching_sha2_password on MySQL 8.x)
// transparently — that is the plugin support dbbat deliberately does not
// implement on its own server-facing side.
func ConnectMySQL(ctx context.Context, dial DialFunc, cfg MySQLConfig) (*gomysqlclient.Conn, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	dialer := func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dial(dialCtx)
	}

	return gomysqlclient.ConnectWithDialer(
		ctx, "tcp", addr,
		cfg.Username, cfg.Password, cfg.Database,
		dialer,
		mysqlOptions(cfg),
	)
}

// mysqlOptions configures the connection before its handshake: TLS per the
// ssl_mode plan, the dbbat program_name attribute, and an explicit refusal of
// CLIENT_LOCAL_FILES.
func mysqlOptions(cfg MySQLConfig) func(*gomysqlclient.Conn) error {
	return func(c *gomysqlclient.Conn) error {
		// Only the modes that *mandate* TLS set a config here. go-mysql decides
		// to encrypt from the handshake's CLIENT_SSL capability, and this
		// callback runs before that handshake is read, so an opportunistic mode
		// cannot be expressed as a single attempt — it needs the redial chain
		// ConnectMySQL wraps around this.
		plan := PlanFor(cfg.SSLMode, cfg.Host)
		if plan.RequiresTLS() {
			c.SetTLSConfig(plan.TLSConfig())
		}

		// Defense-in-depth: refuse the LOCAL INFILE capability on the upstream
		// connection. The shared SQL validator already blocks the keyword in
		// inbound client SQL, but a malicious upstream could still issue a
		// LOCAL_INFILE_REQUEST (0xFB) packet as part of any query response —
		// and the go-mysql client would happily read the file from the dbbat
		// host's filesystem unless we opt out at handshake time.
		c.UnsetCapability(gomysql.CLIENT_LOCAL_FILES)

		if cfg.ProgramName != "" {
			c.SetAttributes(map[string]string{"program_name": cfg.ProgramName})
		}

		return nil
	}
}

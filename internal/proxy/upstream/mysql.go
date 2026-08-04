package upstream

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	gomysqlclient "github.com/go-mysql-org/go-mysql/client"
	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

// ErrMySQLNoAttempt is returned when the attempt chain ended without producing
// either a connection or an error. It cannot happen with a plan from PlanFor
// (which always has at least one attempt) and exists so the exhausted-loop path
// is not a silent nil.
var ErrMySQLNoAttempt = errors.New("mysql: ssl_mode produced no connection attempt")

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

// MySQLUpstream is an authenticated MySQL/MariaDB connection plus the one fact
// the row could not state: whether it ended up encrypted.
type MySQLUpstream struct {
	// Conn is the live go-mysql client connection.
	Conn *gomysqlclient.Conn
	// TLS reports whether the connection is encrypted.
	TLS bool
}

// Close tears the connection down. Safe on a nil receiver.
func (u *MySQLUpstream) Close() error {
	if u == nil || u.Conn == nil {
		return nil
	}

	return u.Conn.Close()
}

// ConnectMySQL opens an authenticated MySQL/MariaDB connection over the
// injected transport. It is the one implementation both the proxy and the
// connectivity check use, so a green check exercises the proxy's exact login.
//
// go-mysql handles auth-plugin negotiation (caching_sha2_password on MySQL 8.x)
// transparently — that is the plugin support dbbat deliberately does not
// implement on its own server-facing side.
//
// Opportunistic ssl_modes need two attempts. go-mysql decides whether to
// encrypt from the handshake's CLIENT_SSL capability, and the option callback
// runs before that handshake is read, so a single connection cannot express
// "encrypt if you can". The chain therefore redials — and only when the failure
// says the *transport* was the problem. An authentication failure ends it,
// exactly as pgconn's fallback chain does: retrying a rejected password in
// plaintext would be a downgrade triggered by the wrong signal.
func ConnectMySQL(ctx context.Context, dial DialFunc, cfg MySQLConfig) (*MySQLUpstream, error) {
	plan := PlanFor(cfg.SSLMode, cfg.Host)

	var lastErr error

	for i, attempt := range plan.Attempts {
		conn, err := connectMySQLOnce(ctx, dial, cfg, attempt)
		if err == nil {
			return &MySQLUpstream{Conn: conn, TLS: attempt.Encrypted()}, nil
		}

		lastErr = err

		if i == len(plan.Attempts)-1 || !mysqlRetryable(attempt, err) {
			break
		}
	}

	if lastErr == nil {
		return nil, ErrMySQLNoAttempt
	}

	return nil, lastErr
}

// connectMySQLOnce performs a single login attempt on a fresh transport.
func connectMySQLOnce(
	ctx context.Context,
	dial DialFunc,
	cfg MySQLConfig,
	attempt Attempt,
) (*gomysqlclient.Conn, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	dialer := func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return dial(dialCtx)
	}

	return gomysqlclient.ConnectWithDialer(
		ctx, "tcp", addr,
		cfg.Username, cfg.Password, cfg.Database,
		dialer,
		mysqlOptions(cfg, attempt),
	)
}

// mysqlOptions configures the connection before its handshake: TLS for this
// attempt, the dbbat program_name attribute, and an explicit refusal of
// CLIENT_LOCAL_FILES.
func mysqlOptions(cfg MySQLConfig, attempt Attempt) func(*gomysqlclient.Conn) error {
	return func(c *gomysqlclient.Conn) error {
		if attempt.Encrypted() {
			c.SetTLSConfig(attempt.TLS)
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

// mysqlRetryable reports whether a failed attempt should be followed by the
// next one in the plan. The predicate is deliberately narrow and
// direction-specific: only a *transport* mismatch justifies changing the
// encryption of the retry.
//
//   - an encrypted attempt may fall back to plaintext when the server does not
//     advertise CLIENT_SSL at all;
//   - a plaintext attempt may be retried encrypted when the server refuses
//     insecure transport (MySQL 8's require_secure_transport, error 3159).
//
// Everything else — wrong password, unknown database, host not allowed — ends
// the chain with that error.
func mysqlRetryable(attempt Attempt, err error) bool {
	msg := strings.ToLower(err.Error())

	if attempt.Encrypted() {
		return strings.Contains(msg, "does not support tls") ||
			strings.Contains(msg, "does not support ssl")
	}

	return strings.Contains(msg, "insecure transport are prohibited") ||
		strings.Contains(msg, "secure transport")
}

package upstream

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"time"

	goora "github.com/sijms/go-ora/v3"
)

// ErrOracleConnectorShape guards against a go-ora upgrade changing what
// NewConnector returns: without the concrete type we cannot inject the
// transport, and a caller that silently dialed on its own would bypass the SSH
// tunnel entirely.
var ErrOracleConnectorShape = errors.New("go-ora connector does not expose a dialer hook")

// OracleConfig is everything an Oracle login needs from a server row.
type OracleConfig struct {
	// Host and Port build the DSN go-ora reports in its errors; the transport
	// comes from the injected DialFunc.
	Host string
	Port int
	// ServiceName is the SERVICE_NAME to present, already resolved by the
	// caller (the dedicated column, or the database name as a fallback).
	ServiceName string
	// Username and Password are the stored upstream credentials.
	Username string
	Password string
	// ProgramName is the PROGRAM option, so a DBA reading v$session can tell
	// who is connected.
	ProgramName string
	// SSLMode is the row's ssl_mode; interpreted by PlanFor.
	SSLMode string
}

// ConnectOracle runs a real TNS Connect + TTC login against the target with
// go-ora, over the injected transport.
//
// Unlike the other three protocols, this is *not* what the Oracle proxy does:
// the proxy's TNS/TTC handshake only exists inside a live session with a
// downstream client attached, because it relays the client's own Connect
// descriptor byte for byte. go-ora is the standalone client half of that same
// protocol. The connector lives here anyway so the ssl_mode policy and the
// transport injection are the shared ones, and so there is a single place to
// look for "how does dbbat log in to an Oracle server".
//
// Encryption is where that divergence bites, so state it plainly: the Oracle
// proxy has no upstream TLS at all — an Oracle session's upstream leg is
// plaintext whatever the row's ssl_mode says, which is why its connection rows
// hardcode upstream_tls=false. This function, which only the connectivity check
// runs, does encrypt under require/verify-*. A green check on an Oracle row at
// ssl_mode=require therefore proves more than a real session will do. See the
// package doc.
func ConnectOracle(ctx context.Context, dial DialFunc, cfg OracleConfig) (driver.Conn, error) {
	opts := map[string]string{"PROGRAM": cfg.ProgramName}

	// Deliberately no TIMEOUT/READ TIMEOUT option: it makes go-ora arm real
	// socket deadlines, which an SSH-tunneled conn cannot honor. Callers bound
	// the attempt by closing the transport instead.
	//
	// Only RequiresTLS is consulted: go-ora's SSL option is a yes/no on the DSN
	// with no fallback, so the opportunistic modes stay plaintext rather than
	// silently becoming mandatory-TLS. Oracle does not honor the plan's attempt
	// order — see the Plan doc.
	plan := PlanFor(cfg.SSLMode, cfg.Host)
	if plan.RequiresTLS() {
		opts["SSL"] = "TRUE"
		// go-ora's own verification switch maps onto the same distinction
		// PlanFor draws: verify-ca/verify-full authenticate the server,
		// require only encrypts.
		opts["SSL VERIFY"] = boolOption(!plan.TLSConfig().InsecureSkipVerify)
	}

	dsn := goora.BuildUrl(cfg.Host, cfg.Port, cfg.ServiceName, cfg.Username, cfg.Password, opts)

	connector, ok := goora.NewConnector(dsn).(*goora.OracleConnector)
	if !ok {
		return nil, ErrOracleConnectorShape
	}

	connector.Dialer(oracleDialer(dial))

	return connector.Connect(ctx)
}

// boolOption renders a go-ora boolean option.
func boolOption(v bool) string {
	if v {
		return "TRUE"
	}

	return "FALSE"
}

// oracleDialer adapts a DialFunc to go-ora's ContextDialer, wrapping every conn
// so go-ora's unconditional deadline calls do not break on a tunneled socket.
func oracleDialer(dial DialFunc) contextDialerFunc {
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := dial(ctx)
		if err != nil {
			return nil, err
		}

		return tolerantDeadlineConn{Conn: conn}, nil
	}
}

// contextDialerFunc adapts a DialFunc to the driver interfaces (go-ora's and
// the mongo driver's) that want a DialContext method.
type contextDialerFunc func(ctx context.Context) (net.Conn, error)

// DialContext satisfies the ContextDialer shape those drivers expect.
func (d contextDialerFunc) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return d(ctx)
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

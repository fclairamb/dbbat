package mssql

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/version"
)

// Upstream-leg errors.
var (
	// ErrUpstreamUnexpectedMessage — the upstream answered with a TDS message
	// type that does not belong at that point in the handshake.
	ErrUpstreamUnexpectedMessage = errors.New("mssql: unexpected TDS message from the upstream")
	// ErrUpstreamNoLoginAck — the upstream's login response carried neither a
	// LOGINACK nor an ERROR token, so there is no way to tell whether the
	// session is usable.
	ErrUpstreamNoLoginAck = errors.New("mssql: upstream login response has no LOGINACK")
)

// upstreamCltIntName is the client-library name dbbat presents in LOGIN7 when
// it has no client login to copy one from (the connectivity check).
const upstreamCltIntName = "dbbat"

// maxUpstreamAppNameLen bounds the LOGIN7 AppName dbbat sends upstream.
// sys.dm_exec_sessions.program_name is nvarchar(128), so anything longer is
// truncated by the server anyway.
const maxUpstreamAppNameLen = 128

// UpstreamConn is an authenticated TDS connection to a target SQL Server,
// ready to relay packets over.
//
// It is what both entry points get back: the proxy, which then MITMs it, and
// the connectivity check, which closes it immediately.
type UpstreamConn struct {
	// conn is the raw transport (a direct dial or an SSH-tunnelled one).
	conn net.Conn

	// stream is what TDS is read from and written to: the socket, or the TLS
	// session once the encapsulated handshake has run.
	stream *revertibleConn

	// pkt frames TDS packets over stream.
	pkt *packetRW

	// TLS reports whether the proxy→upstream leg is encrypted. It is the value
	// recorded on the connection row's upstream_tls field.
	TLS bool

	// LoginResponse is the raw token stream the upstream answered LOGIN7 with
	// (LOGINACK, ENVCHANGEs, INFO messages, DONE). The proxy forwards it to the
	// client verbatim, so the client sees the real server's identity, collation
	// and database context rather than something dbbat made up.
	LoginResponse []byte

	closeOnce sync.Once
}

// Close tears the connection down. Safe on a nil receiver and safe to call
// twice, which the session's teardown relies on.
func (u *UpstreamConn) Close() error {
	if u == nil || u.conn == nil {
		return nil
	}

	var err error

	u.closeOnce.Do(func() { err = u.conn.Close() })

	return err
}

// ConnectUpstream opens an authenticated TDS connection over the injected
// transport. It is the one implementation both the proxy and the connectivity
// check use, so a green check exercises the proxy's exact login.
//
// template is the client's own parsed LOGIN7, whose non-credential fields
// (TDS version, packet size, option flags, host name, FEATUREEXT block) are
// replayed so the upstream negotiates with the client's real capabilities and
// the response dbbat forwards back actually answers what the client asked. A
// nil template — the connectivity check, which has no client — gets a plain
// TDS 7.4 login.
//
// Opportunistic ssl_modes need two attempts: TDS settles encryption in PRELOGIN
// before a single byte of login is sent, and a server that refuses the offered
// transport cannot be talked round on the same socket. The chain redials, and
// only when the failure says the *transport* was the problem — a rejected login
// ends it, exactly as the MySQL and PostgreSQL chains do.
func ConnectUpstream(
	ctx context.Context,
	dial upstream.DialFunc,
	cfg upstream.MSSQLConfig,
	template *Login7,
) (*UpstreamConn, error) {
	plan := upstream.MSSQLPlan(cfg)

	var lastErr error

	for i, attempt := range plan.Attempts {
		conn, err := connectUpstreamOnce(ctx, dial, cfg, attempt, template)
		if err == nil {
			return conn, nil
		}

		lastErr = err

		if i == len(plan.Attempts)-1 || !upstream.MSSQLRetryable(err) {
			break
		}
	}

	if lastErr == nil {
		return nil, upstream.ErrMSSQLNoAttempt
	}

	return nil, lastErr
}

// connectUpstreamOnce performs a single PRELOGIN + (TLS) + LOGIN7 exchange on a
// fresh transport.
func connectUpstreamOnce(
	ctx context.Context,
	dial upstream.DialFunc,
	cfg upstream.MSSQLConfig,
	attempt upstream.Attempt,
	template *Login7,
) (*UpstreamConn, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("mssql: dial upstream: %w", err)
	}

	// Every failure path below has to hand the socket back, including the ones
	// that will be retried on the next attempt.
	handedOver := false

	defer func() {
		if !handedOver {
			_ = conn.Close()
		}
	}()

	stream := newRevertibleConn(conn)
	pkt := newPacketRW(stream)

	if err := negotiateUpstreamEncryption(ctx, conn, pkt, stream, cfg, attempt); err != nil {
		return nil, err
	}

	login := buildUpstreamLogin(template, cfg)

	if err := pkt.WriteMessage(packetTypeLogin7, login.serialize()); err != nil {
		return nil, err
	}

	// The negotiated size applies from the login response onwards; PRELOGIN and
	// LOGIN7 themselves always travel at the default size.
	pkt.SetPacketSize(int(login.PacketSize))

	response, err := readLoginResponse(pkt)
	if err != nil {
		return nil, err
	}

	handedOver = true

	return &UpstreamConn{
		conn:          conn,
		stream:        stream,
		pkt:           pkt,
		TLS:           attempt.Encrypted(),
		LoginResponse: response,
	}, nil
}

// negotiateUpstreamEncryption runs the PRELOGIN exchange and, when the attempt
// calls for it, the encapsulated TLS handshake — putting the TLS session in
// force on stream.
func negotiateUpstreamEncryption(
	ctx context.Context,
	conn net.Conn,
	pkt *packetRW,
	stream *revertibleConn,
	cfg upstream.MSSQLConfig,
	attempt upstream.Attempt,
) error {
	offered := upstream.MSSQLEncryptionOption(attempt)

	if err := pkt.WriteMessage(packetTypePrelogin, buildUpstreamPrelogin(offered).serialize()); err != nil {
		return err
	}

	msgType, payload, err := pkt.ReadMessage()
	if err != nil {
		return fmt.Errorf("mssql: read upstream PRELOGIN response: %w", err)
	}

	// SQL Server answers PRELOGIN with a Tabular Result (0x04) packet, but some
	// implementations echo the PRELOGIN type. Both are accepted; anything else
	// means the stream is not TDS.
	if msgType != packetTypeReply && msgType != packetTypePrelogin {
		return fmt.Errorf("%w: PRELOGIN answered with %s", ErrUpstreamUnexpectedMessage, packetTypeName(msgType))
	}

	response, err := parsePrelogin(payload)
	if err != nil {
		return err
	}

	answer := response.Encryption()
	if !upstream.MSSQLAnswerAcceptable(attempt, answer) {
		return fmt.Errorf("%w: offered %s, upstream answered %s (ssl_mode=%q)",
			upstream.ErrMSSQLEncryptionMismatch, encryptionName(offered), encryptionName(answer), cfg.SSLMode)
	}

	if !attempt.Encrypted() {
		return nil
	}

	adapter := newHandshakeConn(conn, pkt)

	tlsConn := tls.Client(adapter, upstreamTLSConfig(attempt))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("%w: %w", upstream.ErrMSSQLTLSHandshake, err)
	}

	// Stop encapsulating (flushing whatever the handshake left pending) before
	// any application byte is written, exactly as the client leg does.
	if err := adapter.deactivate(); err != nil {
		return fmt.Errorf("%w: %w", upstream.ErrMSSQLTLSHandshake, err)
	}

	stream.switchTo(tlsConn)

	return nil
}

// upstreamTLSConfig adapts the plan's TLS config for a TDS handshake.
//
// The clone exists to cap the version at TLS 1.2 without touching the shared
// policy: TDS un-wraps the handshake the moment it completes, and under TLS 1.3
// the *client* finishes on a write, so the two peers can disagree about whether
// that last flight is still encapsulated. That disagreement presents as a hang.
// Every SQL Server speaks TLS 1.2 for this handshake, so both legs pin it — see
// tlsMaxVersion and docs/mssql.md.
func upstreamTLSConfig(attempt upstream.Attempt) *tls.Config {
	cfg := attempt.TLS.Clone()
	cfg.MaxVersion = tlsMaxVersion

	return cfg
}

// readLoginResponse reads the upstream's answer to LOGIN7 and decides whether
// the login succeeded.
func readLoginResponse(pkt *packetRW) ([]byte, error) {
	msgType, payload, err := pkt.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("mssql: read upstream login response: %w", err)
	}

	if msgType != packetTypeReply {
		return nil, fmt.Errorf("%w: LOGIN7 answered with %s", ErrUpstreamUnexpectedMessage, packetTypeName(msgType))
	}

	outcome := scanLoginResponse(payload)

	if outcome.Failure != nil {
		return nil, fmt.Errorf("%w: %s", upstream.ErrMSSQLLoginRejected, outcome.Failure.Error())
	}

	if !outcome.Acked {
		return nil, ErrUpstreamNoLoginAck
	}

	return payload, nil
}

// buildUpstreamPrelogin assembles the PRELOGIN message dbbat sends upstream.
//
// MARS is offered as 0x00 because dbbat refuses it on the client leg too: a
// session that cannot multiplex downstream has nothing to gain from an upstream
// that can. INSTOPT is the empty (default-instance) name — dbbat dials the host
// and port on the server row, never the SQL Server Browser.
func buildUpstreamPrelogin(encryption byte) *preloginMessage {
	return &preloginMessage{
		Options: []preloginOption{
			{Token: preloginVersion, Data: versionBlob()},
			{Token: preloginEncryption, Data: []byte{encryption}},
			{Token: preloginInstOpt, Data: []byte{0x00}},
			{Token: preloginThreadID, Data: []byte{0x00, 0x00, 0x00, 0x00}},
			{Token: preloginMARS, Data: []byte{0x00}},
		},
	}
}

// buildUpstreamLogin produces the LOGIN7 dbbat replays upstream: the client's
// own login with the credentials swapped for the stored ones.
//
// What is kept from the client, and why: the TDS version, packet size, option
// flags, collation and FEATUREEXT block decide the *shape* of everything the
// server sends back, and dbbat forwards that response to the client untouched.
// Negotiating anything different upstream would hand the client a token stream
// it did not ask for. The host name and client id are kept so a DBA reading
// sys.dm_exec_sessions sees the real originating machine rather than the proxy.
//
// What is dropped: anything that would make the upstream negotiate an
// authentication scheme dbbat does not speak (integrated security, an SSPI
// blob), a password change, or an attached database file.
func buildUpstreamLogin(template *Login7, cfg upstream.MSSQLConfig) *Login7 {
	login := defaultUpstreamLogin()

	if template != nil {
		clone := *template
		login = &clone
	}

	login.UserName = cfg.Username
	login.Password = cfg.Password
	login.Database = cfg.Database

	if cfg.AppName != "" {
		login.AppName = cfg.AppName
	}

	login.OptionFlags2 &^= optionFlags2IntSecurity
	login.SSPI = nil
	login.ChangePassword = ""
	login.AtchDBFile = ""

	if login.TDSVersion == 0 {
		login.TDSVersion = tdsVersion74
	}

	if login.CltIntName == "" {
		login.CltIntName = upstreamCltIntName
	}

	login.PacketSize = clampPacketSize(login.PacketSize)

	return login
}

// clampPacketSize brings a requested packet size into the range TDS allows, the
// same way packetRW.SetPacketSize does — the two must agree or the upstream
// would frame at a size the codec is not using.
func clampPacketSize(size uint32) uint32 {
	switch {
	case size < minPacketSize:
		return defaultPacketSize
	case size > maxPacketSize:
		return maxPacketSize
	default:
		return size
	}
}

// defaultUpstreamLogin is the login used when there is no client to copy one
// from — the connectivity check.
func defaultUpstreamLogin() *Login7 {
	hostName, err := os.Hostname()
	if err != nil {
		hostName = upstreamCltIntName
	}

	return &Login7{
		TDSVersion: tdsVersion74,
		PacketSize: defaultPacketSize,
		HostName:   hostName,
		AppName:    upstreamCltIntName,
		CltIntName: upstreamCltIntName,
	}
}

// buildUpstreamAppName constructs the LOGIN7 AppName dbbat presents upstream:
// "dbbat/$version @$username", plus " for $appName" when the client declared
// one of its own. See shared.BuildUpstreamName for the truncation rules.
func buildUpstreamAppName(username, clientAppName string) string {
	return shared.BuildUpstreamName(version.Version, username, clientAppName, maxUpstreamAppNameLen)
}

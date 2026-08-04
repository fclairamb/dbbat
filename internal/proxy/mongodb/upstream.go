package mongodb

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"

	"github.com/xdg-go/scram"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/proxy/upstream"
	"github.com/fclairamb/dbbat/internal/version"
)

// maxAppNameLen bounds the application name dbbat advertises to the upstream.
const maxAppNameLen = 128

// UpstreamConn is an authenticated connection to a target MongoDB.
//
// The connector below is the MongoDB entry of the shared upstream-connect set
// (see internal/proxy/upstream). It lives in this package rather than in
// internal/proxy/upstream only because it is written on top of this package's
// OP_MSG codec, which the whole proxy is built from; the ssl_mode policy it
// applies is the shared one, so there is still exactly one description of what
// a mode means. The connectivity check calls ConnectUpstream directly.
type UpstreamConn struct {
	conn   net.Conn
	reader *bufio.Reader
	reqID  int32
	// tls records whether the connection ended up encrypted, which under an
	// opportunistic ssl_mode is not knowable from the row alone.
	tls bool
}

func (u *UpstreamConn) nextReqID() int32 {
	u.reqID++

	return u.reqID
}

// TLS reports whether this connection is encrypted.
func (u *UpstreamConn) TLS() bool {
	return u != nil && u.tls
}

// Close tears the connection down. Safe on a nil receiver.
func (u *UpstreamConn) Close() error {
	if u == nil || u.conn == nil {
		return nil
	}

	return u.conn.Close()
}

// close is the internal, error-swallowing form used on failure paths.
func (u *UpstreamConn) close() {
	_ = u.Close()
}

// sendCommand writes an OP_MSG request carrying doc and returns the reply's
// command-body document.
func (u *UpstreamConn) sendCommand(doc any) (bson.Raw, error) {
	req, err := buildOpMsgReply(u.nextReqID(), 0, doc)
	if err != nil {
		return nil, err
	}

	if _, err := u.conn.Write(req); err != nil {
		return nil, fmt.Errorf("upstream write: %w", err)
	}

	m, err := readMessage(u.reader)
	if err != nil {
		return nil, fmt.Errorf("upstream read: %w", err)
	}

	parsed, err := parseOpMsg(m.body)
	if err != nil {
		return nil, err
	}

	body, ok := parsed.commandBody()
	if !ok {
		return nil, ErrNoCommandBody
	}

	return body, nil
}

// UpstreamConfig is everything a MongoDB login needs from a server row.
type UpstreamConfig struct {
	// Host is the target hostname, used for TLS server-name verification; the
	// transport comes from the injected dial function.
	Host string
	// Username and Password are the stored upstream credentials.
	Username string
	Password string
	// AuthSource is the upstream user's own auth database. It is deliberately
	// NOT the client's authSource (which carries the dbbat database selector,
	// contract §5).
	AuthSource string
	// AppName is advertised in the hello handshake's client metadata.
	AppName string
	// SSLMode is the row's ssl_mode; interpreted by upstream.PlanFor.
	SSLMode string
}

// ConnectUpstream dials the target through dial, applies the ssl_mode policy,
// runs the hello handshake and authenticates via SCRAM-SHA-256 (contract §5).
// It is the one implementation the proxy and the connectivity check share.
func ConnectUpstream(ctx context.Context, dial upstream.DialFunc, cfg UpstreamConfig) (*UpstreamConn, error) {
	conn, encrypted, err := dialUpstream(ctx, dial, cfg)
	if err != nil {
		return nil, err
	}

	up := &UpstreamConn{conn: conn, reader: bufio.NewReader(conn), tls: encrypted}

	if err := up.handshake(cfg.AppName); err != nil {
		up.close()

		return nil, err
	}

	if err := up.authenticate(cfg); err != nil {
		up.close()

		return nil, err
	}

	return up, nil
}

// dialUpstream opens the transport and applies the ssl_mode plan. MongoDB has
// no in-band negotiation — a connection is TLS from the first byte or not at
// all — so an encrypted attempt is simply a TLS handshake on a fresh conn.
func dialUpstream(ctx context.Context, dial upstream.DialFunc, cfg UpstreamConfig) (net.Conn, bool, error) {
	plan := upstream.PlanFor(cfg.SSLMode, cfg.Host)

	conn, err := dial(ctx)
	if err != nil {
		return nil, false, err
	}

	tlsCfg := plan.TLSConfig()
	if tlsCfg == nil || !plan.RequiresTLS() {
		// Opportunistic modes still connect in plaintext at this phase; the
		// TLS-first attempt chain arrives with the opportunistic-TLS work.
		return conn, false, nil
	}

	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()

		return nil, false, err
	}

	return tlsConn, true, nil
}

// connectUpstream opens the session's upstream connection using the shared
// connector.
func (s *Session) connectUpstream() error {
	if err := s.database.DecryptPassword(s.server.encryptionKey); err != nil {
		return fmt.Errorf("decrypt upstream password: %w", err)
	}

	addr := net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))

	up, err := ConnectUpstream(s.ctx, s.dialUpstream, UpstreamConfig{
		Host:       s.database.Host,
		Username:   s.database.Username,
		Password:   s.database.Password,
		AuthSource: s.scramAuthDB(),
		AppName:    shared.BuildUpstreamName(version.Version, s.user.Username, "", maxAppNameLen),
		SSLMode:    s.database.SSLMode,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	s.upstream = up
	s.upstreamTLS = up.TLS()

	s.logger.DebugContext(s.ctx, "upstream MongoDB connected",
		slog.String("addr", addr),
		slog.String("user", s.database.Username),
		slog.String("database", s.database.DatabaseName))

	return nil
}

// dialUpstream opens the transport to the target: a direct TCP dial, or a
// tunnel through the SSH bastion chain when the server row's via_uid is set.
func (s *Session) dialUpstream(ctx context.Context) (net.Conn, error) {
	return shared.DialUpstream(ctx, s.server.store, s.server.encryptionKey, s.database)
}

// handshake sends our own hello with client metadata (application name via
// shared.BuildUpstreamName for branding parity) and reads the reply.
func (u *UpstreamConn) handshake(appName string) error {
	hello := bson.D{
		{Key: "hello", Value: 1},
		{Key: "helloOk", Value: true},
		{Key: "client", Value: bson.D{
			{Key: "driver", Value: bson.D{
				{Key: "name", Value: "dbbat"},
				{Key: "version", Value: version.Version},
			}},
			{Key: "application", Value: bson.D{{Key: "name", Value: appName}}},
			{Key: "os", Value: bson.D{{Key: "type", Value: runtime.GOOS}}},
		}},
		{Key: "$db", Value: "admin"},
	}

	body, err := u.sendCommand(hello)
	if err != nil {
		return fmt.Errorf("upstream hello: %w", err)
	}

	if !replyOK(body) {
		return fmt.Errorf("%w: hello: %s", ErrUpstreamRejected, lookupString(body, "errmsg"))
	}

	return nil
}

// authenticate runs SCRAM-SHA-256 as a client against the upstream
// (contract §5). The password is SASLprep-normalized by the scram client.
func (u *UpstreamConn) authenticate(cfg UpstreamConfig) error {
	authDB := cfg.AuthSource

	client, err := scram.SHA256.NewClient(cfg.Username, cfg.Password, "")
	if err != nil {
		return fmt.Errorf("scram client: %w", err)
	}

	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		return fmt.Errorf("scram client-first: %w", err)
	}

	body, err := u.sendCommand(bson.D{
		{Key: "saslStart", Value: 1},
		{Key: "mechanism", Value: "SCRAM-SHA-256"},
		{Key: "payload", Value: bson.Binary{Subtype: 0, Data: []byte(clientFirst)}},
		{Key: "options", Value: bson.D{{Key: "skipEmptyExchange", Value: true}}},
		{Key: "$db", Value: authDB},
	})
	if err != nil {
		return err
	}

	return u.scramLoop(conv, body, authDB)
}

// scramLoop drives the saslContinue exchange until the server signals done.
func (u *UpstreamConn) scramLoop(conv *scram.ClientConversation, body bson.Raw, authDB string) error {
	convID, payload, done, err := parseSaslReply(body)
	if err != nil {
		return err
	}

	for !done {
		resp, stepErr := conv.Step(string(payload))
		if stepErr != nil {
			return fmt.Errorf("scram step: %w", stepErr)
		}

		body, err = u.sendCommand(bson.D{
			{Key: "saslContinue", Value: 1},
			{Key: "conversationId", Value: convID},
			{Key: "payload", Value: bson.Binary{Subtype: 0, Data: []byte(resp)}},
			{Key: "$db", Value: authDB},
		})
		if err != nil {
			return err
		}

		convID, payload, done, err = parseSaslReply(body)
		if err != nil {
			return err
		}
	}

	// Validate the server-final message when skipEmptyExchange short-circuited
	// the exchange (the conversation hasn't verified the server yet).
	if !conv.Done() && len(payload) > 0 {
		if _, err := conv.Step(string(payload)); err != nil {
			return fmt.Errorf("scram server validation: %w", err)
		}
	}

	return nil
}

// scramAuthDB returns the authSource for the upstream SCRAM exchange. It is
// intentionally NOT the client's authSource (which carries the dbbat database
// selector, contract §5) — the upstream user's credentials live in its own auth
// database. Configurable per-database via the mongo_auth_source column,
// defaulting to "admin" (the MongoDB default where service/root users are
// created, e.g. MONGO_INITDB_ROOT_USERNAME).
func (s *Session) scramAuthDB() string {
	return s.database.MongoAuthSourceOrDefault()
}

// parseSaslReply extracts (conversationId, payload, done) from a SASL reply,
// returning an error when the server rejected the exchange (ok != 1).
func parseSaslReply(body bson.Raw) (int32, []byte, bool, error) {
	if !replyOK(body) {
		return 0, nil, false, fmt.Errorf("%w: SASL: %s", ErrUpstreamRejected, lookupString(body, "errmsg"))
	}

	convID, _ := body.Lookup("conversationId").Int32OK()
	_, payload, _ := body.Lookup("payload").BinaryOK()
	done := lookupBool(body, "done")

	return convID, payload, done, nil
}

// replyOK reports whether a command reply has ok == 1 (accepting the double or
// int32 encodings servers use).
func replyOK(body bson.Raw) bool {
	if d, ok := body.Lookup("ok").DoubleOK(); ok {
		return d == 1
	}

	if i, ok := body.Lookup("ok").Int32OK(); ok {
		return i == 1
	}

	if i, ok := body.Lookup("ok").Int64OK(); ok {
		return i == 1
	}

	return false
}

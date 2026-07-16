package mongodb

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"

	"github.com/xdg-go/scram"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/version"
)

// maxAppNameLen bounds the application name dbbat advertises to the upstream.
const maxAppNameLen = 128

// upstreamConn is the authenticated connection to the target MongoDB.
type upstreamConn struct {
	conn   net.Conn
	reader *bufio.Reader
	reqID  int32
}

func (u *upstreamConn) nextReqID() int32 {
	u.reqID++

	return u.reqID
}

func (u *upstreamConn) close() {
	if u == nil || u.conn == nil {
		return
	}

	_ = u.conn.Close()
}

// sendCommand writes an OP_MSG request carrying doc and returns the reply's
// command-body document.
func (u *upstreamConn) sendCommand(doc any) (bson.Raw, error) {
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

// connectUpstream dials the target MongoDB and authenticates via SCRAM-SHA-256
// using the stored (decrypted) credentials (contract §5).
func (s *Session) connectUpstream() error {
	if err := s.database.DecryptPassword(s.server.encryptionKey); err != nil {
		return fmt.Errorf("decrypt upstream password: %w", err)
	}

	addr := net.JoinHostPort(s.database.Host, strconv.Itoa(s.database.Port))

	conn, err := s.dialUpstream()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	up := &upstreamConn{conn: conn, reader: bufio.NewReader(conn)}
	s.upstream = up

	if err := s.upstreamHandshake(up); err != nil {
		up.close()
		s.upstream = nil

		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	if err := s.upstreamSCRAM(up); err != nil {
		up.close()
		s.upstream = nil

		return fmt.Errorf("%w: %w", ErrUpstreamConnect, err)
	}

	s.logger.DebugContext(s.ctx, "upstream MongoDB connected",
		slog.String("addr", addr),
		slog.String("user", s.database.Username),
		slog.String("database", s.database.DatabaseName))

	return nil
}

// dialUpstream opens the TCP (optionally TLS) connection per the database's
// ssl_mode, mirroring the MySQL upstream mapping.
func (s *Session) dialUpstream() (net.Conn, error) {
	// Dial directly, or tunnel through an SSH bastion when via_uid is set.
	conn, err := shared.DialUpstream(s.ctx, s.server.store, s.server.encryptionKey, s.database)
	if err != nil {
		return nil, err
	}

	switch s.database.SSLMode {
	case "require":
		tlsConn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true, // "require" = encrypt without cert verification
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()

			return nil, err
		}

		return tlsConn, nil

	case "verify-ca", "verify-full":
		tlsConn := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: s.database.Host})
		if err := tlsConn.Handshake(); err != nil {
			_ = conn.Close()

			return nil, err
		}

		return tlsConn, nil

	default:
		// "", "disable", "prefer", "allow" — plaintext upstream.
		return conn, nil
	}
}

// upstreamHandshake sends our own hello with client metadata (application name
// via shared.BuildUpstreamName for branding parity) and reads the reply.
func (s *Session) upstreamHandshake(up *upstreamConn) error {
	appName := shared.BuildUpstreamName(version.Version, s.user.Username, "", maxAppNameLen)

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

	body, err := up.sendCommand(hello)
	if err != nil {
		return fmt.Errorf("upstream hello: %w", err)
	}

	if !replyOK(body) {
		return fmt.Errorf("%w: hello: %s", ErrUpstreamRejected, lookupString(body, "errmsg"))
	}

	return nil
}

// upstreamSCRAM runs SCRAM-SHA-256 as a client against the upstream
// (contract §5). The password is SASLprep-normalized by the scram client.
func (s *Session) upstreamSCRAM(up *upstreamConn) error {
	authDB := s.scramAuthDB()

	client, err := scram.SHA256.NewClient(s.database.Username, s.database.Password, "")
	if err != nil {
		return fmt.Errorf("scram client: %w", err)
	}

	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		return fmt.Errorf("scram client-first: %w", err)
	}

	body, err := up.sendCommand(bson.D{
		{Key: "saslStart", Value: 1},
		{Key: "mechanism", Value: "SCRAM-SHA-256"},
		{Key: "payload", Value: bson.Binary{Subtype: 0, Data: []byte(clientFirst)}},
		{Key: "options", Value: bson.D{{Key: "skipEmptyExchange", Value: true}}},
		{Key: "$db", Value: authDB},
	})
	if err != nil {
		return err
	}

	return s.scramLoop(up, conv, body, authDB)
}

// scramLoop drives the saslContinue exchange until the server signals done.
func (s *Session) scramLoop(up *upstreamConn, conv *scram.ClientConversation, body bson.Raw, authDB string) error {
	convID, payload, done, err := parseSaslReply(body)
	if err != nil {
		return err
	}

	for !done {
		resp, stepErr := conv.Step(string(payload))
		if stepErr != nil {
			return fmt.Errorf("scram step: %w", stepErr)
		}

		body, err = up.sendCommand(bson.D{
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

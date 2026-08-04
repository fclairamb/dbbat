package upstream

import (
	"context"
	"crypto/md5" // PostgreSQL's md5 auth method is defined in terms of MD5; not our choice
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"

	"github.com/jackc/pgx/v5/pgproto3"
)

// PostgreSQL upstream errors. They are the classification surface both callers
// key off: the proxy to decide what to tell its client, the connectivity check
// to decide whether it is looking at a TLS problem or a credentials problem.
var (
	// ErrPostgresTLSRequired means the server refused to encrypt while the
	// ssl_mode demanded it.
	ErrPostgresTLSRequired = errors.New("upstream rejected TLS but ssl_mode requires it")
	// ErrPostgresSSLResponse means the server answered the SSLRequest with
	// something other than 'S' or 'N' — it is probably not a Postgres server.
	ErrPostgresSSLResponse = errors.New("unexpected upstream SSL response byte")
	// ErrPostgresAuthFailed means the server sent an ErrorResponse during
	// login: bad password, unknown role, no matching pg_hba line, missing
	// database.
	ErrPostgresAuthFailed = errors.New("upstream authentication failed")
)

// PostgresConfig is everything the login needs from a server row. The caller
// resolves it (decrypting the password, building the application name) so this
// package stays free of store and crypto concerns.
type PostgresConfig struct {
	// Host is the target hostname. Used only for TLS server-name
	// verification — the transport comes from the injected DialFunc.
	Host string
	// Username, Password and Database are the stored upstream credentials.
	Username string
	Password string
	Database string
	// ApplicationName is advertised in the StartupMessage, so a DBA reading
	// pg_stat_activity can tell who is connected and why.
	ApplicationName string
	// SSLMode is the row's ssl_mode; interpreted by PlanFor.
	SSLMode string
}

// PostgresUpstream is an authenticated upstream connection, plus everything the
// login produced that a proxy has to replay to its own client.
//
// The connector stops the instant the upstream is authenticated and ready: it
// does not forward anything, set session state, or register cancel keys. The
// proxy does that from these fields; the connectivity check ignores them and
// closes Conn.
type PostgresUpstream struct {
	// Conn is the live connection — TLS-wrapped when the negotiation upgraded.
	Conn net.Conn
	// Frontend speaks pgproto3 over Conn with dbbat in the client role.
	Frontend *pgproto3.Frontend
	// ParameterStatuses are the server's startup parameters, in arrival order.
	// Copies: pgproto3 reuses its message structs.
	ParameterStatuses []*pgproto3.ParameterStatus
	// BackendKeyData is the cancellation key the server issued, or nil when it
	// issued none.
	BackendKeyData *pgproto3.BackendKeyData
	// ReadyForQuery is the message that ended the login.
	ReadyForQuery *pgproto3.ReadyForQuery
	// TLS reports whether the connection is encrypted. It answers "are we
	// actually encrypted?" for a row whose ssl_mode only expressed a
	// preference.
	TLS bool
}

// Close tears the connection down. Safe on a nil receiver.
func (u *PostgresUpstream) Close() error {
	if u == nil || u.Conn == nil {
		return nil
	}

	return u.Conn.Close()
}

// PostgresAuthError is an ErrorResponse the upstream sent during login. It
// keeps the raw message so the proxy can hand its own client the server's
// verbatim error instead of a paraphrase, and unwraps to ErrPostgresAuthFailed
// so callers that only classify can use errors.Is.
type PostgresAuthError struct {
	// Response is the upstream's ErrorResponse, copied out of pgproto3's
	// reusable buffer.
	Response *pgproto3.ErrorResponse
}

// Error renders the upstream's message.
func (e *PostgresAuthError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPostgresAuthFailed.Error(), e.Response.Message)
}

// Unwrap makes errors.Is(err, ErrPostgresAuthFailed) true.
func (e *PostgresAuthError) Unwrap() error { return ErrPostgresAuthFailed }

// ConnectPostgres dials the target through dial, negotiates TLS per cfg.SSLMode
// and completes the PostgreSQL login with cfg's credentials. It returns as soon
// as the upstream reports ReadyForQuery.
//
// logger may be nil; it is only used to note protocol messages that arrive
// where none were expected.
func ConnectPostgres(ctx context.Context, dial DialFunc, cfg PostgresConfig, logger *slog.Logger) (*PostgresUpstream, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}

	// negotiatePostgresSSL returns a nil conn on failure and leaves the one it
	// was given open, so the close below must name the original.
	negotiated, encrypted, err := negotiatePostgresSSL(ctx, conn, PlanFor(cfg.SSLMode, cfg.Host))
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("upstream SSL negotiation: %w", err)
	}

	conn = negotiated

	up := &PostgresUpstream{
		Conn:     conn,
		Frontend: pgproto3.NewFrontend(conn, conn),
		TLS:      encrypted,
	}

	if err := sendPostgresStartup(conn, cfg); err != nil {
		_ = conn.Close()

		return nil, err
	}

	if err := up.login(ctx, cfg, logger); err != nil {
		_ = conn.Close()

		return nil, err
	}

	return up, nil
}

// sendPostgresStartup writes the StartupMessage that opens the login.
func sendPostgresStartup(conn net.Conn, cfg PostgresConfig) error {
	startupMsg := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{
			"user":             cfg.Username,
			"database":         cfg.Database,
			"application_name": cfg.ApplicationName,
		},
	}

	buf, err := startupMsg.Encode(nil)
	if err != nil {
		return fmt.Errorf("failed to encode startup message: %w", err)
	}

	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("failed to send startup message to upstream: %w", err)
	}

	return nil
}

// login drives the authentication exchange to ReadyForQuery, collecting the
// startup state on u along the way.
func (u *PostgresUpstream) login(ctx context.Context, cfg PostgresConfig, logger *slog.Logger) error {
	var scram *scramClient

	for {
		msg, err := u.Frontend.Receive()
		if err != nil {
			return fmt.Errorf("failed to receive from upstream: %w", err)
		}

		// The message classes are split so each half stays readable: auth
		// challenges need the password and the SCRAM state machine, startup
		// state is pure bookkeeping.
		handled, err := u.handleAuthMessage(msg, cfg, &scram)
		if err != nil {
			return err
		}

		if handled {
			continue
		}

		done, err := u.handleStartupMessage(ctx, msg, logger)
		if err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

// handleAuthMessage answers the server's authentication challenges. It reports
// whether msg was one; anything else is left to handleStartupMessage.
func (u *PostgresUpstream) handleAuthMessage(
	msg pgproto3.BackendMessage,
	cfg PostgresConfig,
	scram **scramClient,
) (bool, error) {
	switch typedMsg := msg.(type) {
	case *pgproto3.AuthenticationOk:
		// Nothing to answer; the startup state follows.
		return true, nil

	case *pgproto3.AuthenticationCleartextPassword:
		if err := u.send(&pgproto3.PasswordMessage{Password: cfg.Password}); err != nil {
			return true, fmt.Errorf("failed to send password: %w", err)
		}

		return true, nil

	case *pgproto3.AuthenticationMD5Password:
		hash := computeMD5Password(cfg.Password, cfg.Username, typedMsg.Salt)
		if err := u.send(&pgproto3.PasswordMessage{Password: hash}); err != nil {
			return true, fmt.Errorf("failed to send MD5 password: %w", err)
		}

		return true, nil

	case *pgproto3.AuthenticationSASL:
		return true, u.beginSCRAM(typedMsg, cfg.Password, scram)

	case *pgproto3.AuthenticationSASLContinue:
		return true, u.continueSCRAM(typedMsg, *scram)

	case *pgproto3.AuthenticationSASLFinal:
		err := finalizeSCRAM(typedMsg, *scram)
		*scram = nil

		return true, err

	default:
		return false, nil
	}
}

// handleStartupMessage collects the post-authentication startup state. It
// returns true once ReadyForQuery ends the login.
func (u *PostgresUpstream) handleStartupMessage(
	ctx context.Context,
	msg pgproto3.BackendMessage,
	logger *slog.Logger,
) (bool, error) {
	switch typedMsg := msg.(type) {
	case *pgproto3.ParameterStatus:
		// Copy: pgproto3 reuses the same struct across Receive calls.
		u.ParameterStatuses = append(u.ParameterStatuses, &pgproto3.ParameterStatus{
			Name:  typedMsg.Name,
			Value: typedMsg.Value,
		})

		return false, nil

	case *pgproto3.BackendKeyData:
		u.BackendKeyData = &pgproto3.BackendKeyData{
			ProcessID: typedMsg.ProcessID,
			SecretKey: slices.Clone(typedMsg.SecretKey),
		}

		return false, nil

	case *pgproto3.ReadyForQuery:
		u.ReadyForQuery = &pgproto3.ReadyForQuery{TxStatus: typedMsg.TxStatus}

		return true, nil

	case *pgproto3.ErrorResponse:
		// Copy before returning: the caller may forward it to its own client
		// long after pgproto3 has reused the struct.
		resp := *typedMsg

		return false, &PostgresAuthError{Response: &resp}

	default:
		if logger != nil {
			logger.WarnContext(ctx, "unexpected message during upstream auth",
				slog.String("type", fmt.Sprintf("%T", typedMsg)))
		}

		return false, nil
	}
}

// send writes a frontend message and flushes it.
func (u *PostgresUpstream) send(msg pgproto3.FrontendMessage) error {
	u.Frontend.Send(msg)

	return u.Frontend.Flush()
}

// beginSCRAM answers AuthenticationSASL: pick a mechanism, build the client
// first message, send a SASLInitialResponse. The state is parked in *scram
// until the matching Continue/Final messages arrive.
func (u *PostgresUpstream) beginSCRAM(
	msg *pgproto3.AuthenticationSASL,
	password string,
	scram **scramClient,
) error {
	mech := pickSCRAMMechanism(msg.AuthMechanisms)
	if mech == "" {
		return fmt.Errorf("%w: offered=%v", ErrSCRAMNoSupportedMechanism, msg.AuthMechanisms)
	}

	client, err := newSCRAMClient(password)
	if err != nil {
		return fmt.Errorf("scram client: %w", err)
	}

	*scram = client

	if err := u.send(&pgproto3.SASLInitialResponse{AuthMechanism: mech, Data: client.firstMessage()}); err != nil {
		return fmt.Errorf("send SASLInitialResponse: %w", err)
	}

	return nil
}

// continueSCRAM answers AuthenticationSASLContinue (the server first message):
// compute the client proof and send the SASLResponse.
func (u *PostgresUpstream) continueSCRAM(msg *pgproto3.AuthenticationSASLContinue, scram *scramClient) error {
	if scram == nil {
		return fmt.Errorf("%w: SASLContinue without SASL", ErrSCRAMUnexpectedMessage)
	}

	final, err := scram.finalMessage(msg.Data)
	if err != nil {
		return err
	}

	if err := u.send(&pgproto3.SASLResponse{Data: final}); err != nil {
		return fmt.Errorf("send SASLResponse: %w", err)
	}

	return nil
}

// finalizeSCRAM verifies the server's signature on AuthenticationSASLFinal, so
// we know the upstream really possesses the password's SaltedPassword rather
// than merely having said yes.
func finalizeSCRAM(msg *pgproto3.AuthenticationSASLFinal, scram *scramClient) error {
	if scram == nil {
		return fmt.Errorf("%w: SASLFinal without SASL", ErrSCRAMUnexpectedMessage)
	}

	return scram.verifyServerFinal(msg.Data)
}

// computeMD5Password computes the PostgreSQL MD5 password hash:
// "md5" + md5(md5(password + username) + salt).
func computeMD5Password(password, username string, salt [4]byte) string {
	h1 := md5.New()
	h1.Write([]byte(password))
	h1.Write([]byte(username))
	sum1 := hex.EncodeToString(h1.Sum(nil))

	h2 := md5.New()
	h2.Write([]byte(sum1))
	h2.Write(salt[:])

	return "md5" + hex.EncodeToString(h2.Sum(nil))
}

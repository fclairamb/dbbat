package postgresql

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
	"github.com/fclairamb/dbbat/internal/store"
)

// pendingQuery tracks a query that is currently being executed.
type pendingQuery struct {
	sql        string
	startTime  time.Time
	parameters *store.QueryParameters

	// Result capture state
	columnNames   []string         // From RowDescription
	columnOIDs    []uint32         // Type OIDs for decoding
	capturedRows  []store.QueryRow // Accumulated result rows
	capturedBytes int64            // Total bytes captured
	rowNumber     int              // Current row counter
	truncated     bool             // True if limits exceeded
}

// preparedStatement tracks a prepared statement with its type information.
type preparedStatement struct {
	sql      string
	typeOIDs []uint32
}

// portalState tracks a portal with its bound parameters.
type portalState struct {
	stmtName   string
	parameters *store.QueryParameters
}

// copyState tracks a COPY operation in progress.
type copyState struct {
	direction   string // "out" (COPY TO) or "in" (COPY FROM)
	format      byte   // 0=text, 1=binary
	columnNames []string
	dataChunks  [][]byte // Raw CopyData chunks
	totalBytes  int64
	truncated   bool
}

// extendedQueryState tracks state for Extended Query Protocol.
type extendedQueryState struct {
	preparedStatements map[string]*preparedStatement // stmt name -> prepared statement
	portals            map[string]*portalState       // portal name -> portal state
	pendingQueries     []*pendingQuery               // Queue for multiple Execute before Sync
}

// Session represents a proxy session.
type Session struct {
	clientConn    net.Conn
	clientReader  *bufio.Reader // Buffered reader over clientConn so SSL detection can peek the first 4 bytes.
	upstreamConn  net.Conn
	store         *store.Store
	encryptionKey []byte
	logger        *slog.Logger
	ctx           context.Context //nolint:containedctx // Context is needed for the session lifecycle
	queryStorage  config.QueryStorageConfig
	dumpConfig    config.DumpConfig
	dumpWriter    *dump.Writer
	authCache     *cache.AuthCache
	tlsConfig     *tls.Config // nil when TLS is disabled

	// Session state
	user                   *store.User
	database               *store.Database
	grant                  *store.Grant
	connectionUID          uuid.UUID
	clientBackend          *pgproto3.Backend  // To communicate with client (we're the server)
	upstreamFrontend       *pgproto3.Frontend // To communicate with upstream (we're the client)
	authenticated          bool
	bufferedParamStatus    []*pgproto3.ParameterStatus // Buffer for ParameterStatus during upstream auth
	bufferedBackendKeyData *pgproto3.BackendKeyData    // Buffer for BackendKeyData during upstream auth
	currentQuery           *pendingQuery               // Track query in progress for logging
	extendedState          *extendedQueryState         // State for Extended Query Protocol
	clientApplicationName  string                      // application_name provided by the client
	copyState              *copyState                  // Track COPY operation in progress
	upstreamSCRAM          *scramClient                // SCRAM-SHA-256 state for upstream SASL auth

	// Wire-level byte counters for the client-facing socket. Reads count as
	// bytes-from-client (queries the client sent), writes count as
	// bytes-to-client (responses the proxy returned). Together they capture
	// every byte the proxy exchanged with the client during the session
	// — including framing, auth, and error packets — which is the basis for
	// accurate bytes_transferred quotas. Atomics because the read and write
	// halves are driven by separate goroutines (proxyClientToUpstream and
	// proxyUpstreamToClient).
	bytesFromClient *atomic.Int64
	bytesToClient   *atomic.Int64
	// lastBytesSnapshot is the cumulative client-side byte count at the end
	// of the previous query. Per-query bytes = current cumulative - snapshot.
	// Only mutated from the upstream→client goroutine where logQuery runs,
	// so no atomic needed.
	lastBytesSnapshot int64
}

// NewSession creates a new session.
func NewSession(
	clientConn net.Conn,
	dataStore *store.Store,
	encryptionKey []byte,
	logger *slog.Logger,
	ctx context.Context, //nolint:revive // Context parameter order is intentional for this factory
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
	authCache *cache.AuthCache,
	tlsConfig *tls.Config,
) *Session {
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}
	// Wrap before constructing the buffered reader so every byte the
	// pgproto3 backend reads is counted. The TLS upgrade in handleSSLRequest
	// reassigns clientConn to a tls.Conn that wraps this CountingConn, so
	// encrypted bytes still flow through the counter post-upgrade.
	counted := shared.NewCountingConn(clientConn, bytesFromClient, bytesToClient)

	return &Session{
		clientConn:      counted,
		clientReader:    bufio.NewReader(counted),
		store:           dataStore,
		encryptionKey:   encryptionKey,
		logger:          logger,
		ctx:             ctx,
		queryStorage:    queryStorage,
		dumpConfig:      dumpConfig,
		authCache:       authCache,
		tlsConfig:       tlsConfig,
		bytesFromClient: bytesFromClient,
		bytesToClient:   bytesToClient,
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
	}
}

// Run runs the session.
func (s *Session) Run() error {
	defer s.cleanup()

	// Negotiate TLS before constructing the backend — wrapping clientConn
	// after the backend is built would leave the backend reading from the
	// plaintext side.
	if err := s.negotiateSSL(); err != nil {
		return fmt.Errorf("SSL negotiation failed: %w", err)
	}

	// Create backend for client (we're the server to the client). Read side
	// uses the buffered reader so any bytes peeked during negotiation are
	// still in scope.
	s.clientBackend = pgproto3.NewBackend(s.clientReader, s.clientConn)

	// Authenticate
	if err := s.authenticate(); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Connect to upstream (this creates s.upstreamFrontend)
	if err := s.connectUpstream(); err != nil {
		return fmt.Errorf("upstream connection failed: %w", err)
	}

	// Create connection record
	sourceIP := store.ExtractSourceIP(s.clientConn.RemoteAddr())

	conn, err := s.store.CreateConnection(s.ctx, s.user.UID, s.database.UID, sourceIP)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to create connection record", slog.Any("error", err))
	} else {
		s.connectionUID = conn.UID
	}

	s.logger = s.logger.With("connection_uid", s.connectionUID)

	// Initialize dump writer if configured
	if s.dumpConfig.Dir != "" && s.connectionUID != uuid.Nil {
		dumpPath := filepath.Join(s.dumpConfig.Dir, s.connectionUID.String()+dump.FileExt)

		dw, err := dump.NewWriter(dumpPath, dump.Header{
			SessionID: s.connectionUID.String(),
			Protocol:  dump.ProtocolPostgreSQL,
			StartTime: time.Now(),
			Connection: map[string]any{
				"database":      s.database.DatabaseName,
				"user":          s.user.Username,
				"upstream_addr": net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port)),
			},
		}, s.dumpConfig.MaxSize)
		if err != nil {
			s.logger.WarnContext(s.ctx, "failed to create dump writer", slog.Any("error", err))
		} else {
			s.dumpWriter = dw
			// Wrap connections to capture traffic during the proxy phase
			clientTap := dump.NewTapConn(s.clientConn, dw, dump.DirClientToServer, dump.DirServerToClient)
			upstreamTap := dump.NewTapConn(s.upstreamConn, dw, dump.DirServerToClient, dump.DirClientToServer)
			s.clientBackend = pgproto3.NewBackend(clientTap, clientTap)
			s.upstreamFrontend = pgproto3.NewFrontend(upstreamTap, upstreamTap)
		}
	}

	// Proxy messages
	return s.proxyMessages()
}

// proxyMessages proxies messages between client and upstream.
func (s *Session) proxyMessages() error {
	// Channel to receive errors from goroutines
	errChan := make(chan error, 2)

	// Client to upstream
	go func() {
		err := s.proxyClientToUpstream()
		errChan <- err
	}()

	// Upstream to client
	go func() {
		err := s.proxyUpstreamToClient()
		errChan <- err
	}()

	// Wait for either direction to close or error
	err := <-errChan

	return err
}

// proxyClientToUpstream proxies messages from client to upstream.
func (s *Session) proxyClientToUpstream() error {
	for {
		msg, err := s.clientBackend.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("failed to receive from client: %w", err)
		}

		s.logger.InfoContext(s.ctx, "received message from client", slog.Any("message", msg))

		// Handle query interception for Simple and Extended Query Protocols
		var interceptErr error

		switch m := msg.(type) {
		case *pgproto3.Query:
			interceptErr = s.handleQuery(m)
		case *pgproto3.Parse:
			interceptErr = s.handleParse(m)
		case *pgproto3.Bind:
			s.handleBind(m)
		case *pgproto3.Execute:
			interceptErr = s.handleExecute(m)
		case *pgproto3.Close:
			s.handleClose(m)
		case *pgproto3.CopyData:
			// Client sending COPY data to server (COPY FROM)
			if s.copyState != nil && s.copyState.direction == "in" {
				s.captureCopyData(m.Data)
			}
		case *pgproto3.CopyDone:
			// Client finished sending COPY data
			if s.copyState != nil && s.copyState.direction == "in" {
				s.logger.InfoContext(s.ctx, "COPY IN done (from client)", slog.Int64("total_bytes", s.copyState.totalBytes), slog.Bool("truncated", s.copyState.truncated))
			}
		case *pgproto3.CopyFail:
			// Client aborted COPY
			if s.copyState != nil {
				s.logger.WarnContext(s.ctx, "COPY failed", slog.String("message", m.Message))
				s.copyState = nil
			}
		}

		if interceptErr != nil {
			s.sendQueryError(interceptErr)

			continue
		}

		// Forward message to upstream
		s.upstreamFrontend.Send(msg)

		if err := s.upstreamFrontend.Flush(); err != nil {
			return fmt.Errorf("failed to send to upstream: %w", err)
		}
	}
}

// sendQueryError sends a query error to the client.
func (s *Session) sendQueryError(queryErr error) {
	errMsg := &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "42000",
		Message:  queryErr.Error(),
	}

	errBuf, encodeErr := errMsg.Encode(nil)
	if encodeErr != nil {
		s.logger.ErrorContext(s.ctx, "failed to encode error message", slog.Any("error", encodeErr))

		return
	}

	if _, err := s.clientConn.Write(errBuf); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to write error to client", slog.Any("error", err))

		return
	}

	readyMsg := &pgproto3.ReadyForQuery{TxStatus: 'I'}

	readyBuf, encodeErr := readyMsg.Encode(nil)
	if encodeErr != nil {
		s.logger.ErrorContext(s.ctx, "failed to encode ready message", slog.Any("error", encodeErr))

		return
	}

	if _, err := s.clientConn.Write(readyBuf); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to write ready to client", slog.Any("error", err))
	}
}

// getCurrentPendingQuery returns the query that should receive result data.
// For Simple Query Protocol: s.currentQuery
// For Extended Query Protocol: last item in pendingQueries (most recent Execute).
func (s *Session) getCurrentPendingQuery() *pendingQuery {
	// Simple Query Protocol
	if s.currentQuery != nil {
		return s.currentQuery
	}

	// Extended Query Protocol - return the most recent pending query
	if len(s.extendedState.pendingQueries) > 0 {
		return s.extendedState.pendingQueries[len(s.extendedState.pendingQueries)-1]
	}

	return nil
}

// proxyUpstreamToClient proxies messages from upstream to client.
//
//nolint:gocognit,cyclop // Protocol handling with many message types inherently has high complexity
func (s *Session) proxyUpstreamToClient() error {
	var rowsAffected *int64
	var queryError *string

	for {
		msg, err := s.upstreamFrontend.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("failed to receive from upstream: %w", err)
		}

		// Capture query result data for logging
		switch m := msg.(type) {
		case *pgproto3.RowDescription:
			// Capture column metadata for the current/pending query
			query := s.getCurrentPendingQuery()
			if query != nil {
				query.columnNames = make([]string, len(m.Fields))
				query.columnOIDs = make([]uint32, len(m.Fields))
				for i, field := range m.Fields {
					query.columnNames[i] = string(field.Name)
					query.columnOIDs[i] = field.DataTypeOID
				}
			}

		case *pgproto3.CommandComplete:
			// Parse rows affected from CommandTag (e.g., "UPDATE 5")
			rowsAffected = parseRowsAffected(string(m.CommandTag))
			// Pop from pending queue if using Extended Query Protocol
			if len(s.extendedState.pendingQueries) > 0 {
				s.currentQuery = s.extendedState.pendingQueries[0]
				s.extendedState.pendingQueries = s.extendedState.pendingQueries[1:]
			}

		case *pgproto3.ErrorResponse:
			// Capture error message
			errMsg := m.Message
			queryError = &errMsg
			// Pop from pending queue if using Extended Query Protocol
			if len(s.extendedState.pendingQueries) > 0 {
				s.currentQuery = s.extendedState.pendingQueries[0]
				s.extendedState.pendingQueries = s.extendedState.pendingQueries[1:]
			}

		case *pgproto3.DataRow:
			// Compute the row's payload size for the result-capture limits
			// only — wire-level bytes_transferred is tracked via the
			// CountingConn around clientConn, not field-summed here.
			rowSize := int64(0)
			for _, val := range m.Values {
				rowSize += int64(len(val))
			}

			// Capture row data if enabled and within limits
			query := s.getCurrentPendingQuery()
			if query != nil && s.queryStorage.StoreResults && !query.truncated {
				// Check if this row would exceed limits
				if query.rowNumber >= s.queryStorage.MaxResultRows ||
					query.capturedBytes+rowSize > s.queryStorage.MaxResultBytes {
					// Limits exceeded - discard all captured rows and stop capturing
					query.truncated = true
					query.capturedRows = nil // Discard all previously captured rows
					s.logger.WarnContext(s.ctx, "result capture refused - limits exceeded",
						slog.Int("rows_captured", query.rowNumber),
						slog.Int64("bytes_captured", query.capturedBytes),
						slog.Int("max_rows", s.queryStorage.MaxResultRows),
						slog.Int64("max_bytes", s.queryStorage.MaxResultBytes))
				} else {
					row := s.convertDataRow(m.Values, query.columnNames, query.columnOIDs)
					query.capturedRows = append(query.capturedRows, row)
					query.capturedBytes += rowSize
					query.rowNumber++
				}
			}

		case *pgproto3.CopyOutResponse:
			// Server is starting a COPY TO operation (sending data to client)
			s.copyState = &copyState{
				direction: "out",
				format:    m.OverallFormat,
			}
			// Extract column names from the current query if available
			if s.currentQuery != nil {
				s.copyState.columnNames = parseCopyColumnNames(s.currentQuery.sql)
			}
			s.logger.InfoContext(s.ctx, "COPY OUT started", slog.Int("format", int(m.OverallFormat)))

		case *pgproto3.CopyInResponse:
			// Server is ready for a COPY FROM operation (receiving data from client)
			s.copyState = &copyState{
				direction: "in",
				format:    m.OverallFormat,
			}
			// Extract column names from the current query if available
			if s.currentQuery != nil {
				s.copyState.columnNames = parseCopyColumnNames(s.currentQuery.sql)
			}
			s.logger.InfoContext(s.ctx, "COPY IN started", slog.Int("format", int(m.OverallFormat)))

		case *pgproto3.CopyData:
			// Server sending COPY data to client (COPY TO).
			// Wire bytes are counted by the CountingConn — no manual
			// addition here.
			if s.copyState != nil && s.copyState.direction == "out" {
				s.captureCopyData(m.Data)
			}

		case *pgproto3.CopyDone:
			// COPY operation complete - finalize capture
			if s.copyState != nil && s.copyState.direction == "out" {
				s.logger.InfoContext(s.ctx, "COPY OUT done", slog.Int64("total_bytes", s.copyState.totalBytes), slog.Bool("truncated", s.copyState.truncated))
			}

		case *pgproto3.ReadyForQuery:
			// Query complete - log it
			if s.currentQuery != nil {
				// Wire-level diff: cumulative client-side bytes since the
				// previous query end (or session start). Captures the query
				// text the client sent, the response framing, error
				// packets, and pre-first-query auth bytes — everything the
				// row-summed counter previously missed.
				total := s.bytesFromClient.Load() + s.bytesToClient.Load()
				bytesTransferred := total - s.lastBytesSnapshot
				s.lastBytesSnapshot = total

				s.logQuery(rowsAffected, queryError, bytesTransferred)
				s.currentQuery = nil
				s.copyState = nil // Reset copy state
				rowsAffected = nil
				queryError = nil
			}
		}

		// Forward message to client (send as backend message to client)
		s.clientBackend.Send(msg)

		if err := s.clientBackend.Flush(); err != nil {
			return fmt.Errorf("failed to send to client: %w", err)
		}
	}
}

// cleanup closes connections and updates records.
func (s *Session) cleanup() {
	if s.dumpWriter != nil {
		if err := s.dumpWriter.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close dump writer", slog.Any("error", err))
		}
	}

	if s.connectionUID != uuid.Nil {
		if err := s.store.CloseConnection(s.ctx, s.connectionUID); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close connection record", slog.Any("error", err))
		}
	}

	if s.upstreamConn != nil {
		if err := s.upstreamConn.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close upstream connection", slog.Any("error", err))
		}
	}
}

// sendError sends an error to the client and closes the connection.
func (s *Session) sendError(message string) {
	errMsg := &pgproto3.ErrorResponse{
		Severity: "FATAL",
		Code:     "28000", // invalid_authorization_specification
		Message:  message,
	}

	buf, err := errMsg.Encode(nil)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "failed to encode error message", slog.Any("error", err))

		return
	}

	if _, err := s.clientConn.Write(buf); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to write error to client", slog.Any("error", err))
	}
}

// Magic version numbers that identify the optional pre-StartupMessage frames
// PostgreSQL clients can send.
//
//	pgSSLRequestCode    — TLS upgrade probe.
//	pgGSSEncRequestCode — Kerberos/GSSAPI encryption probe (libpq 17 sends
//	                      this by default with gssencmode=prefer).
const (
	pgSSLRequestCode    = 80877103
	pgGSSEncRequestCode = 80877104
)

// maxStartupNegotiationRounds caps how many SSL/GSS denials we'll loop through
// before giving up. A real client does at most two (GSS then SSL); three is
// the bound where a misbehaving or malicious client gets cut off.
const maxStartupNegotiationRounds = 3

// negotiateSSL handles any optional SSL/GSS encryption probes a PG client
// sends before the real StartupMessage. The upgrade has to happen here — once
// pgproto3.NewBackend captures the conn, wrapping clientConn would leave the
// backend reading from the wrong side.
//
// libpq 17 with default settings sends GSSEncRequest, then SSLRequest, then
// the StartupMessage. Both probes get a 'N' refusal byte unless the proxy
// can speak that encryption (only TLS today). The loop bounds at
// maxStartupNegotiationRounds so a buggy client looping SSL/GSS forever
// can't pin a goroutine.
func (s *Session) negotiateSSL() error {
	for round := 0; round < maxStartupNegotiationRounds; round++ {
		header, err := s.clientReader.Peek(8)
		if err != nil {
			return fmt.Errorf("peek startup header: %w", err)
		}

		length := int(header[0])<<24 | int(header[1])<<16 | int(header[2])<<8 | int(header[3])
		if length != 8 {
			// Not a length-8 negotiation frame — leave the bytes for
			// receiveStartupMessage to parse as a real StartupMessage
			// (or whatever frame the client actually sent).
			return nil
		}

		version := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		switch version {
		case pgSSLRequestCode:
			done, err := s.handleSSLRequest()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
		case pgGSSEncRequestCode:
			if err := s.handleGSSRequest(); err != nil {
				return err
			}
		default:
			// Length-8 with an unknown magic is malformed; surface a clear
			// error rather than letting receiveStartupMessage stall on a
			// truncated parse.
			return fmt.Errorf("%w: 0x%08x", ErrUnknownStartupMagic, version)
		}
	}

	return ErrTooManyNegotiationRounds
}

// handleSSLRequest consumes the 8-byte SSLRequest, either denies it ('N',
// returns done=false to allow another round e.g. GSS-then-SSL on a libpq
// client whose order we're not strict about) or accepts it ('S', runs the
// handshake, returns done=true because the client only ever sends the real
// StartupMessage after a successful TLS upgrade).
func (s *Session) handleSSLRequest() (bool, error) {
	if _, err := s.clientReader.Discard(8); err != nil {
		return false, fmt.Errorf("discard SSLRequest: %w", err)
	}

	if s.tlsConfig == nil {
		if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
			return false, fmt.Errorf("write SSL deny: %w", err)
		}
		return false, nil
	}

	if _, err := s.clientConn.Write([]byte{'S'}); err != nil {
		return false, fmt.Errorf("write SSL accept: %w", err)
	}

	// tls.Server wraps s.clientConn — which is itself a CountingConn around
	// the raw socket. Encrypted bytes still flow through the counter, so
	// post-upgrade reads/writes continue to be accounted in the session
	// bytes_transferred quota.
	tlsConn := tls.Server(s.clientConn, s.tlsConfig)
	if err := tlsConn.HandshakeContext(s.ctx); err != nil {
		return false, fmt.Errorf("TLS handshake: %w", err)
	}

	s.clientConn = tlsConn
	s.clientReader = bufio.NewReader(tlsConn)
	return true, nil
}

// handleGSSRequest consumes the 8-byte GSSEncRequest and refuses with 'N'.
// dbbat doesn't terminate Kerberos, so the client falls back to the next
// negotiation step (typically SSLRequest, then plain StartupMessage).
func (s *Session) handleGSSRequest() error {
	if _, err := s.clientReader.Discard(8); err != nil {
		return fmt.Errorf("discard GSSEncRequest: %w", err)
	}

	if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
		return fmt.Errorf("write GSS deny: %w", err)
	}

	return nil
}

// receiveStartupMessage receives the startup message from the client. By the
// time this is called, negotiateSSL has already consumed any SSLRequest
// preamble — the bytes here are guaranteed to be the StartupMessage proper.
func (s *Session) receiveStartupMessage() (pgproto3.FrontendMessage, error) {
	lengthBuf := make([]byte, 4)
	if _, err := io.ReadFull(s.clientReader, lengthBuf); err != nil {
		return nil, err
	}

	length := int(lengthBuf[0])<<24 | int(lengthBuf[1])<<16 | int(lengthBuf[2])<<8 | int(lengthBuf[3])

	msgBuf := make([]byte, length)
	copy(msgBuf, lengthBuf)

	if _, err := io.ReadFull(s.clientReader, msgBuf[4:]); err != nil {
		return nil, err
	}

	startup := &pgproto3.StartupMessage{}
	if err := startup.Decode(msgBuf[4:]); err != nil {
		return nil, err
	}

	return startup, nil
}

// receivePasswordMessage receives a password message from the client. Reads
// go through clientReader so any bytes pipelined by the client during the
// startup phase are still seen here.
func (s *Session) receivePasswordMessage() (*pgproto3.PasswordMessage, error) {
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(s.clientReader, typeBuf); err != nil {
		return nil, err
	}

	lengthBuf := make([]byte, 4)
	if _, err := io.ReadFull(s.clientReader, lengthBuf); err != nil {
		return nil, err
	}

	length := int(lengthBuf[0])<<24 | int(lengthBuf[1])<<16 | int(lengthBuf[2])<<8 | int(lengthBuf[3])

	dataBuf := make([]byte, length-4)
	if _, err := io.ReadFull(s.clientReader, dataBuf); err != nil {
		return nil, err
	}

	password := &pgproto3.PasswordMessage{}
	// Null-terminated password string
	password.Password = string(dataBuf[:len(dataBuf)-1])

	return password, nil
}

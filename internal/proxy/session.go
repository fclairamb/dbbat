package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
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
	upstreamConn  net.Conn
	store         *store.Store
	encryptionKey []byte
	logger        *slog.Logger
	ctx           context.Context //nolint:containedctx // Context is needed for the session lifecycle
	queryStorage  config.QueryStorageConfig
	authCache     *cache.AuthCache

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
}

// NewSession creates a new session.
func NewSession(
	clientConn net.Conn,
	dataStore *store.Store,
	encryptionKey []byte,
	logger *slog.Logger,
	ctx context.Context, //nolint:revive // Context parameter order is intentional for this factory
	queryStorage config.QueryStorageConfig,
	authCache *cache.AuthCache,
) *Session {
	return &Session{
		clientConn:    clientConn,
		store:         dataStore,
		encryptionKey: encryptionKey,
		logger:        logger,
		ctx:           ctx,
		queryStorage:  queryStorage,
		authCache:     authCache,
		extendedState: &extendedQueryState{
			preparedStatements: make(map[string]*preparedStatement),
			portals:            make(map[string]*portalState),
		},
	}
}

// Run runs the session.
func (s *Session) Run() error {
	defer s.cleanup()

	// Create backend for client (we're the server to the client)
	s.clientBackend = pgproto3.NewBackend(s.clientConn, s.clientConn)

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
	var bytesTransferred int64

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
			// Track bytes transferred (approximate size of row data)
			rowSize := int64(0)
			for _, val := range m.Values {
				rowSize += int64(len(val))
			}
			bytesTransferred += rowSize

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
			// Server sending COPY data to client (COPY TO)
			if s.copyState != nil && s.copyState.direction == "out" {
				s.captureCopyData(m.Data)
			}
			bytesTransferred += int64(len(m.Data))

		case *pgproto3.CopyDone:
			// COPY operation complete - finalize capture
			if s.copyState != nil && s.copyState.direction == "out" {
				s.logger.InfoContext(s.ctx, "COPY OUT done", slog.Int64("total_bytes", s.copyState.totalBytes), slog.Bool("truncated", s.copyState.truncated))
			}

		case *pgproto3.ReadyForQuery:
			// Query complete - log it
			if s.currentQuery != nil {
				s.logQuery(rowsAffected, queryError, bytesTransferred)
				s.currentQuery = nil
				s.copyState = nil // Reset copy state
				rowsAffected = nil
				queryError = nil
				bytesTransferred = 0
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

// receiveStartupMessage receives the startup message from the client.
func (s *Session) receiveStartupMessage() (pgproto3.FrontendMessage, error) {
	// Read message length (4 bytes)
	lengthBuf := make([]byte, 4)
	if _, err := io.ReadFull(s.clientConn, lengthBuf); err != nil {
		return nil, err
	}

	length := int(lengthBuf[0])<<24 | int(lengthBuf[1])<<16 | int(lengthBuf[2])<<8 | int(lengthBuf[3])

	// Check for SSLRequest
	if length == 8 {
		// Read protocol version
		versionBuf := make([]byte, 4)
		if _, err := io.ReadFull(s.clientConn, versionBuf); err != nil {
			return nil, err
		}

		version := int(versionBuf[0])<<24 | int(versionBuf[1])<<16 | int(versionBuf[2])<<8 | int(versionBuf[3])

		const sslRequestCode = 80877103
		if version == sslRequestCode {
			// Deny SSL
			if _, err := s.clientConn.Write([]byte{'N'}); err != nil {
				return nil, fmt.Errorf("failed to deny SSL: %w", err)
			}
			// Read actual startup message
			return s.receiveStartupMessage()
		}
	}

	// Read the rest of the message
	msgBuf := make([]byte, length)
	copy(msgBuf, lengthBuf)

	if _, err := io.ReadFull(s.clientConn, msgBuf[4:]); err != nil {
		return nil, err
	}

	// Parse startup message
	startup := &pgproto3.StartupMessage{}
	if err := startup.Decode(msgBuf[4:]); err != nil {
		return nil, err
	}

	return startup, nil
}

// receivePasswordMessage receives a password message from the client.
func (s *Session) receivePasswordMessage() (*pgproto3.PasswordMessage, error) {
	// Read message type (1 byte)
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(s.clientConn, typeBuf); err != nil {
		return nil, err
	}

	// Read message length (4 bytes)
	lengthBuf := make([]byte, 4)
	if _, err := io.ReadFull(s.clientConn, lengthBuf); err != nil {
		return nil, err
	}

	length := int(lengthBuf[0])<<24 | int(lengthBuf[1])<<16 | int(lengthBuf[2])<<8 | int(lengthBuf[3])

	// Read the password data
	dataBuf := make([]byte, length-4)
	if _, err := io.ReadFull(s.clientConn, dataBuf); err != nil {
		return nil, err
	}

	// Parse password message
	password := &pgproto3.PasswordMessage{}
	// Null-terminated password string
	password.Password = string(dataBuf[:len(dataBuf)-1])

	return password, nil
}

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
	"sync"
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
	columnNames   []string // From RowDescription
	columnOIDs    []uint32 // Type OIDs for decoding
	capturedBytes int64    // Total bytes captured
	rowNumber     int      // Current row counter
	truncated     bool     // True if limits exceeded

	// rowSink streams captured rows to the shared batching writer instead of
	// holding the whole capture in RAM until the query ends. Created on the
	// first captured row (see ensureRowSink), which is also where the parent
	// queries row starts being inserted — query_rows.query_id is a foreign
	// key, so the parent has to exist before the first batch flushes.
	rowSink *shared.QuerySink

	// approvalUID is the uid of the row an approval hold already inserted for
	// this statement (uuid.Nil when no hold happened). Non-nil means the
	// completion path must UPDATE that row rather than INSERT a second one.
	approvalUID uuid.UUID
}

// preparedStatement tracks a prepared statement with its type information.
type preparedStatement struct {
	sql string
	// typeOIDs are the parameter types the *client* declared in Parse. Most
	// modern drivers (pgx included) leave this empty and let the server infer
	// the types.
	typeOIDs []uint32
	// resolvedOIDs are the parameter types the *server* reported in the
	// ParameterDescription answering a Describe('S'). They are the fallback
	// used to decode binary bind parameters when typeOIDs is empty.
	resolvedOIDs []uint32
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
	// mu guards preparedStatements and pendingDescribes, which are touched from
	// both proxy goroutines: the client→upstream one on Parse/Bind/Execute/
	// Close/Describe, and the upstream→client one on ParameterDescription.
	mu                 sync.Mutex
	preparedStatements map[string]*preparedStatement // stmt name -> prepared statement
	portals            map[string]*portalState       // portal name -> portal state
	pendingQueries     []*pendingQuery               // Queue for multiple Execute before Sync
	// pendingDescribes is a FIFO of statement names for which the client sent
	// Describe('S', name) and the server has not yet answered with a
	// ParameterDescription. The server answers describes in order, so the head
	// of the queue names the statement the next ParameterDescription belongs to.
	pendingDescribes []string

	// errorUntilSync mirrors PostgreSQL's own extended-protocol error state:
	// once a message in a batch fails, every following message is discarded
	// until the client sends Sync. dbbat has to honor it for the statements
	// *it* refuses too — otherwise a refused Parse is followed by a Bind and an
	// Execute for a statement upstream never heard of, which both confuses
	// upstream and queues a phantom pendingQuery here that the next
	// CommandComplete would pop and log as a second row.
	//
	// Only ever touched from the client→upstream goroutine.
	errorUntilSync bool
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
	// dumpUploader ships the finished capture to blob storage when the
	// session ends. nil = local-only captures (the default).
	dumpUploader *dump.Uploader
	authCache    *cache.AuthCache
	tlsConfig    *tls.Config // nil when TLS is disabled

	// Session state
	user                  *store.User
	database              *store.Server
	grant                 *store.Grant
	connectionUID         uuid.UUID
	clientBackend         *pgproto3.Backend  // To communicate with client (we're the server)
	upstreamFrontend      *pgproto3.Frontend // To communicate with upstream (we're the client)
	authenticated         bool
	currentQuery          *pendingQuery           // Track query in progress for logging
	extendedState         *extendedQueryState     // State for Extended Query Protocol
	clientApplicationName string                  // application_name provided by the client
	copyState             *copyState              // Track COPY operation in progress
	upstreamTLS           bool                    // Whether the proxy→upstream leg ended up encrypted
	guard                 *shared.LimitGuard      // Mid-stream time/bandwidth limit enforcement
	revocation            *cache.RevocationHandle // Signaled when this session's grant is revoked mid-flight

	// watched sits below the counting conn (and below TLS) so an approval
	// hold can keep reading the client socket while the session goroutine is
	// blocked on a human. Without it a client FIN would go unnoticed and the
	// hold — which has no timeout by design — would never end.
	watched *shared.WatchedConn

	// approvalDeps/approvalGate implement pattern-triggered approval holds.
	// The gate is built once the grant is known; before that it is inert.
	approvalDeps shared.ApprovalDeps
	approvalGate *shared.ApprovalGate

	// stream publishes this session's queries and lifecycle to the live
	// event stream. Best-effort and non-blocking, always.
	stream *shared.StreamPublisher

	// heldQueryUID is the uid of the statement currently parked on a human,
	// uuid.Nil when nothing is parked. Read by the out-of-band CancelRequest
	// path, which arrives on a different TCP connection entirely.
	heldMu       sync.Mutex
	heldQueryUID uuid.UUID

	// cancels routes PostgreSQL CancelRequests (separate TCP connection) back
	// to the session that owns the BackendKeyData they carry.
	cancels     *cancelRegistry
	cancelKeyMu sync.Mutex
	cancelKey   *cancelKey

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

	// rowWriter batches captured result rows off the data path. Shared with
	// every other session and protocol in the process.
	rowWriter *shared.RowWriter

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
	rowWriter *shared.RowWriter,
) *Session {
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}
	// Wrap before constructing the buffered reader so every byte the
	// pgproto3 backend reads is counted. The TLS upgrade in handleSSLRequest
	// reassigns clientConn to a tls.Conn that wraps this CountingConn, so
	// encrypted bytes still flow through the counter post-upgrade.
	//
	// WatchedConn goes *under* the counter (and therefore under any later TLS
	// upgrade) so a parked hold reads raw transport bytes it never has to
	// decrypt, and so replayed bytes are counted exactly once, on replay.
	watched := shared.NewWatchedConn(clientConn)
	counted := shared.NewCountingConn(watched, bytesFromClient, bytesToClient)

	// Without keepalive, a hard-killed client or a dead network path never
	// produces a FIN, and "held until the client disconnects" silently means
	// "held forever".
	_ = shared.EnableClientKeepAlive(clientConn)

	return &Session{
		watched:         watched,
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
		rowWriter:       rowWriter,
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

	// s.grant is always set here: authenticate() returns an error (aborting
	// Run before this point) whenever GetActiveGrant fails.
	conn, err := s.store.CreateConnection(s.ctx, s.user.UID, s.database.UID, sourceIP,
		store.WithUpstreamTLS(s.upstreamTLS), store.WithGrantUID(s.grant.UID))
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
	// Register this live session against its grant so an admin revoke can
	// signal it (block the next query + tear it down) instead of waiting for a
	// reconnect. s.grant is always set here (authentication requires a grant);
	// deregistered in cleanup.
	s.revocation = s.store.Revocations().Register(s.grant.UID)

	// Build the limit guard once the grant is known, then run a watchdog that
	// tears the session down if a limit is crossed (or the grant is revoked)
	// while a query is blocked producing no traffic (the inline check in
	// proxyUpstreamToClient handles the actively-streaming case with a clean
	// error frame).
	s.guard = shared.NewLimitGuard(s.grant, s.bytesFromClient, s.bytesToClient).
		WithRevocation(s.revocation.Flag())

	// The approval gate compiles the grant's patterns once, here, so the
	// per-statement cost of the (overwhelmingly common) no-pattern case is a
	// single nil check on the hot path.
	s.approvalGate = shared.NewApprovalGate(
		s.approvalDeps, s.grant, s.connectionUID, s.user, s.database.Name,
	)
	s.stream = shared.NewStreamPublisher(s.approvalDeps, s.connectionUID, s.user, s.database.Name).
		WithApprovals(s.approvalGate)
	s.stream.Connection(s.ctx, shared.ConnectionOpened)

	watchCtx, cancelWatch := context.WithCancel(s.ctx)
	defer cancelWatch()

	go s.guard.Watch(watchCtx, shared.DefaultLimitPollInterval, s.onLimitViolation)

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

// onLimitViolation is invoked by the limit watchdog when a time/bandwidth limit
// is crossed. It force-closes both conns, unblocking whichever relay goroutine
// is parked in a Read/Write so the session tears down. The idle case (a query
// blocked without producing traffic) can only be terminated this way — there is
// no message boundary at which to inject a clean ErrorResponse.
func (s *Session) onLimitViolation(err error) {
	s.logger.WarnContext(s.ctx, "terminating session: grant no longer valid mid-stream",
		slog.Any("error", err))

	if s.upstreamConn != nil {
		_ = s.upstreamConn.Close()
	}

	if s.clientConn != nil {
		_ = s.clientConn.Close()
	}
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

		// A batch dbbat already failed is dead until Sync — see
		// extendedQueryState.errorUntilSync.
		if s.discardUntilSync(msg) {
			continue
		}

		if interceptErr := s.interceptClientMessage(msg); interceptErr != nil {
			s.answerRefusal(msg, interceptErr)

			continue
		}

		// Forward message to upstream
		s.upstreamFrontend.Send(msg)

		if err := s.upstreamFrontend.Flush(); err != nil {
			return fmt.Errorf("failed to send to upstream: %w", err)
		}
	}
}

// interceptClientMessage runs one client message through the gate for its
// protocol. A non-nil result means the message must not be forwarded and the
// client is owed a refusal.
func (s *Session) interceptClientMessage(msg pgproto3.FrontendMessage) error {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return s.handleQuery(m)
	case *pgproto3.Parse:
		return s.handleParse(m)
	case *pgproto3.Bind:
		s.handleBind(m)
	case *pgproto3.Execute:
		return s.handleExecute(m)
	case *pgproto3.Describe:
		s.handleDescribe(m)
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

	return nil
}

// answerRefusal tells the client dbbat will not forward the message it just
// sent.
//
// A refusal inside an extended-query batch follows the protocol's error rule:
// ErrorResponse now, the rest of the batch discarded, and ReadyForQuery only
// once the client's Sync has been through upstream. The simple-query path owns
// its whole exchange, so it gets the ReadyForQuery straight away.
func (s *Session) answerRefusal(msg pgproto3.FrontendMessage, refusal error) {
	extended := isExtendedQueryMessage(msg)
	if extended {
		s.extendedState.errorUntilSync = true
	}

	s.sendQueryError(refusal, !extended)
}

// isExtendedQueryMessage reports whether a client message belongs to the
// extended query protocol, i.e. whether refusing it puts the connection into
// the "discard until Sync" error state rather than ending an exchange.
func isExtendedQueryMessage(msg pgproto3.FrontendMessage) bool {
	switch msg.(type) {
	case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe, *pgproto3.Execute, *pgproto3.Close:
		return true
	default:
		return false
	}
}

// discardUntilSync drops the remainder of an extended-query batch dbbat has
// already failed, and reports whether the message was swallowed.
//
// Sync itself is *forwarded*: upstream answers every Sync with a
// ReadyForQuery, which is the frame the client is waiting for, and forwarding
// it also resynchronises an upstream that did see earlier messages of the same
// batch. Anything that is not an extended-query message (a simple Query, a
// Terminate) ends the error state and is handled normally.
func (s *Session) discardUntilSync(msg pgproto3.FrontendMessage) bool {
	if !s.extendedState.errorUntilSync {
		return false
	}

	switch msg.(type) {
	case *pgproto3.Parse, *pgproto3.Bind, *pgproto3.Describe, *pgproto3.Execute,
		*pgproto3.Close, *pgproto3.Flush, *pgproto3.CopyData, *pgproto3.CopyDone, *pgproto3.CopyFail:
		s.logger.DebugContext(s.ctx, "discarding message from a refused extended-query batch",
			slog.Any("message", msg))

		return true
	default:
		// Sync (and anything else) clears the error state.
		s.extendedState.errorUntilSync = false

		return false
	}
}

// sendQueryError sends a query error to the client. ready adds the trailing
// ReadyForQuery: correct for the simple query protocol, wrong for the extended
// one, where ReadyForQuery is owed to the client's Sync and nothing else.
func (s *Session) sendQueryError(queryErr error, ready bool) {
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

	if !ready {
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

// captureRowDescription records the result column metadata of the current or
// pending query so DataRow values can later be decoded and named.
func (s *Session) captureRowDescription(msg *pgproto3.RowDescription) {
	query := s.getCurrentPendingQuery()
	if query == nil {
		return
	}

	query.columnNames = make([]string, len(msg.Fields))
	query.columnOIDs = make([]uint32, len(msg.Fields))

	for i, field := range msg.Fields {
		query.columnNames[i] = string(field.Name)
		query.columnOIDs[i] = field.DataTypeOID
	}
}

// captureDataRow records one upstream result row against the pending query,
// honoring the result-storage limits.
//
// When a limit is reached the rows already captured are KEPT and capture simply
// stops — every other capture path (COPY, MySQL, MongoDB, Oracle) behaves this
// way, and discarding here meant the very queries a sample is most useful for
// (the huge ones) stored nothing at all. The truncation is persisted as
// queries.results_truncated so a short row set is never mistaken for a complete
// one.
func (s *Session) captureDataRow(msg *pgproto3.DataRow) {
	// Compute the row's payload size for the result-capture limits only —
	// wire-level bytes_transferred is tracked via the CountingConn around
	// clientConn, not field-summed here.
	rowSize := int64(0)
	for _, val := range msg.Values {
		rowSize += int64(len(val))
	}

	query := s.getCurrentPendingQuery()
	if query == nil || !s.queryStorage.StoreResults || query.truncated {
		return
	}

	// Check if this row would exceed limits
	if query.rowNumber >= s.queryStorage.MaxResultRows ||
		query.capturedBytes+rowSize > s.queryStorage.MaxResultBytes {
		query.truncated = true

		s.logger.WarnContext(s.ctx, "result capture truncated - limits exceeded",
			slog.Int("rows_captured", query.rowNumber),
			slog.Int64("bytes_captured", query.capturedBytes),
			slog.Int("max_rows", s.queryStorage.MaxResultRows),
			slog.Int64("max_bytes", s.queryStorage.MaxResultBytes))

		return
	}

	row := s.convertDataRow(msg.Values, query.columnNames, query.columnOIDs)

	query.capturedBytes += rowSize
	query.rowNumber++

	// Row numbers are a producer-side running counter: batches flush while
	// the result set is still streaming, so there is no slice left at the end
	// to number after the fact.
	row.RowNumber = query.rowNumber

	// Non-blocking: this runs inline with forwarding rows to the client, so a
	// slow moment in dbbat's own storage must never stall the customer's
	// query. A refused row marks the capture as having dropped rows, which is
	// reported separately from truncation.
	s.ensureRowSink(query).Add(row)
}

// ensureRowSink returns the query's handle on the shared row writer, creating
// it — and starting the parent queries row's INSERT — on first use.
//
// The parent record is inserted asynchronously: it must exist before the first
// batch of rows flushes, but PostgreSQL's capture path runs inline with
// forwarding rows to the client and must not acquire a synchronous store
// round-trip it never had. The writer holds the query's rows until the sink is
// resolved, which is what keeps the query_rows -> queries foreign key
// satisfied without coupling the two.
//
// Returns nil when there is no writer, in which case rows are simply not
// captured.
func (s *Session) ensureRowSink(query *pendingQuery) *shared.QuerySink {
	if query.rowSink != nil {
		return query.rowSink
	}

	if s.rowWriter == nil {
		return nil
	}

	// An approval hold already inserted this statement's row, so the parent
	// is there and nothing else needs creating.
	if query.approvalUID != uuid.Nil {
		query.rowSink = s.rowWriter.NewSinkFor(query.approvalUID)

		return query.rowSink
	}

	sink := s.rowWriter.NewSink()
	query.rowSink = sink

	if s.store == nil || s.connectionUID == uuid.Nil {
		// Nothing to hang rows from. Fail the sink rather than leaving it
		// unresolved, which would make the shared writer wait on it.
		sink.Fail()

		return sink
	}

	record := &store.Query{
		ConnectionID: s.connectionUID,
		SQLText:      query.sql,
		Parameters:   query.parameters,
		ExecutedAt:   query.startTime,
	}

	go func() {
		created, err := s.store.CreateQuery(s.ctx, record)
		if err != nil {
			s.logger.ErrorContext(s.ctx, "failed to create query record for result capture",
				slog.Any("error", err))
			sink.Fail()

			return
		}

		sink.Resolve(created.UID)
	}()

	return sink
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
		case *pgproto3.ParameterDescription:
			// Server-resolved bind parameter types for the statement the client
			// most recently described. Recorded so binary bind values can be
			// decoded even when the client declared no types in Parse.
			s.handleParameterDescription(m)

		case *pgproto3.RowDescription:
			s.captureRowDescription(m)

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
			s.captureDataRow(m)

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

		// Enforce time/bandwidth limits mid-stream. While a query is in flight,
		// re-check after every forwarded message: the moment the running total
		// crosses the grant's byte quota or the grant expires, abort with a
		// clean ErrorResponse + ReadyForQuery instead of streaming the rest of a
		// potentially huge result. Cheap (two atomic loads + a time compare).
		if verr := s.enforceStreamLimits(); verr != nil {
			return verr
		}
	}
}

// enforceStreamLimits aborts the in-flight query when a time/bandwidth limit
// has been crossed, sending the client a clean error frame first. It returns
// the abort reason, or nil when no query is in flight or the grant is still
// within bounds.
func (s *Session) enforceStreamLimits() error {
	if s.getCurrentPendingQuery() == nil {
		return nil
	}

	verr := s.guard.Check()
	if verr != nil {
		s.abortStream(verr)
	}

	return verr
}

// abortStream cuts an in-flight query off at a message boundary, sending the
// client a real ErrorResponse (SQLSTATE 53400, configuration_limit_exceeded) and
// a ReadyForQuery so it observes a failed query rather than a bare connection
// reset. The session then tears down (the grant is exhausted/expired, so every
// subsequent command would be refused anyway).
func (s *Session) abortStream(cause error) {
	s.logger.WarnContext(s.ctx, "aborting in-flight query: grant limit reached",
		slog.Any("error", cause))

	errMsg := &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "53400", // configuration_limit_exceeded
		Message:  cause.Error(),
	}

	errBuf, encodeErr := errMsg.Encode(nil)
	if encodeErr != nil {
		s.logger.ErrorContext(s.ctx, "failed to encode limit error", slog.Any("error", encodeErr))

		return
	}

	if _, err := s.clientConn.Write(errBuf); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to write limit error to client", slog.Any("error", err))

		return
	}

	readyMsg := &pgproto3.ReadyForQuery{TxStatus: 'I'}

	readyBuf, encodeErr := readyMsg.Encode(nil)
	if encodeErr != nil {
		return
	}

	if _, err := s.clientConn.Write(readyBuf); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to write ready to client", slog.Any("error", err))
	}

	// Attribute the in-flight query's streamed bytes to the grant before the
	// session tears down. Without this, the bytes an aborted query already
	// streamed are live in the CountingConn atomics but never persisted, so a
	// reconnect recomputes BytesTransferred without them and the cumulative cap
	// can be bypassed across short-lived connections.
	s.persistAbortedQuery(cause)
}

// persistAbortedQuery logs the query cut off by abortStream as a failed query
// and attributes its streamed-so-far bytes to the connection/grant. Called at
// the end of abortStream, once the error frame has been written (so the frame's
// bytes are included in the attribution).
func (s *Session) persistAbortedQuery(cause error) {
	query := s.getCurrentPendingQuery()
	if query == nil {
		return
	}

	// Wire-level diff since the previous query end: the aborted query's text
	// plus every response byte streamed before the abort.
	total := s.bytesFromClient.Load() + s.bytesToClient.Load()

	bytesTransferred := total - s.lastBytesSnapshot
	if bytesTransferred > 0 {
		s.lastBytesSnapshot = total
	} else {
		// Nothing new to attribute. Still log the abort when result capture
		// already inserted a queries row for this statement: skipping would
		// leave that row with no duration, no error and no capture flags,
		// which reads as a query that is still running.
		if query.rowSink == nil {
			return
		}

		bytesTransferred = 0
	}

	// Extended Query Protocol leaves the in-flight query in the pending queue
	// (currentQuery is nil until CommandComplete/ErrorResponse pops it); logQuery
	// persists s.currentQuery, so promote it. The session is tearing down, so
	// mutating currentQuery here is safe.
	if s.currentQuery == nil {
		s.currentQuery = query
	}

	errMsg := "aborted: " + cause.Error()
	s.logQuery(nil, &errMsg, bytesTransferred)
}

// cleanup closes connections and updates records.
func (s *Session) cleanup() {
	s.releaseCancelKey()
	s.stream.Connection(s.ctx, shared.ConnectionClosed)

	if s.grant != nil && s.revocation != nil {
		s.store.Revocations().Deregister(s.grant.UID, s.revocation)
	}

	if s.dumpWriter != nil {
		if err := s.dumpWriter.Close(); err != nil {
			s.logger.ErrorContext(s.ctx, "failed to close dump writer", slog.Any("error", err))
		}

		// The file is complete now; hand it to the uploader. No-op when
		// uploads are not configured, and it never blocks on the network.
		s.dumpUploader.Finish(s.ctx, s.connectionUID)
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

	// A CancelRequest arrives on its own TCP connection and is *not* a
	// StartupMessage — decoding it as one would fail on the magic version.
	// Recognizing it is what keeps a parked statement cancellable: the held
	// session's own socket is blocked, so this is the only channel left.
	if len(msgBuf) >= 8 {
		version := int(msgBuf[4])<<24 | int(msgBuf[5])<<16 | int(msgBuf[6])<<8 | int(msgBuf[7])
		if version == pgCancelRequestCode {
			cancel := &pgproto3.CancelRequest{}
			if err := cancel.Decode(msgBuf[4:]); err != nil {
				return nil, err
			}

			return cancel, nil
		}
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

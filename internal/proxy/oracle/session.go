package oracle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// session represents a single Oracle proxy session.
type session struct {
	clientConn    net.Conn
	upstreamConn  net.Conn
	store         *store.Store
	encryptionKey []byte
	logger        *slog.Logger
	ctx           context.Context //nolint:containedctx
	authCache     *cache.AuthCache

	// Connection metadata
	serviceName   string
	username      string
	database      *store.Database
	user          *store.User //nolint:unused // will be used when TTC auth is re-enabled
	grant         *store.Grant
	connectionUID uuid.UUID

	// Query tracking
	tracker      *oracleQueryTracker
	queryStorage config.QueryStorageConfig

	// Dump
	dumpConfig config.DumpConfig
	dump       *dump.Writer
}

// newSession creates a new Oracle proxy session.
func newSession(
	clientConn net.Conn,
	dataStore *store.Store,
	encryptionKey []byte,
	logger *slog.Logger,
	ctx context.Context, //nolint:revive
	authCache *cache.AuthCache,
	queryStorage config.QueryStorageConfig,
	dumpConfig config.DumpConfig,
) *session {
	return &session{
		clientConn:    clientConn,
		store:         dataStore,
		encryptionKey: encryptionKey,
		logger:        logger,
		ctx:           ctx,
		authCache:     authCache,
		tracker:       newOracleQueryTracker(),
		queryStorage:  queryStorage,
		dumpConfig:    dumpConfig,
	}
}

// run executes the full session lifecycle.
func (s *session) run() error {
	defer s.cleanup()

	// Step 1: Receive TNS Connect from client
	connectPkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read connect packet: %w", err)
	}

	if connectPkt.Type != TNSPacketTypeConnect {
		s.sendRefuse(ORA12520, "expected TNS Connect packet")

		return fmt.Errorf("%w: got %s", ErrExpectedConnectPacket, connectPkt.Type)
	}

	// Step 2: Parse connect descriptor to find target database
	connectStr := extractConnectString(connectPkt.Payload)
	s.logger.DebugContext(s.ctx, "TNS Connect received",
		slog.Int("payload_len", len(connectPkt.Payload)),
		slog.String("connect_string", connectStr),
	)
	cd := parseConnectDescriptor(connectStr)
	s.serviceName = cd.ServiceName

	// Also try EZ Connect if no service name found
	if s.serviceName == "" {
		s.serviceName = parseServiceNameEZConnect(connectStr)
	}

	// Try SID as fallback
	if s.serviceName == "" {
		s.serviceName = cd.SID
	}

	if s.serviceName == "" {
		s.sendRefuse(ORA12505, "missing SERVICE_NAME in connect descriptor")

		return ErrNoServiceName
	}

	s.logger = s.logger.With("service_name", s.serviceName)

	// Step 3: Look up database in store (by name first, then by oracle_service_name)
	db, err := s.store.GetDatabaseByName(s.ctx, s.serviceName)
	if err != nil {
		db, err = s.store.GetDatabaseByOracleServiceName(s.ctx, s.serviceName)
		if err != nil {
			s.sendRefuse(ORA12514, "database not found")

			return fmt.Errorf("%w: %s: %w", ErrDatabaseNotFound, s.serviceName, err)
		}
	}

	s.database = db

	// Step 4: Connect to upstream Oracle
	upstreamAddr := net.JoinHostPort(db.Host, fmt.Sprintf("%d", db.Port))

	s.upstreamConn, err = net.Dial("tcp", upstreamAddr)
	if err != nil {
		s.sendRefuse(ORA12541, "cannot reach upstream database")

		return fmt.Errorf("failed to connect to upstream %s: %w", upstreamAddr, err)
	}

	// Step 5: Forward the original Connect packet to upstream
	if err := writeTNSPacket(s.upstreamConn, connectPkt); err != nil {
		s.sendRefuse(ORA12541, "upstream connection failed")

		return fmt.Errorf("failed to forward connect to upstream: %w", err)
	}

	// Step 6: Read upstream response and handle Resend loop.
	// Oracle may respond with Resend (type 11) asking us to retransmit.
	// Redirect (type 4) and Accept (type 2) are forwarded to the client as-is.
	upstreamResp, err := s.readUpstreamConnectResponse(connectPkt)
	if err != nil {
		return err
	}

	if upstreamResp.Type == TNSPacketTypeRefuse {
		_ = writeTNSPacket(s.clientConn, upstreamResp)

		return ErrUpstreamRefused
	}

	// Forward the response (Accept or Redirect) to client as-is.
	// For Redirect: the client handles it natively by reconnecting.
	// For Accept: the session continues into proxy mode.
	if err := writeTNSPacket(s.clientConn, upstreamResp); err != nil {
		return fmt.Errorf("failed to forward upstream response to client: %w", err)
	}

	// If the upstream sent a Redirect, the client will close this connection
	// and reconnect directly to the target. We're done.
	if upstreamResp.Type != TNSPacketTypeAccept {
		return nil
	}

	// Step 7: Skip TTC-level auth interception for now.
	// TODO: implement TTC AUTH username extraction and per-user grant checking
	s.username = "proxy"

	// Step 8: Create connection record (best-effort)
	// Without TTC auth interception, we don't know the dbbat user.
	// Use a placeholder approach: find a user with an active grant for this database.
	sourceIP := store.ExtractSourceIP(s.clientConn.RemoteAddr())
	if s.store != nil {
		grants, _ := s.store.ListGrants(s.ctx, store.GrantFilter{
			DatabaseID: &s.database.UID,
			ActiveOnly: true,
		})
		if len(grants) > 0 {
			s.grant = &grants[0]
			conn, err := s.store.CreateConnection(s.ctx, grants[0].UserID, s.database.UID, sourceIP)
			if err == nil {
				s.connectionUID = conn.UID
			}
		}
	}

	s.logger.InfoContext(s.ctx, "Oracle session established, entering proxy mode",
		slog.Any("connection_uid", s.connectionUID))

	// Step 9: Initialize dump writer if configured
	if s.dumpConfig.Dir != "" && s.connectionUID != uuid.Nil {
		dumpPath := filepath.Join(s.dumpConfig.Dir, s.connectionUID.String()+dump.FileExt)

		dw, err := dump.NewWriter(dumpPath, dump.Header{
			SessionID: s.connectionUID.String(),
			Protocol:  dump.ProtocolOracle,
			StartTime: time.Now(),
			Connection: map[string]any{
				"service_name":  s.serviceName,
				"upstream_addr": upstreamAddr,
			},
		}, s.dumpConfig.MaxSize)
		if err != nil {
			s.logger.WarnContext(s.ctx, "failed to create dump writer", slog.Any("error", err))
		} else {
			s.dump = dw
		}
	}

	// Step 10: Enter bidirectional TNS relay with query interception
	return s.proxyMessages()
}

// maxResendAttempts limits the number of Resend retries to prevent infinite loops.
const maxResendAttempts = 3

// readUpstreamConnectResponse reads the upstream response to a Connect packet,
// handling Resend (type 11) by retransmitting. Returns the final response
// (Accept, Refuse, Redirect, or other).
func (s *session) readUpstreamConnectResponse(connectPkt *TNSPacket) (*TNSPacket, error) {
	for attempt := range maxResendAttempts {
		resp, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			s.sendRefuse(ORA12535, "upstream did not respond")

			return nil, fmt.Errorf("failed to read upstream response: %w", err)
		}

		if resp.Type != TNSPacketTypeResend {
			return resp, nil
		}

		// Oracle wants us to resend the Connect on the same connection.
		s.logger.DebugContext(s.ctx, "upstream sent Resend, retrying",
			slog.Int("attempt", attempt+1))

		if err := writeTNSPacket(s.upstreamConn, connectPkt); err != nil {
			return nil, fmt.Errorf("failed to resend connect: %w", err)
		}
	}

	s.sendRefuse(ORA12535, "too many resend attempts")

	return nil, fmt.Errorf("exceeded maximum resend attempts (%d)", maxResendAttempts)
}

// proxyMessages relays TNS packets bidirectionally with TTC-aware interception.
func (s *session) proxyMessages() error {
	errChan := make(chan error, 2)

	// Client → Upstream (with query interception)
	go func() {
		errChan <- s.clientToUpstream()
	}()

	// Upstream → Client (with response interception)
	go func() {
		errChan <- s.upstreamToClient()
	}()

	// Wait for either direction to close
	return <-errChan
}

// clientToUpstream reads TNS packets from the client, intercepts Data packets
// for TTC-level query interception, and forwards to upstream.
func (s *session) clientToUpstream() error {
	for {
		pkt, err := readTNSPacket(s.clientConn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("client read error: %w", err)
		}

		// Dump client->upstream packet
		if s.dump != nil {
			_ = s.dump.WritePacket(dump.DirClientToServer, pkt.Raw)
		}

		// Only intercept Data packets
		if pkt.Type == TNSPacketTypeData && len(pkt.Payload) >= ttcDataFlagsSize+1 {
			if blocked := s.interceptClientMessage(pkt); blocked {
				continue // Don't forward — error already sent to client
			}
		}

		// Forward to upstream
		if err := writeTNSPacket(s.upstreamConn, pkt); err != nil {
			return fmt.Errorf("upstream write error: %w", err)
		}
	}
}

// interceptClientMessage examines a TNS Data packet from the client.
// Returns true if the packet was blocked (error sent to client), false if it should be forwarded.
func (s *session) interceptClientMessage(pkt *TNSPacket) bool {
	funcCode, err := parseTTCFunctionCode(pkt.Payload)
	if err != nil {
		return false
	}

	s.logger.DebugContext(s.ctx, "TTC message", slog.String("func", funcCode.String()))

	ttcPayload := extractTTCPayload(pkt.Payload)
	if ttcPayload == nil {
		return false
	}

	switch funcCode { //nolint:exhaustive // only intercepting specific TTC functions, rest pass through
	case TTCFuncPiggyback:
		// v315+ piggyback: check sub-operation to determine action
		if IsPiggybackExecSQL(ttcPayload) {
			if err := s.checkQuotas(); err != nil {
				_ = s.sendOracleError(err)
				return true
			}

			if err := s.handlePiggybackExec(ttcPayload); err != nil {
				_ = s.sendOracleError(err)
				return true
			}
		} else if IsPiggybackClose(ttcPayload) {
			// Sub-op 0x09 = close cursor
			if len(ttcPayload) > 2 {
				s.handleOCLOSE(uint16(ttcPayload[2]))
			}
		}

	case TTCFuncOALL8:
		// Legacy OALL8 (pre-v315)
		if err := s.checkQuotas(); err != nil {
			_ = s.sendOracleError(err)
			return true
		}

		if err := s.handleOALL8(ttcPayload); err != nil {
			_ = s.sendOracleError(err)
			return true
		}

	case TTCFuncOFETCH:
		// JDBC thin driver reuses func=0x11 with sub-op 0x69 for execute-with-SQL.
		// Distinguish from plain OFETCH by checking the sub-operation byte.
		if IsExecSQL(ttcPayload) {
			if err := s.checkQuotas(); err != nil {
				_ = s.sendOracleError(err)
				return true
			}

			s.handleJDBCExec(ttcPayload)
		} else {
			s.handleOFETCH(ttcPayload)
		}

	case TTCFuncOCLOSE, TTCFuncOClosev2:
		cursorID, err := decodeCursorIDFromOCLOSE(ttcPayload)
		if err == nil {
			s.handleOCLOSE(cursorID)
		}

	default:
		// Other TTC functions are forwarded as-is
	}

	return false
}

// upstreamToClient reads TNS packets from upstream, intercepts Data packets
// for response tracking and row capture, and forwards to the client.
func (s *session) upstreamToClient() error {
	for {
		pkt, err := readTNSPacket(s.upstreamConn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("upstream read error: %w", err)
		}

		// Track bytes transferred
		bytesTransferred := int64(len(pkt.Payload))

		// Intercept Data packets for response handling
		if pkt.Type == TNSPacketTypeData && len(pkt.Payload) >= ttcDataFlagsSize+1 {
			s.interceptUpstreamMessage(pkt, bytesTransferred)
		}

		// Dump upstream->client packet
		if s.dump != nil {
			_ = s.dump.WritePacket(dump.DirServerToClient, pkt.Raw)
		}

		// Forward to client
		if err := writeTNSPacket(s.clientConn, pkt); err != nil {
			return fmt.Errorf("client write error: %w", err)
		}
	}
}

// interceptUpstreamMessage handles response interception from upstream.
func (s *session) interceptUpstreamMessage(pkt *TNSPacket, bytesTransferred int64) {
	funcCode, err := parseTTCFunctionCode(pkt.Payload)
	if err != nil {
		return
	}

	ttcPayload := extractTTCPayload(pkt.Payload)
	if ttcPayload == nil {
		return
	}

	switch funcCode { //nolint:exhaustive // only handling response-related codes
	case TTCFuncQueryResult:
		s.handleQueryResultV2(ttcPayload, bytesTransferred)
	case TTCFuncResponse:
		s.handleResponse(ttcPayload, bytesTransferred)
	case TTCFuncContinuation:
		s.handleContinuation(ttcPayload, bytesTransferred)
	}
}

// handleContinuation processes continuation packets (func=0x06) containing
// additional rows in multi-packet result sets.
//
// Oracle uses column-level compression: only columns whose values changed
// from the previous row are transmitted. A bitmask descriptor after each
// row (0x15 [flag] [count] [bitmask] 0x07) indicates which columns will
// have new values in the NEXT row. Columns not in the bitmask retain their
// previous values.
func (s *session) handleContinuation(ttcPayload []byte, bytesTransferred int64) {
	if s.tracker.pendingQuery == nil || s.tracker.pendingQuery.cursor == nil {
		return
	}

	columns := s.tracker.pendingQuery.cursor.columns
	numCols := len(columns)

	if numCols > 0 {
		rows := parseContinuationRows(ttcPayload, numCols, s.tracker.pendingQuery.lastRow)

		for _, row := range rows {
			s.captureRow(columns, row)

			// Update lastRow for cross-packet tracking
			strRow := make([]string, len(row))
			for i, v := range row {
				if v != nil {
					strRow[i] = fmt.Sprintf("%v", v)
				}
			}

			s.tracker.pendingQuery.lastRow = strRow
		}
	}

	// Check for ORA-01403 (no data found) which signals end of data
	if findBytes(ttcPayload, []byte("ORA-01403")) >= 0 {
		s.completeQuery(nil, nil, bytesTransferred)
	}
}

// handleResponse processes a legacy TTC Response (func=0x08).
// In v315+, most responses don't follow the legacy format so we skip them.
// Query completion is handled by handleQueryResultV2 for func=0x10.
func (s *session) handleResponse(ttcPayload []byte, bytesTransferred int64) {
	resp, err := decodeTTCResponse(ttcPayload)
	if err != nil {
		// v315+ auth/negotiation responses don't follow legacy format — ignore
		return
	}

	// Store column definitions in the pending cursor for multi-fetch
	if s.tracker.pendingQuery != nil && s.tracker.pendingQuery.cursor != nil && len(resp.Columns) > 0 {
		s.tracker.pendingQuery.cursor.columns = resp.Columns
	}

	// Row capture disabled — TTC binary row format cannot be reliably decoded.

	// If error or no more data, complete the query
	if resp.IsError {
		errMsg := resp.ErrorMessage
		s.completeQuery(nil, &errMsg, bytesTransferred)
	} else if !resp.MoreData {
		var rowsAffected *int64
		if resp.RowCount > 0 {
			rc := int64(resp.RowCount)
			rowsAffected = &rc
		}

		s.completeQuery(rowsAffected, nil, bytesTransferred)
	}
	// If MoreData is true, we wait for the next OFETCH response
}

// sendRefuse sends a TNS Refuse packet to the client with a properly
// formatted Oracle descriptor so JDBC clients can parse the error.
func (s *session) sendRefuse(oraCode uint16, reason string) {
	pkt := &TNSPacket{
		Type:    TNSPacketTypeRefuse,
		Payload: buildRefusePayload(oraCode, reason),
	}

	if err := writeTNSPacket(s.clientConn, pkt); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to send refuse", slog.Any("error", err))
	}
}

// cleanup closes upstream connection and updates records.
func (s *session) cleanup() {
	if s.dump != nil {
		if err := s.dump.Close(); err != nil {
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

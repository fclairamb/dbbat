package oracle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
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
	user          *store.User
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

// run executes the full session lifecycle with terminated authentication.
// dbbat acts as an Oracle server toward the client (O5LOGON auth with API key)
// and as an Oracle client toward the upstream database (stored credentials).
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

	// Step 2: Parse service name and resolve database
	if err := s.resolveDatabase(connectPkt.Payload); err != nil {
		return err
	}

	// Step 3: Send TNS Accept + post-accept notification (dbbat acts as Oracle server)
	if _, err := s.clientConn.Write(buildTNSAccept()); err != nil {
		return fmt.Errorf("failed to send TNS Accept: %w", err)
	}

	if _, err := s.clientConn.Write(buildPostAcceptNotification()); err != nil {
		return fmt.Errorf("failed to send post-accept notification: %w", err)
	}

	// Step 4: TTC negotiation with client (Marker + Set Protocol + Set Data Types)
	if err := s.handleClientNegotiation(); err != nil {
		return fmt.Errorf("client negotiation failed: %w", err)
	}

	// Step 5: Authenticate client via O5LOGON (API key as Oracle password)
	if err := s.authenticateClient(); err != nil {
		s.logger.WarnContext(s.ctx, "client authentication failed", slog.Any("error", err))
		s.sendAuthFailed(ORA01017, "invalid username/password; logon denied")

		return fmt.Errorf("%w: %w", ErrClientAuthFailed, err)
	}

	s.logger.InfoContext(s.ctx, "client authenticated",
		slog.String("username", s.username),
		slog.String("database", s.database.Name))

	// Step 6: Connect to upstream Oracle and authenticate with stored credentials
	if err := s.upstreamAuth(); err != nil {
		return fmt.Errorf("upstream auth failed: %w", err)
	}

	// Step 7: Record connection
	sourceIP := store.ExtractSourceIP(s.clientConn.RemoteAddr())
	conn, err := s.store.CreateConnection(s.ctx, s.user.UID, s.database.UID, sourceIP)
	if err == nil {
		s.connectionUID = conn.UID
	}

	upstreamAddr := net.JoinHostPort(s.database.Host, fmt.Sprintf("%d", s.database.Port))
	s.logger.InfoContext(s.ctx, "Oracle session established, entering proxy mode",
		slog.Any("connection_uid", s.connectionUID),
		slog.String("upstream", upstreamAddr))

	// Step 8: Initialize dump writer if configured
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

	// Step 9: Enter bidirectional TNS relay with query interception
	return s.proxyMessages()
}

// sendAuthFailed sends an ORA-01017 style auth failure to the client.
func (s *session) sendAuthFailed(oraCode uint16, message string) {
	payload := buildAuthFailed(int(oraCode), message)
	pkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: payload,
	}

	if err := writeTNSPacket(s.clientConn, pkt); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to send auth failed", slog.Any("error", err))
	}
}

// resolveDatabase parses the service name from the Connect payload and looks up the database.
func (s *session) resolveDatabase(connectPayload []byte) error {
	connectStr := extractConnectString(connectPayload)
	s.logger.DebugContext(s.ctx, "TNS Connect received",
		slog.Int("payload_len", len(connectPayload)),
		slog.String("connect_string", connectStr),
	)

	cd := parseConnectDescriptor(connectStr)
	s.serviceName = cd.ServiceName

	if s.serviceName == "" {
		s.serviceName = parseServiceNameEZConnect(connectStr)
	}

	if s.serviceName == "" {
		s.serviceName = cd.SID
	}

	if s.serviceName == "" {
		s.sendRefuse(ORA12505, "missing SERVICE_NAME in connect descriptor")

		return ErrNoServiceName
	}

	s.logger = s.logger.With("service_name", s.serviceName)

	db, err := s.store.GetDatabaseByName(s.ctx, s.serviceName)
	if err != nil {
		db, err = s.store.GetDatabaseByOracleServiceName(s.ctx, s.serviceName)
		if err != nil {
			s.sendRefuse(ORA12514, "database not found")

			return fmt.Errorf("%w: %s: %w", ErrDatabaseNotFound, s.serviceName, err)
		}
	}

	s.database = db

	return nil
}

// handleClientNegotiation handles the TTC negotiation exchange with the client.
// After receiving Accept + post-accept notification, Oracle thin clients send:
//  1. Marker packet (type 12) — consumed and discarded
//  2. Set Protocol request (Data packet, func=0x01) — we respond with captured template
//  3. Set Data Types request (Data packet, func=0x02) — we respond with captured template
//
// All responses are raw captured packets from Oracle 19c, written directly to the wire.
func (s *session) handleClientNegotiation() error {
	// Read client's first packet — expect Marker (type 12) from thin clients
	firstPkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read first negotiation packet: %w", err)
	}

	s.logger.DebugContext(s.ctx, "negotiation: first packet", slog.String("type", firstPkt.Type.String()), slog.Int("len", len(firstPkt.Payload)))

	// Consume non-Data packets until we get the Set Protocol request.
	// Different clients send different preambles (Marker, Control, etc.) before Set Protocol.
	for firstPkt.Type != TNSPacketTypeData {
		s.logger.DebugContext(s.ctx, "negotiation: skipping packet", slog.String("type", firstPkt.Type.String()))
		firstPkt, err = readTNSPacket(s.clientConn)
		if err != nil {
			return fmt.Errorf("failed to read Set Protocol: %w", err)
		}

		s.logger.DebugContext(s.ctx, "negotiation: next packet", slog.String("type", firstPkt.Type.String()), slog.Int("len", len(firstPkt.Payload)))
	}

	// Send Set Protocol response (raw captured packet including TNS header)
	s.logger.DebugContext(s.ctx, "negotiation: sending Set Protocol response", slog.Int("len", len(buildSetProtocolResponse())))
	if _, err := s.clientConn.Write(buildSetProtocolResponse()); err != nil {
		return fmt.Errorf("failed to send Set Protocol response: %w", err)
	}

	// Read Set Data Types from client
	s.logger.DebugContext(s.ctx, "negotiation: waiting for Set Data Types")
	setDT, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read Set Data Types: %w", err)
	}

	if setDT.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for Set Data Types", ErrUnexpectedPacketType, setDT.Type)
	}

	// Send Set Data Types response (raw captured packet including TNS header)
	if _, err := s.clientConn.Write(buildSetDataTypesResponse()); err != nil {
		return fmt.Errorf("failed to send Set Data Types response: %w", err)
	}

	return nil
}

// authenticateClient performs O5LOGON server-side authentication.
// The client sends AUTH Phase 1 (username), dbbat sends a challenge,
// the client sends AUTH Phase 2 (encrypted password), dbbat decrypts and verifies.
func (s *session) authenticateClient() error {
	// Receive AUTH Phase 1 from client
	phase1Pkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read AUTH Phase 1: %w", err)
	}

	if phase1Pkt.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for AUTH Phase 1", ErrUnexpectedPacketType, phase1Pkt.Type)
	}

	// Extract username from AUTH Phase 1
	s.logger.DebugContext(s.ctx, "AUTH Phase 1 payload",
		slog.Int("len", len(phase1Pkt.Payload)),
		slog.String("hex_head", fmt.Sprintf("%x", phase1Pkt.Payload[:min(len(phase1Pkt.Payload), 40)])))
	username, err := parseAuthPhase1(phase1Pkt.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse AUTH Phase 1: %w", err)
	}

	// Oracle clients uppercase usernames — normalize to lowercase for dbbat lookup
	s.username = strings.ToLower(username)

	// Look up dbbat user
	user, err := s.store.GetUserByUsername(s.ctx, s.username)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, username)
	}

	s.user = user

	// Check for active grant
	grant, err := s.store.GetActiveGrant(s.ctx, user.UID, s.database.UID)
	if err != nil {
		return fmt.Errorf("%w: user=%s database=%s", ErrNoActiveGrant, username, s.database.Name)
	}

	s.grant = grant

	// Check quotas
	if err := s.checkQuotas(); err != nil {
		return err
	}

	// Load O5LOGON verifier for this user
	verifier, err := s.loadO5LogonVerifier(user.UID)
	if err != nil {
		return fmt.Errorf("failed to load O5LOGON verifier: %w", err)
	}

	// Generate O5LOGON challenge
	o5 := NewO5LogonServer(verifier.O5LogonSalt, verifier.decryptedVerifier)
	encSessKey, vfrData, err := o5.GenerateChallenge()
	if err != nil {
		return fmt.Errorf("failed to generate O5LOGON challenge: %w", err)
	}

	// Send AUTH challenge to client
	challengePayload := buildAuthChallenge(encSessKey, vfrData)
	challengePkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: challengePayload,
	}

	if err := writeTNSPacket(s.clientConn, challengePkt); err != nil {
		return fmt.Errorf("failed to send AUTH challenge: %w", err)
	}

	// Receive AUTH Phase 2 from client
	phase2Pkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return fmt.Errorf("failed to read AUTH Phase 2: %w", err)
	}

	if phase2Pkt.Type != TNSPacketTypeData {
		return fmt.Errorf("%w: got %s for AUTH Phase 2", ErrUnexpectedPacketType, phase2Pkt.Type)
	}

	// Parse AUTH Phase 2 to get encrypted password
	clientSessKey, encPassword, err := parseAuthPhase2(phase2Pkt.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse AUTH Phase 2: %w", err)
	}

	// Decrypt the password (should be an API key)
	plainPassword, err := o5.DecryptPassword(clientSessKey, encPassword)
	if err != nil {
		return fmt.Errorf("failed to decrypt password: %w", err)
	}

	// Verify the decrypted password as an API key
	apiKey, err := s.store.VerifyAPIKey(s.ctx, plainPassword)
	if err != nil {
		return fmt.Errorf("API key verification failed: %w", err)
	}

	if apiKey.UserID != user.UID {
		return ErrAPIKeyOwnerMismatchOracle
	}

	// Increment usage asynchronously
	go func() { _ = s.store.IncrementAPIKeyUsage(context.Background(), apiKey.ID) }()

	// Send AUTH OK to client
	authOKPayload := buildAuthOK()
	authOKPkt := &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: authOKPayload,
	}

	if err := writeTNSPacket(s.clientConn, authOKPkt); err != nil {
		return fmt.Errorf("failed to send AUTH OK: %w", err)
	}

	return nil
}

// o5LogonVerifierData holds decrypted O5LOGON verifier data for a user's API key.
type o5LogonVerifierData struct {
	O5LogonSalt       []byte
	decryptedVerifier []byte
}

// loadO5LogonVerifier finds and decrypts the O5LOGON verifier for a user.
// Returns the first valid API key with an O5LOGON verifier.
func (s *session) loadO5LogonVerifier(userID uuid.UUID) (*o5LogonVerifierData, error) {
	keys, err := s.store.ListAPIKeys(s.ctx, store.APIKeyFilter{
		UserID:  &userID,
		KeyType: strPtr(store.KeyTypeAPI),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	for i := range keys {
		if len(keys[i].O5LogonSalt) == 0 || len(keys[i].O5LogonVerifier) == 0 {
			continue
		}

		// Decrypt the verifier with dbbat master key
		decrypted, err := decryptO5LogonVerifier(keys[i].O5LogonVerifier, s.encryptionKey, keys[i].KeyPrefix)
		if err != nil {
			s.logger.WarnContext(s.ctx, "failed to decrypt O5LOGON verifier",
				slog.String("key_prefix", keys[i].KeyPrefix),
				slog.Any("error", err))

			continue
		}

		return &o5LogonVerifierData{
			O5LogonSalt:       keys[i].O5LogonSalt,
			decryptedVerifier: decrypted,
		}, nil
	}

	return nil, ErrNoO5LogonVerifier
}

// strPtr returns a pointer to the given string.
func strPtr(s string) *string {
	return &s
}

// maxResendAttempts limits the number of Resend retries to prevent infinite loops.
const maxResendAttempts = 3

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

// sendRefuse sends a TNS Redirect packet carrying an Oracle error descriptor.
// Oracle listeners use Redirect (type 4) — not Refuse (type 3) — to report
// errors like ORA-12514 to JDBC and thin clients.
func (s *session) sendRefuse(oraCode uint16, reason string) {
	pkt := &TNSPacket{
		Type:    TNSPacketTypeRedirect,
		Payload: buildErrorRedirectPayload(oraCode, reason),
	}

	if err := writeTNSPacket(s.clientConn, pkt); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to send error redirect", slog.Any("error", err))
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

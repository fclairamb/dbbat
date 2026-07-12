package oracle

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/cache"
	"github.com/fclairamb/dbbat/internal/config"
	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/proxy/shared"
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

	// databaseCandidates holds every dbbat database sharing the connect
	// string's oracle_service_name when that name is ambiguous (a mutualized
	// upstream instance). The database is resolved at TNS Connect time —
	// before the username is known — so final selection is deferred to
	// disambiguateDatabase, which filters the candidates by the connecting
	// user's active grants once AUTH Phase 1 has revealed the username.
	// Empty when the connect string resolved to exactly one database.
	databaseCandidates []store.Database

	// upstreamCustomHash records whether the upstream's Set Protocol response
	// had caps[4]&0x20 set (customHash). Captured during the pre-auth relay
	// before we strip the bit for the client. The upstream AUTH client uses it
	// to switch between the legacy 6949 / MD5-XOR path and the modern 18453 /
	// PBKDF2 path.
	upstreamCustomHash bool

	// clientWideEncoding records whether the client encodes TTC AUTH key/value
	// lengths as fixed 4-byte little-endian integers (OCI / sqlplus) rather than
	// the compressed form (thin clients). Detected from AUTH Phase 1 and used to
	// shape the challenge dbbat issues so OCI can parse it.
	clientWideEncoding bool

	// clientBigClrChunks records whether the upstream advertised
	// ServerCompileTimeCaps[37]&0x20 (UseBigClrChunks) during the pre-auth relay.
	// When set, clients encode long CLR values with compressed-int chunk lengths
	// after the 0xFE long-form marker instead of single-byte lengths. Used to
	// harden the Phase 2 rewrite fallback (rewritePhase2KVPairs) so a long
	// AUTH_CONNECT_STRING (e.g. a load-balancer DNS host) decodes correctly. The
	// primary anchored rewrite never decodes long values, so it is unaffected.
	clientBigClrChunks bool

	// clientCombinedKey is the AES key derived from the dbbat-as-server O5LOGON
	// session keys (MD5(serverSessKey || clientSessKey)). Captured by
	// authenticateClient on success so the AUTH OK forwarded back to the client
	// can carry an AUTH_SVR_RESPONSE encrypted with the same key the client
	// expects. Empty when the client used the empty-AUTH_PASSWORD path.
	clientCombinedKey []byte

	// upstreamAuthOKResponse is the reassembled upstream Phase 2 response as a
	// single TNS Data packet, captured during runUpstreamClientAuth. Used as the
	// AUTH OK forwarded to the client after AUTH_SVR_RESPONSE patching.
	upstreamAuthOKResponse []byte

	// upstreamAuthOKFlags / upstreamAuthOKFragLens record the data-flags prefix
	// and per-packet TTC lengths the upstream used to fragment the AUTH OK, so
	// the patched AUTH OK can be re-fragmented at the same boundaries before
	// being forwarded (a single merged packet can exceed the client's SDU →
	// ORA-12592). See reframeAuthOK.
	upstreamAuthOKFlags    []byte
	upstreamAuthOKFragLens []int

	// clientAuthPhase1Pkt is the actual AUTH Phase 1 packet the client (SQLcl,
	// go-ora, python-oracledb) sent during pre-auth relay. dbbat reuses its
	// wire-shape (with the username swapped to the upstream DB user) when
	// driving Phase 1 against the upstream socket so the TTC-cap-conditioned
	// upstream parser accepts it. nil for legacy paths that read Phase 1
	// directly from the client connection.
	clientAuthPhase1Pkt *TNSPacket

	// upstreamAuthResp is the parsed upstream AUTH Phase 1 challenge, set by
	// beginUpstreamAuth. For OCI (wide-encoding) clients it is populated BEFORE
	// dbbat challenges the client, so the client challenge can reuse the
	// upstream's end-of-call summary (challengeTrailer) — the summary's width
	// depends on the negotiated TTC caps and a hard-coded capture only fits the
	// client it was captured from. finishUpstreamAuth consumes it for Phase 2.
	upstreamAuthResp *upstreamAuthResponse

	// clientAuthPhase2Pkt is the AUTH Phase 2 packet the client sent. dbbat
	// reuses its wire-shape (with username + AUTH_SESSKEY/AUTH_PASSWORD/
	// AUTH_PBKDF2_SPEEDY_KEY values swapped) when driving Phase 2 against the
	// upstream socket. This carries the client-specific KV pairs the upstream
	// conditions its AUTH OK on — notably AUTH_CONNECT_STRING, AUTH_COPYRIGHT,
	// AUTH_ACL, and the SESSION_CLIENT_DRIVER_NAME / VERSION pair that JDBC
	// thin sends. Without forwarding, dbbat's hand-built Phase 2 omits those
	// and JDBC trips ORA-17401 in T4CTTIfun.receive.
	clientAuthPhase2Pkt *TNSPacket

	// Query tracking
	tracker      *oracleQueryTracker
	queryStorage config.QueryStorageConfig

	// Dump
	dumpConfig config.DumpConfig
	dump       *dump.Writer

	// Wire-level byte counters for the client-facing socket. Reads = bytes
	// sent by the client; writes = bytes returned to the client. Together
	// they capture every byte the proxy exchanged with the client (TNS
	// framing, AUTH packets, OALL8 SQL, OFETCH responses, errors). Atomics
	// because Read/Write may be called from goroutines other than the
	// per-query bookkeeping path.
	bytesFromClient *atomic.Int64
	bytesToClient   *atomic.Int64
	// lastBytesSnapshot is the cumulative client-side total at the end of
	// the previous query. completeQuery diffs against it to attribute
	// bytes to the just-finished query (the first query absorbs the
	// auth/handshake traffic, which is the right place for it).
	lastBytesSnapshot int64
}

// cumulativeClientBytes returns the running total of bytes exchanged with
// the client. Used by per-query bookkeeping to take snapshots at query
// boundaries.
//
// Counters may be nil when sessions are constructed by tests that don't go
// through newSession; treat that as zero rather than panic.
func (s *session) cumulativeClientBytes() int64 {
	var total int64
	if s.bytesFromClient != nil {
		total += s.bytesFromClient.Load()
	}
	if s.bytesToClient != nil {
		total += s.bytesToClient.Load()
	}
	return total
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
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}

	return &session{
		clientConn:      shared.NewCountingConn(clientConn, bytesFromClient, bytesToClient),
		store:           dataStore,
		encryptionKey:   encryptionKey,
		logger:          logger,
		ctx:             ctx,
		authCache:       authCache,
		tracker:         newOracleQueryTracker(),
		queryStorage:    queryStorage,
		dumpConfig:      dumpConfig,
		bytesFromClient: bytesFromClient,
		bytesToClient:   bytesToClient,
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

	// Step 3+4: Transparent pre-auth relay.
	// Proxy TNS Accept + Set Protocol + Set Data Types through to the real upstream
	// so each client (go-ora, python-oracledb, dbeaver, sqlplus 23c…) receives a
	// server response tailored to its own capability/datatype registration.
	// The relay returns the client's AUTH Phase 1 packet (not forwarded) AND the
	// still-open upstream socket. Reusing that socket through AUTH keeps the TTC
	// capability levels aligned end-to-end so OALL8 from caps-rich clients (SQLcl
	// JDBC thin) parses correctly upstream.
	phase1Pkt, upstreamConn, err := s.relayPreAuthNegotiation(connectPkt)
	if err != nil {
		return fmt.Errorf("pre-auth relay failed: %w", err)
	}

	s.upstreamConn = upstreamConn
	s.clientAuthPhase1Pkt = phase1Pkt

	// Step 4a: If the connect string's service name matched several dbbat
	// databases (mutualized upstream), pick the real one now that AUTH Phase 1
	// has revealed the username — the user's active grants decide. This MUST
	// run before beginUpstreamAuth (step 4b), which authenticates upstream
	// with the selected database's stored schema credentials.
	if err := s.disambiguateDatabase(phase1Pkt); err != nil {
		s.logger.WarnContext(s.ctx, "database disambiguation failed", slog.Any("error", err))
		oraCode, message := authRejectFor(err)
		s.sendAuthFailed(oraCode, message)

		return fmt.Errorf("%w: %w", ErrClientAuthFailed, err)
	}

	// Step 4b: For OCI (wide-encoding) clients, drive AUTH Phase 1 against the
	// upstream BEFORE challenging the client. The upstream's challenge carries
	// the end-of-call summary shaped for the exact TTC caps this client
	// negotiated (the relay forwarded them verbatim); dbbat's client challenge
	// reuses those bytes. A wrong-width summary (the old hard-coded capture)
	// leaves unread bytes in the OCI client's TTC buffer, and the client aborts
	// the AUTH call with a break/reset marker exchange — the "sqlplus stalls
	// before AUTH Phase 2" failure. Thin clients keep the proven hand-built
	// summaries and the original ordering.
	s.clientWideEncoding = payloadUsesWideKVEncoding(phase1Pkt.Payload)
	if s.clientWideEncoding {
		if err := s.beginUpstreamAuth(); err != nil {
			return fmt.Errorf("upstream auth failed: %w", err)
		}
	}

	// Step 5: Authenticate client via O5LOGON (API key as Oracle password)
	if err := s.authenticateClient(phase1Pkt); err != nil {
		s.logger.WarnContext(s.ctx, "client authentication failed", slog.Any("error", err))
		oraCode, message := authRejectFor(err)
		s.sendAuthFailed(oraCode, message)

		return fmt.Errorf("%w: %w", ErrClientAuthFailed, err)
	}

	s.logger.InfoContext(s.ctx, "client authenticated",
		slog.String("username", s.username),
		slog.String("database", s.database.Name))

	// Step 6: Authenticate to upstream Oracle on the relay-phase socket using
	// stored database credentials.
	if err := s.upstreamAuth(); err != nil {
		return fmt.Errorf("upstream auth failed: %w", err)
	}

	// Step 6b: Forward the upstream's real AUTH OK packet to the client, with
	// AUTH_SVR_RESPONSE re-encrypted under the client's O5LOGON combined key.
	// Without that patch, modern clients (python-oracledb thin → DPY-4035,
	// JDBC thin / SQLcl → ORA-17401) reject the AUTH OK because the upstream
	// encrypted that field with its own combined key. go-ora ignores the
	// field, so the previous static-captured-response path worked for it
	// while silently breaking everyone else.
	authOK := s.upstreamAuthOKResponse
	if authOK == nil {
		authOK = capturedAuthOKResponse
	}

	if len(s.clientCombinedKey) > 0 && len(s.upstreamAuthOKResponse) > 0 {
		patched, err := patchAuthSvrResponse(authOK, s.clientCombinedKey)
		if err != nil {
			s.logger.WarnContext(s.ctx, "failed to patch AUTH_SVR_RESPONSE; forwarding upstream packet unchanged",
				slog.Any("error", err))
		} else {
			authOK = patched
		}
	}

	// Re-fragment the (patched) AUTH OK at the upstream's original packet
	// boundaries. dbbat reassembles the upstream's multi-packet AUTH OK into one
	// packet to patch AUTH_SVR_RESPONSE contiguously, but an OCI client rejects a
	// single packet that exceeds its negotiated SDU with ORA-12592; splitting it
	// back reproduces what a direct client accepts. No-op for single-packet
	// (thin-client) AUTH OKs.
	if len(s.upstreamAuthOKResponse) > 0 {
		authOK = reframeAuthOK(authOK, s.upstreamAuthOKFlags, s.upstreamAuthOKFragLens)
	}

	if _, err := s.clientConn.Write(authOK); err != nil {
		return fmt.Errorf("failed to send AUTH OK: %w", err)
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

// encodeV315DataPacket wraps a TTC payload in a v315+ TNS Data packet.
// v315+ format: 4-byte BE total length + type(0x06) + 3 reserved bytes + payload.
func encodeV315DataPacket(payload []byte) []byte {
	totalLen := 8 + len(payload) // 4(len) + 1(type) + 3(reserved) + payload
	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint32(buf[0:4], uint32(totalLen))
	buf[4] = byte(TNSPacketTypeData) // 0x06
	// buf[5:8] = 0x00, 0x00, 0x00 (reserved)
	copy(buf[8:], payload)

	return buf
}

// sendAuthFailed sends an ORA error TTC AUTH-reject frame to the client before
// the socket is closed, so the client renders a real ORA code instead of a
// generic ORA-12566 / ORA-03113 protocol error.
//
// The frame MUST use v315+ framing (4-byte length header, the 2-byte length
// field left 0x0000) — the same as the AUTH challenge (encodeV315DataPacket).
// After the TNS Accept, modern clients read the packet length as a 4-byte field;
// a legacy 2-byte-framed reject (the old writeTNSPacket path) is misread as an
// oversized/malformed packet and surfaces as ORA-12566 with no useful reason.
func (s *session) sendAuthFailed(oraCode uint16, message string) {
	frame := encodeV315DataPacket(buildAuthFailed(int(oraCode), message))
	if _, err := s.clientConn.Write(frame); err != nil {
		s.logger.ErrorContext(s.ctx, "failed to send auth failed", slog.Any("error", err))
	}
}

// authRejectFor maps a client-authentication failure to the ORA code and message
// dbbat surfaces to the client. A missing grant is actionable — the user simply
// needs to request access — so it gets its own ORA-01045 code and message. Every
// other failure (unknown user, wrong password) returns the generic ORA-01017 so
// the response never reveals whether the username exists or the password was wrong.
func authRejectFor(err error) (uint16, string) {
	if errors.Is(err, ErrNoActiveGrant) {
		return ORA01045, "no active grant for this database; request access via dbbat"
	}

	// An ambiguous shared service name is actionable too: the user holds
	// grants on several databases behind this service name and must pick one
	// explicitly by connecting with the dbbat database name.
	if errors.Is(err, ErrAmbiguousServiceName) {
		return ORA01045, "service name matches multiple dbbat databases; connect using the dbbat database name instead"
	}

	return ORA01017, "invalid username/password; logon denied"
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
	if err == nil {
		s.database = db

		return nil
	}

	// Fallback: the connect string carries a raw upstream service name. That
	// name may be shared by several dbbat databases (mutualized instance), in
	// which case the true database can only be chosen once the username is
	// known (AUTH Phase 1) — see disambiguateDatabase.
	candidates, err := s.store.ListDatabasesByOracleServiceName(s.ctx, s.serviceName)
	if err != nil || len(candidates) == 0 {
		s.sendRefuse(ORA12514, "database not found")

		return fmt.Errorf("%w: %s", ErrDatabaseNotFound, s.serviceName)
	}

	if len(candidates) == 1 {
		s.database = &candidates[0]

		return nil
	}

	// The pre-auth relay connects upstream BEFORE authentication, so an
	// ambiguous name is only workable when every candidate shares the same
	// upstream address (the mutualized-instance case). Otherwise refuse now —
	// there is no address to relay to.
	firstAddr := net.JoinHostPort(candidates[0].Host, fmt.Sprintf("%d", candidates[0].Port))
	for i := 1; i < len(candidates); i++ {
		addr := net.JoinHostPort(candidates[i].Host, fmt.Sprintf("%d", candidates[i].Port))
		if addr != firstAddr {
			s.sendRefuse(ORA12514,
				"service name matches multiple dbbat databases with different upstreams; connect using the dbbat database name")

			return fmt.Errorf("%w: %s: candidates have different upstream addresses", ErrAmbiguousServiceName, s.serviceName)
		}
	}

	s.logger.InfoContext(s.ctx, "service name matches multiple dbbat databases; deferring selection to AUTH Phase 1",
		slog.Int("candidates", len(candidates)))

	// Use the first candidate for the relay (all upstream-relevant fields —
	// host, port, oracle_service_name — are identical across candidates); the
	// final choice happens in disambiguateDatabase.
	s.database = &candidates[0]
	s.databaseCandidates = candidates

	return nil
}

// disambiguateDatabase finalizes the database selection when the connect
// string's service name matched several dbbat databases. It parses the
// username from the client's AUTH Phase 1 packet and keeps the candidates the
// user holds an active grant on:
//
//   - exactly one → that database is selected;
//   - zero → ErrNoActiveGrant (the user has no access however you slice it);
//   - several → ErrAmbiguousServiceName; the user must connect with the
//     unambiguous dbbat database name instead of the shared service name.
//
// No-op when the connect string already resolved to exactly one database.
func (s *session) disambiguateDatabase(phase1Pkt *TNSPacket) error {
	if len(s.databaseCandidates) < 2 {
		return nil
	}

	username, err := parseAuthPhase1(phase1Pkt.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse AUTH Phase 1: %w", err)
	}

	// Oracle clients uppercase usernames — normalize like authenticateClient.
	user, err := s.store.GetUserByUsername(s.ctx, strings.ToLower(username))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUserNotFound, username)
	}

	var matched []*store.Database

	for i := range s.databaseCandidates {
		if _, err := s.store.GetActiveGrant(s.ctx, user.UID, s.databaseCandidates[i].UID); err == nil {
			matched = append(matched, &s.databaseCandidates[i])
		}
	}

	switch len(matched) {
	case 0:
		return fmt.Errorf("%w: user=%s service_name=%s (no grant on any of the %d databases sharing this service name)",
			ErrNoActiveGrant, username, s.serviceName, len(s.databaseCandidates))
	case 1:
		s.database = matched[0]
		s.logger.InfoContext(s.ctx, "ambiguous service name resolved by user grants",
			slog.String("username", username),
			slog.String("database", s.database.Name))

		return nil
	default:
		names := make([]string, len(matched))
		for i, db := range matched {
			names[i] = db.Name
		}

		return fmt.Errorf("%w: user=%s has active grants on %s; connect using the dbbat database name",
			ErrAmbiguousServiceName, username, strings.Join(names, ", "))
	}
}

// readPhase1Packet returns the AUTH Phase 1 packet, reading from the client when
// not already provided by the pre-auth relay. Non-Data packets sent before Phase 1
// are silently consumed.
func (s *session) readPhase1Packet(phase1Pkt *TNSPacket) (*TNSPacket, error) {
	if phase1Pkt != nil {
		return phase1Pkt, nil
	}

	pkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read AUTH Phase 1: %w", err)
	}

	for pkt.Type != TNSPacketTypeData {
		s.logger.DebugContext(s.ctx, "AUTH Phase 1: skipping non-Data packet",
			slog.String("type", pkt.Type.String()),
			slog.Int("len", len(pkt.Payload)),
			slog.String("hex", fmt.Sprintf("%x", pkt.Payload[:min(len(pkt.Payload), 40)])))

		pkt, err = readTNSPacket(s.clientConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read AUTH Phase 1: %w", err)
		}
	}

	return pkt, nil
}

// readPhase2Packet returns the AUTH Phase 2 Data packet from the client. When a
// break/reset marker pair arrives instead, we honor the inline resync protocol
// by replying with a reset marker, then keep reading.
//
// NOTE: an OCI client (sqlplus / instant client) sends break+reset here when it
// rejects the challenge dbbat sent — most notably when the challenge's trailing
// end-of-call summary width does not match the client's negotiated TTC caps
// (fixed by clientChallengeTrailer, which reuses the live upstream summary).
// After the resync the client waits for the aborted call's completion, which
// dbbat does not synthesize, so the session stalls or ends with ORA-03106 — a
// failure historically mis-attributed to the TCP-urgent (out-of-band) break
// probe. See docs/oracle.md ("OCI break/reset before AUTH Phase 2").
func (s *session) readPhase2Packet() (*TNSPacket, error) {
	phase2Pkt, err := readTNSPacket(s.clientConn)
	if err != nil {
		return nil, fmt.Errorf("failed to read AUTH Phase 2: %w", err)
	}

	sawBreak := false

	for phase2Pkt.Type != TNSPacketTypeData {
		s.logger.DebugContext(s.ctx, "AUTH Phase 2: non-Data packet",
			slog.String("type", phase2Pkt.Type.String()),
			slog.Int("len", len(phase2Pkt.Payload)),
			slog.String("hex", fmt.Sprintf("%x", phase2Pkt.Payload[:min(len(phase2Pkt.Payload), 40)])))

		if isBreakMarker(phase2Pkt) {
			sawBreak = true
		}

		if isResetMarker(phase2Pkt) && sawBreak {
			if _, err := s.clientConn.Write(buildResetMarker()); err != nil {
				return nil, fmt.Errorf("failed to send reset marker: %w", err)
			}

			s.logger.DebugContext(s.ctx, "AUTH Phase 2: responded to break with reset marker")

			sawBreak = false
		}

		phase2Pkt, err = readTNSPacket(s.clientConn)
		if err != nil {
			return nil, fmt.Errorf("failed to read AUTH Phase 2: %w", err)
		}
	}

	return phase2Pkt, nil
}

// resolveAPIKeyFromPhase2 returns the API key that authenticated the client,
// trying every loaded verifier candidate (all of a user's user-salt API keys
// share the user's O5LOGON salts, so any of them may be the password in use).
//
// Two paths:
//   - encPassword == "" (SQLcl / JDBC thin 23c+): the client doesn't send the
//     password text, so candidates CANNOT be disambiguated. Deterministic
//     rule: assume the most-recently-created active key with user-salt
//     verifiers (candidates are ordered created_at DESC, so verifiers[0]).
//     Proof of knowledge is implicit — if the wrong key were typed, the
//     client fails to validate our AUTH_SVR_RESPONSE marker and disconnects.
//   - encPassword non-empty (go-ora, python-oracledb thin, sqlplus): for each
//     candidate, derive that candidate's view of the combined key (the
//     challenge ciphertext decrypted under ITS verifier — see
//     CloneForCandidate), attempt AUTH_PASSWORD decryption, and accept only a
//     plaintext that verifies as a real API key.
func (s *session) resolveAPIKeyFromPhase2(o5 *O5LogonServer, verifiers []*o5LogonVerifierData, clientSessKey, encPassword, encChallenge string) (*store.APIKey, error) {
	if encPassword == "" {
		primary := verifiers[0]
		s.logger.InfoContext(s.ctx, "AUTH Phase 2: empty AUTH_PASSWORD — cannot disambiguate candidates; assuming most-recently-created key",
			slog.String("key_id", primary.apiKeyID.String()),
			slog.String("key_prefix", primary.keyPrefix),
			slog.Int("candidates", len(verifiers)))

		// Derive the combined key the way the client did so the AUTH_SVR_RESPONSE
		// patch can run. Without this, dbbat forwards the upstream's
		// AUTH_SVR_RESPONSE verbatim — that ciphertext is encrypted under
		// dbbat's upstream-side combined key, so JDBC's client-side decrypt
		// returns garbage and fails the "SERVER_TO_CLIENT" marker check with
		// ORA-17401.
		if err := o5.DeriveCombinedKey(clientSessKey); err != nil {
			s.logger.WarnContext(s.ctx, "AUTH Phase 2: failed to derive combined key for empty-password path",
				slog.Any("error", err))
		}

		apiKey, err := s.store.GetAPIKeyByID(s.ctx, primary.apiKeyID)
		if err != nil {
			return nil, fmt.Errorf("failed to load API key by ID: %w", err)
		}

		return apiKey, nil
	}

	for i, verifier := range verifiers {
		cand := o5

		if i > 0 {
			// The challenge went out encrypted under verifiers[0]'s key; rebuild
			// the server state as a client holding THIS candidate key saw it.
			salt, verifierKey := verifier.O5LogonSalt, verifier.decryptedVerifier
			if o5.VerifierType() == VerifierType18453 {
				if len(verifier.decryptedVerifier18453) == 0 {
					continue // candidate lacks the negotiated verifier type
				}

				salt, verifierKey = verifier.salt18453, verifier.decryptedVerifier18453
			}

			clone, err := o5.CloneForCandidate(salt, verifierKey, encChallenge)
			if err != nil {
				s.logger.DebugContext(s.ctx, "AUTH Phase 2: candidate clone failed",
					slog.String("key_prefix", verifier.keyPrefix), slog.Any("error", err))

				continue
			}

			cand = clone
		}

		plainPassword, err := cand.DecryptPassword(clientSessKey, encPassword)
		if err != nil {
			s.logger.DebugContext(s.ctx, "AUTH Phase 2: candidate did not decrypt AUTH_PASSWORD",
				slog.String("key_prefix", verifier.keyPrefix))

			continue
		}

		apiKey, err := s.store.VerifyAPIKey(s.ctx, plainPassword)
		if err != nil {
			s.logger.DebugContext(s.ctx, "AUTH Phase 2: candidate plaintext failed API key verification",
				slog.String("key_prefix", verifier.keyPrefix))

			continue
		}

		// Propagate the winning candidate's combined key so the AUTH OK's
		// AUTH_SVR_RESPONSE is re-encrypted under the key the client derived.
		o5.CombinedKey = cand.CombinedKey

		s.logger.InfoContext(s.ctx, "AUTH Phase 2: API key authenticated",
			slog.String("key_prefix", apiKey.KeyPrefix),
			slog.String("key_id", apiKey.ID.String()),
			slog.Int("candidate_index", i),
			slog.Int("candidates", len(verifiers)))

		return apiKey, nil
	}

	return nil, fmt.Errorf("%w: no candidate key decrypted AUTH_PASSWORD (%d tried)",
		ErrAPIKeyVerification, len(verifiers))
}

// authenticateClient performs O5LOGON server-side authentication.
// The client sends AUTH Phase 1 (username), dbbat sends a challenge,
// the client sends AUTH Phase 2 (encrypted password), dbbat decrypts and verifies.
// phase1Pkt may be nil (legacy path) or pre-read from the transparent pre-auth relay.
func (s *session) authenticateClient(phase1Pkt *TNSPacket) error {
	phase1Pkt, err := s.readPhase1Packet(phase1Pkt)
	if err != nil {
		return err
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

	// Load the O5LOGON verifier candidates for this user: all of the user's
	// user-salt API keys (they share the user's salts, so any of them can
	// answer the challenge), or the single first legacy per-key-salt key.
	verifiers, err := s.loadO5LogonVerifiers(user.UID)
	if err != nil {
		return fmt.Errorf("failed to load O5LOGON verifier: %w", err)
	}

	// Build the O5LOGON server from the primary (most recently created)
	// candidate. Default to legacy verifier-6949 (go-ora and other legacy
	// clients). When the client negotiated customHash (Oracle 12c+/23ai —
	// observed from the upstream Set Protocol response) and the API key carries
	// a verifier-18453, switch to the modern PBKDF2/HMAC-SHA512 challenge that
	// python-oracledb thin, JDBC thin / SQLcl, and sqlplus require — they
	// reject the 6949 challenge against a 23ai-version server.
	primary := verifiers[0]

	o5 := NewO5LogonServer(primary.O5LogonSalt, primary.decryptedVerifier)
	if s.upstreamCustomHash && len(primary.decryptedVerifier18453) > 0 {
		o5.UseVerifier18453(primary.salt18453, primary.decryptedVerifier18453)
	}

	encSessKey, vfrData, err := o5.GenerateChallenge()
	if err != nil {
		return fmt.Errorf("failed to generate O5LOGON challenge: %w", err)
	}

	// OCI clients (sqlplus / instant client) negotiate fixed 4-byte little-endian
	// key/value lengths in the AUTH messages, whereas thin clients use the
	// compressed length-prefixed form. Detect the client's encoding from its
	// AUTH Phase 1 and mirror it in the challenge — OCI breaks/aborts on a
	// compressed challenge it cannot parse.
	s.clientWideEncoding = payloadUsesWideKVEncoding(phase1Pkt.Payload)

	// Send AUTH challenge to client
	s.logger.DebugContext(s.ctx, "sending AUTH challenge",
		slog.Int("sesskey_len", len(encSessKey)),
		slog.Int("vfrdata_len", len(vfrData)),
		slog.Bool("custom_hash", o5.CustomHashEnabled()),
		slog.Int("verifier_type", o5.VerifierType()),
		slog.Bool("wide_encoding", s.clientWideEncoding))
	challengePayload := buildAuthChallenge(encSessKey, vfrData, o5.PBKDF2ChkSalt(), o5.PBKDF2VgenCount(), o5.PBKDF2SderCount(), o5.VerifierType(), s.clientWideEncoding)
	challengePayload = append(challengePayload, s.clientChallengeTrailer(o5.VerifierType())...)
	s.logger.DebugContext(s.ctx, "AUTH challenge payload",
		slog.Int("len", len(challengePayload)),
		slog.String("hex_head", fmt.Sprintf("%x", challengePayload[:min(len(challengePayload), 60)])))
	// Write as raw v315+ TNS Data packet (4-byte length header, not 2-byte)
	// After Accept, all packets must use v315+ format.
	challengeRaw := encodeV315DataPacket(challengePayload)
	if _, err := s.clientConn.Write(challengeRaw); err != nil {
		return fmt.Errorf("failed to send AUTH challenge: %w", err)
	}

	phase2Pkt, err := s.readPhase2Packet()
	if err != nil {
		return err
	}

	s.clientAuthPhase2Pkt = phase2Pkt

	s.logger.DebugContext(s.ctx, "AUTH Phase 2 packet received",
		slog.String("type", phase2Pkt.Type.String()),
		slog.Int("payload_len", len(phase2Pkt.Payload)))

	// Parse AUTH Phase 2 to get encrypted password
	s.logger.DebugContext(s.ctx, "AUTH Phase 2 payload",
		slog.Int("len", len(phase2Pkt.Payload)),
		slog.String("hex_head", fmt.Sprintf("%x", phase2Pkt.Payload[:min(len(phase2Pkt.Payload), 60)])))
	clientSessKey, encPassword, err := parseAuthPhase2(phase2Pkt.Payload)
	if err != nil {
		return fmt.Errorf("failed to parse AUTH Phase 2: %w", err)
	}

	s.logger.DebugContext(s.ctx, "AUTH Phase 2 parsed",
		slog.Int("client_sesskey_len", len(clientSessKey)),
		slog.Int("enc_password_len", len(encPassword)),
		slog.String("client_sesskey", clientSessKey),
		slog.String("enc_password", encPassword))

	apiKey, err := s.resolveAPIKeyFromPhase2(o5, verifiers, clientSessKey, encPassword, encSessKey)
	if err != nil {
		return err
	}

	if apiKey.UserID != user.UID {
		return ErrAPIKeyOwnerMismatchOracle
	}

	s.clientCombinedKey = o5.CombinedKey

	// Increment usage asynchronously
	go func() { _ = s.store.IncrementAPIKeyUsage(context.Background(), apiKey.ID) }()

	// NOTE: AUTH OK is NOT sent here. It's sent in run() AFTER upstream auth completes,
	// so the relay can immediately forward go-ora's post-auth messages to upstream.

	return nil
}

// clientChallengeTrailer returns the end-of-call summary appended to the AUTH
// challenge dbbat sends the client.
//
// The Summary's exact width is conditioned on the TTC compile-time caps the
// client negotiated (relayed verbatim to the upstream during pre-auth): the
// macOS/Windows Oracle Instant Client 23.3 parses an 80-byte wide summary while
// the 23.26 DB-bundled OCI client parses a 153-byte one. A fixed capture only
// fits the client it came from — any other client leaves unread bytes in its
// TTC read buffer, treats the next stale byte as a message code, and aborts the
// AUTH call with a break/reset marker exchange, stalling before AUTH Phase 2
// (historically mis-attributed to the TCP-urgent OOB probe; see docs/oracle.md).
//
// For wide-encoding (OCI) clients the session therefore runs upstream AUTH
// Phase 1 first (beginUpstreamAuth) and reuses the live upstream challenge's
// summary bytes, which the real server sized for these exact caps. Thin clients
// keep the proven hand-built summaries.
func (s *session) clientChallengeTrailer(verifierType int) []byte {
	if s.clientWideEncoding && s.upstreamAuthResp != nil {
		if t := s.upstreamAuthResp.challengeTrailer; len(t) > 0 && t[0] == byte(TTCFuncOERR) {
			return t
		}
	}

	return buildAuthChallengeEndMarker(verifierType, s.clientWideEncoding)
}

// o5LogonVerifierData holds decrypted O5LOGON verifier data for a user's API key.
// apiKeyID is the UUID of the API key whose verifier was used. Needed for the
// empty-AUTH_PASSWORD path (SQLcl / JDBC thin), where dbbat trusts the primary
// candidate as the key the client must have authenticated with.
type o5LogonVerifierData struct {
	O5LogonSalt       []byte
	decryptedVerifier []byte
	apiKeyID          uuid.UUID
	keyPrefix         string

	// userSalt records whether the verifiers were derived from the USER's
	// shared salts (all such keys are interchangeable login candidates) or
	// legacy per-key random salts (only usable alone).
	userSalt bool

	// Modern verifier-18453 material (empty if the key predates it). When the
	// client negotiates customHash (Oracle 12c+/23ai), the proxy issues an
	// 18453 challenge from these instead of the legacy 6949 verifier above.
	salt18453              []byte
	decryptedVerifier18453 []byte
}

// decryptVerifierData decrypts a key's stored O5LOGON material (6949 required,
// 18453 optional) with the dbbat master key. Returns nil when the key has no
// usable 6949 verifier.
func (s *session) decryptVerifierData(key *store.APIKey) *o5LogonVerifierData {
	oracleData := key.OracleData()
	if oracleData == nil || len(oracleData.O5LogonSalt6949) == 0 || len(oracleData.O5LogonVerifier6949) == 0 {
		return nil
	}

	decrypted, err := decryptO5LogonVerifier(oracleData.O5LogonVerifier6949, s.encryptionKey, key.KeyPrefix)
	if err != nil {
		s.logger.WarnContext(s.ctx, "failed to decrypt O5LOGON verifier",
			slog.String("key_prefix", key.KeyPrefix),
			slog.Any("error", err))

		return nil
	}

	data := &o5LogonVerifierData{
		O5LogonSalt:       oracleData.O5LogonSalt6949,
		decryptedVerifier: decrypted,
		apiKeyID:          key.ID,
		keyPrefix:         key.KeyPrefix,
		userSalt:          oracleData.UserSalt,
	}

	// Also decrypt the modern verifier-18453 material if present, so the
	// proxy can serve customHash (Oracle 12c+/23ai) clients.
	if len(oracleData.O5LogonSalt18453) > 0 && len(oracleData.O5LogonVerifier18453) > 0 {
		if v18453, err := decryptO5LogonVerifier(oracleData.O5LogonVerifier18453, s.encryptionKey, key.KeyPrefix); err != nil {
			s.logger.WarnContext(s.ctx, "failed to decrypt O5LOGON verifier-18453",
				slog.String("key_prefix", key.KeyPrefix), slog.Any("error", err))
		} else {
			data.salt18453 = oracleData.O5LogonSalt18453
			data.decryptedVerifier18453 = v18453
		}
	}

	return data
}

// loadO5LogonVerifiers finds and decrypts the O5LOGON verifier candidates for
// a user, ordered most-recently-created first (ListAPIKeys order).
//
// When the user has keys whose verifiers were derived from the USER's shared
// salts (OracleAPIKeyData.UserSalt), ALL of them are returned: the challenge
// commits to the shared salt, and AUTH Phase 2 tries each candidate — so any
// of the user's API keys works as the Oracle password. Keys whose salts
// disagree with the newest user-salt key (possible after a rollback that
// regenerated the user salts) are dropped: they could never answer a
// challenge built from the current salts.
//
// Otherwise (legacy keys only, with per-key random salts) the first
// verifier-bearing key is the single candidate — the pre-user-salt behavior,
// where only that specific key can authenticate.
func (s *session) loadO5LogonVerifiers(userID uuid.UUID) ([]*o5LogonVerifierData, error) {
	keys, err := s.store.ListAPIKeys(s.ctx, store.APIKeyFilter{
		UserID:  &userID,
		KeyType: strPtr(store.KeyTypeAPI),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	all := make([]*o5LogonVerifierData, 0, len(keys))
	for i := range keys {
		if data := s.decryptVerifierData(&keys[i]); data != nil {
			all = append(all, data)
		}
	}

	candidates := selectVerifierCandidates(all)
	if len(candidates) == 0 {
		return nil, ErrNoO5LogonVerifier
	}

	primary := candidates[0]

	if primary.userSalt {
		s.logger.InfoContext(s.ctx, "O5LOGON verifiers loaded — any of these API keys works for Oracle login",
			slog.Int("candidates", len(candidates)),
			slog.String("primary_key_prefix", primary.keyPrefix),
			slog.Bool("has_18453", len(primary.decryptedVerifier18453) > 0))
	} else {
		s.logger.InfoContext(s.ctx, "O5LOGON verifier loaded (legacy per-key salt) — only this API key works for Oracle login",
			slog.String("key_prefix", primary.keyPrefix),
			slog.String("key_id", primary.apiKeyID.String()),
			slog.Bool("has_18453", len(primary.decryptedVerifier18453) > 0))
	}

	return candidates, nil
}

// selectVerifierCandidates picks the login candidates from a user's decrypted
// verifier data (ordered most-recently-created first):
//
//   - any user-salt keys present → ALL of them (minus keys whose salt
//     disagrees with the newest one — possible after a rollback regenerated
//     the user salts; they could never answer the current challenge);
//   - otherwise → the first legacy per-key-salt key alone (pre-user-salt
//     behavior: the challenge salt is bound to that specific key).
func selectVerifierCandidates(all []*o5LogonVerifierData) []*o5LogonVerifierData {
	var (
		userSaltCandidates []*o5LogonVerifierData
		legacy             *o5LogonVerifierData
	)

	for _, data := range all {
		switch {
		case data.userSalt:
			userSaltCandidates = append(userSaltCandidates, data)
		case legacy == nil:
			legacy = data
		}
	}

	if len(userSaltCandidates) > 0 {
		primary := userSaltCandidates[0]

		matching := userSaltCandidates[:0]
		for _, c := range userSaltCandidates {
			if bytes.Equal(c.O5LogonSalt, primary.O5LogonSalt) {
				matching = append(matching, c)
			}
		}

		return matching
	}

	if legacy != nil {
		return []*o5LogonVerifierData{legacy}
	}

	return nil
}

// strPtr returns a pointer to the given string.
func strPtr(s string) *string {
	return &s
}

// maxResendAttempts limits the number of Resend retries to prevent infinite loops.

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
//
// Query interception is best-effort observability: a malformed or unexpected
// TTC layout must never crash the proxy or break the connection. Any panic in
// the decode path is recovered here and the packet is forwarded unchanged.
func (s *session) interceptClientMessage(pkt *TNSPacket) bool {
	defer func() {
		if r := recover(); r != nil {
			// A recovered panic leaves the function returning the zero value
			// (false) — i.e. don't block the message.
			s.logger.WarnContext(s.ctx, "recovered from panic intercepting client message",
				slog.Any("panic", r))
		}
	}()

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

		// Intercept Data packets for response handling. Wire-level byte
		// tracking lives on the client-side CountingConn — we no longer
		// pass per-packet sizes here.
		if pkt.Type == TNSPacketTypeData && len(pkt.Payload) >= ttcDataFlagsSize+1 {
			s.interceptUpstreamMessage(pkt)
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
//
// Like interceptClientMessage, this is best-effort observability: any panic in
// the response-decode path is recovered so the upstream packet is still
// forwarded to the client and the session survives.
func (s *session) interceptUpstreamMessage(pkt *TNSPacket) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.WarnContext(s.ctx, "recovered from panic intercepting upstream message",
				slog.Any("panic", r))
		}
	}()

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
		s.handleQueryResultV2(ttcPayload)
	case TTCFuncResponse:
		s.handleResponse(ttcPayload)
	case TTCFuncOERR:
		s.handleOERStatus(ttcPayload)
	case TTCFuncContinuation:
		s.handleContinuation(ttcPayload)
	}
}

// handleOERStatus processes a standalone OER (func=0x04) message. Servers send
// it directly (after a marker exchange) when a statement fails, and as an
// end-of-call status in some flows.
func (s *session) handleOERStatus(ttcPayload []byte) {
	info := decodeOERAt(ttcPayload, 0)
	if info == nil {
		return
	}

	s.completeQueryFromOER(info)
}

// completeQueryFromOER finalizes the pending query from decoded OER fields:
// rows affected on success, error text on failure, plain completion on
// ORA-01403 (end-of-data, keeps captured-row counts).
func (s *session) completeQueryFromOER(info *oerInfo) {
	switch {
	case info.ErrorCode == oraNoDataFound:
		s.completeQuery(nil, nil)
	case info.ErrorCode != 0:
		msg := info.ErrorMessage
		if msg == "" {
			msg = fmt.Sprintf("ORA-%05d", info.ErrorCode)
		}

		s.completeQuery(nil, &msg)
	default:
		rows := int64(info.CurRowNumber)
		s.completeQuery(&rows, nil)
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
func (s *session) handleContinuation(ttcPayload []byte) {
	if s.tracker.pendingQuery == nil || s.tracker.pendingQuery.cursor == nil {
		return
	}

	columns := s.tracker.pendingQuery.cursor.columns
	numCols := len(columns)

	if numCols > 0 {
		rows := parseContinuationRows(ttcPayload, numCols, s.tracker.pendingQuery.lastRow, columnTypeCodes(columns))

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
		s.completeQuery(nil, nil)
	}
}

// handleResponse processes a legacy TTC Response (func=0x08).
// In v315+, most responses don't follow the legacy format so we skip them.
// Query completion is handled by handleQueryResultV2 for func=0x10.
func (s *session) handleResponse(ttcPayload []byte) {
	// v315+ DML responses embed an OER (func=0x04) status block carrying the
	// affected-row count (INSERT/UPDATE/DELETE) or the ORA error. This is the
	// reliable source — the legacy fixed-offset layout below misreads v315+
	// responses, so prefer the OER whenever one is present.
	if oer := findOERInResponse(ttcPayload); oer != nil {
		s.completeQueryFromOER(oer)
		return
	}

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
		s.completeQuery(nil, &errMsg)
	} else if !resp.MoreData {
		var rowsAffected *int64
		if resp.RowCount > 0 {
			rc := int64(resp.RowCount)
			rowsAffected = &rc
		}

		s.completeQuery(rowsAffected, nil)
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

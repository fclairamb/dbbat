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
	"sync"
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
	clientConn   net.Conn
	upstreamConn net.Conn
	store        *store.Store
	// completionStore is the slice of the store that query completion writes
	// through. Narrowing it to an interface keeps the completion path — and in
	// particular the error text a query ends up recording — assertable in a
	// unit test, which matters because that is the path a mid-fetch ORA error
	// travels. Always the session's store in production; nil in tests that
	// don't care.
	completionStore queryCompletionStore
	encryptionKey   []byte
	logger          *slog.Logger
	ctx             context.Context //nolint:containedctx
	authCache       *cache.AuthCache

	// Connection metadata
	serviceName   string
	username      string
	database      *store.Server
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
	databaseCandidates []store.Server

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

	// clientWide64Encoding records the *other* OCI dialect: the one whose TTC
	// op headers and summary objects are marshaled at 64-bit widths (the client
	// bundled in gvenzl/oracle-free:23-slim, 23.26), as opposed to the 32-bit
	// ones the Instant Client 23.3 writes. Detected from the same AUTH Phase 1,
	// and read by nextOERFrame: a session that has to refuse *before* any
	// upstream OER taught it a shape would otherwise answer a 64-bit client
	// with a 32-bit frame, which that client cannot parse — it waits for the
	// rest of a response that has already arrived. See docs/oracle.md,
	// "Two OCI encodings, not one".
	clientWide64Encoding bool

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

	// Query tracking.
	//
	// trackerMu guards every piece of per-session query bookkeeping: the
	// cursor map, the in-flight query pointer *and the fields of the
	// pendingOracleQuery (and trackedCursor) it points at*, lastBytesSnapshot,
	// and the in-session grant counters (QueryCount / BytesTransferred).
	//
	// All of it is touched by both relay goroutines. The client reader starts,
	// gates and completes statements (handleOALL8, handlePiggybackExec,
	// completeQuery); the upstream reader learns cursor ids, captures rows and
	// completes the *same* statement off the server's own answer
	// (learnCursorID, handleQueryResultV2, completeQueryFromOER) while reading
	// pendingQuery once per forwarded packet for the mid-stream limit check.
	// Two of those pairs were live data races until this mutex existed; they
	// were invisible because make test-e2e-oracle ran without -race, unlike
	// make test. See
	// specs/todos/2026-08-11-10-race-detector-on-the-integration-suites.md.
	//
	// The convention, in the same spirit as heldMu / oerMu above: it is a
	// per-concern lock, never a session-wide one. It is taken around
	// bookkeeping only — never across a socket read, a forwarded write, or an
	// approval hold — so the two directions keep relaying concurrently. The
	// upstream leg takes it once for the whole of interceptUpstreamMessage
	// (which does no I/O); the client leg takes it in explicit regions, with
	// holdIfNeeded deliberately outside them. Functions below are annotated
	// with which side of that boundary they sit on.
	trackerMu    sync.Mutex
	tracker      *oracleQueryTracker
	queryStorage config.QueryStorageConfig

	// rowWriter batches captured result rows. Oracle used to INSERT one row
	// at a time, synchronously, on the capture path — up to 100 000 sequential
	// round-trips on a single proxied query.
	rowWriter *shared.RowWriter

	// Dump
	dumpConfig config.DumpConfig
	dump       *dump.Writer
	// dumpUploader ships the finished capture to blob storage when the
	// session ends. nil = local-only captures (the default).
	dumpUploader *dump.Uploader

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
	//
	// Guarded by trackerMu: completeQuery runs on either relay goroutine, and
	// a read-modify-write of this from both is how bytes get double-counted
	// against a byte quota.
	lastBytesSnapshot int64

	// watched sits below the counting conn so an approval hold can keep
	// reading the client socket while the session goroutine is parked on a
	// human. Oracle clients are among the least forgiving about a silent
	// connection, which makes disconnect detection matter more here, not less.
	watched *shared.WatchedConn

	// approvalDeps/approvalGate implement pattern-triggered approval holds;
	// stream publishes this session's activity to the live event stream.
	approvalDeps shared.ApprovalDeps
	approvalGate *shared.ApprovalGate
	stream       *shared.StreamPublisher

	// heldQueryUID is the statement currently parked on a human, uuid.Nil
	// otherwise.
	heldMu       sync.Mutex
	heldQueryUID uuid.UUID

	// guard enforces the grant's time-window and bandwidth limits mid-stream.
	guard *shared.LimitGuard

	// held is the mid-stream limit violation waiting for a call boundary, nil
	// when none is armed. It is written by the upstream reader (which arms it),
	// read by the client reader (which answers the client's next call with it)
	// and by the limit watchdog (which must not pre-empt that handoff), hence
	// the mutex. See holdRefusal.
	refusalMu sync.Mutex
	held      *heldRefusal

	// refusalHoldBytes / refusalHoldGrace override the two fail-safe bounds
	// below, and exist so a test can actually *reach* them. Both are sized so a
	// live client never hits them — that is the whole point of their values —
	// which used to leave the relay and the watchdog that enforce them coverable
	// only by hand-mutating a held refusal's own marks. Zero means "use the
	// constant", so a bare &session{} (which is how a dozen unit tests build
	// one) keeps the production bounds; read them through refusalBytesBound and
	// refusalGrace rather than directly.
	refusalHoldBytes int64
	refusalHoldGrace time.Duration

	// revocation is signaled when this session's grant is revoked mid-flight,
	// so the next command is rejected and the watchdog tears the session down.
	revocation *cache.RevocationHandle

	// oer holds the negotiated layout of the TTC summary object, so a refusal
	// dbbat synthesizes is framed the way this client parses one. Its
	// capability half is filled in from the relayed Set Protocol / Set Data
	// Types exchange; its tail half is learned from the upstream's own OERs.
	// See ttc_oer_encode.go.
	//
	// oerSeq is the highest end-to-end sequence number seen from the upstream,
	// so a synthesized OER continues the session's count.
	//
	// oerCallNumber is the TTC sequence number of the call the client is
	// waiting on, taken off its own request (clientCallNumber). A server echoes
	// it in the summary object's callNumber, and ojdbc 26.1 refuses to read the
	// error out of an OER whose callNumber is not the one it sent.
	//
	// All three are written by the upstream reader or the client reader and read
	// by whichever goroutine refuses a statement — the client reader, the
	// upstream reader on a mid-stream limit violation, or the limit watchdog —
	// hence the mutex.
	oerMu         sync.Mutex
	oer           oerShape
	oerSeq        int
	oerCallNumber byte
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
	rowWriter *shared.RowWriter,
) *session {
	bytesFromClient := &atomic.Int64{}
	bytesToClient := &atomic.Int64{}

	watched := shared.NewWatchedConn(clientConn)
	_ = shared.EnableClientKeepAlive(clientConn)

	s := &session{
		watched:         watched,
		clientConn:      shared.NewCountingConn(watched, bytesFromClient, bytesToClient),
		store:           dataStore,
		encryptionKey:   encryptionKey,
		logger:          logger,
		ctx:             ctx,
		authCache:       authCache,
		tracker:         newOracleQueryTracker(),
		queryStorage:    queryStorage,
		dumpConfig:      dumpConfig,
		rowWriter:       rowWriter,
		bytesFromClient: bytesFromClient,
		bytesToClient:   bytesToClient,
		oer:             defaultOERShape(),
	}

	// Assigned separately so a nil store stays a nil interface rather than a
	// non-nil interface wrapping a nil pointer, which the nil checks on the
	// completion path rely on.
	if dataStore != nil {
		s.completionStore = dataStore
	}

	return s
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
	s.observeClientAuthEncoding(phase1Pkt.Payload)

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
	// Oracle is the one protocol dbbat never upgrades: the proxy relays the
	// client's own TNS Connect descriptor over a plain socket, so the
	// upstream leg is unencrypted regardless of the row's ssl_mode.
	//
	// s.grant is always set here: authenticateClient (step 5, above) returns
	// an error — aborting run() before this point — whenever the grant lookup
	// fails.
	conn, err := s.store.CreateConnection(s.ctx, s.user.UID, s.database.UID, sourceIP,
		store.WithUpstreamTLS(false), store.WithGrantUID(s.grant.UID))
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

	db, err := s.store.GetServerByName(s.ctx, s.serviceName)
	if err == nil {
		s.database = db

		return nil
	}

	// Fallback: the connect string carries a raw upstream service name. That
	// name may be shared by several dbbat databases (mutualized instance), in
	// which case the true database can only be chosen once the username is
	// known (AUTH Phase 1) — see disambiguateDatabase.
	candidates, err := s.store.ListServersByOracleServiceName(s.ctx, s.serviceName)
	if err != nil || len(candidates) == 0 {
		s.sendRefuse(ORA12514, "database not found")

		return fmt.Errorf("%w: %s", ErrServerNotFound, s.serviceName)
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
	if errors.Is(err, store.ErrUserNotFound) {
		return fmt.Errorf("%w: %s", ErrUserNotFound, username)
	} else if err != nil {
		return fmt.Errorf("user lookup failed for %s: %w", username, err)
	}

	var matched []*store.Server

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

		// Opportunistically migrate a legacy per-key-salt key to the user's
		// shared salts now that we briefly hold the plaintext — this is the only
		// point where it's available. Once upgraded, the key joins the user's
		// other keys as an interchangeable Oracle login candidate, without
		// forcing a rotation. Best-effort and async (like IncrementAPIKeyUsage);
		// failures never block the login. The empty-password path above cannot
		// do this (it has no plaintext).
		if od := apiKey.OracleData(); od != nil && !od.UserSalt {
			keyID, plain, encKey := apiKey.ID, plainPassword, s.encryptionKey
			go shared.RunGuarded(context.Background(), s.logger, goroutineNameVerifierUpgrade, func() {
				if err := s.store.UpgradeAPIKeyO5LogonVerifiers(context.Background(), keyID, plain, encKey); err != nil {
					s.logger.WarnContext(context.Background(), "failed to upgrade legacy O5LOGON verifiers to user salts",
						slog.String("key_id", keyID.String()), slog.Any("error", err))
				}
			})
		}

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
	if errors.Is(err, store.ErrUserNotFound) {
		return fmt.Errorf("%w: %s", ErrUserNotFound, username)
	} else if err != nil {
		return fmt.Errorf("user lookup failed for %s: %w", username, err)
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
	s.observeClientAuthEncoding(phase1Pkt.Payload)

	// Send AUTH challenge to client
	s.logger.DebugContext(s.ctx, "sending AUTH challenge",
		slog.Int("sesskey_len", len(encSessKey)),
		slog.Int("vfrdata_len", len(vfrData)),
		slog.Bool("custom_hash", o5.CustomHashEnabled()),
		slog.Int("verifier_type", o5.VerifierType()),
		slog.Bool("wide_encoding", s.clientWideEncoding),
		slog.Bool("wide_encoding_64", s.clientWide64Encoding),
		slog.Bool("big_clr_chunks", s.clientBigClrChunks))
	challengePayload := buildAuthChallenge(encSessKey, vfrData, o5.PBKDF2ChkSalt(), o5.PBKDF2VgenCount(), o5.PBKDF2SderCount(), o5.VerifierType(), s.clientWideEncoding, s.clientBigClrChunks)
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
	clientSessKey, encPassword, err := parseAuthPhase2(phase2Pkt.Payload, s.clientBigClrChunks)
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
	shared.BumpAPIKeyUsage(s.ctx, s.logger, s.store, apiKey.ID)

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

// Names the per-session goroutines report themselves under when one of them
// panics. They are log labels, not identifiers — the point is that the record
// says which leg died.
const (
	relayNameClientToUpstream = "oracle client→upstream"
	relayNameUpstreamToClient = "oracle upstream→client"
	relayNameWatchdog         = "oracle limit watchdog"
	relayNamePreAuthPump      = "oracle pre-auth upstream→client"

	// The detached store writes. Each is named after what it writes, because
	// that is the only thing the log line can usefully say: these goroutines
	// outlive the call that spawned them, so the recover on handleConnection
	// never sees them and nothing else records which write died.
	goroutineNameBlockedQuery    = "oracle blocked query record"
	goroutineNameQueryRecord     = "oracle query record"
	goroutineNameQueryCompletion = "oracle query completion"
	goroutineNameVerifierUpgrade = "oracle api key verifier upgrade"
	goroutineNameDumpRetention   = "oracle dump retention sweep"
)

// proxyMessages relays TNS packets bidirectionally with TTC-aware interception.
func (s *session) proxyMessages() error {
	// Register this live session against its grant so an admin revoke can
	// signal it (block the next command + tear it down) instead of waiting for
	// a reconnect. Deregistered in cleanup. s.grant may be nil for sessions
	// that never resolved one; Register(uuid.Nil) still yields a usable handle.
	var grantUID uuid.UUID
	if s.grant != nil {
		grantUID = s.grant.UID
	}

	s.revocation = s.store.Revocations().Register(grantUID)

	// Build the limit guard now that the grant is known, and run a watchdog to
	// tear the session down if a limit is crossed (or the grant is revoked)
	// while a query is blocked producing no traffic. The inline check in
	// upstreamToClient handles the actively-streaming case with a clean TTC
	// error frame.
	s.guard = shared.NewLimitGuard(s.grant, s.bytesFromClient, s.bytesToClient).
		WithRevocation(s.revocation.Flag())

	databaseName := ""
	if s.database != nil {
		databaseName = s.database.Name
	}

	s.approvalGate = shared.NewApprovalGate(s.approvalDeps, s.grant, s.connectionUID, s.user, databaseName)
	s.stream = shared.NewStreamPublisher(s.approvalDeps, s.connectionUID, s.user, databaseName).
		WithApprovals(s.approvalGate)
	s.stream.Connection(s.ctx, shared.ConnectionOpened)

	watchCtx, cancelWatch := context.WithCancel(s.ctx)
	defer cancelWatch()

	// RunWatchdog, not RunGuarded: a watchdog that merely survives its own panic
	// leaves the session running with nothing enforcing its expiry, quota or
	// revocation. onLimitViolation is where a panic is most plausible (it walks a
	// held refusal's handoff), so the teardown here is the blunt half of what it
	// does — closing both conns, which ends whichever relay is parked on them.
	go shared.RunWatchdog(watchCtx, s.logger, relayNameWatchdog, func() {
		s.guard.Watch(watchCtx, shared.DefaultLimitPollInterval, s.onLimitViolation)
	}, s.closeConns)

	errChan := make(chan error, 2)

	// Both directions run under shared.RunRelay, which turns a panic into an
	// error on errChan instead of a dead process: these are goroutines of their
	// own, and the only other recover in the Oracle proxy is on
	// handleConnection, which runs on a *different* goroutine and catches
	// nothing raised here. Everything they touch outside the two intercept paths
	// — packet framing, the dump writer, the mid-stream limit check,
	// holdIfNeeded, a held refusal's teardown — was otherwise a process-wide
	// fault on one malformed session. The error genuinely reaching errChan is
	// the load-bearing half: without it the wait below never returns and the
	// session leaks its conns. errChan is buffered at 2, so neither send blocks
	// even though only the first is read.
	go func() {
		errChan <- shared.RunRelay(s.ctx, s.logger, relayNameClientToUpstream, s.clientToUpstream)
	}()

	go func() {
		errChan <- shared.RunRelay(s.ctx, s.logger, relayNameUpstreamToClient, s.upstreamToClient)
	}()

	// Wait for either direction to close
	return <-errChan
}

// heldRefusal is a mid-stream limit violation that has been *decided* but not
// yet delivered: the client is parked in the middle of a reply, so there is no
// point in the stream where a frame can be written that it will read.
//
// It carries what the two escalation bounds need — when it was armed, and the
// session's cumulative byte count at that moment — plus a channel the client leg
// closes once it has answered, which is how the watchdog knows the handoff
// landed and it must not drop the sockets on top of it.
type heldRefusal struct {
	err     error
	armedAt time.Time
	atBytes int64
	done    chan struct{}
}

// refusalHandoffGrace bounds how long a held refusal may wait for the client to
// speak again, and refusalHoldMaxBytes bounds how much of the in-flight reply
// may still be relayed while it waits.
//
// Neither is a tuning knob; both are the fail-safes that keep "hold the refusal
// for the next call" from becoming an enforcement hole. Crossing either falls
// back to the socket close, which is the pre-fix behavior: an ORA-03113 that is
// meant.
//
// The values are measured rather than reasoned — docs/oracle.md, "What a
// legitimate handoff costs, measured", and TestIntegration_HeldRefusalHandoffCost
// — and the measurement is not the comfortable one the reasoning expected:
//
//   - the cost is the tail of one fetch batch, and a fetch batch is whatever the
//     client's array size makes it. Five clients at a 500-row fetch on 400-byte
//     rows cost 19 B to 128 KB and 0 to 119 ms — three orders of magnitude
//     inside both bounds, as expected. One client at a 3000-row fetch on
//     4000-byte rows needed ~11.7 MiB and **crossed the 8 MiB bound**, so its
//     session ended on the socket close. 8 MiB is therefore not slack; and no
//     finite value would be, because the array size is the client's to pick.
//   - the two bounds meet at a link speed. Measured over a throttled tap, a
//     537 KB tail took 6.5s (an effective 80 KiB/s); a tail at the full byte
//     bound would take ~105s there, so below roughly 280 KiB/s the grace runs
//     out first and the byte bound is unreachable.
//
// They are kept where they are because both are enforcement limits before they
// are ergonomics. 8 MiB is already a large overrun to allow past an exhausted
// quota, and what a client loses beyond either bound is the *message*, not the
// enforcement: the session still ends and the statement is still recorded with
// the real reason. Raising them to cover the widest imaginable fetch would trade
// that away for an error code.
const (
	refusalHandoffGrace = 30 * time.Second
	refusalHoldMaxBytes = 8 << 20
)

// refusalBytesBound and refusalGrace are how the two bounds are read: the
// session's own override when it has one, the constant otherwise. See the
// fields on session for why the override exists.
func (s *session) refusalBytesBound() int64 {
	if s.refusalHoldBytes > 0 {
		return s.refusalHoldBytes
	}

	return refusalHoldMaxBytes
}

func (s *session) refusalGrace() time.Duration {
	if s.refusalHoldGrace > 0 {
		return s.refusalHoldGrace
	}

	return refusalHandoffGrace
}

// holdRefusal arms a mid-stream refusal, reporting false when one is already
// armed (the relay keeps checking after every packet, and the violation stays
// true the whole time it is held).
func (s *session) holdRefusal(verr error) bool {
	s.refusalMu.Lock()
	defer s.refusalMu.Unlock()

	if s.held != nil {
		return false
	}

	s.held = &heldRefusal{
		err:     verr,
		armedAt: time.Now(),
		atBytes: s.cumulativeClientBytes(),
		done:    make(chan struct{}),
	}

	return true
}

// heldRefusalNow returns the armed refusal, or nil.
func (s *session) heldRefusalNow() *heldRefusal {
	s.refusalMu.Lock()
	defer s.refusalMu.Unlock()

	return s.held
}

// finishRefusalHandoff marks a held refusal as delivered (or as abandoned by an
// escalation), releasing whatever is waiting on it. Idempotent.
func (s *session) finishRefusalHandoff(held *heldRefusal) {
	s.refusalMu.Lock()
	defer s.refusalMu.Unlock()

	select {
	case <-held.done:
	default:
		close(held.done)
	}
}

// awaitRefusalHandoff blocks while a mid-stream refusal is held and undelivered,
// and reports whether the client leg answered it. False means the caller owns
// the teardown.
//
// This exists because LimitGuard.Watch calls its violation hook exactly once and
// then returns, while the violation itself stays true for as long as the refusal
// is held: without the wait, the watchdog would drop both sockets ~250ms after
// the quota was crossed and the client would meet the same ORA-03113 this whole
// change exists to replace. Waiting rather than skipping is what keeps the
// watchdog as the fail-safe — a client that never speaks again still loses the
// session, just at the grace rather than at the poll.
func (s *session) awaitRefusalHandoff(held *heldRefusal) bool {
	timer := time.NewTimer(time.Until(held.armedAt.Add(s.refusalGrace())))
	defer timer.Stop()

	select {
	case <-held.done:
		return true
	case <-s.ctx.Done():
		// The session is going away on its own; nothing left to tear down.
		return true
	case <-timer.C:
		return false
	}
}

// onLimitViolation is invoked by the limit watchdog when a time/bandwidth limit
// is crossed. It force-closes both conns, unblocking whichever relay goroutine
// is parked in a Read/Write so the session tears down. This is the only way to
// terminate a query that is blocked producing no traffic (idle expiry).
//
// It deliberately sends no ORA-00028 frame, and that is the decision rather than
// an omission: this is the one refusal path that can fire while the client is
// idle between calls, so there is no call to end. TTC has no unsolicited server
// message — an OER written to an idle socket is not read when it is sent, it
// waits in the buffer for the client's next request and is consumed as *that*
// request's answer, carrying by construction the previous call's number. On
// ojdbc 26.1 that mismatch is exactly what handleOutOfSequenceError turns into
// an ORA-18745 wrapping the real code, so a "graceful" frame here would be
// strictly worse than the close, which surfaces as a plain I/O error. A real
// Oracle does the same: ALTER SYSTEM KILL SESSION pushes nothing and the client
// learns at its next call; DISCONNECT SESSION drops the socket, which is what
// this imitates. See docs/oracle.md, "An asynchronous refusal: which call
// number, and whether to send one at all", and TestIdleLimitViolationSendsNoOER.
//
// The one case it stands down for is a refusal already held for the client's
// next call (holdRefusal): there the client leg has a call to answer and will
// answer it, so closing the socket here would race the ORA-00028 and win. It
// stands down for a bounded wait only — a handoff that never happens is exactly
// what this path is the fail-safe for.
func (s *session) onLimitViolation(err error) {
	attrs := []any{slog.Any("error", err)}

	if held := s.heldRefusalNow(); held != nil {
		if s.awaitRefusalHandoff(held) {
			return
		}

		// The handoff never landed: the client stopped talking with a refusal
		// undelivered. Fall back to the close, and record the statement the way
		// the delivered path would have — the reason must survive either way.
		s.abortHeldQuery(held.err)
		s.finishRefusalHandoff(held)

		// What the abandoned hold cost, on the same two axes every other exit
		// reports. The time is the grace by construction; the bytes are not, and
		// they say how much of a reply a client that went quiet was still fed.
		cost := s.refusalHandoffCost(held)
		attrs = append(attrs,
			slog.Int64(logAttrRelayedBytesSince, cost.bytes),
			slog.Int64(logAttrHeldForMillis, cost.millis))
	}

	s.logger.WarnContext(s.ctx, logMsgWatchdogTeardown, attrs...)

	s.closeConns()
}

// closeConns drops both sockets, which is how a session is ended from outside
// its relays: whichever leg is parked in a Read or Write returns, and
// proxyMessages runs its cleanup. It is the enforcement half of
// onLimitViolation, split out so the watchdog's panic guard can perform it
// without re-entering the held-refusal walk that may be what panicked.
//
// Safe to call twice, and safe concurrently with a blocked Read/Write.
func (s *session) closeConns() {
	if s.upstreamConn != nil {
		_ = s.upstreamConn.Close()
	}

	if s.clientConn != nil {
		_ = s.clientConn.Close()
	}
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
//
// That fail-open has exactly one exception, and it is why the return is named:
// once a mid-reply limit refusal is held (enforceMidStreamLimits), the grant no
// longer permits anything, so "forward what I could not read" would make an
// unreadable frame — or a panicking decode — the one way a client message
// travels under an exhausted grant. Every unreadable exit therefore routes
// through heldRefusalBlocks, which forwards as before when no refusal is held
// and ends the session when one is.
//
//nolint:nonamedreturns // the recover below has to *change* the answer, which only a named return allows
func (s *session) interceptClientMessage(pkt *TNSPacket) (blocked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.WarnContext(s.ctx, "recovered from panic intercepting client message",
				slog.Any("panic", r))

			blocked = s.heldRefusalBlocks()
		}
	}()

	funcCode, err := parseTTCFunctionCode(pkt.Payload)
	if err != nil {
		return s.heldRefusalBlocks()
	}

	ttcPayload := extractTTCPayload(pkt.Payload)
	if ttcPayload == nil {
		return s.heldRefusalBlocks()
	}

	s.logger.DebugContext(s.ctx, "TTC message",
		slog.String("func", funcCode.String()),
		slog.String("op", ttcOpFunction(ttcPayload)))

	// Whatever this message turns out to be, it names the call the client is
	// now waiting on, and a refusal has to end *that* call by number — see
	// oerSummary.CallNumber.
	named := s.observeClientCallNumber(ttcPayload)

	// A limit crossed while dbbat was relaying a reply is answered here rather
	// than written into that reply: this message is the proof the client has
	// finished consuming it and is parked on a fresh call that can be ended.
	// See enforceMidStreamLimits.
	if held := s.heldRefusalNow(); held != nil {
		return s.answerHeldRefusal(held, named)
	}

	switch funcCode { //nolint:exhaustive // only intercepting specific TTC functions, rest pass through
	case TTCFuncPiggyback:
		// v315+ piggyback: check sub-operation to determine action
		if IsPiggybackExecSQL(ttcPayload) {
			return s.gateStatement(s.handlePiggybackExec, ttcPayload)
		} else if IsPiggybackCursorReexec(ttcPayload) {
			// A re-execution is a fresh execution of a statement: it goes
			// through the same pre-flight as one carrying its own SQL, quota
			// check included. This frame is never a continuation of a result
			// set already streaming, so checking quotas here cannot strand a
			// client mid-fetch.
			return s.gateStatement(s.handlePiggybackReexec, ttcPayload)
		} else if IsPiggybackLogoff(ttcPayload) {
			// Sub-op 0x09 is the session logoff, not a cursor close. It used
			// to delete s.tracker.cursors[ttcPayload[2]] in the belief that
			// byte 2 was a cursor id; it is the TTC sequence number, and the
			// frame carries no cursor id at all. The real close list is the
			// 0x11/0x69 piggyback handled below.
			s.logger.DebugContext(s.ctx, "client logoff")
		}

	case TTCFuncOALL8:
		// Legacy OALL8 (pre-v315)
		return s.gateStatement(s.handleOALL8, ttcPayload)

	case TTCFuncOFETCH:
		// Func 0x11 sub-op 0x69 is Oracle's close-cursors piggyback, and a
		// client with a statement to run staples that execute behind the close
		// list in the same packet — which is the frame dbbat also knows as the
		// JDBC/DBeaver execute. Wire order is closes first, so the tracker
		// drops them before the execute below can be assigned a recycled id.
		if cursorIDs, err := decodeCloseCursors(ttcPayload); err == nil {
			s.handleCloseCursors(cursorIDs)
		}

		// A frame whose call dbbat could not name is handled apart from
		// everything below, and *before* the readings below, because both of
		// them end in an OER: the refusal would be stamped with whatever call
		// number dbbat last saw, which ends a call the client is not waiting
		// for and parks it forever (the ORA-18745 / hang mode of
		// specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md).
		// That includes the exec reading — a `11 69` execute whose close list
		// does not walk is exactly as unnameable as any other piggyback, and
		// gating it here while stamping a stale number was the one place this
		// invariant leaked.
		//
		// It is emphatically not a bypass: gateUnnameableFrame still gates the
		// statement, it just refuses by ending the session rather than by
		// answering a call it cannot name.
		if !named {
			return s.gateUnnameableFrame(ttcPayload)
		}

		// JDBC thin driver reuses func=0x11 with sub-op 0x69 for execute-with-SQL.
		// Distinguish it by checking the sub-operation byte.
		if IsExecSQL(ttcPayload) {
			return s.gateStatement(s.handleJDBCExec, ttcPayload)
		}

		// Nothing else in a 0x11 frame is intercepted. There used to be a
		// third re-execution reading here — "a fetch arriving with no query in
		// flight is a re-execution" — but it was written against a layout no
		// Oracle client sends (see the note on TTCFuncOFETCH in ttc.go), so it
		// only ever fired on misparsed piggybacks. It is gone; the two real
		// re-execution frames (the SQL-less OALL8 and the 03/0x4e|0x04
		// piggyback) are unaffected.
		//
		// Its companion guarantee outlives it and needs no guard: "a fetch that
		// merely continues a result set already streaming is never re-gated,
		// and no client is ever cut off mid-result-set" is now true by
		// construction rather than by an early return that could regress. A
		// real fetch is message type 0x03 function 0x05, which never enters
		// this switch at all. That is why the two tests that used to pin the
		// continuation path were deleted with the gate rather than rewired:
		// there is no longer a code path for them to guard.

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

// gateStatement runs a statement-carrying TTC op's handler and answers the
// client itself when the handler refuses. Reports true when the packet must NOT
// be forwarded; the client has already been answered with a TTC error by then.
//
// The pre-flight itself — quotas, expiry and revocation, then the static
// controls, the approval hold and the query recording — lives inside the
// handler, in that order. The quota check used to sit here instead, ahead of
// the handler, and that is precisely why an over-quota statement left no
// `queries` row: at this point the TTC payload is still undecoded, so there is
// no SQL to record the refusal against. Each handler decodes differently
// (decodeOALL8, decodePiggybackExecSQL, decodeExecSQL, decodeCursorReexec), so
// the check belongs where the SQL is known — after the decode and before the
// static controls. See regateCursor for the two re-execution frames, which
// share one insertion point.
//
// All three SQL-carrying ops still go through here (OALL8, the v315+ piggyback
// exec, and the JDBC thin driver's func=0x11 exec) so that adding a fourth
// cannot quietly acquire only half of the answer-the-client behavior — which
// is exactly how the JDBC exec ended up recording queries while enforcing
// nothing.
func (s *session) gateStatement(handle func([]byte) error, ttcPayload []byte) bool {
	if err := handle(ttcPayload); err != nil {
		_ = s.sendOracleError(err)

		return true
	}

	return false
}

// gateUnnameableFrame decides what happens to a client message whose call dbbat
// could not name — a piggyback (message type 0x11) whose body it cannot walk,
// so the call the client is parked on is stapled behind bytes of unknown
// length. Reports true when the packet must NOT be forwarded.
//
// The default is to forward it, and that is the point: dbbat could not identify
// this frame, so refusing it protects nothing, while answering it ends the
// wrong call and strands the client. That is how the DB-bundled OCI client's
// first message (`11 6b …`) stopped being refused as "a re-execution of cursor
// 27396" and stopped hanging sqlplus.
//
// But "forward it" cannot be unconditional, and this is the bound. A piggyback
// is by construction a frame with something stapled behind it — dbbat's own
// recordings show `11 69 … 03 5e <exec>` — so a frame dbbat cannot walk can
// carry a statement past the gate. Under a restrictive grant that would be a
// smuggling channel for the exact adversary the controls exist for: an
// authenticated user who has a grant and wants to exceed it. So the frame is
// scanned for a stapled statement (stapledStatement) and put through the same
// validators the JDBC exec path runs, in the same order.
//
// "The same validators" is literal, and includes the ones that fire with no
// grant controls at all: the Oracle blocked patterns (ALTER SYSTEM, UTL_HTTP,
// DBMS_SCHEDULER…) and the password-change guard. Gating this path on
// hasStatementControls would have made an unwalkable piggyback the one place
// `ALTER SYSTEM KILL SESSION` travels while the identical statement in a
// nameable frame is refused — a statement hiding in a way it cannot hide from
// the ordinary gate, which is exactly what this bound exists to prevent.
//
// The rest of handleJDBCExec's pre-flight is here too, and for the same reason:
// the quota check ahead of the validators, and — on the allow branch — the
// pending query and the store write. A statement that runs while leaving no
// `queries` row would make this path the one place a session's SQL escapes the
// audit trail, and an unrecorded statement is also an uncounted one, since
// MaxQueryCounts is charged when a pending query completes on the response leg.
//
// It is refused by **ending the session**, not by an OER, and that is the whole
// reason this is a separate path: dbbat cannot name the call, so it has no
// legitimate frame to answer with. Dropping the socket is the same answer
// onLimitViolation gives for the same reason (there is no call to end), it is
// what a real Oracle does on DISCONNECT SESSION, and it surfaces to the client
// as a plain I/O error rather than a wait that never returns. The statement is
// recorded as blocked first, so the refusal is in the audit trail exactly like
// every other one.
func (s *session) gateUnnameableFrame(ttcPayload []byte) bool {
	statements := stapledStatements(ttcPayload)

	if len(statements) == 0 {
		s.logger.DebugContext(s.ctx, logMsgUnnamedCallForwarded,
			slog.String("op", ttcOpFunction(ttcPayload)),
			slog.Bool("carries_statement", false))

		return false
	}

	// Every statement, not the first: a frame that staples two executes runs
	// both, so enforcing against one of them would leave the other exactly the
	// smuggling channel this path exists to close.
	for _, sql := range statements {
		if s.gateUnnameableStatement(ttcPayload, sql) {
			return true
		}
	}

	return false
}

// gateUnnameableStatement runs one stapled statement through the gate. Reports
// true when the session was torn down and the packet must not be forwarded.
func (s *session) gateUnnameableStatement(ttcPayload []byte, sql string) bool {
	// Normalized, like handleJDBCExec: the text the patterns run against is the
	// text recorded in /queries.
	sql = shared.NormalizeSQL(sql)

	// The pre-flight, in handleJDBCExec's order: quotas, expiry and revocation
	// first, then the static validators. refuseStatement records the refusal
	// against the statement, so a teardown is not a refusal that vanished.
	err := s.book(func() error {
		if err := s.checkQuotas(); err != nil {
			return s.refuseStatement(sql, nil, err)
		}

		if s.grant != nil {
			if err := shared.ValidateOracleQuery(sql, s.grant); err != nil {
				return s.refuseStatement(sql, nil, err)
			}
		}

		s.flushPendingQuery()

		return nil
	})

	// The approval hold, outside trackerMu like every other one. It works here
	// even though a refusal does not: it parks the *forwarding* and lets the
	// packet through once a human approves, with no call to answer either way.
	var approvalUID uuid.UUID

	if err == nil {
		approvalUID, err = s.holdIfNeeded(sql)
	}

	if err != nil {
		return s.endSessionOnRefusal(ttcPayload, sql, err)
	}

	// Allowed — and therefore recorded, exactly as the JDBC exec path records
	// it. A statement that runs and leaves no `queries` row would make this
	// path the one place a session's SQL escapes the audit trail, which is a
	// worse hole than the one the fail-open was introduced to fix. It is also
	// what makes MaxQueryCounts apply: the quota is charged when a pending
	// query completes on the response leg, so a statement nobody tracks is a
	// statement nobody counts.
	cursor := &trackedCursor{sql: sql, parsedAt: time.Now()}

	_ = s.book(func() error {
		s.tracker.pendingQuery = &pendingOracleQuery{cursor: cursor, startTime: time.Now()}

		// An approval hold already inserted the row; reuse it rather than
		// writing a second one for the same statement.
		if approvalUID != uuid.Nil {
			s.tracker.pendingQuery.queryUID = approvalUID
			s.tracker.pendingQuery.queryPersisted = true
		} else {
			s.persistQueryRecord()
		}

		return nil
	})

	s.logger.InfoContext(s.ctx, logMsgUnnameableStatementRecorded,
		slog.String("op", ttcOpFunction(ttcPayload)),
		slog.String("sql", truncateSQL(sql, 200)))

	return false
}

// endSessionOnRefusal is the refusal half of gateUnnameableFrame: there is no
// call to answer, so the session ends instead. The statement was recorded by
// refuseStatement (or by the hold) before this is reached.
func (s *session) endSessionOnRefusal(ttcPayload []byte, sql string, refusal error) bool {
	s.logger.WarnContext(s.ctx, logMsgUnnameableStatementRefused,
		slog.String("op", ttcOpFunction(ttcPayload)),
		slog.String("sql", truncateSQL(sql, 200)),
		slog.Any("error", refusal))

	if s.upstreamConn != nil {
		_ = s.upstreamConn.Close()
	}

	if s.clientConn != nil {
		_ = s.clientConn.Close()
	}

	return true
}

// stapledStatements returns every distinct statement a frame carries, in wire
// order, or nil when it carries none.
//
// The extractor is the one the JDBC exec path gates on (decodeExecSQL: the
// execute's own declared length first, then the legacy offset window and
// keyword search). Using the same one is deliberate — a statement dbbat would
// enforce against if it could name the call is a statement it has to enforce
// against when it cannot.
//
// What is *not* the same is where it is allowed to look. On this path a
// false positive is not a refused call the client can retry; it ends the
// session. And the frames that reach here are the ones dbbat cannot parse,
// which includes piggybacks that carry caller-supplied text by design —
// `11 87` set-end-to-end-attrs carries the module, action and client-identifier
// strings an application chooses. A bare keyword scan would kill a session
// whose client set its module to "DELETE ORDERS".
//
// So the scan is anchored: the SQL is only read from the start of a TTC op that
// can carry one. That is a bound and not a loophole, and the argument is what
// makes it safe — bytes the *server* will execute have to be a statement-carrying
// op too, so a payload with no such header is one the upstream will not run
// either. Hiding an executable statement from dbbat while keeping it executable
// by Oracle is precisely what this refuses to allow.
//
// Every anchor is read rather than the first that answers: `11 69 <closes>
// 03 5e <exec>` is the recorded shape, and a frame that staples two executes
// runs both. Duplicates are dropped because the two anchors of that shape name
// the same execute.
func stapledStatements(ttcPayload []byte) []string {
	var (
		out  []string
		seen = map[string]struct{}{}
	)

	for _, at := range statementOpOffsets(ttcPayload) {
		result, err := decodeExecSQL(ttcPayload[at:])
		if err != nil || result == nil || result.SQL == "" {
			continue
		}

		if _, dup := seen[result.SQL]; dup {
			continue
		}

		seen[result.SQL] = struct{}{}

		out = append(out, result.SQL)
	}

	return out
}

// ttcStatementOpHeaders are the op headers that carry SQL text: the v315+
// piggyback execute, and the two func-0x11 execute sub-ops dbbat's own JDBC
// path recognizes.
//
// A legacy OALL8 (message type 0x0e, a single byte) is deliberately not in the
// list: one common byte is not a header, and matching it would trade the false
// positive this anchoring removes straight back. That exclusion is now measured
// rather than argued — TestSurveyStapledOALL8 walks every `0x11` piggyback in
// testdata/ and finds six `0x0e` bytes at a non-zero offset, **none** of which
// decodes as an OALL8 carrying plausible SQL. Adding the op would buy nothing
// and cost the false positives anchoring removed. Whether Oracle would *execute*
// a `0e` stapled behind a `0x11` piggyback is still not measured — no recorded
// client sends OALL8 at all — and this list does not depend on the answer.
//
// The python sub-op (`11 98`) is likewise in no recording: python-oracledb thin
// sends the piggyback execute like every other thin client. It stays because
// keeping an anchor that never fires costs nothing, while removing one that
// turns out to fire costs a statement.
var ttcStatementOpHeaders = [][2]byte{
	{byte(TTCFuncPiggyback), PiggybackSubExecSQL},
	{byte(TTCFuncOFETCH), execSubOpJDBC},
	{byte(TTCFuncOFETCH), execSubOpPython},
}

// statementOpOffsets returns every offset in ttcPayload where a
// statement-carrying op header begins, earliest first.
//
// Offset 0 counts, and that is a carve-out worth naming rather than a detail:
// when the frame's own header is a statement op (`11 69`, `11 98` — an exec
// that only reached this path because its close list did not walk) the anchor
// matches immediately and the scan covers the whole payload, i.e. it degenerates
// to the unanchored scan the anchoring exists to avoid. Deliberate: such a
// frame really is an execute and its SQL really is in there, so declining to
// look would forward a live statement ungated. The cost is that a `11 69` frame
// whose stapled set-end-to-end-attrs strings read as a refused statement ends
// the session — fail-closed on a shape no tested client produces (every
// recorded `11 69` walks) against fail-open on a live exec. See
// TestUnnameableExecFrameIsGatedOnItsOwnPayload.
//
// The carve-out survived the measurement that was meant to settle it (the
// 2026-08-13-05 spec), and it is worth being precise about what did and did not
// change. decodeExecSQL reads the execute's *declared* length now, so the
// ordinary case — a `11 69` close list that walks, with `03 5e <exec>` behind
// it — is decoded from the stapled op's own header and never scans loose bytes.
//
// The exposure this carve-out names is **unchanged**, though. When the close
// list does not walk, the offset-0 anchor calls decodeExecSQL on the whole
// payload; the precise decode declines (a `11 69` header is not an exec header
// and closeCursorsEnd already failed), and the window scan and findSQLInPayload
// run over the whole frame exactly as before. A `03 5e` elsewhere in the
// payload adds a *second* anchor that decodes precisely — it does not suppress
// the offset-0 loose scan. So a caller-supplied module string that reads as a
// refused statement still ends the session, and that is the trade this
// carve-out was always making.
func statementOpOffsets(ttcPayload []byte) []int {
	var out []int

	for i := 0; i+1 < len(ttcPayload); i++ {
		for _, header := range ttcStatementOpHeaders {
			if ttcPayload[i] == header[0] && ttcPayload[i+1] == header[1] {
				out = append(out, i)

				break
			}
		}
	}

	return out
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

		if verr := s.enforceMidStreamLimits(); verr != nil {
			return verr
		}
	}
}

// enforceMidStreamLimits runs after every forwarded packet: the moment the
// grant's byte quota is crossed, the grant expires or it is revoked, the rest of
// a huge result must not keep streaming. A non-nil return ends the relay, and
// with it the session.
//
// What it does *not* do is write the refusal here, and that is the whole point
// of this function. dbbat cuts into the reply at a **TNS packet** boundary,
// while a fetch reply is a **TTC message** stream whose messages straddle
// packets — which is why handleContinuation carries lastRow state across them.
// An OER written at this point therefore lands inside a half-delivered row
// batch: measured against Oracle 23ai Free, dbbat wrote exactly one well-formed
// ORA-00028 with the right call number and *neither* ojdbc 23.7 nor go-ora
// reported it — both consumed it as row bytes and then met EOF (ORA-03113 /
// "driver: bad connection"). Two drivers reading it the same way is what makes
// the injection point, and not either driver's parser, the thing that was wrong.
//
// So the violation is *held* instead, and the client announces the boundary
// itself: a client that sends its next TTC op has by construction finished
// consuming the previous reply, so interceptClientMessage answers that op with
// the refusal through the ordinary path — the same one measured working on all
// four clients. It is also what a real Oracle does, measured in
// TestIntegration_RealOracleSessionTermination: ALTER SYSTEM KILL SESSION pushes
// nothing at the parked client and answers its *next* call with an ORA-00028
// stamped with that call's own number.
//
// The cost is the tail of the fetch batch already in flight, bounded by the
// client's fetch size — and, when the reply has no end at all, by
// refusalHoldMaxBytes below. See docs/oracle.md, "An asynchronous refusal: which
// call number, and whether to send one at all".
func (s *session) enforceMidStreamLimits() error {
	if held := s.heldRefusalNow(); held != nil {
		// A refusal is already decided and waiting for the client's next call.
		// Keep relaying so the client can finish the reply it is parked in and
		// get there — but not without end: a reply whose boundary never arrives
		// would otherwise stream past the quota indefinitely.
		if s.cumulativeClientBytes()-held.atBytes <= s.refusalBytesBound() {
			return nil
		}

		cost := s.refusalHandoffCost(held)

		s.logger.WarnContext(s.ctx, logMsgRefusalHandoffAbandoned,
			slog.Int64(logAttrRelayedBytesSince, cost.bytes),
			slog.Int64(logAttrHeldForMillis, cost.millis),
			slog.Any("error", held.err))

		s.abortHeldQuery(held.err)
		s.finishRefusalHandoff(held)

		return held.err
	}

	if !s.hasPendingQuery() {
		return nil
	}

	verr := s.guard.Check()
	if verr == nil {
		return nil
	}

	if s.holdRefusal(verr) {
		s.logger.WarnContext(s.ctx, logMsgRefusalHeld, slog.Any("error", verr))
	}

	return nil
}

// abortHeldQuery finalizes the in-flight query a held refusal cut short, so its
// streamed-so-far bytes are flushed to the store and the statement carries the
// real reason it ended.
//
// Both halves matter. Without the flush those bytes live only in the
// CountingConn atomics, and a reconnect recomputes BytesTransferred without
// them — which lets a cumulative cap be bypassed across short-lived
// connections. completeQuery diffs the bytes since the last query boundary,
// persists, bumps the in-session grant and clears pendingQuery (no
// double-count). Every path that ends a held refusal calls this: the delivered
// one, and both escalations.
func (s *session) abortHeldQuery(verr error) {
	errMsg := "aborted: " + verr.Error()

	// completeQuery expects trackerMu; this is one of the few callers that
	// reaches it from outside an intercept.
	s.trackerMu.Lock()
	s.completeQuery(nil, &errMsg)
	s.trackerMu.Unlock()
}

// answerHeldRefusal ends the call the client has just made with the limit
// violation dbbat decided mid-reply but could not deliver then. Reports true so
// the caller does not forward the packet.
//
// It runs before anything else interceptClientMessage would do with the message,
// and immediately after observeClientCallNumber, which is what makes the frame
// readable where the mid-reply one was not: the number stamped on it is the
// number of the call the client is parked on *right now*, and the client is
// between TTC messages by construction — it would not have sent this op
// otherwise.
//
// A message dbbat could not name gets the gateUnnameableFrame answer instead:
// no frame, both sockets dropped. Stamping such a frame with the last number
// dbbat saw ends a call the client is not waiting for, which is the ORA-18745 /
// hang mode of
// specs/done/2026/08/2026-08-12-02-oracle-async-refusal-call-number.md.
func (s *session) answerHeldRefusal(held *heldRefusal, named bool) bool {
	// Answered once, and once only. The client leg keeps reading until its
	// socket actually closes, so a pipelined second message — or a panic
	// recovered after the frame went out — must not produce a second
	// end-of-call OER for a call nobody is waiting on. That would be the
	// unsolicited frame onLimitViolation exists to avoid, and it would break
	// the one-frame invariant the measurement rests on.
	select {
	case <-held.done:
		return true
	default:
	}

	// The teardown is deferred, not sequential, and that is deliberate: writing
	// the frame is the one step here that can fail in an unbounded way (a
	// panicking encode, a socket error surfacing as a panic in a wrapper), and
	// an exit that skipped the recording and left both sockets open would be a
	// fifth, panic-shaped path — one that the "every exit ends the session and
	// records the reason" claim in docs/oracle.md does not cover. Deferring it
	// keeps the exits at four.
	//
	// The session is over either way: the message says "session terminated" and
	// the grant no longer permits anything. Closing the upstream first stops any
	// tail of the reply from being relayed on top of the frame just written.
	defer func() {
		s.finishRefusalHandoff(held)
		s.abortHeldQuery(held.err)

		if s.upstreamConn != nil {
			_ = s.upstreamConn.Close()
		}

		if s.clientConn != nil {
			_ = s.clientConn.Close()
		}
	}()

	// What the handoff actually cost, recorded on the way out. Both bounds are
	// sized against these two numbers and nothing else, so they are logged on
	// every delivery rather than inferred from a client's row count: the byte
	// side is the tail of the in-flight fetch batch, the time side is how long
	// the client took to announce its boundary. See refusalHandoffCost and
	// docs/oracle.md, "What a legitimate handoff costs, measured".
	cost := s.refusalHandoffCost(held)

	if named {
		_ = s.writeTTCError(int(ORA00028), "session terminated: "+held.err.Error())

		s.logger.WarnContext(s.ctx, logMsgRefusalDelivered,
			slog.Any("error", held.err),
			slog.Int64(logAttrRelayedBytesSince, cost.bytes),
			slog.Int64(logAttrHeldForMillis, cost.millis))
	} else {
		s.logger.WarnContext(s.ctx, logMsgRefusalUnnameable,
			slog.Any("error", held.err),
			slog.Int64(logAttrRelayedBytesSince, cost.bytes),
			slog.Int64(logAttrHeldForMillis, cost.millis))
	}

	return true
}

// handoffCost is what one held refusal cost before it was answered: the bytes
// relayed to the client past the violation, and how long the hold lasted.
//
// These are exactly the two quantities refusalHoldMaxBytes and
// refusalHandoffGrace bound, which is the point — the constants are sized
// against observations of this struct, taken from live clients through the
// integration suites, rather than against a reasoned expectation.
type handoffCost struct {
	bytes  int64
	millis int64
}

// refusalHandoffCost measures a held refusal at the moment it is resolved.
func (s *session) refusalHandoffCost(held *heldRefusal) handoffCost {
	return handoffCost{
		bytes:  s.cumulativeClientBytes() - held.atBytes,
		millis: time.Since(held.armedAt).Milliseconds(),
	}
}

// heldRefusalBlocks is the exit interceptClientMessage takes when it could not
// read a client message at all — an unwalkable payload, or a decode that
// panicked. Reports true when the packet must NOT be forwarded.
//
// With no refusal held it reports false, which is the fail-open the whole
// intercept path is built on: dbbat could not read this frame, so refusing it
// protects nothing, while blocking it would strand the client (see
// gateUnnameableFrame for the same argument at length).
//
// With one held, the answer flips, and the asymmetry is the point: the grant is
// exhausted, the session is already over, and forwarding is the one thing that
// must not happen. A frame dbbat cannot parse is by construction a frame it
// cannot *name*, so it gets exactly what a named-but-unwalkable call gets —
// both sockets dropped and no frame written, because an OER stamped with a
// stale call number ends a call the client is not parked on.
//
// Of its three callers, only the recovered panic is live traffic. The two
// length-based early returns above it cannot fire from clientToUpstream, which
// pre-gates every packet at ttcDataFlagsSize+1 bytes — they are guarded because
// the guarantee has to belong to this function rather than to a length check one
// caller happens to perform.
func (s *session) heldRefusalBlocks() bool {
	held := s.heldRefusalNow()
	if held == nil {
		return false
	}

	// The nested recover is not belt-and-braces, even though the client relay
	// goroutine now runs under shared.RunRelay and a panic escaping here would
	// no longer reach the runtime. What it buys is one level finer: this runs
	// from the recovery of a *decode* panic, and the session is meant to survive
	// that — the relay's recover would end it instead. The teardown it performs
	// (completeQuery, the store write it schedules) is exactly the kind of work
	// that panicking on a malformed session would be worst in. Blocking is still
	// the answer: a refusal is held, so forwarding was never on the table, even
	// when the teardown did not finish.
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.ErrorContext(s.ctx, logMsgRefusalTeardownPanic, slog.Any("panic", r))
			}
		}()

		s.answerHeldRefusal(held, false)
	}()

	return true
}

// interceptUpstreamMessage handles response interception from upstream.
//
// Like interceptClientMessage, this is best-effort observability: any panic in
// the response-decode path is recovered so the upstream packet is still
// forwarded to the client and the session survives.
//
// This is the upstream leg's single trackerMu boundary: everything it reaches
// (learnCursorID, handleQueryResultV2, handleResponse, handleContinuation,
// handleOERStatus, completeQuery, captureRow…) runs with the lock held and
// therefore takes it nowhere itself. Nothing in here does socket I/O, so
// holding it for the whole call cannot stall the client leg on the network.
// Note the defer order: the unlock is registered last and so runs *before* the
// recover, releasing the lock even on a recovered decode panic.
func (s *session) interceptUpstreamMessage(pkt *TNSPacket) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.WarnContext(s.ctx, "recovered from panic intercepting upstream message",
				slog.Any("panic", r))
		}
	}()

	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()

	funcCode, err := parseTTCFunctionCode(pkt.Payload)
	if err != nil {
		return
	}

	ttcPayload := extractTTCPayload(pkt.Payload)
	if ttcPayload == nil {
		return
	}

	// Before anything is completed: the response to an execute is where the
	// server names the cursor it allotted, and that mapping is what lets a
	// later re-execution be gated against the right statement.
	s.learnCursorID(ttcPayload)

	// Every upstream response is also a sample of the OER layout this client
	// parses — the upstream negotiated with the client's own forwarded
	// capabilities — which is what a refusal dbbat synthesizes has to match.
	s.learnOERTail(ttcPayload)

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

// observeClientAuthEncoding reads both OCI dialect flags off the client's AUTH
// Phase 1, which is the earliest — and, for a session that never gets an OER
// before it has to refuse one, the only — evidence of which one this client
// speaks.
//
// The two are not redundant. clientWideEncoding says the key/value *lengths*
// are fixed 4-byte fields rather than compressed ones, and shapes the challenge
// dbbat issues. clientWide64Encoding says the *op headers and summary objects*
// are 64-bit, and shapes the OER dbbat writes when it has learned nothing. Every
// 64-bit client is also a wide one; the reverse does not hold, which is exactly
// the Instant Client.
func (s *session) observeClientAuthEncoding(phase1Payload []byte) {
	s.clientWideEncoding = payloadUsesWideKVEncoding(phase1Payload)

	if ttc := extractTTCPayload(phase1Payload); ttc != nil {
		s.clientWide64Encoding = usesWide64OpHeader(ttc)
	}
}

// learnOERTail keeps the session's picture of the TTC summary object honest
// against the one the upstream actually sends, and tracks the end-to-end
// sequence number so a synthesized error continues the session's count instead
// of restarting it.
//
// Both are read from OERs dbbat is relaying anyway. The upstream leg negotiated
// with this client's own forwarded capabilities, so its OERs are shaped exactly
// the way the client parses one — which is the only reliable source for the
// part of the layout no capability bit predicts (see oerShape.extraTailFields).
// Learning is idempotent and cheap; it re-runs on every response so a session
// that starts before the first sample still converges.
func (s *session) learnOERTail(ttcPayload []byte) {
	s.oerMu.Lock()
	defer s.oerMu.Unlock()

	// A session built without the constructor (tests, and any future path that
	// skips it) carries a zero shape, which would decode nothing.
	s.oer = s.oer.orDefault()

	if info, _ := decodeOERFieldsAt(ttcPayload, 0); info != nil && info.SeqNumber > s.oerSeq {
		s.oerSeq = info.SeqNumber
	}

	if s.oer.tailLearned {
		return
	}

	if learnOERShape(&s.oer, ttcPayload) {
		// fixed_width_64 is the discriminator between the two OCI layouts, and
		// it is on this line because it is the first thing anyone debugging a
		// hung OCI client needs: `fixed_width=true fixed_width_64=false` on a
		// 64-bit client is the whole bug, and nothing else in the log says it.
		s.logger.DebugContext(s.ctx, logMsgLearnedOERTail,
			slog.Int("extra_tail_fields", s.oer.extraTailFields),
			slog.Bool("fixed_width", s.oer.fixedWidth),
			slog.Bool("fixed_width_64", s.oer.fixedWidth64),
			slog.Bool("end_of_response", s.oer.endOfResponse))
	}
}

// nextOERFrame returns the summary-object layout to write the next synthesized
// OER with, and the end-to-end sequence number to stamp on it. No client
// validates the sequence, but a value that walks forward with the session is
// what a server sends and what dbbat's own OER locator bounds.
//
// When nothing has been sampled from the upstream, the client's AUTH framing
// stands in for **all three** halves of the OCI shape. Seeding only the encoding
// would be worse than useless: by encodeOERFixedWidth's own measurement, an OCI
// client handed a fixed-width body with no end-of-response marker hangs exactly
// as it did on the frame this whole change replaces. The two travel together
// because the same client is on both sides of them — an OCI session's messages
// carry the marker whatever the Accept said, and no thin session's do.
//
// fixedWidth64 is seeded from the same place, and it is not a nicety: learning
// happens off the upstream's own OERs, so a session that must refuse *before*
// one has arrived — an approval pattern matching the opening statement, a quota
// already exhausted, a first statement that is a write under `read_only` — gets
// this fallback and nothing else. A 64-bit client handed the 32-bit layout there
// hangs exactly the way it did on every other frame written for the wrong
// dialect, and no integration test can reach it, because sqlplus issues its own
// login SELECTs before anything a grant would refuse. See
// TestUnlearnedRefusalFollowsTheClientsOwnDialect.
func (s *session) nextOERFrame() (oerShape, int, byte) {
	s.oerMu.Lock()
	defer s.oerMu.Unlock()

	s.oerSeq++

	shape := s.oer.orDefault()
	if !shape.tailLearned {
		shape.fixedWidth = s.clientWideEncoding
		shape.endOfResponse = s.clientWideEncoding
		shape.fixedWidth64 = s.clientWide64Encoding
	}

	return shape, s.oerSeq, s.oerCallNumber
}

// observeClientCallNumber records the TTC sequence number of the call the
// client is waiting on, so a refusal can end that call by number rather than by
// zero. Every client message goes through here, including the ones dbbat has no
// opinion about — a fetch continuing a result set in particular, which is its
// own call with its own sequence number. That is what makes the number right for
// a limit violation caught on the *response* leg: the call was forwarded
// upstream a while ago, but its end-of-call OER has not reached the client, so
// the client is still parked in the receive for it.
//
// It is never read outside a call. The one refusal that can fire while the
// client is idle — the limit watchdog — writes no OER at all, deliberately; see
// onLimitViolation.
//
// Reports whether the call could be named. False means the previous number is
// kept rather than overwritten with a number that is *wrong* — a piggyback's
// own sequence, with the real call stapled behind a body dbbat cannot walk.
// Both legs depend on that: the client leg refuses nothing on a message it
// could not name, and the response leg's mid-stream limit refusal keeps
// pointing at the last call dbbat actually saw.
func (s *session) observeClientCallNumber(ttcPayload []byte) bool {
	number, ok := clientCallNumber(ttcPayload)
	if !ok {
		return false
	}

	s.oerMu.Lock()
	defer s.oerMu.Unlock()

	s.oerCallNumber = number

	return true
}

// observeOERServerCaps and observeOERClientVersion are the locked wrappers the
// pre-auth relay uses.
//
// The relay is two goroutines by design (see relayPreAuthNegotiation): the pump
// forwards the upstream's Set Protocol reply while the main loop is already
// forwarding the client's Set Data Types. Both land on this shape, and the
// version each writes is `min(existing, observed)` — a read-modify-write, not a
// torn word, so it needs the mutex and not just an atomic.
//
// This did not show up in testing because make test-e2e-oracle runs without
// -race, unlike make test. See
// specs/todos/2026-08-11-10-race-detector-on-the-integration-suites.md.
func (s *session) observeOERServerCaps(raw []byte) {
	s.oerMu.Lock()
	defer s.oerMu.Unlock()

	observeOERCapabilities(&s.oer, raw)
}

func (s *session) observeOERClientVersion(ttcBody []byte) {
	s.oerMu.Lock()
	defer s.oerMu.Unlock()

	observeClientTTCVersion(&s.oer, ttcBody)
}

// handleOERStatus processes a standalone OER (func=0x04) message. Servers send
// it directly (after a marker exchange) when a statement fails, and as an
// end-of-call status in some flows.
//
// This is the path a failing statement takes — measured, on both clients: a
// SELECT against a missing table, a unique-key violation, a divide-by-zero, a
// PL/SQL RAISE and a PL/SQL compile error all come back as a standalone 0x04
// after a marker exchange, never as an OER embedded in a Response. So this
// function is where queries.error is won or lost.
//
// It used to be lost. decodeOERAt demands the end-of-call bit, and the bit was
// believed to be a client trait; it is not, it tracks the call. Only the failed
// *DDL* carried it in either capture, so on every client the other five shapes
// were dropped here and the statement was closed as a *success* by the next
// statement's flushPendingQuery — a failed UPDATE logged with no error at all.
//
// What stands in for the bit is decodeErrorOER, which proves the tail is an
// Oracle diagnostic naming the very code the fields reported. That is enough
// outside a row stream, where the payload cannot be row bytes.
//
// Mid-fetch it is not, because this function is routed on byte 0 alone and a
// row value's four-byte length prefix landing at the start of a TNS packet is
// indistinguishable from an OER's marker. A failure raised there used to be
// dropped outright — see midFetchOERNamesTheStreamingCursor for what replaced
// that, and why the bar is higher on this side of the boundary.
//
// Callers hold trackerMu (see interceptUpstreamMessage).
func (s *session) handleOERStatus(ttcPayload []byte) {
	if info := decodeOERAt(ttcPayload, 0); info != nil {
		s.completeQueryFromOER(info)

		return
	}

	if s.tracker.pendingQuery == nil {
		return
	}

	info := decodeErrorOER(ttcPayload)
	if info == nil {
		return
	}

	if s.rowStreamActive() && !s.midFetchOERNamesTheStreamingCursor(info) {
		return
	}

	s.completeQueryFromOER(info)
}

// midFetchOERNamesTheStreamingCursor is the extra anchor a proven diagnostic has
// to clear to end a call *while rows are streaming*.
//
// Measured (testdata/{python_thin,go_ora}_midfetch_fail.pcapng: a TO_NUMBER that
// blows up 14 900 rows into a 20 000-row fetch), a mid-fetch failure arrives
// exactly the way a pre-first-row one does — a standalone func 0x04, CallStatus
// 0x1, no end-of-call bit — and its cursorID field is the cursor whose rows are
// on the wire, on both clients. Replaying the whole testdata corpus through a
// real session says the cost of believing decodeErrorOER here is nil: of the 342
// server packets that arrive mid-row-stream, 340 do not even begin with 0x04,
// the two that do are these very failures, and a scan of the same predicate at
// every 0x04 offset inside all of them accepts nothing. Those figures are
// printed, not remembered — see TestDumpReplay_MidStreamOERFalsePositiveRate.
//
// The cursor check is not what that measurement justifies; it is aimed at the
// one shape a corpus of numeric and temporal fixtures cannot contain — a result
// set whose rows *carry* ORA- text, as `SELECT message FROM error_log` does.
// Such a row would have to decode as seven bounded ints, be followed by the
// ASCII spelling of the number its fourth field landed on, *and* have its
// seventh field land on the streaming cursor's own id.
//
// It fails closed twice over: a mid-fetch diagnostic naming another cursor is
// dropped, and so is one arriving on a fetch whose cursor id was never learned.
// Either way the failure mode is the old missing error text and never a
// fabricated one. The debug line is there because that is otherwise invisible —
// if an unmeasured client ever reports a different cursor, this is what says so.
//
// One honest caveat about the reference value. `learnCursorID` runs on every
// upstream packet and latches only once it has succeeded, so for a statement
// whose id is never learned the anchored scan behind it keeps running over
// row-stream bytes for the whole fetch — meaning the id this compares against
// could itself have originated in row data. That is pre-existing (cursor-id
// learning has always worked this way, and re-execution gating already trusts
// it), but this anchor is what makes it load-bearing for query error text too.
//
// Callers hold trackerMu.
func (s *session) midFetchOERNamesTheStreamingCursor(info *oerInfo) bool {
	streaming := s.tracker.pendingQuery.cursor.cursorID
	if streaming != 0 && info.CursorID == int(streaming) {
		return true
	}

	s.logger.DebugContext(s.ctx, "mid-fetch OER does not name the streaming cursor; leaving the call open",
		slog.Int("oer_cursor_id", info.CursorID),
		slog.Int("streaming_cursor_id", int(streaming)),
		slog.Int("ora_code", info.ErrorCode))

	return false
}

// completeQueryFromOER finalizes the pending query from decoded OER fields:
// rows affected on success, error text on failure, plain completion on
// ORA-01403 (end-of-data, keeps captured-row counts).
//
// Callers hold trackerMu (see interceptUpstreamMessage).
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
//
// Callers hold trackerMu (see interceptUpstreamMessage).
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

// rowStreamActive reports whether the session is in the middle of streaming a
// result set: a query is pending, its cursor is known, and the column
// definitions have already been decoded from the QueryResult (func=0x10) that
// opened the fetch.
//
// This is the session's own notion of a call boundary: from the QueryResult
// until the query completes, every upstream packet is row-stream content, so
// its first byte is a value length or a stream marker — not a TTC function
// code. Column definitions are the marker of that window because they are set
// exactly once per fetch (handleQueryResultV2 / handleResponse) and cleared
// with the cursor.
//
// Callers hold trackerMu (see interceptUpstreamMessage).
func (s *session) rowStreamActive() bool {
	pending := s.tracker.pendingQuery

	return pending != nil && pending.cursor != nil && len(pending.cursor.columns) > 0
}

// handleResponse processes a legacy TTC Response (func=0x08).
// In v315+, most responses don't follow the legacy format so we skip them.
// Query completion is handled by handleQueryResultV2 for func=0x10.
//
// Callers hold trackerMu (see interceptUpstreamMessage).
func (s *session) handleResponse(ttcPayload []byte) {
	// Mid-fetch, a leading 0x08 is NOT a fresh Response: it is row-stream
	// content — an 8-byte first column value's length prefix, or the
	// 0x08 0x01 0x06 end-of-rows footer — that happened to land at the start
	// of a TNS packet. Only an embedded OER, whose end-of-call bit decodeOERAt
	// verifies, marks a real call boundary here; anything else is continuation
	// data and is decoded as such.
	//
	// Without this guard the legacy fixed-offset decoder below read row bytes
	// as an error code plus a length-prefixed message, wrote them into
	// Query.Error, and cleared pendingQuery — which silently truncated row
	// capture, stopped mid-stream quota enforcement, and mis-charged the rest
	// of the stream's bytes for the remainder of the fetch.
	if s.rowStreamActive() {
		if oer := findOERInResponse(ttcPayload); oer != nil {
			s.completeQueryFromOER(oer)
			return
		}

		s.handleContinuation(ttcPayload)

		return
	}

	// v315+ DML responses embed an OER (func=0x04) status block carrying the
	// affected-row count (INSERT/UPDATE/DELETE) or the ORA error. This is the
	// reliable source — the legacy fixed-offset layout below misreads v315+
	// responses, so prefer the OER whenever one is present.
	if oer := findOERInResponse(ttcPayload); oer != nil {
		s.completeQueryFromOER(oer)
		return
	}

	// ...but the end-of-call bit findOERInResponse insists on is not a protocol
	// invariant. On the successful calls that reach this branch it reads like a
	// client trait — a python-oracledb thin session's OERs come with CallStatus
	// 1–2 where go-ora's carry the bit — so every INSERT/UPDATE/DELETE those
	// clients ran used to fall through here, stay pending, and be closed only by
	// the *next* statement's flushPendingQuery — recording no rows_affected and a
	// duration_ms that measured the client's think time (one live UPDATE was
	// logged at 74 s because the session then sat idle). A session whose last
	// statement was such a DML had it sealed by cleanup instead, timed to the
	// disconnect.
	//
	// "Client trait" is the wrong generalisation, though, and only holds for the
	// successful calls this branch sees: measured across six failure shapes, the
	// bit tracks the *call*, and go-ora emits bit-less OERs too. That matters in
	// handleOERStatus, not here — no failure arrives embedded in a Response at
	// all (TestDumpReplay_NoFailureArrivesEmbeddedInAResponse).
	//
	// Outside a row stream the payload is a return-parameter block rather than
	// row bytes, which is what the bit was defending against, so the anchored
	// bounds that cursor-id learning already trusts on this very message are
	// enough to complete on. They have to be: dbbat was reading cursor ids off
	// these exact OERs while refusing to read the row count out of the same
	// seven fields.
	if s.tracker.pendingQuery != nil {
		if oer := findPlausibleOERInResponse(ttcPayload); oer != nil {
			s.completeQueryFromOER(oer)
			return
		}
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
	// Finish any query still in flight before the connection record closes.
	// A client that disconnects mid-fetch (or an upstream that drops) otherwise
	// leaves its row forever incomplete — duration_ms NULL, results_truncated
	// unset, and the streamed bytes never charged to the connection or the
	// grant, which is a way to under-report against a byte quota. Same reasoning
	// as the completeQuery on the quota-kill path in upstreamToClient.
	//
	// Under trackerMu: cleanup runs as soon as *one* relay direction returns,
	// and the other goroutine can still be intercepting packets on the same
	// tracker for as long as its own socket stays open.
	s.trackerMu.Lock()
	s.flushPendingQuery()
	s.trackerMu.Unlock()

	s.stream.Connection(s.ctx, shared.ConnectionClosed)

	if s.grant != nil && s.revocation != nil {
		s.store.Revocations().Deregister(s.grant.UID, s.revocation)
	}

	if s.dump != nil {
		if err := s.dump.Close(); err != nil {
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

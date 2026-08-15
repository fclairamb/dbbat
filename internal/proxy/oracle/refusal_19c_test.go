package oracle

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
	"github.com/fclairamb/dbbat/internal/store"
)

// This file is the Oracle **19c** half of the mid-session refusal coverage, and
// it exists because of a production measurement the 23ai suites cannot reach.
//
// Measured 2026-08-13 against production (image 0.23.2), upstream `Oracle
// Database 19c Standard Edition 2`, under a `read_only` grant with
// python-oracledb 3.4.2 thin: dbbat blocked the write correctly ("write
// operations not permitted with read-only access") and the client never
// received it. The call hung past two minutes; with `call_timeout` set it came
// back after 30s as `DPY-4011` (the connection closed), every later statement
// on that connection failed with `DPY-1001`, and the interpreter segfaulted in
// driver teardown. `TestIntegration_BlockedStatementRefusesPythonThin` asserts
// exactly that shape and passes — against Oracle 23ai Free, the only image with
// no license. So the three properties a refusal owes a client were pinned on one
// server version and unpinned on the other:
//
//  1. an ORA error arrives,
//  2. it arrives promptly,
//  3. the connection survives it.
//
// The refusal frame is shaped from what the session negotiated (see
// nextOERFrame / encodeOER), and 19c negotiates differently — a 42-entry
// capability array against 23ai's 54, and TTC field version 12 against 27. That
// is what makes the 23ai pass not carry over.
//
// **These fixtures are real 19c recordings, not synthesized ones.** The spec
// this implements assumed the corpus was 23ai-only and asked for a synthetic
// 19c shape; it is not. `go_ora.pcapng` and `dbeaver_init.pcapng` both carry the
// 19c banner verbatim, and `python_thin.pcapng` / `dbeaver.pcapng` carry the same
// server fingerprint (see TestOracle19cCaptures_AreReallyA19cUpstream). So the
// session under test here negotiates against a real 19c upstream's own Set
// Protocol reply, learns its OER layout off that server's own end-of-call
// frames, and is then made to refuse a statement.
//
// What is still synthesized, and cannot be otherwise: the **statement** being
// refused. No recording contains one, because nobody records a session getting
// refused — and the refusal frame itself is dbbat's own invention, never
// something the server sends, so there is nothing to replay for it. The exec
// frames below therefore come out of this package's own builders, in the wire
// shape each recorded client used (piggyback exec for go-ora, the JDBC/DBeaver
// `11 69` exec for DBeaver), and the *negotiation* they run against is measured.
//
// The live re-measurement against production 19c stays with the owner: it needs
// a prod deploy and a 19c instance. This is the part that runs in CI on every
// commit.

// oracle19cBanner is the upstream version string the recordings carry verbatim.
// It is the evidence that these captures are 19c, and the reason this file can
// claim a version at all.
const oracle19cBanner = "Oracle Database 19c Standard Edition 2 Release 19.0.0.0.0 - Production"

// The recordings taken against that 19c upstream. Both carry the banner; each
// is a different client, and therefore a different exec frame shape and a
// different summary-object tail.
const (
	goOra19cDump   = "go_ora.pcapng"
	dbeaver19cDump = "dbeaver_init.pcapng"
)

// Two more recordings share the same 19c upstream but do not carry the banner in
// the captured range (it rides the AUTH response, and these two start after it).
// They are asserted on the fingerprint only.
const (
	pythonThin19cDump  = "python_thin.pcapng"
	dbeaverFull19cDump = "dbeaver.pcapng"
)

// oracle19cFingerprint is that upstream's Set Protocol reply, read off the
// capability array the production code reads. It is *not* the 23ai one — which
// is the whole premise of this file: docs/oracle.md's capability table gives
// `numCaps` 0x2a (42) for 19c against 0x36 (54) for 23ai, and the negotiated TTC
// field version differs with it.
var oracle19cFingerprint = struct {
	numCaps      int
	customHash   byte // caps[4]
	fieldVersion byte // caps[7] — 12 on this 19c, 27 on 23ai
	endOfCall    byte // caps[15]
	ecid         byte // caps[16]
	bigChunks    byte // caps[37]
}{numCaps: 42, customHash: 0x6f, fieldVersion: 12, endOfCall: 0x7f, ecid: 0xff, bigChunks: 0x7f}

// TestOracle19cCaptures_AreReallyA19cUpstream is the provenance assertion the
// rest of this file rests on: these captures are 19c, and they are all the same
// 19c.
//
// It is worth a test rather than a comment because the claim is what makes the
// coverage below mean anything, and because it was nearly missed — the corpus
// was believed to be 23ai-only. Two of the four recordings carry the version
// banner outright; the other two carry a capability array that is byte-for-byte
// the same server and differs from every 23ai capture in the corpus.
func TestOracle19cCaptures_AreReallyA19cUpstream(t *testing.T) {
	t.Parallel()

	for _, name := range []string{goOra19cDump, dbeaver19cDump, pythonThin19cDump, dbeaverFull19cDump} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, caps := firstSetProtocolReply(t, name)

			require.Len(t, caps, oracle19cFingerprint.numCaps,
				"19c frames a 42-entry capability array; 23ai frames 54")
			assert.Equal(t, oracle19cFingerprint.customHash, caps[customHashCapIndex])
			assert.Equal(t, oracle19cFingerprint.fieldVersion, caps[oerCapFieldVersionIndex],
				"the negotiated TTC field version is the one 19c advertises, not 23ai's")
			assert.Equal(t, oracle19cFingerprint.endOfCall, caps[oerCapEndOfCallIndex])
			assert.Equal(t, oracle19cFingerprint.ecid, caps[oerCapECIDIndex])
			assert.Equal(t, oracle19cFingerprint.bigChunks, caps[bigClrChunksCapIndex])
		})
	}

	for _, name := range []string{goOra19cDump, dbeaver19cDump} {
		t.Run(name+" carries the 19c banner", func(t *testing.T) {
			t.Parallel()

			assert.Contains(t, string(recordedBytes(t, name)), oracle19cBanner,
				"this recording is what licenses the word 19c in this file")
		})
	}
}

// recordedBytes concatenates every packet of a recording, in wire order. Used
// only to look for the version banner, which can sit anywhere in the AUTH
// response's key/value stream.
func recordedBytes(t *testing.T, name string) []byte {
	t.Helper()

	var out []byte

	for _, pkt := range loadTestDump(t, name).Packets {
		out = append(out, pkt.Data...)
	}

	return out
}

// TestOracle19cSummaryTail_IsWhatThatServerSends pins the one field a refusal
// frame hinges on, measured on both server versions — and it is the concrete
// sense in which "19c negotiates differently".
//
// The two trailing fields of the summary object are, in Oracle's own words,
// "fields added in Oracle Database 20c" (see oerModernExtraTailFields). 19c
// predates 20c, so it sends **none of them, to any client** — including
// python-oracledb thin, which the *23ai* server sends two to. A client's parser
// reads exactly the count its server sends, so a frame built for the wrong
// server does not merely lose the message text: python-oracledb reports
// DPY-5002 on a tail it cannot walk.
//
// dbbat gets this right by not guessing mid-session: nextOERFrame leaves the
// count at zero until an upstream OER teaches it, and on 19c what it learns is
// zero. The test is here so a future change that starts *assuming* the 20c tail
// fails on the version that has no such fields.
func TestOracle19cSummaryTail_IsWhatThatServerSends(t *testing.T) {
	t.Parallel()

	for name, wantTail := range map[string]int{
		// 19c: no 20c fields, whichever client asked.
		goOra19cDump:       0,
		dbeaver19cDump:     0,
		pythonThin19cDump:  0,
		dbeaverFull19cDump: 0,

		// 23ai, same client as pythonThin19cDump above: two.
		"python_thin_failed_stmt.pcapng": oerModernExtraTailFields,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			replay19cNegotiation(t, s, name)

			shape := s.oerShapeSnapshot()
			require.True(t, shape.tailLearned, "the tail must come off the server's own OERs")
			assert.Equal(t, wantTail, shape.extraTailFields)
		})
	}
}

// TestOracle19cLearnsItsTailBeforeAuthPhase2 measures *when* that tail arrives,
// which is what decides whether the AUTH-phase refusal has to guess it.
//
// authRefusalOERShape guesses the 20c tail for a modern (customHash / 18453)
// client when nothing has been learned — a guess that would be wrong on 19c,
// since 19c has no such fields. On these recordings it never applies: the
// upstream's AUTH **Phase 1** response already carries a summary object, so the
// tail is learned one packet before the client's Phase 2 — the earliest moment a
// key can be refused. Pinned here so the interaction between the two specs is a
// measurement rather than an assumption.
func TestOracle19cLearnsItsTailBeforeAuthPhase2(t *testing.T) {
	t.Parallel()

	for _, name := range []string{goOra19cDump, dbeaver19cDump, pythonThin19cDump} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := newTestSession(nil)
			s.clientConn = drainedPipe(t)

			learned := false

			for _, pkt := range loadTestDump(t, name).Packets {
				tns, err := parseTNSFromDumpPacket(pkt.Data)
				if err != nil || tns.Type != TNSPacketTypeData {
					continue
				}

				if pkt.Direction == dump.DirClientToServer {
					if len(tns.Payload) > ttcDataFlagsSize+1 &&
						tns.Payload[ttcDataFlagsSize] == byte(TTCFuncPiggyback) &&
						tns.Payload[ttcDataFlagsSize+1] == PiggybackSubAuth2 {
						assert.True(t, learned,
							"the tail must be learned off the upstream's Phase 1 response, "+
								"before a Phase 2 refusal would have to guess it")

						return
					}

					s.interceptClientMessage(tns)

					continue
				}

				s.observeOERServerCaps(tns.Raw)
				s.interceptUpstreamMessage(tns)

				learned = learned || s.oerShapeSnapshot().tailLearned
			}

			t.Fatalf("%s carries no client AUTH Phase 2", name)
		})
	}
}

// refusal19cDeadline bounds every read the probe below makes off the client
// socket. It is a deadlock detector, not a grace period: a refusal never leaves
// the proxy, so it answers within a round trip on a `net.Pipe`.
//
// It is the *promptness* property, asserted the only way promptness can be: the
// production failure was a call that hung past two minutes and then reported a
// closed connection. Anything that reintroduces it fails here in five seconds
// instead of hanging the suite.
const refusal19cDeadline = 5 * time.Second

// oraRefusalCode is the ORA code every mid-session refusal carries —
// ORA-01031, insufficient privileges — as sendOracleError assigns it. It is
// spelled out here rather than imported because errors.go names only the codes
// the TNS-level refusals use.
const oraRefusalCode = 1031

// oracle19cProbe is a client on one end of the real proxy relay and an upstream
// on the other, with the session's negotiation replayed from a 19c recording.
//
// It drives clientToUpstream itself rather than calling interceptClientMessage
// directly, which is what lets the third property be asserted at all: "the
// connection survives" is a claim about the relay loop still reading, the client
// socket still being open, and the next statement still traveling — none of
// which is observable from a single intercept call.
type oracle19cProbe struct {
	t       *testing.T
	session *session

	// client is the client's end of the proxy's client socket: the probe writes
	// statements into it and reads refusals out of it.
	client net.Conn

	// upstream is the upstream's end: whatever the proxy forwards lands here,
	// and a refused statement must never appear.
	upstream net.Conn

	// relay carries clientToUpstream's return value. A clean client EOF must
	// come back nil — a session torn down by its own refusal would not.
	relay chan error
}

// newOracle19cProbe replays a recorded 19c session into a fresh session, arms it
// with grant, and starts the real client relay.
//
// The replay runs the same three observers pumpPreAuthUpstream runs on every
// upstream packet, the same observeClientAuthEncoding the AUTH path runs on the
// client's Phase 1, and the same interceptUpstreamMessage the response relay
// runs — so the session's OER shape is learned off this 19c server's own
// end-of-call frames rather than assumed.
//
// The grant is armed *after* the replay on purpose: the recorded statements are
// the client's own login and metadata SELECTs, and refusing those would prove
// nothing about the statement this test is actually here to refuse. A session
// resolves its grant at auth in production, which is before any of this.
func newOracle19cProbe(t *testing.T, dumpName string, grant *store.Grant) *oracle19cProbe {
	t.Helper()

	s := newTestSession(nil)

	replay19cNegotiation(t, s, dumpName)

	// The negotiation has to have actually landed, or everything below would be
	// asserting the default shape under a 19c label.
	shape := s.oerShapeSnapshot()
	require.True(t, s.upstreamCustomHash, "19c advertises customHash (caps[4]&0x20)")
	require.True(t, s.clientBigClrChunks, "19c advertises UseBigClrChunks (caps[37]&0x20)")
	require.False(t, s.clientWideEncoding, "these recordings are thin clients, not OCI")
	require.True(t, shape.tailLearned,
		"the summary-object layout must come from this 19c server's own OERs, not from the default")

	s.grant = grant

	clientEnd, proxyClientEnd := net.Pipe()
	upstreamEnd, proxyUpstreamEnd := net.Pipe()

	s.clientConn = proxyClientEnd
	s.upstreamConn = proxyUpstreamEnd

	p := &oracle19cProbe{
		t:        t,
		session:  s,
		client:   clientEnd,
		upstream: upstreamEnd,
		relay:    make(chan error, 1),
	}

	go func() { p.relay <- s.clientToUpstream() }()

	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = proxyClientEnd.Close()
		_ = upstreamEnd.Close()
		_ = proxyUpstreamEnd.Close()
	})

	return p
}

// replay19cNegotiation feeds a recording through the production observers, in
// wire order and in both directions, exactly as the relay does.
func replay19cNegotiation(t *testing.T, s *session, dumpName string) {
	t.Helper()

	s.clientConn = drainedPipe(t)

	for _, pkt := range loadTestDump(t, dumpName).Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil {
			continue
		}

		if pkt.Direction == dump.DirClientToServer {
			// What relayPreAuthNegotiation and the AUTH path read off the
			// client: its AUTH framing dialect, and its own TTC field version.
			if isAuthPhase1(tns) {
				s.observeClientAuthEncoding(tns.Payload)
			}

			if ttc := extractTTCPayload(tns.Payload); ttc != nil {
				s.observeOERClientVersion(ttc)
			}

			if tns.Type == TNSPacketTypeData {
				s.interceptClientMessage(tns)
			}

			continue
		}

		// What pumpPreAuthUpstream reads off every upstream packet.
		if observeCustomHashFlag(tns.Raw) {
			s.upstreamCustomHash = true
		}

		if observeBigClrChunksFlag(tns.Raw) {
			s.clientBigClrChunks = true
		}

		s.observeOERServerCaps(tns.Raw)

		if tns.Type == TNSPacketTypeData {
			s.interceptUpstreamMessage(tns)
		}
	}
}

// exec writes one statement-carrying TTC frame to the proxy as the client.
func (p *oracle19cProbe) exec(ttc []byte) {
	p.t.Helper()

	require.NoError(p.t, p.client.SetWriteDeadline(time.Now().Add(refusal19cDeadline)))
	require.NoError(p.t, writeTNSPacket(p.client, &TNSPacket{
		Type:    TNSPacketTypeData,
		Payload: append([]byte{0x00, 0x00}, ttc...),
	}))
}

// expectRefusal reads the proxy's answer off the client socket and decodes it
// the way the client's own error parser does. It is properties 1 and 2: an ORA
// error, within the deadline.
func (p *oracle19cProbe) expectRefusal(ttc []byte, wantFragment string) *oerInfo {
	p.t.Helper()

	require.NoError(p.t, p.client.SetReadDeadline(time.Now().Add(refusal19cDeadline)))

	started := time.Now()
	body := readAuthRejectFrame(p.t, p.client)
	elapsed := time.Since(started)

	assert.Less(p.t, elapsed, refusal19cDeadline,
		"the refusal has to end the client's call, not leave it waiting: the measured "+
			"19c failure hung past two minutes and then reported a closed connection")

	// The strict walk — the same one the AUTH-phase refusal is decoded with. A
	// decode that found the text by searching for "ORA-" would pass on a frame
	// no client can parse; this one only succeeds when every field of the summary
	// object lands where this session's negotiation says it does.
	shape := p.session.oerShapeSnapshot()
	code, message, callNumber := decodeAuthRejectOER(p.t, shape, body)

	assert.Equal(p.t, oraRefusalCode, code,
		"a refused statement comes back as ORA-01031, insufficient privileges")
	assert.Contains(p.t, message, "ORA-01031: ", "the client renders this text verbatim")
	assert.Contains(p.t, message, wantFragment, "the message has to name the reason")

	if want, ok := clientCallNumber(ttc); ok {
		assert.Equal(p.t, want, callNumber,
			"the refusal must end the call the client is parked on, by number")
	}

	// A third, independent reading of the same bytes: this package's own OER
	// decoder, which is what a relayed server error goes through.
	info := decodeOERAt(shape, body, 0)
	require.NotNil(p.t, info, "the end-of-call bit must be set or no call boundary is recognized")
	assert.Equal(p.t, oraRefusalCode, info.ErrorCode)

	return info
}

// expectForwarded reads the next packet off the upstream socket and requires it
// to be ttc, unchanged.
//
// This is property 3, and it is also how "the refused statement never reached
// upstream" is asserted: the pipe is unbuffered, so a proxy that had forwarded
// the refused frame would be parked on that write and the refusal would never
// have arrived at all.
func (p *oracle19cProbe) expectForwarded(ttc []byte) {
	p.t.Helper()

	require.NoError(p.t, p.upstream.SetReadDeadline(time.Now().Add(refusal19cDeadline)))

	pkt, err := readTNSPacket(p.upstream)
	require.NoError(p.t, err,
		"the connection that carried a refusal must still forward the next statement")
	assert.Equal(p.t, ttc, extractTTCPayload(pkt.Payload),
		"an allowed statement must travel unchanged after a refusal")
}

// expectCleanEOF closes the client socket and requires the relay to end on it
// without an error — the session was never torn down by its own refusal.
func (p *oracle19cProbe) expectCleanEOF() {
	p.t.Helper()

	require.NoError(p.t, p.client.Close())

	select {
	case err := <-p.relay:
		require.NoError(p.t, err,
			"a session that refused a statement must end on the client's own close, not on an error")
	case <-time.After(refusal19cDeadline):
		p.t.Fatal("the client relay never returned after the client closed")
	}
}

// TestRefusalOn19c_ReachesTheClientOnAStillUsableSession is the regression this
// spec delivers: on a session negotiated against a real Oracle 19c upstream, a
// statement refused by access control comes back to the client as a readable ORA
// error, promptly, on a connection that keeps working.
//
// Each capture drives the exec frame shape its own client used, because that is
// what decides which call the refusal has to end and how the frame is framed.
func TestRefusalOn19c_ReachesTheClientOnAStillUsableSession(t *testing.T) {
	t.Parallel()

	// The two 19c recordings, each with the exec frame shape its client sends.
	// go-ora sends the v315+ piggyback exec; DBeaver/JDBC thin staples its
	// execute behind a close-cursors list (`11 69`).
	captures := map[string]func(sql string) []byte{
		goOra19cDump:   buildPiggybackExec,
		dbeaver19cDump: buildJDBCExec,
	}

	refusals := map[string]struct {
		controls    []string
		blockedSQL  string
		wantMessage string
	}{
		"read_only refuses a write": {
			controls:    []string{store.ControlReadOnly},
			blockedSQL:  "INSERT INTO dbbat_19c_probe VALUES (1)",
			wantMessage: "read-only",
		},
		"block_ddl refuses a CREATE TABLE": {
			controls:    []string{store.ControlBlockDDL},
			blockedSQL:  "CREATE TABLE dbbat_19c_probe (id NUMBER)",
			wantMessage: "DDL",
		},
		// Not a grant control: the Oracle pattern list, which refuses through
		// the very same frame whatever the grant says. It is here because it is
		// the one refusal a full-write grant can still meet, so it covers the
		// shape for a session with no restrictive control at all.
		"the Oracle pattern list refuses ALTER SYSTEM": {
			blockedSQL:  "ALTER SYSTEM KILL SESSION '42,1'",
			wantMessage: "not permitted through the proxy",
		},
	}

	for dumpName, buildExec := range captures {
		for name, tc := range refusals {
			t.Run(dumpName+"/"+name, func(t *testing.T) {
				t.Parallel()

				grant := &store.Grant{Definition: &store.GrantDefinition{Controls: tc.controls}}
				p := newOracle19cProbe(t, dumpName, grant)

				blocked := buildExec(tc.blockedSQL)

				// 1 + 2: the ORA error arrives, and it arrives promptly.
				p.exec(blocked)
				first := p.expectRefusal(blocked, tc.wantMessage)

				// 3: the connection survives. A read still travels upstream,
				// unchanged, on the same session.
				allowed := buildExec("SELECT 1 FROM DUAL")
				p.exec(allowed)
				p.expectForwarded(allowed)

				// And the refusal path is still good for a second one: a
				// session left in a poisoned state after the first refusal
				// would fail here rather than on the first frame.
				p.exec(blocked)
				second := p.expectRefusal(blocked, tc.wantMessage)

				assert.Greater(t, second.SeqNumber, first.SeqNumber,
					"the session's end-to-end sequence continues across a refusal instead of restarting")

				p.expectCleanEOF()
			})
		}
	}
}

// TestRefusalOn19c_BlockCopyRefusesNothing is the third control the spec names,
// and the honest answer for Oracle: there is no statement for it to refuse.
//
// An Oracle COPY is a client-side SQL*Plus command, never a server statement, so
// block_copy is deliberately not among the per-statement controls
// (hasStatementControls, intercept.go). A write under a block_copy-only grant is
// therefore *allowed* — and this pins that on a 19c-negotiated session, so
// "block_copy is covered" cannot later be read as "block_copy refuses writes
// here".
func TestRefusalOn19c_BlockCopyRefusesNothing(t *testing.T) {
	t.Parallel()

	grant := &store.Grant{Definition: &store.GrantDefinition{Controls: []string{store.ControlBlockCopy}}}
	p := newOracle19cProbe(t, goOra19cDump, grant)

	write := buildPiggybackExec("INSERT INTO dbbat_19c_probe VALUES (1)")

	p.exec(write)
	p.expectForwarded(write)
	p.expectCleanEOF()
}

// TestRefusalOn19c_QuotaRefusalTakesTheSameFrame covers the other family of
// refusal that rides writeTTCError: an exhausted grant, checked ahead of the
// static controls. Same session shape, same three properties — and unlike
// read_only it can refuse a **recorded** 19c client frame verbatim, because it
// refuses every statement rather than only a write.
//
// That is as close as this corpus gets to replaying a refusal end to end: the
// negotiation, the client's exec frame and the call number are all measured off
// the 19c recording, and only the refusal itself is dbbat's own.
func TestRefusalOn19c_QuotaRefusalTakesTheSameFrame(t *testing.T) {
	t.Parallel()

	recorded := recorded19cExec(t, goOra19cDump)

	p := newOracle19cProbe(t, goOra19cDump, exhaustedGrant())

	p.exec(recorded)
	p.expectRefusal(recorded, "query limit exceeded")
	p.expectCleanEOF()
}

// recorded19cExec returns the first statement-carrying client frame in a 19c
// recording, byte for byte as that client sent it.
func recorded19cExec(t *testing.T, dumpName string) []byte {
	t.Helper()

	for _, ttc := range clientTTCPayloads(t, dumpName) {
		if IsPiggybackExecSQL(ttc) {
			return bytes.Clone(ttc)
		}
	}

	t.Fatalf("%s carries no piggyback execute", dumpName)

	return nil
}

package oracle

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/version"
)

// widePhase1Payload builds a minimal wide/OCI-encoded AUTH Phase 1 TNS Data
// payload: 2-byte data flags + a [03 76] piggyback header + a CLR-prefixed
// username immediately followed by AUTH_TERMINAL / AUTH_PROGRAM_NM KV pairs
// in wide (4-byte little-endian length) framing via ttcKeyValWide.
//
// Unlike the real captures in oci_instantclient_test.go (instantclientPhase1Body,
// bundledPhase1Body), this fixture doesn't reproduce the full OCI preamble
// (pointer runs, buffer-sized lengths): the anchored locators exercised here
// (locateAnchoredUsername -> locateWideUsername, rewriteAuthPhase1UsernameAnchored)
// only require the CLR-prefixed username to sit immediately before the first
// wide-framed AUTH_ key, so a simplified preamble is sufficient to drive
// payloadUsesWideKVEncoding's true branch — which thinPhase1Payload
// (phase1_forward_test.go) never does.
func widePhase1Payload(username, clientProgramName string) []byte {
	payload := make([]byte, 0, 64)
	payload = append(payload, 0x00, 0x00) // TNS data flags
	payload = append(payload, byte(TTCFuncPiggyback), PiggybackSubAuth1, 0x00, 0x00)
	payload = append(payload, byte(len(username))) // CLR length prefix
	payload = append(payload, []byte(username)...)
	payload = append(payload, ttcKeyValWide("AUTH_TERMINAL", "unknown", 0)...)
	payload = append(payload, ttcKeyValWide(authKeyProgramNM, clientProgramName, 0)...)

	return payload
}

// widePhase2Payload is the Phase 2 counterpart of widePhase1Payload: a
// [03 73 b0] piggyback header, a CLR-prefixed username, and wide-framed
// AUTH_SESSKEY / AUTH_PASSWORD / AUTH_TERMINAL / AUTH_PROGRAM_NM KV pairs.
// The anchored Phase 2 rewrite (rewriteAuthPhase2Anchored) is preamble-
// independent in the same way as Phase 1, so this simplified shape is enough
// to exercise the wide value-replacement path end to end.
func widePhase2Payload(username, sessKey, password, clientProgramName string) []byte {
	body := make([]byte, 0, 96)
	body = append(body, byte(TTCFuncPiggyback), PiggybackSubAuth2, 0x00)
	body = append(body, byte(len(username))) // CLR length prefix
	body = append(body, []byte(username)...)
	body = append(body, ttcKeyValWide(authKeySessKey, sessKey, 1)...)
	body = append(body, ttcKeyValWide(authKeyPassword, password, 0)...)
	body = append(body, ttcKeyValWide("AUTH_TERMINAL", "unknown", 0)...)
	body = append(body, ttcKeyValWide(authKeyProgramNM, clientProgramName, 0)...)

	payload := make([]byte, 0, len(body)+2)
	payload = append(payload, 0x00, 0x00) // TNS data flags prefix
	payload = append(payload, body...)

	return payload
}

// TestWidePhase1Payload_IsWideEncoding is a fixture sanity check: confirms
// widePhase1Payload actually trips payloadUsesWideKVEncoding, so the tests
// below are exercising the wide/OCI path (replaceAuthKVValueWide /
// findKVByKeyBytesWide) and not silently falling back to thin.
func TestWidePhase1Payload_IsWideEncoding(t *testing.T) {
	t.Parallel()

	body := widePhase1Payload("connector", "sqlplus")[ttcDataFlagsSize:]
	if !payloadUsesWideKVEncoding(body) {
		t.Fatalf("widePhase1Payload fixture not detected as wide-encoded")
	}
}

// TestSendUpstreamAuthPhase1RewritesProgramName_Wide is the OCI/wide-encoding
// counterpart of TestSendUpstreamAuthPhase1RewritesProgramName
// (upstream_auth_client_program_name_test.go), which only covers thin. It
// confirms AUTH_PROGRAM_NM is rewritten via the wide KV-value splice
// (replaceAuthKVValueWide, dispatched from replaceAuthKVValue when
// payloadUsesWideKVEncoding(clientBody) is true) when the relay forwards a
// wide-encoded client Phase 1 packet upstream.
func TestSendUpstreamAuthPhase1RewritesProgramName_Wide(t *testing.T) {
	t.Parallel()

	clientPipe, serverPipe := net.Pipe()
	defer func() { _ = clientPipe.Close() }()
	defer func() { _ = serverPipe.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	if err := clientPipe.SetDeadline(deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if err := serverPipe.SetDeadline(deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	s := &session{
		ctx:                 context.Background(),
		logger:              slog.New(slog.DiscardHandler),
		upstreamConn:        clientPipe,
		clientAuthPhase1Pkt: &TNSPacket{Payload: widePhase1Payload("connector", "sqlplus")},
	}

	wantProgramName := "dbbat/" + version.Version + " @florent for sqlplus"

	errCh := make(chan error, 1)

	go func() {
		errCh <- s.sendUpstreamAuthPhase1("UPSTREAM_USER", driverIdentity{ProgramName: wantProgramName}, logonModeNoNewPass)
	}()

	pkt, err := readTNSPacket(serverPipe)
	if err != nil {
		t.Fatalf("readTNSPacket: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("sendUpstreamAuthPhase1: %v", err)
	}

	if !payloadUsesWideKVEncoding(pkt.Payload[ttcDataFlagsSize:]) {
		t.Fatalf("forwarded packet lost wide KV encoding")
	}

	if got := findKVByKeyBytesWide(pkt.Payload, []byte(authKeyProgramNM)); got != wantProgramName {
		t.Errorf("forwarded AUTH_PROGRAM_NM = %q, want %q", got, wantProgramName)
	}

	gotUser, err := parseAuthPhase1(pkt.Payload)
	if err != nil {
		t.Fatalf("parseAuthPhase1: %v", err)
	}

	if gotUser != "UPSTREAM_USER" {
		t.Errorf("forwarded username = %q, want UPSTREAM_USER (username swap must still work)", gotUser)
	}
}

// TestSendUpstreamAuthPhase2RewritesProgramName_Wide is the wide-encoding
// counterpart of TestSendUpstreamAuthPhase2RewritesProgramName.
func TestSendUpstreamAuthPhase2RewritesProgramName_Wide(t *testing.T) {
	t.Parallel()

	clientPipe, serverPipe := net.Pipe()
	defer func() { _ = clientPipe.Close() }()
	defer func() { _ = serverPipe.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	if err := clientPipe.SetDeadline(deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	if err := serverPipe.SetDeadline(deadline); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	s := &session{
		ctx:                 context.Background(),
		logger:              slog.New(slog.DiscardHandler),
		upstreamConn:        clientPipe,
		clientAuthPhase2Pkt: &TNSPacket{Payload: widePhase2Payload("connector", "OLDSESS_HEX", "OLDPWD_HEX", "SourceLauncher")},
	}

	sec := &upstreamAuthSecrets{
		encClientSessKey: "NEWSESS_HEX",
		encPassword:      "NEWPWD_HEX",
	}

	wantProgramName := "dbbat/" + version.Version + " @florent for SourceLauncher"

	errCh := make(chan error, 1)

	go func() {
		errCh <- s.sendUpstreamAuthPhase2(
			"UPSTREAM_USER", driverIdentity{ProgramName: wantProgramName}, sec, logonModeNoNewPass|logonModeUserAndPass)
	}()

	pkt, err := readTNSPacket(serverPipe)
	if err != nil {
		t.Fatalf("readTNSPacket: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("sendUpstreamAuthPhase2: %v", err)
	}

	if got := findKVByKeyBytesWide(pkt.Payload, []byte(authKeyProgramNM)); got != wantProgramName {
		t.Errorf("forwarded AUTH_PROGRAM_NM = %q, want %q", got, wantProgramName)
	}

	if got := findKVByKeyBytesWide(pkt.Payload, []byte(authKeySessKey)); got != sec.encClientSessKey {
		t.Errorf("forwarded AUTH_SESSKEY = %q, want %q (secret swap must still work)", got, sec.encClientSessKey)
	}
}

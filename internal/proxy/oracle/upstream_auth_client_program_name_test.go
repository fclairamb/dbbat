package oracle

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/version"
)

// TestOracleDbbatUsername covers both sources session.oracleDbbatUsername can
// draw from: the captured AUTH Phase 1 packet (preferred, available before
// s.user is resolved for OCI/wide-encoding clients) and the s.username
// fallback (set once authenticateClient has run).
func TestOracleDbbatUsername(t *testing.T) {
	t.Parallel()

	t.Run("parses from captured Phase 1 packet", func(t *testing.T) {
		t.Parallel()

		s := &session{
			clientAuthPhase1Pkt: &TNSPacket{Payload: thinPhase1Payload("florent.clairambault", true)},
		}

		if got, want := s.oracleDbbatUsername(), "florent.clairambault"; got != want {
			t.Errorf("oracleDbbatUsername() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to s.username when packet unavailable", func(t *testing.T) {
		t.Parallel()

		s := &session{username: "fallback-user"}

		if got, want := s.oracleDbbatUsername(), "fallback-user"; got != want {
			t.Errorf("oracleDbbatUsername() = %q, want %q", got, want)
		}
	})
}

// TestClientDeclaredProgramName covers extraction of the client's own
// AUTH_PROGRAM_NM value — the "$appName" dbbat intercepts and folds into the
// upstream-facing name.
func TestClientDeclaredProgramName(t *testing.T) {
	t.Parallel()

	t.Run("thin/compressed encoding", func(t *testing.T) {
		t.Parallel()

		pkt := &TNSPacket{Payload: thinPhase1Payload("connector", true)}

		if got, want := clientDeclaredProgramName(pkt), "python"; got != want {
			t.Errorf("clientDeclaredProgramName() = %q, want %q", got, want)
		}
	})

	t.Run("nil packet", func(t *testing.T) {
		t.Parallel()

		if got := clientDeclaredProgramName(nil); got != "" {
			t.Errorf("clientDeclaredProgramName(nil) = %q, want empty", got)
		}
	})
}

// TestBuildUpstreamProgramName confirms the composed AUTH_PROGRAM_NM matches
// the spec's canonical format, combining the dbbat username and the client's
// own declared program name.
func TestBuildUpstreamProgramName(t *testing.T) {
	t.Parallel()

	s := &session{
		clientAuthPhase1Pkt: &TNSPacket{Payload: thinPhase1Payload("florent.clairambault", true)},
	}

	want := "dbbat/" + version.Version + " @florent.clairambault for python"
	if got := s.buildUpstreamProgramName(); got != want {
		t.Errorf("buildUpstreamProgramName() = %q, want %q", got, want)
	}
}

// TestSendUpstreamAuthPhase1RewritesProgramName exercises the production
// relay path: when the client's actual Phase 1 packet is forwarded upstream
// (not the synthetic builder fallback), AUTH_PROGRAM_NM must carry the
// dbbat-branded name, not the client's original value ("python" here).
func TestSendUpstreamAuthPhase1RewritesProgramName(t *testing.T) {
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
		clientAuthPhase1Pkt: &TNSPacket{Payload: thinPhase1Payload("connector", true)},
	}

	wantProgramName := "dbbat/" + version.Version + " @florent for python"

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

	if got := findKVByKeyBytes(pkt.Payload, []byte(authKeyProgramNM)); got != wantProgramName {
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

// TestSendUpstreamAuthPhase2RewritesProgramName is the Phase 2 counterpart of
// TestSendUpstreamAuthPhase1RewritesProgramName.
func TestSendUpstreamAuthPhase2RewritesProgramName(t *testing.T) {
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

	pairs := []phase2KVForTest{
		{"AUTH_SESSKEY", "OLDSESS_HEX", 1},
		{"AUTH_PASSWORD", "OLDPWD_HEX", 0},
		{"AUTH_TERMINAL", "unknown", 0},
		{"AUTH_PROGRAM_NM", "SourceLauncher", 0},
	}
	body := buildPhase2Body(t, "connector", pairs, true /* CLR */, 0x00)
	payload := append([]byte{0x00, 0x00}, body...) // TNS data flags prefix

	s := &session{
		ctx:                 context.Background(),
		logger:              slog.New(slog.DiscardHandler),
		upstreamConn:        clientPipe,
		clientAuthPhase2Pkt: &TNSPacket{Payload: payload},
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

	if got := findKVByKeyBytes(pkt.Payload, []byte(authKeyProgramNM)); got != wantProgramName {
		t.Errorf("forwarded AUTH_PROGRAM_NM = %q, want %q", got, wantProgramName)
	}

	if got := findKVByKeyBytes(pkt.Payload, []byte(authKeySessKey)); got != sec.encClientSessKey {
		t.Errorf("forwarded AUTH_SESSKEY = %q, want %q (secret swap must still work)", got, sec.encClientSessKey)
	}
}

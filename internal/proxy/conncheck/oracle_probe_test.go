package conncheck

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// tnsRefuseListener is a minimal TNS listener: it reads the client's Connect
// packet and answers with a Refuse packet carrying oraCode.
//
// That is a real listener-side rejection, and it is enough to drive the whole
// probe — go-ora's DSN parsing, the injected transport, the Connect exchange
// and the error classification — without an Oracle instance. Cases that only a
// live server can produce (a server-side ORA-01017 after the O5LOGON exchange)
// are covered by the integration tests behind -tags integration.
func tnsRefuseListener(t *testing.T, oraCode int) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tns listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				defer func() { _ = c.Close() }()

				if err := readTNSPacket(c); err != nil {
					return
				}

				_, _ = c.Write(tnsRefusePacket(oraCode))
			}(conn)
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// readTNSPacket consumes one pre-handshake TNS packet (2-byte big-endian length
// in the first field of an 8-byte header, then the body).
func readTNSPacket(c net.Conn) error {
	header := make([]byte, 8)
	if _, err := io.ReadFull(c, header); err != nil {
		return err
	}

	total := int(binary.BigEndian.Uint16(header[0:2]))
	if total < 8 {
		return errors.New("short tns packet")
	}

	_, err := io.ReadFull(c, make([]byte, total-8))

	return err
}

// tnsRefusePacket builds a Refuse packet (type 4) whose descriptor carries
// (ERR=<code>) — the field go-ora parses into the ORA error it returns.
func tnsRefusePacket(oraCode int) []byte {
	msg := []byte(fmt.Sprintf(
		"(DESCRIPTION=(TMP=)(VSNNUM=0)(ERR=%d)(ERROR_STACK=(ERROR=(CODE=%d)(EMFI=4))))",
		oraCode, oraCode,
	))

	pkt := make([]byte, 12, 12+len(msg))
	pkt[4] = 4 // REFUSE
	pkt[8] = 1 // user reason
	pkt[9] = 1 // system reason
	binary.BigEndian.PutUint16(pkt[10:12], uint16(len(msg)))
	pkt = append(pkt, msg...)
	binary.BigEndian.PutUint16(pkt[0:2], uint16(len(pkt)))

	return pkt
}

// newOracleTarget builds an Oracle server row pointing at host:port.
func newOracleTarget(host string, port int) *store.Server {
	service := "ORCLPDB1"

	return &store.Server{
		UID: uuid.New(), Host: host, Port: port,
		Protocol: store.ProtocolOracle, Username: "scott", Password: "tiger",
		DatabaseName: "ORCLPDB1", OracleServiceName: &service,
	}
}

// TestCheck_OracleTarget_AuthRejected is the spec's core case: credentials the
// target refuses must land on target_auth / db_auth_failed, not on a vague
// "reachable, credentials not verified".
func TestCheck_OracleTarget_AuthRejected(t *testing.T) {
	t.Parallel()

	ln := tnsRefuseListener(t, 1017)
	host, port := splitHostPort(t, ln.Addr().String())

	checker := New(newFakeResolver(), testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), newOracleTarget(host, port))

	if res.OK {
		t.Fatal("Check() ok = true, want an Oracle auth rejection")
	}

	if res.Stage != StageTargetAuth || res.Code != CodeDBAuthFailed {
		t.Errorf("stage/code = %s/%s, want %s/%s (msg=%s)",
			res.Stage, res.Code, StageTargetAuth, CodeDBAuthFailed, res.Message)
	}
}

// TestCheck_OracleTarget_ServiceUnknown pins the other half of the
// classification: a listener that does not know the service name is a
// handshake problem (wrong service/port), not a credentials problem.
func TestCheck_OracleTarget_ServiceUnknown(t *testing.T) {
	t.Parallel()

	ln := tnsRefuseListener(t, 12514)
	host, port := splitHostPort(t, ln.Addr().String())

	checker := New(newFakeResolver(), testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), newOracleTarget(host, port))

	if res.OK {
		t.Fatal("Check() ok = true, want a handshake failure")
	}

	if res.Stage != StageTargetAuth || res.Code != CodeDBHandshakeFailed {
		t.Errorf("stage/code = %s/%s, want %s/%s (msg=%s)",
			res.Stage, res.Code, StageTargetAuth, CodeDBHandshakeFailed, res.Message)
	}
}

// TestCheck_OracleTarget_NotOracle proves Oracle no longer stops at
// reachability: a host that answers TCP but speaks no TNS must fail at
// target_auth rather than pass with auth_not_verified.
func TestCheck_OracleTarget_NotOracle(t *testing.T) {
	t.Parallel()

	echo := startEchoTarget(t)
	host, port := splitHostPort(t, echo.Addr().String())

	checker := New(newFakeResolver(), testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), newOracleTarget(host, port))

	if res.OK {
		t.Fatal("Check() ok = true, want a database handshake failure")
	}

	if res.Stage != StageTargetAuth {
		t.Errorf("stage = %s, want %s (code=%s msg=%s)", res.Stage, StageTargetAuth, res.Code, res.Message)
	}

	if res.Code == CodeUnsupported {
		t.Error("code = auth_not_verified: Oracle must now be probed, not just dialed")
	}
}

// TestCheck_OracleTarget_ThroughTunnel is the regression guard for the deadline
// shim: go-ora arms a (zero) deadline before every read and write, and an SSH
// channel rejects SetDeadline outright — without the shim every tunneled Oracle
// check would fail on the transport rather than on the credentials.
func TestCheck_OracleTarget_ThroughTunnel(t *testing.T) {
	t.Parallel()

	pk, pub := genClientKey(t)
	sshLn := startFakeSSHServer(t, pub)
	bastionHost, bastionPort := splitHostPort(t, sshLn.Addr().String())

	ln := tnsRefuseListener(t, 1017)
	host, port := splitHostPort(t, ln.Addr().String())

	resolver := newFakeResolver()
	bastion := newBastion(resolver, bastionHost, bastionPort, pk)

	target := newOracleTarget(host, port)
	target.ViaUID = &bastion.UID

	checker := New(resolver, testKey())
	checker.timeout = 10 * time.Second

	res := checker.Check(context.Background(), target)

	if res.OK {
		t.Fatal("Check() ok = true, want an Oracle auth rejection through the tunnel")
	}

	if res.Stage != StageTargetAuth || res.Code != CodeDBAuthFailed {
		t.Errorf("stage/code = %s/%s, want %s/%s (msg=%s)",
			res.Stage, res.Code, StageTargetAuth, CodeDBAuthFailed, res.Message)
	}
}

// TestIsDBAuthRejection_OracleCodes pins the two renderings go-ora produces for
// the same condition — the server's zero-padded ORA-01017 and go-ora's own
// unpadded ORA-1017 — plus the listener codes that must NOT be read as auth
// failures.
func TestIsDBAuthRejection_OracleCodes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		msg  string
		want bool
	}{
		"server ORA-01017":       {"ORA-01017: invalid username/password; logon denied", true},
		"go-ora ORA-1017":        {"ORA-1017", true},
		"expired password":       {"ORA-28001: the password has expired", true},
		"account locked":         {"ORA-28000: the account is locked", true},
		"no create session":      {"ORA-01045: user SCOTT lacks CREATE SESSION privilege; logon denied", true},
		"insufficient privilege": {"ORA-01031: insufficient privileges", true},
		"unknown service":        {"ORA-12514: TNS:listener does not currently know of service requested", false},
		"no listener":            {"ORA-12541: TNS:no listener", false},
		"connection refused TNS": {"ORA-12564: TNS connection refused", false},
		"longer code is not 1017": {
			"ORA-10171: some unrelated internal error", false,
		},
	} {
		if got := isDBAuthRejection(errors.New(tc.msg)); got != tc.want {
			t.Errorf("%s: isDBAuthRejection(%q) = %v, want %v", name, tc.msg, got, tc.want)
		}
	}
}

// TestCheck_OracleTarget_NoServiceName covers the row that cannot be probed at
// all: no service name and no database name. Nothing is dialed, so the answer
// must point at the empty field, not at the network.
func TestCheck_OracleTarget_NoServiceName(t *testing.T) {
	t.Parallel()

	ln := tnsRefuseListener(t, 1017)
	host, port := splitHostPort(t, ln.Addr().String())

	target := newOracleTarget(host, port)
	target.OracleServiceName = nil
	target.DatabaseName = ""

	checker := New(newFakeResolver(), testKey())
	checker.timeout = 5 * time.Second

	res := checker.Check(context.Background(), target)

	if res.OK {
		t.Fatal("Check() ok = true, want a config failure")
	}

	if res.Stage != StageConfig || res.Code != CodeMissingConfig {
		t.Errorf("stage/code = %s/%s, want %s/%s (msg=%s)",
			res.Stage, res.Code, StageConfig, CodeMissingConfig, res.Message)
	}
}

// TestCheck_OracleTarget_ServiceNameFallsBackToDatabase pins the resolution
// order the proxy uses: the dedicated column wins, the database name is the
// fallback.
func TestCheck_OracleTarget_ServiceNameFallsBackToDatabase(t *testing.T) {
	t.Parallel()

	explicit := "EXPLICIT"

	for name, tc := range map[string]struct {
		srv  *store.Server
		want string
	}{
		"explicit service wins": {
			&store.Server{OracleServiceName: &explicit, DatabaseName: "DBNAME"}, "EXPLICIT",
		},
		"database name is the fallback": {
			&store.Server{DatabaseName: "DBNAME"}, "DBNAME",
		},
		"empty service falls back too": {
			&store.Server{OracleServiceName: new(string), DatabaseName: "DBNAME"}, "DBNAME",
		},
		"nothing set": {
			&store.Server{}, "",
		},
	} {
		if got := oracleServiceName(tc.srv); got != tc.want {
			t.Errorf("%s: oracleServiceName() = %q, want %q", name, got, tc.want)
		}
	}
}

// TestProbeFor_OracleHasAProbe is the small structural guard: the spec's whole
// point is that Oracle stops falling through to the no-probe branch.
func TestProbeFor_OracleHasAProbe(t *testing.T) {
	t.Parallel()

	if probeFor(store.ProtocolOracle) == nil {
		t.Fatal("probeFor(oracle) = nil, want a login probe")
	}
}

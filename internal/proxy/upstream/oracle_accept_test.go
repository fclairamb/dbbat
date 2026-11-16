package upstream

import (
	"bufio"
	"context"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// loadAccept18c decodes testdata/accept_18c.hex: '#' lines are comments,
// whitespace is ignored.
func loadAccept18c(t *testing.T) []byte {
	t.Helper()

	f, err := os.Open("testdata/accept_18c.hex")
	if err != nil {
		t.Fatalf("open accept fixture: %v", err)
	}

	defer func() { _ = f.Close() }()

	var digits strings.Builder

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}

	raw, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("decode accept fixture: %v", err)
	}

	if len(raw) != 41 || int(binary.BigEndian.Uint16(raw)) != len(raw) {
		t.Fatalf("accept fixture is %d bytes (header says %d), want 41", len(raw), binary.BigEndian.Uint16(raw))
	}

	return raw
}

// fakeTNSListener accepts one connection, reads the client's Connect (and the
// separate connect-data packet, if any), answers with accept, then drains
// whatever the client sends next until it hangs up or a deadline passes.
func fakeTNSListener(t *testing.T, accept []byte) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}

		if _, err := io.ReadFull(conn, make([]byte, int(binary.BigEndian.Uint16(header))-len(header))); err != nil {
			return
		}

		// A long connect descriptor travels in a second packet: drain it.
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, _ = io.Copy(io.Discard, conn)

		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(accept); err != nil {
			return
		}

		// Read the next request, then hang up without answering it.
		_, _ = conn.Read(make([]byte, 4096))
	}()

	return ln.Addr().String()
}

// TestConnectOracleSurvives18cAccept is issue #500: go-ora v3.0.1 indexed
// [41:45] of Oracle 18c XE's 41-byte Accept and panicked the whole process.
// With the patched go-ora the Accept parses and the attempt fails on the
// listener hanging up afterwards: an error, no panic, and not the recovered
// driver panic either (which would mean the unpatched go-ora is back).
func TestConnectOracleSurvives18cAccept(t *testing.T) {
	t.Parallel()

	addr := fakeTNSListener(t, loadAccept18c(t))

	host, _, _ := net.SplitHostPort(addr)

	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer

		return d.DialContext(ctx, "tcp", addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := ConnectOracle(ctx, dial, OracleConfig{
		Host:        host,
		Port:        1521,
		ServiceName: "XEPDB1",
		Username:    "acme",
		Password:    "secret",
		ProgramName: "dbbat-test",
		SSLMode:     "disable",
	})
	if err == nil {
		_ = conn.Close()

		t.Fatal("ConnectOracle succeeded against a listener that hangs up after the Accept")
	}

	if errors.Is(err, ErrOracleDriverPanic) {
		t.Fatalf("go-ora panicked on the 18c Accept (is the go-ora replace gone?): %v", err)
	}
}

// panickingConnector stands in for a go-ora bug.
type panickingConnector struct{}

func (panickingConnector) Connect(context.Context) (driver.Conn, error) {
	panic("runtime error: index out of range [3] with length 0") // like #500
}

func (panickingConnector) Driver() driver.Driver { return nil }

// TestRecoveringOracleConnectorTurnsPanicIntoError is the defense in depth: a
// driver panic during login comes back as ErrOracleDriverPanic.
func TestRecoveringOracleConnectorTurnsPanicIntoError(t *testing.T) {
	t.Parallel()

	conn, err := RecoveringOracleConnector(panickingConnector{}).Connect(context.Background())
	if conn != nil {
		t.Fatalf("conn = %v, want nil", conn)
	}

	if !errors.Is(err, ErrOracleDriverPanic) {
		t.Fatalf("err = %v, want ErrOracleDriverPanic", err)
	}
}

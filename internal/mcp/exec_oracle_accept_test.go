package mcp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/fclairamb/dbbat/internal/proxy/upstream"
)

// accept18c is the 41-byte Accept of Oracle 18c XE that made go-ora v3.0.1
// panic (issue #500); internal/proxy/upstream/testdata/accept_18c.hex
// documents the layout.
func accept18c() []byte {
	pkt := make([]byte, 41)
	binary.BigEndian.PutUint16(pkt[0:], 41)
	pkt[4] = 2                                   // ACCEPT
	binary.BigEndian.PutUint16(pkt[8:], 315)     // version
	binary.BigEndian.PutUint16(pkt[10:], 0x0801) // options
	binary.BigEndian.PutUint16(pkt[12:], 0x2000) // SDU
	binary.BigEndian.PutUint16(pkt[14:], 0xffff) // TDU
	binary.BigEndian.PutUint16(pkt[16:], 0x7f08) // NT characteristics
	binary.BigEndian.PutUint16(pkt[20:], 41)     // data offset
	binary.BigEndian.PutUint32(pkt[32:], 0x2000) // 32-bit SDU
	binary.BigEndian.PutUint32(pkt[36:], 0xffff) // 32-bit TDU

	return pkt
}

// TestExecuteOracleSurvives18cAccept: the MCP executor's go-ora login against
// a listener answering the 18c Accept and then hanging up fails the execution
// with an error instead of panicking.
func TestExecuteOracleSurvives18cAccept(t *testing.T) {
	t.Parallel()

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

		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, _ = io.Copy(io.Discard, conn)

		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(accept18c()); err != nil {
			return
		}

		_, _ = conn.Read(make([]byte, 4096))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = executeOracle(ctx, ln.Addr().String(), ExecRequest{
		Database: "acme",
		Username: "acme",
		APIKey:   "secret",
		SQL:      "SELECT 1 FROM dual",
	})
	if err == nil {
		t.Fatal("executeOracle succeeded against a listener that hangs up after the Accept")
	}

	if errors.Is(err, upstream.ErrOracleDriverPanic) {
		t.Fatalf("go-ora panicked on the 18c Accept: %v", err)
	}
}

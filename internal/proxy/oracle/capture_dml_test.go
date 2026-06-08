//go:build capture

// Capture tooling for regenerating testdata dump fixtures from a live Oracle.
//
// Usage:
//
//	docker run -d --name dbbat-ora-cap -p 51521:1521 -e ORACLE_PASSWORD=oracle gvenzl/oracle-free:23-slim
//	# wait for "DATABASE IS READY TO USE!" in docker logs
//	go test -tags capture -timeout 120s -run TestCapture_GoOraDML -v ./internal/proxy/oracle/
//
// The capture relays TNS packets between a go-ora client and the Oracle
// container, recording every packet to testdata/go_ora_dml.dbbat-dump.
// Override the Oracle address with ORACLE_ADDR, the service name with
// ORACLE_SERVICE (default FREEPDB1), and the output path with CAPTURE_OUT.
package oracle

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

func captureEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

// relayTNS forwards TNS packets in one direction, recording each to the dump.
func relayTNS(t *testing.T, src, dst net.Conn, w *dump.Writer, dir byte, done chan<- struct{}) {
	t.Helper()

	defer func() { done <- struct{}{} }()

	for {
		pkt, err := readTNSPacket(src)
		if err != nil {
			return // EOF or closed connection ends the relay
		}

		if err := w.WritePacket(dir, pkt.Raw); err != nil {
			t.Logf("dump write error: %v", err)
		}

		if err := writeTNSPacket(dst, pkt); err != nil {
			return
		}
	}
}

// startCaptureRelay stands up a recording TNS relay in front of oracleAddr and
// returns the local host:port a client should dial. Every packet is forwarded
// to Oracle and recorded (both directions) to w. The listener is closed on test
// cleanup; the caller owns w and must Close it once the session has drained.
func startCaptureRelay(t *testing.T, oracleAddr string, w *dump.Writer) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			clientConn, err := listener.Accept()
			if err != nil {
				return
			}

			upstreamConn, err := net.Dial("tcp", oracleAddr)
			if err != nil {
				_ = clientConn.Close()
				return
			}

			done := make(chan struct{}, 2)
			go relayTNS(t, clientConn, upstreamConn, w, dump.DirClientToServer, done)
			go relayTNS(t, upstreamConn, clientConn, w, dump.DirServerToClient, done)
			<-done
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			<-done
		}
	}()

	return listener.Addr().String()
}

// TestCapture_GoOraLargeResult records a go-ora session fetching a multi-packet
// result set in a single array fetch (PREFETCH_ROWS forces the whole set into
// one execute response that spans many TNS Data packets). It is the fixture for
// multi-packet row-capture parsing — see TestDumpReplay_LargeResultRows.
func TestCapture_GoOraLargeResult(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_LARGE", "testdata/go_ora_largeresult.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-largeresult",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	// PREFETCH_ROWS bigger than the row count makes go-ora fetch the whole set in
	// one execute, producing a QRESULT + a run of CONTINUATION packets.
	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	query := fmt.Sprintf(
		"SELECT LEVEL AS id, RPAD('v'||LEVEL, 60, '.') AS payload FROM dual CONNECT BY LEVEL <= %d",
		largeResultRows)

	rows, err := db.QueryContext(ctx, query)
	require.NoError(t, err)

	got := 0

	for rows.Next() {
		var (
			id      int
			payload string
		)

		require.NoError(t, rows.Scan(&id, &payload))

		got++

		require.Equal(t, got, id, "ids must be sequential 1..N")
		require.Equal(t, largeResultPayload(id), payload, "payload for row %d", id)
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, largeResultRows, got, "ground truth: all rows fetched by the driver")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d rows)", outPath, got)
}

// TestCapture_GoOraDML records a go-ora session with DDL + DML statements.
// The resulting dump is the fixture for DML row-count response parsing.
func TestCapture_GoOraDML(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT", "testdata/go_ora_dml.dbbat-dump")

	// Probe Oracle availability before setting anything up.
	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-dml",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer func() { _ = listener.Close() }()

	// Relay loop: accept client connections serially, pipe to Oracle.
	go func() {
		for {
			clientConn, err := listener.Accept()
			if err != nil {
				return
			}

			upstreamConn, err := net.Dial("tcp", oracleAddr)
			if err != nil {
				_ = clientConn.Close()
				return
			}

			done := make(chan struct{}, 2)
			go relayTNS(t, clientConn, upstreamConn, w, dump.DirClientToServer, done)
			go relayTNS(t, upstreamConn, clientConn, w, dump.DirServerToClient, done)
			<-done
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			<-done
		}
	}()

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s", listener.Addr(), oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1) // keep a single session in the dump

	ctx := t.Context()

	exec := func(query string, wantAffected int64) {
		t.Helper()

		res, err := db.ExecContext(ctx, query)
		require.NoError(t, err, "exec: %s", query)

		affected, err := res.RowsAffected()
		require.NoError(t, err)
		t.Logf("rows_affected=%d (want %d): %s", affected, wantAffected, query)
	}

	_, _ = db.ExecContext(ctx, "DROP TABLE dbbat_dml_test") // ignore ORA-00942

	exec("CREATE TABLE dbbat_dml_test (id NUMBER, name VARCHAR2(50))", 0)
	exec("INSERT INTO dbbat_dml_test VALUES (100, 'single')", 1)
	exec("INSERT INTO dbbat_dml_test SELECT LEVEL, 'row'||LEVEL FROM dual CONNECT BY LEVEL <= 5", 5)
	exec("UPDATE dbbat_dml_test SET name = 'updated' WHERE id <= 3", 3)
	exec("DELETE FROM dbbat_dml_test WHERE id IN (4, 5)", 2)

	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbbat_dml_test").Scan(&count))
	t.Logf("remaining rows: %d", count)

	exec("DROP TABLE dbbat_dml_test", 0)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

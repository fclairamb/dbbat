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

// compressedRowsQuery returns a result set that exercises column compression:
//   - GRP runs "AAA" for rows 1-4 then "BBB" for 5-8, so the server omits it on
//     unchanged rows and the proxy must carry the previous value forward.
//   - NUM changes every row.
//   - OPT is NULL on every third row (wire length 0).
//
// Column aliases are ≥2 chars on purpose — the v315 column scanner drops
// single-char names, which corrupts the column count (tracked separately).
const compressedRowsQuery = "SELECT " +
	"CASE WHEN LEVEL <= 4 THEN 'AAA' ELSE 'BBB' END AS grp, " +
	"LEVEL AS num, " +
	"CASE WHEN MOD(LEVEL, 3) = 0 THEN NULL ELSE 'x' || LEVEL END AS opt " +
	"FROM dual CONNECT BY LEVEL <= 8"

// TestCapture_GoOraCompressedRows records a result set crafted to exercise the
// column-compression carry-forward path: GRP repeats in long runs (the server
// omits it on unchanged rows, so the proxy must keep the previous value), N
// changes every row, and OPT is NULL on every third row. See compressedRows for
// the ground truth and TestDumpReplay_CompressedRows for the replay assertions.
func TestCapture_GoOraCompressedRows(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_COMPRESSED", "testdata/go_ora_compressed.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-compressed",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	rows, err := db.QueryContext(ctx, compressedRowsQuery)
	require.NoError(t, err)

	want := compressedRows()
	got := 0

	for rows.Next() {
		var (
			grp string
			n   int
			opt sql.NullString
		)

		require.NoError(t, rows.Scan(&grp, &n, &opt))

		require.Lessf(t, got, len(want), "more rows than ground truth at n=%d", n)

		exp := want[got]
		require.Equal(t, exp.grp, grp, "grp at row %d", got)
		require.Equal(t, got+1, n, "n must be sequential")
		require.Equal(t, exp.opt, opt.String, "opt at row %d", got) // NullString.String is "" when NULL

		got++
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, want, got, "ground truth row count")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d rows)", outPath, got)
}

// colCountQuery has two columns the name scanner cannot detect: N is a single
// char, and LEVEL*10 is an unnamed expression (Oracle names it "LEVEL*10",
// not a valid identifier). Before the describe-header column count was used,
// scanColumnNames returned 0 columns and no rows were captured at all.
const colCountQuery = "SELECT LEVEL AS n, LEVEL * 10 FROM dual CONNECT BY LEVEL <= 4"

// TestCapture_GoOraColCount records colCountQuery — the fixture proving the
// proxy captures rows for queries whose column names the scanner can't detect.
func TestCapture_GoOraColCount(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_COLCOUNT", "testdata/go_ora_colcount.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-colcount",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	rows, err := db.QueryContext(ctx, colCountQuery)
	require.NoError(t, err)

	got := 0

	for rows.Next() {
		var n, tenN int

		require.NoError(t, rows.Scan(&n, &tenN))

		got++

		require.Equal(t, got, n, "n must be sequential")
		require.Equal(t, got*10, tenN, "second column is n*10")
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 4, got, "ground truth row count")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s (%d rows)", outPath, got)
}

// numbersQuery returns one row of positive NUMBERs that exercise the decimal
// decoder: a 2-dp value, a large value with a fraction, an integer that needs
// trailing-zero expansion, and a sub-1 fraction (leading-zero placement).
const numbersQuery = "SELECT 3.14 AS pi, 1234567.89 AS amount, 1000000 AS million, 0.25 AS quarter FROM dual"

// TestCapture_GoOraNumbers records numbersQuery — the fixture proving the proxy
// decodes fractional NUMBERs correctly (previously decimals lost their point,
// e.g. 3.14 was captured as "314").
func TestCapture_GoOraNumbers(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_NUMBERS", "testdata/go_ora_numbers.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-numbers",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	var pi, amount, million, quarter float64

	row := db.QueryRowContext(ctx, numbersQuery)
	require.NoError(t, row.Scan(&pi, &amount, &million, &quarter))

	// Ground truth: the driver's typed values match our expected strings.
	require.InDelta(t, 3.14, pi, 1e-9)
	require.InDelta(t, 1234567.89, amount, 1e-9)
	require.InDelta(t, 1000000, million, 1e-9)
	require.InDelta(t, 0.25, quarter, 1e-9)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// temporalQuery returns one row of temporal types: a DATE, a TIMESTAMP with
// fractional seconds, and a TIMESTAMP WITH TIME ZONE at a numeric offset.
const temporalQuery = "SELECT " +
	"DATE '2024-03-15' AS dt, " +
	"TIMESTAMP '2024-03-15 14:30:45.123456' AS ts, " +
	"FROM_TZ(TIMESTAMP '2024-03-15 14:30:45.123456', '+05:30') AS tstz " +
	"FROM dual"

// TestCapture_GoOraTemporal records temporalQuery — the fixture verifying that
// DATE/TIMESTAMP/TIMESTAMP WITH TIME ZONE decode in the row-capture pipeline.
func TestCapture_GoOraTemporal(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_TEMPORAL", "testdata/go_ora_temporal.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-temporal",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	var dt, ts, tstz time.Time

	row := db.QueryRowContext(ctx, temporalQuery)
	require.NoError(t, row.Scan(&dt, &ts, &tstz))

	// Ground truth: the driver's typed values are what we encoded.
	require.Equal(t, "2024-03-15", dt.Format("2006-01-02"))
	require.Equal(t, "2024-03-15 14:30:45.123456", ts.Format("2006-01-02 15:04:05.999999"))
	require.Equal(t, "2024-03-15 14:30:45", tstz.Format("2006-01-02 15:04:05"))
	_, offset := tstz.Zone()
	require.Equal(t, 5*3600+30*60, offset, "tz offset +05:30")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// mixedQuery returns six rows combining everything the row decoder must handle
// together: an incrementing id, a repeated GRP column (compressed away on
// unchanged rows), a fractional amount, and a DATE that is NULL on even rows.
const mixedQuery = "SELECT " +
	"LEVEL AS id, " +
	"CASE WHEN LEVEL <= 3 THEN 100 ELSE 200 END AS grp, " +
	"LEVEL + 0.5 AS amount, " +
	"CASE WHEN MOD(LEVEL, 2) = 0 THEN NULL ELSE DATE '2024-01-01' + LEVEL END AS maybe_day " +
	"FROM dual CONNECT BY LEVEL <= 6"

// TestCapture_GoOraMixed records mixedQuery — a combined NUMBER/compression/
// NULL/DATE row-capture fixture (see TestDumpReplay_Mixed).
func TestCapture_GoOraMixed(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_MIXED", "testdata/go_ora_mixed.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-mixed",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	rows, err := db.QueryContext(ctx, mixedQuery)
	require.NoError(t, err)

	got := 0

	for rows.Next() {
		var (
			id     int
			grp    int
			amount float64
			day    sql.NullTime
		)

		require.NoError(t, rows.Scan(&id, &grp, &amount, &day))

		got++

		want := mixedExpectedRow(got)
		require.Equal(t, want.id, id)
		require.Equal(t, want.grp, grp)
		require.InDelta(t, want.amount, amount, 1e-9)
		require.Equal(t, want.dayNull, !day.Valid, "NULL on even rows")

		if day.Valid {
			require.Equal(t, want.day[:10], day.Time.Format("2006-01-02"))
		}
	}

	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 6, got, "ground truth row count")

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
}

// negNumbersQuery returns a row of negative and positive NUMBERs. Negative
// NUMBERs encode to bytes that all fall in the printable-ASCII range, so the
// type-less heuristic (ASCII first) captured them as garbage text; type-aware
// decoding (the column type is NUMBER) renders them correctly.
const negNumbersQuery = "SELECT -42 AS neg, -3.14 AS negdec, 100 AS pos, -1000000 AS bignum FROM dual"

// TestCapture_GoOraNegNumbers records negNumbersQuery (see TestDumpReplay_NegNumbers).
func TestCapture_GoOraNegNumbers(t *testing.T) {
	oracleAddr := captureEnv("ORACLE_ADDR", "localhost:51521")
	oracleService := captureEnv("ORACLE_SERVICE", "FREEPDB1")
	outPath := captureEnv("CAPTURE_OUT_NEGNUM", "testdata/go_ora_negnumbers.dbbat-dump")

	probe, err := net.DialTimeout("tcp", oracleAddr, 2*time.Second)
	if err != nil {
		t.Skipf("Oracle not reachable at %s: %v", oracleAddr, err)
	}
	_ = probe.Close()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: "capture-go-ora-negnumbers",
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	relayAddr := startCaptureRelay(t, oracleAddr, w)

	dsn := fmt.Sprintf("oracle://system:oracle@%s/%s?PREFETCH_ROWS=1000", relayAddr, oracleService)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	ctx := t.Context()

	var neg, negdec, pos, bignum float64

	row := db.QueryRowContext(ctx, negNumbersQuery)
	require.NoError(t, row.Scan(&neg, &negdec, &pos, &bignum))

	require.InDelta(t, -42, neg, 1e-9)
	require.InDelta(t, -3.14, negdec, 1e-9)
	require.InDelta(t, 100, pos, 1e-9)
	require.InDelta(t, -1000000, bignum, 1e-9)

	require.NoError(t, db.Close())
	time.Sleep(500 * time.Millisecond) // let the relay drain the final packets
	require.NoError(t, w.Close())

	t.Logf("capture written to %s", outPath)
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

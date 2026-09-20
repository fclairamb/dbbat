//go:build capture || integration

// The recording relay and the fixture distillation behind every OCI hex
// fixture in testdata/, shared by the two places one can be recorded from.
//
// There are two, and the difference is not cosmetic. The standalone capture
// harness (capture_refcursor_test.go) puts a bare relay in front of an Oracle
// container, which is the cheapest way to record what a client marshals. The
// live suite (capture_oci_fixtures_integration_test.go) puts the same relay in
// front of **dbbat**, which is the only way to record what dbbat *reads* — and
// on the 64-bit OCI dialect those are not the same bytes. See
// ociFixtureProvenance.
package oracle

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// The 4-byte OCI dialect's fixtures: the Instant Client's call responses, the
// `PRINT rc` frames recorded beside them, and the scalar-out-bind negative.
const (
	ociRefCursorBindOutputFixture = "testdata/oci_refcursor_bind_output.hex"
	ociRefCursorDrivesFixture     = "testdata/oci_refcursor_drives.hex"
	ociScalarOutBindFixture       = "testdata/oci_scalar_outbind_bind_output.hex"
)

// The 64-bit dialect's own three. They are a separate set rather than a
// regenerated one because the two dialects are two bodies of evidence: a walk
// asked to fit one file recorded from either client is a walk with two chances
// to produce a plausible number, which is the failure mode refcursor_bind.go is
// bounded against. See isCloseCursorsWide8Header.
const (
	oci64RefCursorBindOutputFixture = "testdata/oci64_refcursor_bind_output.hex"
	oci64RefCursorDrivesFixture     = "testdata/oci64_refcursor_drives.hex"
	oci64ScalarOutBindFixture       = "testdata/oci64_scalar_outbind_bind_output.hex"

	// oci64ParseExecsFixture is the negative half of the drive reading: the
	// client frames of the same session that **do** carry a statement. A
	// re-execution reading that answered on one of these would gate a parse
	// against whatever four bytes sit where the cursor id goes, and the visible
	// result is an ORA-01031 on work that used to run.
	oci64ParseExecsFixture = "testdata/oci64_parse_execs.hex"
)

// ociFixtureProvenance is why the 64-bit fixtures are recorded through dbbat
// while the 4-byte ones were recorded through a bare relay, and it is a finding
// rather than a preference.
//
// The 64-bit op header carries an 8-byte field at [9..17] that
// `usesWide64OpHeader` validates as "this header's own sequence plus one" — the
// sequence the next TTC message will carry. Measured 2026-09-20 against sqlplus
// 23.26: through **dbbat** it is exactly that, on every frame, as the fixtures
// recorded for the bundled-client hang already showed
// (testdata/oci_bundled_close_cursors.hex). Through a **bare relay** to the same
// server, the same client writes its own sequence there instead — the session
// is one TTC message shorter, because dbbat's AUTH rewrite makes the client do
// the two-phase O5LOGON it skips when it talks to the server directly.
//
// That one byte decides everything downstream: without it the close-cursors
// list does not decode, so the exec stapled behind it is never found, so a
// drives fixture recorded off a bare relay cannot be replayed through the code
// it is meant to pin. Hence the live route. The 4-byte dialect has no such
// problem — its `[0x01][seq+1]` pad holds either way — which is why its
// fixtures were never affected and are not re-recorded here.
const ociFixtureProvenance = "recorded through dbbat, not through a bare relay: see ociFixtureProvenance"

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

// startCaptureRelay stands up a recording TNS relay in front of targetAddr and
// returns the local host:port a client should dial. Every packet is forwarded
// and recorded (both directions) to w. The listener is closed on test cleanup;
// the caller owns w and must Close it once the session has drained.
func startCaptureRelay(t *testing.T, targetAddr string, w *dump.Writer) string {
	t.Helper()

	return startCaptureRelayOn(t, "127.0.0.1", targetAddr, w)
}

// startCaptureRelayOn is startCaptureRelay with the bind address spelled out.
//
// It exists for the one client that cannot reach a loopback listener: the
// sqlplus bundled in the Oracle image, which dials back out of the container
// over host.docker.internal and therefore needs the relay bound the way a real
// proxy would be. Everything else keeps loopback.
func startCaptureRelayOn(t *testing.T, bindHost, targetAddr string, w *dump.Writer) string {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			clientConn, err := listener.Accept()
			if err != nil {
				return
			}

			upstreamConn, err := net.Dial("tcp", targetAddr)
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

// newCaptureWriter opens a dump writer for a capture, failing the test if the
// path cannot be written.
func newCaptureWriter(t *testing.T, outPath, sessionID string) *dump.Writer {
	t.Helper()

	w, err := dump.NewWriter(outPath, dump.Header{
		SessionID: sessionID,
		Protocol:  dump.ProtocolOracle,
		StartTime: time.Now(),
	}, 32*1024*1024)
	require.NoError(t, err)

	return w
}

// recordedDialectIsWide64 reports which of the two OCI dialects the recorded
// client actually spoke, read off its own frames rather than off which side of
// the container boundary it ran on — the gvenzl images bundle different client
// versions, so the flavor is not the dialect (the same rule the live suite's
// ociSessionSpeaksWide64 follows).
//
// The test is usesWide64OpHeader over the session's client frames, which is the
// same reading production keys its dialect on. A recording it finds nothing in
// is the 4-byte dialect — or a 64-bit session recorded somewhere dbbat is not,
// which is exactly what ociFixtureProvenance is about.
func recordedDialectIsWide64(t *testing.T, dumpPath string) bool {
	t.Helper()

	found := false

	eachRecordedTNSPayload(t, dumpPath, func(clientToServer bool, payload []byte) {
		if clientToServer && usesWide64OpHeader(extractTTCPayload(payload)) {
			found = true
		}
	})

	return found
}

// eachRecordedTNSPayload calls visit with every TNS Data payload in a recording,
// in wire order.
func eachRecordedTNSPayload(t *testing.T, dumpPath string, visit func(clientToServer bool, payload []byte)) {
	t.Helper()

	r, err := dump.OpenReader(dumpPath)
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}

		require.NoError(t, err)

		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData {
			continue
		}

		visit(pkt.Direction == dump.DirClientToServer, tns.Payload)
	}
}

// writeBindOutputHexFixture distils a recording down to the server payloads that
// answer a call with an IO vector — the bind-output responses — and writes them
// as one hex line each, the form recordedFrames reads.
//
// When drivesPath is non-empty it also writes, in the same order, the **client**
// frame that immediately follows each of those responses. That pairing is the
// cross-check the decoder is held to, and it is positional on purpose: which
// frames land in the fixture must not depend on any decode, or the check would
// be the decoder agreeing with itself.
func writeBindOutputHexFixture(t *testing.T, dumpPath, outPath, drivesPath string) {
	t.Helper()

	dialect := "4-byte OCI dialect (Instant Client)"
	regenerate := "#   go test -tags capture -run TestCapture_SQLPlus ./internal/proxy/oracle/\n"

	if recordedDialectIsWide64(t, dumpPath) {
		dialect = "64-bit OCI dialect (the sqlplus bundled in gvenzl/oracle-free:23-slim),\n" +
			"# " + ociFixtureProvenance
		regenerate = "#   ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container \\\n" +
			"#     go test -tags integration -run TestCapture_OCIFixturesThroughDBBat ./internal/proxy/oracle/\n"
	}

	body := "# sqlplus (OCI thick) calling a procedure with an OUT parameter against\n" +
		"# Oracle 23ai Free — the " + dialect + ".\n" +
		"# One line per server response that opens with the IO vector (TTC message\n" +
		"# 0x0b): the TNS Data payload, two data-flag bytes first, exactly as\n" +
		"# extractTTCPayload receives it.\n" +
		"#\n" +
		"# These carry the same field list the thin recordings do, marshaled in the\n" +
		"# wide/fixed-width OCI encoding — little-endian integers of a per-call-site\n" +
		"# width where a thin client sends compressed ones.\n" +
		"#\n" +
		"# Regenerate with:\n" + regenerate

	drives := "# The client frame that follows each response in the fixture next to this\n" +
		"# one, in the same order: the `PRINT rc` that drives the REF cursor the call\n" +
		"# just handed back. Selected by position, never by decoding them.\n" +
		"#\n" +
		"# Regenerate with:\n" + regenerate

	frames, driveCount, wantDrive := 0, 0, false

	eachRecordedTNSPayload(t, dumpPath, func(clientToServer bool, payload []byte) {
		if clientToServer {
			if wantDrive {
				drives += hex.EncodeToString(payload) + "\n"
				driveCount++
				wantDrive = false
			}

			return
		}

		if ttc := extractTTCPayload(payload); len(ttc) == 0 || ttc[0] != ttcMsgIOVector {
			return
		}

		body += hex.EncodeToString(payload) + "\n"
		frames++
		wantDrive = drivesPath != ""
	})

	require.Positive(t, frames, "the sqlplus session must have answered at least one call")
	require.NoError(t, os.WriteFile(outPath, []byte(body), 0o600))

	t.Logf("%d bind-output responses written to %s", frames, outPath)

	if drivesPath == "" {
		return
	}

	require.Equal(t, frames, driveCount,
		"every recorded response must be followed by a client frame, or the pairing is not a pairing")
	require.NoError(t, os.WriteFile(drivesPath, []byte(drives), 0o600))

	t.Logf("%d drive frames written to %s", driveCount, drivesPath)
}

// ociDescribeFixture is where TestCapture_SQLPlusDescribe leaves the describe
// responses of the session below.
const ociDescribeFixture = "testdata/oci_describe.hex"

// ociDescribeQuery is deliberately wide in types rather than in rows: each
// column exercises a different corner of the describe record — a NUMBER with a
// real precision and scale, a VARCHAR2 whose maximum length does not fit a
// byte, a NUMBER whose scale is the -127 float sentinel, temporal types, a
// fixed CHAR, a RAW, and an object whose record carries a non-null 16-byte type
// OID (the one column that proves the toID field is a DLC with a four-byte
// length rather than a bare CLR).
const ociDescribeQuery = `SELECT CAST(1 AS NUMBER(10,2)) AS n2,
       CAST('x' AS VARCHAR2(4000)) AS big,
       1/3 AS flt,
       SYSDATE AS d,
       SYSTIMESTAMP AS ts,
       CAST('ab' AS CHAR(5)) AS c5,
       UTL_RAW.CAST_TO_RAW('zz') AS r,
       dbbat_cap_obj(1, 'x') AS obj
  FROM dual;`

// oci64DescribeFixture is the 64-bit dialect's describe evidence, and it is not
// a nicety: the fixed-width column record is *wider* in that dialect, and every
// field whose width differs is zero-valued in an ordinary two-column REF cursor.
// The type-rich query above is what turns those runs of zeros into measurable
// boundaries — a charset id of 873, a maximum character length of 4000, a
// collation id of 16382, a 16-byte object type OID — which is what
// describeColumnLayoutWide64 is pinned against.
const oci64DescribeFixture = "testdata/oci64_describe.hex"

// ociDescribeObjectType is the object type ociDescribeQuery's last column needs.
const ociDescribeObjectType = `CREATE OR REPLACE TYPE dbbat_cap_obj AS OBJECT (a NUMBER, b VARCHAR2(10))`

// writeDescribeHexFixture keeps every server payload that leads with a describe
// message, as one hex line each.
func writeDescribeHexFixture(t *testing.T, dumpPath, outPath string) {
	t.Helper()

	regenerate := "#   go test -tags capture -run TestCapture_SQLPlusDescribe ./internal/proxy/oracle/\n"
	if recordedDialectIsWide64(t, dumpPath) {
		regenerate = "#   ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container \\\n" +
			"#     go test -tags integration -run TestCapture_OCIFixturesThroughDBBat ./internal/proxy/oracle/\n"
	}

	body := "# Every describe response (TTC message 0x10) of an sqlplus session through\n" +
		"# dbbat against Oracle 23ai Free: the TNS Data payload, two data-flag bytes\n" +
		"# first, exactly as extractTTCPayload receives it.\n" +
		"#\n" +
		"# The last one describes a deliberately wide set of column types — see\n" +
		"# ociDescribeQuery — and is what pins the fixed-width column record against\n" +
		"# real values rather than against runs of zeros.\n" +
		"#\n" +
		"# Regenerate with:\n" + regenerate

	frames := 0

	eachRecordedTNSPayload(t, dumpPath, func(clientToServer bool, payload []byte) {
		if clientToServer {
			return
		}

		if ttc := extractTTCPayload(payload); len(ttc) == 0 || ttc[0] != byte(TTCFuncQueryResult) {
			return
		}

		body += hex.EncodeToString(payload) + "\n"
		frames++
	})

	require.Positive(t, frames, "the sqlplus session must have described at least one query")
	require.NoError(t, os.WriteFile(outPath, []byte(body), 0o600))

	t.Logf("%d describe responses written to %s", frames, outPath)
}

// writeStatementFrameHexFixture keeps every client frame of a recording whose
// payload contains one of markers, as one hex line each.
//
// The selection is by the statement's own text, never by decoding the frame:
// which frames land in a fixture that a decoder is then pinned against must not
// depend on that decoder, or the check is the decoder agreeing with itself.
//
// It takes more than one marker because a session's statements do not all have
// the same shape — a PL/SQL call and an ordinary query reach the wire as the
// same op with different bind and option fields — so a fixture that has to
// stand for "every parse" can be asked for several.
func writeStatementFrameHexFixture(t *testing.T, dumpPath, outPath string, markers ...string) {
	t.Helper()

	body := "# The client frames of the same session that carry a statement, picked by\n" +
		"# searching the payload for the statement's own text rather than by decoding\n" +
		"# anything. They are the negative half of the SQL-less exec reading: not one\n" +
		"# of them may be read as a re-execution.\n" +
		"#\n" +
		"# Regenerate with:\n" +
		"#   ORACLE_CAPTURE_OCI_FIXTURES=1 ORACLE_TEST_OCI_CLIENT=container \\\n" +
		"#     go test -tags integration -run TestCapture_OCIFixturesThroughDBBat ./internal/proxy/oracle/\n"

	frames := 0

	eachRecordedTNSPayload(t, dumpPath, func(clientToServer bool, payload []byte) {
		if !clientToServer || !containsAnyMarker(payload, markers) {
			return
		}

		body += hex.EncodeToString(payload) + "\n"
		frames++
	})

	require.GreaterOrEqual(t, frames, len(markers),
		"the session must have sent a frame for each of %v", markers)
	require.NoError(t, os.WriteFile(outPath, []byte(body), 0o600))

	t.Logf("%d statement-carrying client frames written to %s", frames, outPath)
}

// containsAnyMarker reports whether payload carries any of the statement texts.
func containsAnyMarker(payload []byte, markers []string) bool {
	for _, marker := range markers {
		if bytes.Contains(payload, []byte(marker)) {
			return true
		}
	}

	return false
}

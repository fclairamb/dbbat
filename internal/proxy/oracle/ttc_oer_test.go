package oracle

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

// decodeHexString turns a hex literal captured off the wire into bytes.
func decodeHexString(t *testing.T, s string) []byte {
	t.Helper()

	out, err := hex.DecodeString(s)
	require.NoError(t, err)

	return out
}

// buildOER assembles a synthetic OER message (starting at the 0x04 marker)
// from its leading compressed-int fields, matching the wire layout
// decodeOERAt expects: callStatus, seqNum, curRowNumber, errNum, then three
// trailing zero fields (arrayElemWErr, arrayElemErrNo, cursorID).
func buildOER(callStatus, seqNum, curRowNumber, errNum int) []byte {
	out := make([]byte, 0, 16)
	out = append(out, 0x04)

	for _, v := range []int{callStatus, seqNum, curRowNumber, errNum, 0, 0, 0} {
		out = append(out, ttcCompressedUint(uint64(v))...)
	}

	return out
}

// thinOERShape is a session that has *learned* its upstream marshals the
// summary object as TTC compressed integers — which is what every thin-client
// fixture in this package is, and what buildOER synthesizes.
//
// Spelling it out matters on the decode paths: an unlearned shape lets
// decodeOERFixedFieldsAt also try the two fixed-width layouts, so a test whose
// point is a *refusal* would be asserting against a wider predicate than the
// bytes it was written for. See decodeOERFixedFieldsAt.
func thinOERShape() oerShape {
	shape := defaultOERShape()
	shape.tailLearned = true

	return shape
}

func TestDecodeOERAt_RowCount(t *testing.T) {
	t.Parallel()

	info := decodeOERAt(buildOER(oerEndOfCallBit, 5, 3, 0), 0)
	require.NotNil(t, info)
	assert.Equal(t, 3, info.CurRowNumber)
	assert.Equal(t, 0, info.ErrorCode)
	assert.NotZero(t, info.CallStatus&oerEndOfCallBit)
}

func TestDecodeOERAt_Error(t *testing.T) {
	t.Parallel()

	oer := append(buildOER(oerEndOfCallBit, 1, 0, 942), []byte("\x00\x42ORA-00942: table or view does not exist\n")...)

	info := decodeOERAt(oer, 0)
	require.NotNil(t, info)
	assert.Equal(t, 942, info.ErrorCode)
	assert.Equal(t, "ORA-00942: table or view does not exist", info.ErrorMessage)
}

func TestDecodeOERAt_Invalid(t *testing.T) {
	t.Parallel()

	// Not a 0x04 marker.
	assert.Nil(t, decodeOERAt([]byte{0x08, 0x01}, 0))
	// Truncated right after the marker.
	assert.Nil(t, decodeOERAt([]byte{0x04}, 0))
	// Offset past end.
	assert.Nil(t, decodeOERAt([]byte{0x04}, 5))
	// Decodes cleanly but end-of-call bit is clear (callStatus = 2) — rejected
	// so byte runs inside the return-parameter block aren't mistaken for OERs.
	assert.Nil(t, decodeOERAt(buildOER(2, 0, 0, 0), 0))
}

func TestFindOERInResponse_SkipsDecoy(t *testing.T) {
	t.Parallel()

	// A return-parameter block byte run containing a 0x04 that does NOT decode
	// as an end-of-call OER, followed by the real OER.
	realOER := buildOER(oerEndOfCallBit, 7, 2, 0)
	payload := make([]byte, 0, 7+len(realOER))
	payload = append(payload, 0x08, 0x01, 0x06, 0x04, 0x02, 0x01, 0x00)
	payload = append(payload, realOER...)

	info := findOERInResponse(payload)
	require.NotNil(t, info)
	assert.Equal(t, 2, info.CurRowNumber, "must skip the decoy 0x04 and find the real OER")
}

func TestExtractORAMessage(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ORA-00942: table does not exist",
		extractORAMessage([]byte("\x00\x42ORA-00942: table does not exist\n")))
	assert.Empty(t, extractORAMessage([]byte("no oracle error here")))
}

// TestDumpReplay_DMLRowCounts replays the captured go-ora DML session and
// verifies every execute is paired with the OER outcome we expect. This is the
// real end-to-end check that parsing works against live Oracle Free 23ai bytes.
//
// Regenerate the fixture with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraDML ./internal/proxy/oracle/
func TestDumpReplay_DMLRowCounts(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_dml.pcapng")

	type outcome struct {
		sqlPrefix string
		rows      int
		errCode   int
	}

	// In execution order. The first DROP fails (table absent) with a standalone
	// OER (func=0x04); successful DML carries its OER inside a Response
	// (func=0x08). SELECTs complete via QRESULT and are skipped here.
	expected := []outcome{
		{"DROP TABLE dbbat_dml_test", 0, 942},
		{"CREATE TABLE dbbat_dml_test", 0, 0},
		{"INSERT INTO dbbat_dml_test VALUES", 1, 0},
		{"INSERT INTO dbbat_dml_test SELECT", 5, 0},
		{"UPDATE dbbat_dml_test", 3, 0},
		{"DELETE FROM dbbat_dml_test", 2, 0},
		{"DROP TABLE dbbat_dml_test", 0, 0},
	}

	prefixFor := func(sql string) string {
		for _, e := range expected {
			if strings.HasPrefix(sql, e.sqlPrefix) {
				return e.sqlPrefix
			}
		}

		return ""
	}

	var (
		got        []outcome
		pendingSQL string
	)

	for _, pkt := range td.Packets {
		tns, err := parseTNSFromDumpPacket(pkt.Data)
		if err != nil || tns.Type != TNSPacketTypeData || len(tns.Payload) < ttcDataFlagsSize+1 {
			continue
		}

		funcCode, err := parseTTCFunctionCode(tns.Payload)
		if err != nil {
			continue
		}

		ttcPayload := extractTTCPayload(tns.Payload)

		if pkt.Direction == dump.DirClientToServer {
			if funcCode == TTCFuncPiggyback && IsPiggybackExecSQL(ttcPayload) {
				if result, derr := decodePiggybackExecSQL(ttcPayload); derr == nil {
					pendingSQL = result.SQL
				}
			}

			continue
		}

		// Only the DML statements from our scenario carry an OER we track.
		if prefixFor(pendingSQL) == "" {
			continue
		}

		var info *oerInfo

		switch funcCode { //nolint:exhaustive // only DML completion codes are relevant here
		case TTCFuncResponse:
			info = findOERInResponse(ttcPayload)
		case TTCFuncOERR:
			info = decodeOERAt(ttcPayload, 0)
		default:
			continue
		}

		if info == nil {
			continue
		}

		got = append(got, outcome{prefixFor(pendingSQL), info.CurRowNumber, info.ErrorCode})
		pendingSQL = ""
	}

	require.Len(t, got, len(expected), "every DML statement should pair with one OER")

	for i, want := range expected {
		assert.Equal(t, want, got[i], "statement %d", i)
	}
}

// The two response payloads below are real bytes from Oracle Free 23ai,
// captured through the proxy while go-ora ran a statement-cache churn loop
// (see TestIntegration_CursorIDLearningMissRate). They are consecutive
// statements and differ in almost nothing — except that the second one's
// end-to-end ECID sequence has crossed 255.
const (
	// Sequence 252, cursor 6.
	oerResponseSeqUnderAByte = "1017d6b57e061a8de02cdee3dc8c63c9a88f787e080a0c222c0102010182020000817f0102000000000" +
		"00000000105010505434855524e00000000000000000000010707787e080a0c222c00021fe801020102000622010100011900000007" +
		"02c125080106032049c500010600000000000004010101fc010102057b00000106011803000000000003011d4d02040000028f99010" +
		"40000fc0001010000000002057b0101194f52412d30313430333a206e6f206461746120666f756e640a"

	// Sequence 256, cursor 13 — the very next statement on the same session.
	oerResponseSeqOverAByte = "1017592135b7dea1bf55542c6b3aa67d4b19787e080a0c222c0102010182020000817f01020000000000" +
		"0000000105010505434855524e00000000000000000000010707787e080a0c222c00021fe80102010200062201010001190000000702" +
		"c126080106032049c500010d000000000000040101020100010102057b0000010d011803000000000003011d4d02040000028f990104" +
		"0000020001010000000002057b0101194f52412d30313430333a206e6f206461746120666f756e640a"
)

// TestFindCursorIDInResponse_SequenceNumberPastAByte is the regression that a
// whole class of missing enforcement hid behind: the OER field dbbat calls
// SeqNumber is the end-to-end ECID sequence, a uint16 counting up across the
// session, not a per-call byte that wraps at 255.
//
// While the scan bounded it at 255, every session stopped learning cursor ids
// a few dozen statements in — and because Oracle recycles cursor ids, the
// re-executions that followed did not fail loudly: they resolved to whatever
// stale statement last held that id, so the gate ran the wrong SQL and
// /queries recorded the wrong SQL.
func TestFindCursorIDInResponse_SequenceNumberPastAByte(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		want    uint16
	}{
		{name: "sequence 252", payload: oerResponseSeqUnderAByte, want: 6},
		{name: "sequence 256", payload: oerResponseSeqOverAByte, want: 13},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := decodeHexString(t, tc.payload)

			got, ok := findCursorIDInResponse(thinOERShape(), payload)
			require.True(t, ok, "the server named a cursor in this response")
			assert.Equal(t, tc.want, got)
		})
	}
}

// oerErrorResponse is a real ORA-00942 OER, captured through the proxy when a
// client selected from a missing table. Oracle sent it as a standalone OER
// message, so its 0x04 marker sat at index 0 of the TTC payload — and
// findCursorIDInResponse starts scanning at index 1, since index 0 is the
// function-code byte of whatever message carries the OER. The byte below is
// therefore the leading function code a *carrying* response puts in front of
// it (0x08, Response); everything after it is untouched capture.
//
// Getting that wrong is not hypothetical: this fixture was first written
// trimmed to the marker, which made the test below pass without the scan ever
// reaching the error-code filter.
const oerErrorResponse = "08" +
	"0401010163000203ae00000109010e03000000000003011d4d02040000028f990104000063000000000000" +
	"0203ae004e4f52412d30303934323a207461626c65206f722076696577202253595354454d222e"

// oerErrorCodeFieldStart is where the errNum field sits inside oerErrorResponse:
// one byte of function code, the 0x04 marker, then callStatus (0x0101) and
// sequence (0x0163) and curRowNumber (0x00). Its three bytes are 0x02 0x03 0xae
// — a two-byte 942.
const (
	oerErrorCodeFieldStart = 7
	oerErrorCodeFieldLen   = 3
)

// TestFindCursorIDInResponse_RejectsAnErrorOER pins the other half of the
// anchor: an OER reporting a real failure assigns no usable cursor, so nothing
// is learned from it. That is why a statement that errors never yields a cursor
// id — and it is safe, because no client re-executes a statement that failed.
//
// The second half of the test is what makes the first half mean anything: the
// *same* bytes with the error code zeroed do teach cursor 9, so the rejection
// above is the error-code filter doing its job and not some other anchor (or
// the scan never finding an OER-shaped run at all).
func TestFindCursorIDInResponse_RejectsAnErrorOER(t *testing.T) {
	t.Parallel()

	payload := decodeHexString(t, oerErrorResponse)

	// The fixture really does contain a decodable OER, at an offset the scan
	// looks at, carrying a real ORA code and a plausible cursor id.
	info, _ := decodeOERFieldsAt(payload, 1)
	require.NotNil(t, info, "the fixture must hold an OER the scan can reach")
	assert.Equal(t, 942, info.ErrorCode)
	assert.Equal(t, 9, info.CursorID)

	_, ok := findCursorIDInResponse(thinOERShape(), payload)
	assert.False(t, ok, "an OER carrying a real ORA code must not teach a cursor id")

	// Same bytes, error code cleared: now it teaches.
	succeeded := make([]byte, 0, len(payload))
	succeeded = append(succeeded, payload[:oerErrorCodeFieldStart]...)
	succeeded = append(succeeded, 0x00)
	succeeded = append(succeeded, payload[oerErrorCodeFieldStart+oerErrorCodeFieldLen:]...)

	cursorID, ok := findCursorIDInResponse(thinOERShape(), succeeded)
	require.True(t, ok, "with the error cleared the same OER must be read")
	assert.Equal(t, uint16(9), cursorID)
}

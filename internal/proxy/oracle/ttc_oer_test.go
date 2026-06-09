package oracle

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/dump"
)

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

	td := loadTestDump(t, "go_ora_dml.dbbat-dump")

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

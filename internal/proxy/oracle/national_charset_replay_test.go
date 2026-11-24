package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nvarchar2ReplaySQLMarker identifies the SELECT in the NVARCHAR2 fixtures.
const nvarchar2ReplaySQLMarker = "FROM dbbat_nchar"

// TestDumpReplay_NVarchar2AsText replays real recordings (python-oracledb thin
// and go-ora against Oracle Free 23ai, national charset AL16UTF16) of a row
// whose VARCHAR2, NVARCHAR2 and NCHAR columns all hold "Bâtiment 1". The
// national columns travel as UTF-16BE; they must be captured as that text, not
// as hex of the UTF-16 bytes. NCHAR(12) is blank-padded by the server.
//
// Regenerate with TestCapture_PythonThinNVarchar2 / TestCapture_GoOraNVarchar2.
func TestDumpReplay_NVarchar2AsText(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{"python_thin_nvarchar2.pcapng", "go_ora_nvarchar2.pcapng"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			rows := replayCapturedRows(t, loadTestDump(t, fixture), nvarchar2ReplaySQLMarker)

			require.Len(t, rows, 1, "one row")
			require.Len(t, rows[0], 4)

			assert.Equal(t, "1", rows[0][0])
			assert.Equal(t, "Bâtiment 1", rows[0][1], "VARCHAR2")
			assert.Equal(t, "Bâtiment 1", rows[0][2], "NVARCHAR2 must be text, not UTF-16 hex")
			assert.Equal(t, "Bâtiment 1  ", rows[0][3], "NCHAR(12) is blank-padded by the server")
		})
	}
}

// TestDecodeRowValue_InlineNCLOB pins the inline NCLOB reading: a national CLOB
// column's inlined value is UTF-16BE (go_ora_lob.pcapng, D4 = TO_NCLOB('nn'),
// 006e006e on the wire), while a plain CLOB is never reinterpreted.
func TestDecodeRowValue_InlineNCLOB(t *testing.T) {
	t.Parallel()

	utf16 := []byte{0x00, 0x42, 0x00, 0xe2, 0x00, 0x74}

	assert.Equal(t, "Bât", decodeRowValue([]int{tnsTypeCLOB | colTypeNational}, 0, utf16))
	assert.Equal(t, "006e006e", decodeRowValue([]int{tnsTypeCLOB}, 0, []byte{0, 'n', 0, 'n'}),
		"without the describe's csfrm 2 nothing is guessed")
}

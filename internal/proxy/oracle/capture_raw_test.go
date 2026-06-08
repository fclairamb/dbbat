package oracle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawSQLMarker uniquely identifies rawQuery in the fixture.
const rawSQLMarker = "HEXTORAW"

// rawExpected is the proxy's decoded value for each RAW column of rawQuery — hex,
// regardless of whether the bytes happen to be printable.
var rawExpected = []string{"48656c6c6f", "deadbeef"}

// TestDumpReplay_Raw is the end-to-end check that RAW columns are captured as hex
// (not as text for the printable "Hello" bytes) now that values are decoded by
// column type. The fixture is regenerated with:
//
//	go test -tags capture -timeout 120s -run TestCapture_GoOraRaw ./internal/proxy/oracle/
func TestDumpReplay_Raw(t *testing.T) {
	t.Parallel()

	td := loadTestDump(t, "go_ora_raw.dbbat-dump")

	rows := replayCapturedRows(t, td, rawSQLMarker)

	require.Len(t, rows, 1, "one row")
	require.Len(t, rows[0], len(rawExpected), "both RAW columns captured")

	for i, want := range rawExpected {
		assert.Equalf(t, want, rows[0][i], "column %d decoded value", i)
	}
}

// TestDecodeRowValue_RawAsHex shows the type fixes the printable-RAW ambiguity:
// the heuristic reads "Hello" bytes as text, the RAW column type renders hex.
func TestDecodeRowValue_RawAsHex(t *testing.T) {
	t.Parallel()

	hello := []byte("Hello")

	assert.Equal(t, "Hello", decodeOracleRawValue(hello), "heuristic reads printable RAW as text")
	assert.Equal(t, "48656c6c6f", decodeRowValue([]int{tnsTypeRAW}, 0, hello))
}

package mssql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fclairamb/dbbat/internal/config"
)

// accountantFor builds a session just complete enough to walk a token stream:
// no store, no connection, so finish() records nothing and the test can read
// the counters directly.
func accountantFor(t *testing.T, storage config.QueryStorageConfig) *resultAccountant {
	t.Helper()

	session := &session{
		server: &Server{queryStorage: storage, logger: testLogger()},
		logger: testLogger(),
	}

	return session.newResultAccountant()
}

// intColumn encodes a COLMETADATA entry for a nullable 4-byte integer.
func intColumn(name string) []byte {
	buf := appendUint32(nil, 0)     // UserType
	buf = appendUint16(buf, 0x0001) // Flags (nullable)
	buf = append(buf, typeIntN, 4)  // TYPE_INFO
	return writeBVarchar(buf, name) //nolint:wsl // one logical entry
}

// nvarcharColumn encodes a COLMETADATA entry for an nvarchar column.
func nvarcharColumn(name string) []byte {
	buf := appendUint32(nil, 0)
	buf = appendUint16(buf, 0x0001)
	buf = append(buf, typeNVarChar)
	buf = appendUint16(buf, 8000)
	buf = append(buf, 0, 0, 0, 0, 0) // collation

	return writeBVarchar(buf, name)
}

// colMetadataToken assembles a COLMETADATA token from its column entries.
func colMetadataToken(columns ...[]byte) []byte {
	token := appendUint16([]byte{tokenColMetadata}, uint16(len(columns)))
	for _, col := range columns {
		token = append(token, col...)
	}

	return token
}

// intValue / nvarcharValue / nullByteValue encode ROW cell payloads.
func intValue(v int32) []byte {
	return appendUint32([]byte{4}, uint32(v))
}

func nvarcharValue(text string) []byte {
	encoded := stringToUCS2(text)

	return append(appendUint16(nil, uint16(len(encoded))), encoded...)
}

func nullUShortValue() []byte {
	return appendUint16(nil, 0xFFFF)
}

// rowToken assembles a ROW token from encoded cells.
func rowToken(cells ...[]byte) []byte {
	token := []byte{tokenRow}
	for _, cell := range cells {
		token = append(token, cell...)
	}

	return token
}

// nbcRowToken assembles an NBCROW: a null bitmap, then only the non-null cells.
func nbcRowToken(columns int, nulls []int, cells ...[]byte) []byte {
	bitmap := make([]byte, (columns+7)/8)
	for _, i := range nulls {
		bitmap[i/8] |= 1 << (i % 8)
	}

	token := append([]byte{tokenNBCRow}, bitmap...)
	for _, cell := range cells {
		token = append(token, cell...)
	}

	return token
}

// feedInOnePacket runs a whole response through the accountant as a single
// packet.
func feedInOnePacket(a *resultAccountant, payload []byte) {
	a.feed(context.Background(), payload)
}

// TestAccountantCountsRowsAndCaptures walks the shape a plain SELECT produces.
func TestAccountantCountsRowsAndCaptures(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true})

	stream := colMetadataToken(intColumn("id"), nvarcharColumn("nom"))
	stream = append(stream, rowToken(intValue(1), nvarcharValue("Zoé"))...)
	stream = append(stream, rowToken(intValue(2), nvarcharValue("Ωμέγα"))...)
	stream = append(stream, buildDoneToken(doneCount, 2)...)

	feedInOnePacket(acc, stream)

	assert.False(t, acc.desynced)
	assert.Equal(t, int64(2), acc.rowsSeen)
	assert.True(t, acc.haveCount)
	assert.Equal(t, int64(2), acc.doneRows)

	require.Len(t, acc.rows, 2)

	var first []any

	require.NoError(t, json.Unmarshal(acc.rows[0].RowData, &first))
	assert.Equal(t, []any{float64(1), "Zoé"}, first)

	var second []any

	require.NoError(t, json.Unmarshal(acc.rows[1].RowData, &second))
	assert.Equal(t, []any{float64(2), "Ωμέγα"}, second)
}

// TestAccountantHandlesNBCRow covers the null-bitmap row encoding, whose cells
// are simply absent rather than null-length.
func TestAccountantHandlesNBCRow(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true})

	stream := colMetadataToken(intColumn("clé"), nvarcharColumn("libellé"))
	stream = append(stream, nbcRowToken(2, []int{1}, intValue(7))...)
	stream = append(stream, buildDoneToken(doneCount, 1)...)

	feedInOnePacket(acc, stream)

	require.False(t, acc.desynced)
	require.Len(t, acc.rows, 1)

	var row []any

	require.NoError(t, json.Unmarshal(acc.rows[0].RowData, &row))
	assert.Equal(t, []any{float64(7), nil}, row)
}

// TestAccountantHandlesAnExplicitNull covers the in-band NULL of a
// length-prefixed type, as opposed to the NBCROW bitmap.
func TestAccountantHandlesAnExplicitNull(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true})

	stream := colMetadataToken(intColumn("id"), nvarcharColumn("nom"))
	stream = append(stream, rowToken([]byte{0}, nullUShortValue())...)
	stream = append(stream, buildDoneToken(doneCount, 1)...)

	feedInOnePacket(acc, stream)

	require.Len(t, acc.rows, 1)

	var row []any

	require.NoError(t, json.Unmarshal(acc.rows[0].RowData, &row))
	assert.Equal(t, []any{nil, nil}, row)
}

// TestAccountantSurvivesPacketSplits is the property the response side needs:
// the upstream→client pump forwards a packet at a time, so a token straddling a
// packet boundary is the normal case, not the edge case.
func TestAccountantSurvivesPacketSplits(t *testing.T) {
	t.Parallel()

	stream := colMetadataToken(intColumn("id"), nvarcharColumn("nom"))
	stream = append(stream, rowToken(intValue(1), nvarcharValue("un"))...)
	stream = append(stream, rowToken(intValue(2), nvarcharValue("deux"))...)
	stream = append(stream, buildDoneToken(doneCount, 2)...)

	// Every possible split point, plus byte-at-a-time.
	for split := 1; split < len(stream); split++ {
		acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true})
		feedInOnePacket(acc, stream[:split])
		feedInOnePacket(acc, stream[split:])

		require.False(t, acc.desynced, "split at %d desynchronised the walk", split)
		require.Equal(t, int64(2), acc.rowsSeen, "split at %d lost a row", split)
		require.Equal(t, int64(2), acc.doneRows, "split at %d lost the row count", split)
	}

	acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true})
	for i := range stream {
		feedInOnePacket(acc, stream[i:i+1])
	}

	assert.False(t, acc.desynced)
	assert.Equal(t, int64(2), acc.rowsSeen)
	assert.Len(t, acc.rows, 2)
}

// TestAccountantReadsAnErrorToken proves an ERROR lands as a real decoded
// diagnostic, not as bytes.
func TestAccountantReadsAnErrorToken(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	stream := buildErrorToken(2627, 1, 14, "Violation de contrainte UNIQUE «IX_t».", "", 1)
	stream = append(stream, buildDoneToken(doneError, 0)...)

	feedInOnePacket(acc, stream)

	require.NotNil(t, acc.failure)
	assert.Equal(t, int32(2627), acc.failure.Number)
	assert.Equal(t, "Violation de contrainte UNIQUE «IX_t».", acc.failure.Message)
	assert.Contains(t, acc.failure.String(), "error 2627")
}

// TestAccountantIgnoresACountlessDone: without DONE_COUNT the row-count field
// is padding. Reading it anyway is how a proxy invents rows that never existed.
func TestAccountantIgnoresACountlessDone(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	feedInOnePacket(acc, buildDoneToken(0, 999))

	assert.False(t, acc.haveCount)
	assert.Zero(t, acc.doneRows)
}

// TestAccountantDoesNotDoubleCountDoneProc: a DONEPROC repeats the count of the
// DONEINPROC inside the same procedure, so every sp_executesql would otherwise
// report its rows twice.
func TestAccountantDoesNotDoubleCountDoneProc(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	stream := append([]byte{}, buildDoneToken(doneCount|doneMore, 3)...)
	stream[0] = tokenDoneInProc

	proc := append([]byte{}, buildDoneToken(doneCount, 3)...)
	proc[0] = tokenDoneProc

	feedInOnePacket(acc, append(stream, proc...))

	assert.True(t, acc.haveCount)
	assert.Equal(t, int64(3), acc.doneRows)
}

// TestAccountantFallsBackToTheTailDone is the backstop: a token the walker does
// not model desynchronises it, and the last token of any TDS response is still
// a DONE, so the row count survives.
func TestAccountantFallsBackToTheTailDone(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	// ALTMETADATA (COMPUTE BY) is deliberately unmodelled.
	stream := make([]byte, 0, 32)
	stream = append(stream, tokenAltMetadata, 0x01, 0x02, 0x03)
	stream = append(stream, buildDoneToken(doneCount, 17)...)

	feedInOnePacket(acc, stream)
	require.True(t, acc.desynced)

	acc.readTailDone()
	assert.True(t, acc.haveCount)
	assert.Equal(t, int64(17), acc.doneRows)
}

// TestAccountantTailSurvivesAPacketSplit: the DONE token itself can straddle
// the last packet boundary, which the rolling tail has to cope with.
func TestAccountantTailSurvivesAPacketSplit(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	stream := make([]byte, 0, 32)
	stream = append(stream, tokenAltMetadata, 0x01)
	stream = append(stream, buildDoneToken(doneCount, 5)...)

	for i := range stream {
		feedInOnePacket(acc, stream[i:i+1])
	}

	require.True(t, acc.desynced)

	acc.readTailDone()
	assert.True(t, acc.haveCount)
	assert.Equal(t, int64(5), acc.doneRows)
}

// TestAccountantReadsAPreparedHandle covers the RETURNVALUE token an
// sp_prepare answers with — the link that lets a later sp_execute be enforced
// on the statement it actually runs.
func TestAccountantReadsAPreparedHandle(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	token := appendUint16([]byte{tokenReturnValue}, 1) // ParamOrdinal
	token = writeBVarchar(token, "@handle")
	token = append(token, 0x01)           // Status: output parameter
	token = appendUint32(token, 0)        // UserType
	token = appendUint16(token, 0)        // Flags
	token = append(token, typeIntN, 4, 4) // TYPE_INFO + value length
	token = appendUint32(token, 41)       // value
	token = append(token, buildDoneToken(doneCount, 0)...)

	feedInOnePacket(acc, token)

	require.False(t, acc.desynced)
	assert.True(t, acc.haveHandl)
	assert.Equal(t, int64(41), acc.handle)
}

// TestAccountantHonoursCaptureLimits proves the storage caps are applied and
// that hitting one marks the capture as a prefix rather than the whole thing.
func TestAccountantHonoursCaptureLimits(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{StoreResults: true, MaxResultRows: 2})

	stream := colMetadataToken(intColumn("numéro"))
	for i := range 5 {
		stream = append(stream, rowToken(intValue(int32(i)))...)
	}

	stream = append(stream, buildDoneToken(doneCount, 5)...)

	feedInOnePacket(acc, stream)

	assert.Equal(t, int64(5), acc.rowsSeen, "every row must still be counted")
	assert.Len(t, acc.rows, 2, "only the configured number is captured")
	assert.True(t, acc.truncated)
}

// TestAccountantSkipsCaptureWhenDisabled: rows are still counted, none stored.
func TestAccountantSkipsCaptureWhenDisabled(t *testing.T) {
	t.Parallel()

	acc := accountantFor(t, config.QueryStorageConfig{})

	stream := colMetadataToken(intColumn("id"))
	stream = append(stream, rowToken(intValue(1))...)
	stream = append(stream, buildDoneToken(doneCount, 1)...)

	feedInOnePacket(acc, stream)

	assert.Equal(t, int64(1), acc.rowsSeen)
	assert.Empty(t, acc.rows)
}

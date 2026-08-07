package mssql

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testAllHeaders is the ALL_HEADERS block every TDS 7.2+ request carries: a
// total length, then a single transaction-descriptor header.
func testAllHeaders() []byte {
	return []byte{
		0x16, 0x00, 0x00, 0x00, // total length, this DWORD included
		0x12, 0x00, 0x00, 0x00, // header length
		0x02, 0x00, // type = transaction descriptor
		0, 0, 0, 0, 0, 0, 0, 0, // transaction descriptor
		0x01, 0x00, 0x00, 0x00, // outstanding requests
	}
}

// appendUint16 / appendUint32 keep the encoders below readable.
func appendUint16(buf []byte, v uint16) []byte {
	var scratch [2]byte

	binary.LittleEndian.PutUint16(scratch[:], v)

	return append(buf, scratch[:]...)
}

func appendUint32(buf []byte, v uint32) []byte {
	var scratch [4]byte

	binary.LittleEndian.PutUint32(scratch[:], v)

	return append(buf, scratch[:]...)
}

func appendUint64(buf []byte, v uint64) []byte {
	var scratch [8]byte

	binary.LittleEndian.PutUint64(scratch[:], v)

	return append(buf, scratch[:]...)
}

// paramHead encodes a parameter's name and status byte.
func paramHead(name string) []byte {
	return writeBVarchar(nil, name)
}

// nvarcharParam encodes an nvarchar(n) parameter.
func nvarcharParam(name, text string) []byte {
	const declaredLen = 8000

	buf := append(paramHead(name), 0x00) // status
	buf = append(buf, typeNVarChar)
	buf = appendUint16(buf, declaredLen)
	buf = append(buf, 0, 0, 0, 0, 0) // collation

	encoded := stringToUCS2(text)
	buf = appendUint16(buf, uint16(len(encoded)))

	return append(buf, encoded...)
}

// nvarcharMaxParam encodes an nvarchar(max) parameter, whose value is PLP —
// which is how every real driver sends sp_executesql's @stmt.
func nvarcharMaxParam(name, text string) []byte {
	buf := append(paramHead(name), 0x00)
	buf = append(buf, typeNVarChar)
	buf = appendUint16(buf, 0xFFFF)
	buf = append(buf, 0, 0, 0, 0, 0)

	encoded := stringToUCS2(text)
	buf = appendUint64(buf, uint64(len(encoded)))

	// A zero-length value is the total followed straight by the terminator: an
	// empty chunk *is* the terminator, so emitting one would end the value
	// early and leave four stray bytes where the next parameter starts.
	if len(encoded) > 0 {
		buf = appendUint32(buf, uint32(len(encoded)))
		buf = append(buf, encoded...)
	}

	return appendUint32(buf, 0) // terminator chunk
}

// intNParam encodes a nullable 4-byte integer parameter.
func intNParam(name string, value int32, status byte) []byte {
	buf := append(paramHead(name), status)
	buf = append(buf, typeIntN, 4, 4)

	return appendUint32(buf, uint32(value))
}

// rpcByID builds an RPC message using the procedure-id shorthand.
func rpcByID(procID uint16, params ...[]byte) []byte {
	payload := testAllHeaders()
	payload = appendUint16(payload, procIDByNameMarker)
	payload = appendUint16(payload, procID)
	payload = appendUint16(payload, 0) // option flags

	for _, p := range params {
		payload = append(payload, p...)
	}

	return payload
}

// rpcByName builds an RPC message naming a stored procedure.
func rpcByName(name string, params ...[]byte) []byte {
	encoded := stringToUCS2(name)

	payload := testAllHeaders()
	payload = appendUint16(payload, uint16(len(encoded)/2))
	payload = append(payload, encoded...)
	payload = appendUint16(payload, 0)

	for _, p := range params {
		payload = append(payload, p...)
	}

	return payload
}

// TestParseSQLBatchDecodesUCS2 is the round-trip the spec asks for: SQLBatch
// carries UCS-2LE, not UTF-8, and a naive conversion both mangles non-ASCII
// identifiers and could let a crafted statement evade a pattern match.
func TestParseSQLBatchDecodesUCS2(t *testing.T) {
	t.Parallel()

	statements := []string{
		"SELECT 1",
		`SELECT "prénom", "année" FROM "employés" WHERE "ville" = 'Rendez-vous'`,
		"SELECT * FROM [Ταблица_Ωμέγα] WHERE [столбец] = N'значение'",
		"UPDATE t SET emoji = N'🐟🚀' WHERE id = 1",
		"SELECT 'Ünïcödé' /* commentaire accentué */",
		"",
	}

	for _, sql := range statements {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			decoded, err := parseSQLBatch(sqlBatchPayload(sql))
			require.NoError(t, err)
			assert.Equal(t, sql, decoded, "the statement must survive the UCS-2 round trip byte for byte")
		})
	}
}

// TestParseSQLBatchRefusesAMalformedHeaderBlock is the fail-closed rule.
//
// Skipping a header block that does not validate would decode its bytes as part
// of the statement, and every grant control dbbat applies is prefix-based —
// so a garbage prefix turns `DELETE FROM t` into something `read_only` no longer
// matches while the upstream may still run it. ALL_HEADERS is mandatory from
// TDS 7.2 and the proxy's floor is 7.4, so refusing costs nothing.
func TestParseSQLBatchRefusesAMalformedHeaderBlock(t *testing.T) {
	t.Parallel()

	// The exact evasion: an unknown header type in front of a real write.
	evasion := append(
		[]byte{0x16, 0x00, 0x00, 0x00, 0x12, 0x00, 0x00, 0x00, 0x63, 0x00},
		make([]byte, 12)...)
	evasion = append(evasion, stringToUCS2("DELETE FROM t")...)

	tests := []struct {
		name    string
		payload []byte
	}{
		{"no header block at all", stringToUCS2("SELECT 1")},
		{"an unknown header type in front of a write", evasion},
		{"a length that overruns the message", append([]byte{0xFF, 0xFF, 0x00, 0x00}, stringToUCS2("SELECT 1")...)},
		{"a length smaller than itself", append([]byte{0x02, 0x00, 0x00, 0x00}, stringToUCS2("SELECT 1")...)},
		{"a header length out of range", []byte{0x0C, 0x00, 0x00, 0x00, 0xFF, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}},
		{"too short to hold a block", []byte{0x41, 0x00}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseSQLBatch(tc.payload)
			require.ErrorIs(t, err, ErrMalformedRequest)
		})
	}
}

// TestParseAllHeadersAcceptsTheLegalForms: an empty block, and each of the
// three header types MS-TDS defines.
func TestParseAllHeadersAcceptsTheLegalForms(t *testing.T) {
	t.Parallel()

	offset, err := parseAllHeaders([]byte{0x04, 0x00, 0x00, 0x00})
	require.NoError(t, err)
	assert.Equal(t, 4, offset)

	for _, headerType := range []byte{0x01, 0x02, 0x03} {
		block := append([]byte{}, testAllHeaders()...)
		block[8] = headerType

		offset, err := parseAllHeaders(block)
		require.NoError(t, err)
		assert.Equal(t, len(testAllHeaders()), offset)
	}

	// An empty message carries no statement and no headers.
	offset, err = parseAllHeaders(nil)
	require.NoError(t, err)
	assert.Zero(t, offset)
}

// TestParseSQLBatchRejectsAnOddLength: UCS-2 text always occupies an even
// number of bytes, so an odd one is a malformed request, not a statement.
func TestParseSQLBatchRejectsAnOddLength(t *testing.T) {
	t.Parallel()

	_, err := parseSQLBatch([]byte{0x41, 0x00, 0x42})
	require.ErrorIs(t, err, ErrMalformedRequest)
}

// TestParseRPCExtractsTheStatement covers every RPC form that carries SQL,
// each at the parameter position MS-TDS puts it at. Getting one of these
// indices wrong is a silent read_only bypass, not a cosmetic bug.
func TestParseRPCExtractsTheStatement(t *testing.T) {
	t.Parallel()

	const sql = "UPDATE employés SET salaire = 1 WHERE id = 2"

	tests := []struct {
		name    string
		payload []byte
	}{
		{
			name: "sp_executesql",
			payload: rpcByID(spExecuteSQL,
				nvarcharMaxParam("", sql),
				nvarcharMaxParam("", "@id int"),
				intNParam("@id", 2, 0)),
		},
		{
			name: "sp_prepare",
			payload: rpcByID(spPrepare,
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("@params", "@id int"),
				nvarcharMaxParam("", sql),
				intNParam("", 1, 0)),
		},
		{
			name: "sp_prepexec",
			payload: rpcByID(spPrepExec,
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("", "@id int"),
				nvarcharMaxParam("", sql),
				intNParam("@id", 2, 0)),
		},
		{
			name: "sp_cursorprepare",
			payload: rpcByID(spCursorPrepare,
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("", "@id int"),
				nvarcharMaxParam("", sql),
				intNParam("", 1, 0)),
		},
		{
			name: "sp_cursoropen",
			payload: rpcByID(spCursorOpen,
				intNParam("@cursor", 0, 0x01),
				nvarcharMaxParam("", sql),
				intNParam("", 1, 0)),
		},
		{
			name: "sp_cursorprepexec",
			payload: rpcByID(spCursorPrepExec,
				intNParam("@cursor", 0, 0x01),
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("", "@id int"),
				nvarcharMaxParam("", sql)),
		},
		{
			name: "sp_prepexecrpc",
			payload: rpcByID(spPrepExecRPC,
				intNParam("@handle", 0, 0x01),
				nvarcharMaxParam("", sql)),
		},
		{
			// The fixed-length nvarchar form, which short statements take.
			name: "sp_executesql with a non-max nvarchar",
			payload: rpcByID(spExecuteSQL,
				nvarcharParam("", sql)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requests, err := parseRPC(tc.payload)
			require.NoError(t, err)
			require.Len(t, requests, 1)

			statement, ok := requests[0].statementText()
			require.True(t, ok, "the form must be recognized as carrying a statement")
			assert.Equal(t, sql, statement)
		})
	}
}

// TestParseRPCByName covers the form that names a stored procedure: there is
// no inline SQL, and the logged text is an EXEC of what was called.
func TestParseRPCByName(t *testing.T) {
	t.Parallel()

	requests, err := parseRPC(rpcByName("dbo.Réconcilier", intNParam("@year", 2026, 0)))
	require.NoError(t, err)
	require.Len(t, requests, 1)

	assert.Equal(t, "dbo.Réconcilier", requests[0].name)
	assert.False(t, requests[0].isWellKnown())

	_, ok := requests[0].statementText()
	assert.False(t, ok, "a stored procedure call carries no statement dbbat can read")

	assert.Equal(t, "EXEC dbo.Réconcilier @year = 2026", requests[0].logText())
}

// TestParseRPCHandleForms covers the forms that execute something prepared
// earlier: the handle has to come out, because it is what the grant check is
// resolved through.
func TestParseRPCHandleForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{"sp_execute", rpcByID(spExecute, intNParam("@handle", 41, 0), intNParam("@id", 2, 0))},
		// sp_cursorexecute prepared_handle, cursor OUTPUT, …: the handle comes
		// first. Reading the cursor slot instead would refuse every cursor
		// re-execution under a restrictive grant, and — on a cursor-id/handle-id
		// collision — resolve against the wrong statement.
		{"sp_cursorexecute", rpcByID(spCursorExecute,
			intNParam("@handle", 41, 0), intNParam("@cursor", 0, 0x01))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requests, err := parseRPC(tc.payload)
			require.NoError(t, err)
			require.Len(t, requests, 1)

			handle, ok := requests[0].handle()
			require.True(t, ok)
			assert.Equal(t, int64(41), handle)
		})
	}
}

// TestParseRPCBatch covers several calls in one message, separated by a batch
// flag. Every one of them has to be seen, or the second write in a batch would
// be forwarded unchecked.
func TestParseRPCBatch(t *testing.T) {
	t.Parallel()

	first := rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 1"))
	second := rpcByID(spExecuteSQL, nvarcharMaxParam("", "DELETE FROM t"))

	// Drop the second request's own ALL_HEADERS and join with a batch flag.
	payload := append(append([]byte{}, first...), batchFlagTransaction)
	payload = append(payload, second[len(testAllHeaders()):]...)

	requests, err := parseRPC(payload)
	require.NoError(t, err)
	require.Len(t, requests, 2)

	firstSQL, ok := requests[0].statementText()
	require.True(t, ok)
	assert.Equal(t, "SELECT 1", firstSQL)

	secondSQL, ok := requests[1].statementText()
	require.True(t, ok)
	assert.Equal(t, "DELETE FROM t", secondSQL)
}

// TestParseRPCTruncated pins the failure mode: a request that runs off the end
// of its message must never yield a partially-read statement, because that is
// what enforcement would then run against.
//
// A cut exactly at the end of the header is legal — it is a call with no
// parameters — and must simply carry no statement.
func TestParseRPCTruncated(t *testing.T) {
	t.Parallel()

	full := rpcByID(spExecuteSQL, nvarcharMaxParam("", "SELECT 1"))

	for cut := len(testAllHeaders()) + 1; cut < len(full); cut++ {
		requests, err := parseRPC(full[:cut])
		if err != nil {
			continue
		}

		for _, req := range requests {
			_, ok := req.statementText()
			require.False(t, ok, "a request truncated at %d bytes must not yield a statement", cut)
		}
	}
}

// TestParseRPCParameterValues proves the values are captured for logging, with
// their names, in the order they were sent.
func TestParseRPCParameterValues(t *testing.T) {
	t.Parallel()

	requests, err := parseRPC(rpcByID(spExecuteSQL,
		nvarcharMaxParam("", "SELECT @id, @label"),
		nvarcharMaxParam("", "@id int, @label nvarchar(50)"),
		intNParam("@id", 7, 0),
		nvarcharParam("@label", "étiquette")))
	require.NoError(t, err)
	require.Len(t, requests, 1)

	assert.Equal(t, []string{
		"SELECT @id, @label",
		"@id int, @label nvarchar(50)",
		"@id=7",
		"@label=étiquette",
	}, requests[0].parameterValues())
}

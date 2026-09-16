package oracle

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The writers, one encoding at a time, with the two cases that matter for each:
// the tag fitting the field as it stands, and the tag widening it. A field that
// widens shifts every byte behind it, which is the whole reason the rewriter
// rebuilds the message instead of patching it — and `ORA-03146 invalid buffer
// length for TTC field` is what a server answers when that goes wrong.

// thinExecFrame builds a `03 5e` piggyback execute of the shape every thin
// client sends: a compressed-int length in the header, then filler, then the
// statement — as a CLR value (go-ora, python-oracledb thin) or as a bare run
// (ojdbc, DBeaver).
//
// The filler is the al8i4 array and the pointer words; their contents do not
// matter to the locator, only that they are not text.
func thinExecFrame(sql string, value []byte) []byte {
	body := make([]byte, 0, 48+len(value))
	body = append(body, 0x03, 0x5e, 0x00)
	body = append(body, ttcCompressedUint(0x8021)...) // options
	body = append(body, ttcCompressedUint(0)...)      // cursor id
	body = append(body, 0x01)                         // "cursor id is zero"
	body = append(body, ttcCompressedUint(uint64(len(sql)))...)
	body = append(body, 0x01, 0x01, 0x0d)
	body = append(body, make([]byte, 15)...)
	body = append(body, value...)
	body = append(body, 0x01, 0x01, 0x00, 0x00)

	return body
}

// thinExecCLR is thinExecFrame with the statement in the CLR short form.
func thinExecCLR(sql string) []byte {
	return thinExecFrame(sql, append([]byte{byte(len(sql))}, sql...))
}

// thinExecBare is thinExecFrame with the statement written with no framing of
// its own.
func thinExecBare(sql string) []byte {
	return thinExecFrame(sql, []byte(sql))
}

// wideExecFrame builds the OCI exec header: the `01 seq+1` pad, eight option
// bytes, the `fe x8` pointer sentinel and the `sqlLen * 3` little-endian ub4.
func wideExecFrame(run []byte) []byte {
	body := make([]byte, 0, 64+len(run))
	body = append(body, 0x03, 0x5e, 0x06, 0x01, 0x07)
	body = append(body, make([]byte, 8)...)
	body = append(body, closeCursorsWideSentinel...)

	ub4 := make([]byte, 4)
	binary.LittleEndian.PutUint32(ub4, uint32(len(run)*wideCharWidth))
	body = append(body, ub4...)

	body = append(body, make([]byte, 24)...)
	body = append(body, byte(len(run)))
	body = append(body, run...)
	body = append(body, 0x00, 0x01, 0x00, 0x00)

	return body
}

// oall8Frame builds a legacy OALL8 parse+execute: the text sits immediately
// behind the varlen length and the bind count immediately behind the text, so
// widening the length field moves the binds.
func oall8Frame(sql string, bindCount uint16) []byte {
	body := []byte{byte(TTCFuncOALL8), 0, 0, 0, 0}
	body = append(body, 0x00, 0x07) // cursor id 7
	body = append(body, encodeVarLenBytes(len(sql))...)
	body = append(body, sql...)

	binds := make([]byte, 2)
	binary.BigEndian.PutUint16(binds, bindCount)

	return append(body, binds...)
}

// tagOf returns a prefix of exactly n bytes that reads as a SQL comment, so a
// test can put the statement on either side of a format boundary on purpose.
func tagOf(n int) string {
	return "/*" + strings.Repeat("t", n-5) + "*/ "
}

// rewriteWith locates the statement in body and returns the rewritten body with
// prefix prepended, failing the test if the locator refuses.
func rewriteWith(t *testing.T, body []byte, prefix string, bigChunks bool) []byte {
	t.Helper()

	rw, ok := locateStatementRewrite(body, bigChunks)
	require.True(t, ok, "the locator must answer for this frame")
	require.Equal(t, body, rw.apply(body, rw.run, bigChunks),
		"rewriting a statement to itself must reproduce the frame byte for byte")

	return rw.apply(body, append([]byte(prefix), rw.run...), bigChunks)
}

// TestRewriteCompressedLengthKeepsItsWidth covers the ordinary case: the tag
// fits without the header's length field changing size.
func TestRewriteCompressedLengthKeepsItsWidth(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("a", 90) + " FROM dual"
	body := thinExecCLR(sql)

	before, ok := execSQLLengthField(body)
	require.True(t, ok)
	require.Equal(t, 2, before.width, "a length under 256 is a one-byte compressed int plus its count")

	out := rewriteWith(t, body, tagOf(40), false)

	after, ok := execSQLLengthField(out)
	require.True(t, ok)
	assert.Equal(t, len(sql)+40, after.value)
	assert.Equal(t, 2, after.width, "still under 256, so the field did not move")
	assert.Len(t, out, len(body)+40, "only the tag was added")

	stmt, ok := decodeExecStatementText(out)
	require.True(t, ok)
	assert.Equal(t, tagOf(40)+sql, stmt.Text)
}

// TestRewriteCompressedLengthWidens is the case the spec singles out: the tag
// pushes the declared length past 255, the compressed int grows a byte, and
// every byte behind it shifts.
func TestRewriteCompressedLengthWidens(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("a", 230) + " FROM dual"
	require.Less(t, len(sql), 256)

	body := thinExecBare(sql)

	before, ok := execSQLLengthField(body)
	require.True(t, ok)
	require.Equal(t, 2, before.width)

	const tagLen = 46

	out := rewriteWith(t, body, tagOf(tagLen), false)

	after, ok := execSQLLengthField(out)
	require.True(t, ok)
	require.Greater(t, after.value, 255)
	assert.Equal(t, 3, after.width, "past 255 the compressed int needs a second value byte")
	assert.Len(t, out, len(body)+tagLen+1, "the tag, plus the byte the length field grew by")

	stmt, ok := decodeExecStatementText(out)
	require.True(t, ok)
	assert.Equal(t, tagOf(tagLen)+sql, stmt.Text)
}

// TestRewriteWideUB4Length covers the OCI header's `sqlLen * 3` field: fixed
// width, so the only thing that changes is the number in it.
func TestRewriteWideUB4Length(t *testing.T) {
	t.Parallel()

	sql := "SELECT DECODE(USER, 'XS$NULL', 'x', USER) FROM sys.dual"

	for _, nul := range []bool{false, true} {
		run := []byte(sql)
		if nul {
			run = append(run, 0)
		}

		body := wideExecFrame(run)

		before, ok := execSQLLengthWideField(body)
		require.True(t, ok)
		require.Equal(t, len(run), before.value)
		require.Equal(t, 4, before.width)

		out := rewriteWith(t, body, tagOf(40), false)

		after, ok := execSQLLengthWideField(out)
		require.True(t, ok)
		assert.Equal(t, len(run)+40, after.value, "nul=%v", nul)
		assert.Equal(t, 4, after.width, "the ub4 never changes width")
		assert.Equal(t, uint32((len(run)+40)*3),
			binary.LittleEndian.Uint32(out[after.at:after.at+4]),
			"the field carries three times the length, as the client writes it")
		assert.Len(t, out, len(body)+40)

		stmt, ok := decodeExecStatementText(out)
		require.True(t, ok)
		assert.Equal(t, tagOf(40)+sql, strings.TrimSuffix(stmt.Text, "\x00"))
	}
}

// TestRewriteOALL8VarLenWidens covers the third encoding, and the thing that
// makes it different: the bind count sits immediately behind the text, so a
// length field that grows moves it.
func TestRewriteOALL8VarLenWidens(t *testing.T) {
	t.Parallel()

	short := "SELECT 1 FROM dual"
	body := oall8Frame(short, 3)

	out := rewriteWith(t, body, tagOf(40), false)

	res, err := decodeOALL8(out)
	require.NoError(t, err)
	assert.Equal(t, tagOf(40)+short, res.SQL)
	assert.Equal(t, uint16(7), res.CursorID)
	assert.Len(t, out, len(body)+40, "a length under 0xFE stays one byte")

	// And the widening case: 0xFE plus a big-endian uint16 is three bytes where
	// one was, so the bind count moves two bytes further out.
	long := "SELECT " + strings.Repeat("b", 230) + " FROM dual"
	require.Less(t, len(long), 0xFE)

	body = oall8Frame(long, 3)
	out = rewriteWith(t, body, tagOf(40), false)

	require.Equal(t, byte(oall8LenShort), out[7], "the varlen escaped to its 0xFE form")
	assert.Len(t, out, len(body)+40+2)

	res, err = decodeOALL8(out)
	require.NoError(t, err)
	assert.Equal(t, tagOf(40)+long, res.SQL)

	// The bind count is still readable where decodeOALL8 expects it, which is
	// the check that the shift carried the tail rather than overwriting it.
	assert.Equal(t, uint16(3), binary.BigEndian.Uint16(out[len(out)-2:]))
}

// TestRewriteCLRCrossesTheChunkBoundary is the 252-byte format change the spec
// names as the exact risk: a statement just under the short-form limit, plus a
// ~45-byte tag, is a statement that has to be written in the long form.
func TestRewriteCLRCrossesTheChunkBoundary(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("c", 220) + " FROM dual"
	require.Less(t, len(sql), clrShortMaxLen)

	body := thinExecCLR(sql)

	for _, bigChunks := range []bool{true, false} {
		out := rewriteWith(t, body, tagOf(46), bigChunks)

		require.Greater(t, len(sql)+46, clrShortMaxLen,
			"the point of the fixture is that the tagged value no longer fits the short form")

		rw, ok := locateStatementRewrite(out, bigChunks)
		require.True(t, ok, "bigChunks=%v: the long form must be locatable too", bigChunks)
		assert.Equal(t, stmtClrChunked, rw.clrKind, "bigChunks=%v", bigChunks)
		assert.Equal(t, bigChunks, rw.chunkBig,
			"the value is written in the variant the session negotiated")
		assert.Equal(t, tagOf(46)+sql, rw.text())

		stmt, ok := decodeExecStatementText(out)
		require.True(t, ok, "bigChunks=%v", bigChunks)
		assert.Equal(t, tagOf(46)+sql, stmt.Text)
	}
}

// TestRewriteCLRStaysShortUnderTheLimit is the other side of the boundary: a
// tagged value that still fits keeps the one-byte prefix, so the frame stays the
// shape the client sent.
func TestRewriteCLRStaysShortUnderTheLimit(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("d", 150) + " FROM dual"
	body := thinExecCLR(sql)

	out := rewriteWith(t, body, tagOf(46), true)

	rw, ok := locateStatementRewrite(out, true)
	require.True(t, ok)
	assert.Equal(t, stmtClrShort, rw.clrKind)
	assert.Equal(t, byte(len(sql)+46), out[rw.valueAt], "the prefix is the new length")
}

// TestRewriteChunkedOriginalKeepsTheClientsChunking covers a statement the
// client itself wrote in the long form: it has to be re-chunked the way that
// client chunks, not the way this package would.
func TestRewriteChunkedOriginalKeepsTheClientsChunking(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("e", 400) + " FROM dual"

	for _, bigChunks := range []bool{true, false} {
		const chunkSize = 100

		body := thinExecFrame(sql, encodeChunkedCLR([]byte(sql), chunkSize, bigChunks))

		rw, ok := locateStatementRewrite(body, bigChunks)
		require.True(t, ok, "bigChunks=%v", bigChunks)
		require.Equal(t, stmtClrChunked, rw.clrKind)
		assert.Equal(t, chunkSize, rw.chunkSize, "the client's chunk size, read off the wire")
		assert.Equal(t, bigChunks, rw.chunkBig)
		assert.Equal(t, sql, rw.text())

		out := rewriteWith(t, body, tagOf(46), bigChunks)

		back, ok := locateStatementRewrite(out, bigChunks)
		require.True(t, ok)
		assert.Equal(t, chunkSize, back.chunkSize, "still chunked at the client's size")
		assert.Equal(t, tagOf(46)+sql, back.text())
	}
}

// TestLocatorRefusesAnAmbiguousFrame pins the refusal that keeps the rewriter
// honest: two runs of the declared length means dbbat does not know which bytes
// the server will parse, and a guess there is the dead session this file exists
// to avoid.
func TestLocatorRefusesAnAmbiguousFrame(t *testing.T) {
	t.Parallel()

	sql := "SELECT 1 FROM dual"

	value := append([]byte{byte(len(sql))}, sql...)
	value = append(value, 0x00)
	value = append(value, byte(len(sql)))
	value = append(value, sql...)

	body := thinExecFrame(sql, value)

	_, ok := locateStatementRewrite(body, false)
	assert.False(t, ok, "a frame carrying the statement twice must be refused, not guessed at")
}

// TestLocatorRefusesAFrameItCannotReproduce is the round-trip guard, forced: a
// CLR prefix the encoder would not have written that way means the model of the
// frame is wrong, whatever else looks right.
func TestLocatorRefusesAFrameItCannotReproduce(t *testing.T) {
	t.Parallel()

	sql := "SELECT " + strings.Repeat("f", 235) + " FROM dual"
	require.Len(t, sql, 0xFC, "exactly the byte the short form may not carry")

	body := thinExecCLR(sql)

	_, ok := locateStatementRewrite(body, false)
	assert.False(t, ok,
		"a 0xFC short-form prefix is not something this package writes, so it is not something it rewrites")
}

// TestLocatorRefusesANonStatementRun keeps a run of bind values or a client
// identifier from answering as the statement.
func TestLocatorRefusesANonStatementRun(t *testing.T) {
	t.Parallel()

	notSQL := "jdbc:oracle:thin:@//db.example.com:1521/FREEPDB1"
	body := thinExecCLR(notSQL)

	_, ok := locateStatementRewrite(body, false)
	assert.False(t, ok, "a run that does not open with a SQL verb is not a statement")
}

// TestRewriteSingleChunkLongFormIsNotReadAsABareRun is the regression for the
// one failure a real 23ai found that no byte-level check could: a client whose
// chunk size exceeds the statement writes it as *one* chunk, so its text sits
// contiguously in the payload and the contiguous scan finds it — as a bare run,
// with the chunk's own length prefix left outside the span the rewriter touches.
//
// Rewriting that leaves a chunk header still declaring the old length in front
// of a longer statement, and Oracle answers `ORA-03120: two-task conversion
// routine: integer overflow`. Note that the identity check passes on such a
// frame: re-encoding the *same* value reproduces it either way. Only reading the
// framing first catches it.
func TestRewriteSingleChunkLongFormIsNotReadAsABareRun(t *testing.T) {
	t.Parallel()

	// go-ora chunks at 32767 bytes once the server advertises UseBigClrChunks, so
	// anything under that is one chunk; without the capability the chunk size is
	// small enough that only a statement at the long form's own floor fits in one.
	for _, tc := range []struct {
		name      string
		sql       string
		bigChunks bool
	}{
		{"big chunks, 20KB in one chunk", "SELECT " + strings.Repeat("g", 20000) + " FROM dual", true},
		{"single-byte chunk lengths", "SELECT " + strings.Repeat("g", 235) + " FROM dual", false},
	} {
		body := thinExecFrame(tc.sql, encodeChunkedCLR([]byte(tc.sql), len(tc.sql), tc.bigChunks))

		rw, ok := locateStatementRewrite(body, tc.bigChunks)
		require.True(t, ok, tc.name)
		require.Equal(t, stmtClrChunked, rw.clrKind,
			"%s: a single-chunk long form is still the long form", tc.name)
		require.Equal(t, byte(0xFE), body[rw.valueAt],
			"%s: the span being rewritten must start at the chunk marker, not inside it", tc.name)
		assert.Equal(t, tc.sql, rw.text(), tc.name)

		out := rewriteWith(t, body, tagOf(52), tc.bigChunks)

		back, ok := locateStatementRewrite(out, tc.bigChunks)
		require.True(t, ok, tc.name)
		assert.Equal(t, tagOf(52)+tc.sql, back.text(),
			"%s: the chunk header must declare the new length, not the old one", tc.name)
	}
}

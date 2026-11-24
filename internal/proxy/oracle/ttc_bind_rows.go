package oracle

import (
	"github.com/fclairamb/dbbat/internal/store"
)

// maxCapturedBindRows bounds how many bind rows of one array execution are
// stored. Past it the batch is recorded as its first rows plus its real size
// (store.QueryParameters.BatchTruncated), never not at all.
const maxCapturedBindRows = 1000

// ttcMsgRowData opens one row of bind values in an execute message, the same
// 0x07 that carries a row in a response (ttcMsgBindOutput).
const ttcMsgRowData = 0x07

// execOptionFetch is the execute-options bit asking the server to fetch rows
// on the same round trip. On such a call al8i4[1] is the number of rows to
// prefetch, not the number of executions, and the client sends one bind row.
const execOptionFetch = 0x40

// execBinds is the bind data of one execution: one row per iteration, in wire
// order. An ordinary execute is one row; an array execution (python-oracledb's
// executemany, go-ora's bulk bind) is one row per iteration it ran.
//
// rows keeps at most maxCapturedBindRows of them and count says how many there
// were.
type execBinds struct {
	rows  [][]string
	count int
}

// singleBindRow wraps the binds of an ordinary, one-iteration execution.
func singleBindRow(values []string) execBinds {
	if len(values) == 0 {
		return execBinds{}
	}

	return execBinds{rows: [][]string{values}, count: 1}
}

// first is the execution's first bind row, nil when it bound nothing.
func (b execBinds) first() []string {
	if len(b.rows) == 0 {
		return nil
	}

	return b.rows[0]
}

// parameters renders the binds the way they are stored: Values is the first
// row, and an execution that ran more than one iteration also carries the whole
// batch.
func (b execBinds) parameters() *store.QueryParameters {
	values := b.first()
	if len(values) == 0 {
		return nil
	}

	params := &store.QueryParameters{Values: values}

	if b.count > 1 {
		params.Batch = b.rows
		params.BatchSize = b.count
		params.BatchTruncated = b.count > len(b.rows)
	}

	return params
}

// readBindRows reads data as a run of bind rows, each the 0x07 row-data marker
// followed by count values, and succeeds only when the run consumes data to the
// byte. That exactness is what makes a hit a reading rather than a pattern that
// happened to match, as it is for readLenPrefixedValues.
//
// A value is a CLR: a length byte and that many bytes, 0 for NULL, or the long
// form (0xFE, chunks, a zero-length terminator) a value past 252 bytes takes.
// Chunk lengths are TTC compressed integers when the session negotiated the big
// CLR chunk form, which every server a thin client supports does, and single
// bytes otherwise; both are tried, the exact-consumption rule deciding.
//
// It returns at most maxCapturedBindRows rows and the total it walked.
func readBindRows(data []byte, count int) (execBinds, bool) {
	if count <= 0 || len(data) == 0 {
		return execBinds{}, false
	}

	for _, bigChunks := range [...]bool{true, false} {
		if binds, ok := readBindRowsAs(data, count, bigChunks); ok {
			return binds, true
		}
	}

	return execBinds{}, false
}

func readBindRowsAs(data []byte, count int, bigChunks bool) (execBinds, bool) {
	var binds execBinds

	offset := 0

	for offset < len(data) {
		values, next, ok := readBindRow(data, offset, count, bigChunks)
		if !ok {
			return execBinds{}, false
		}

		if len(binds.rows) < maxCapturedBindRows {
			binds.rows = append(binds.rows, values)
		}

		binds.count++
		offset = next
	}

	return binds, binds.count > 0
}

// readBindRow reads one 0x07-led row of count values at data[offset].
func readBindRow(data []byte, offset, count int, bigChunks bool) ([]string, int, bool) {
	if offset >= len(data) || data[offset] != ttcMsgRowData {
		return nil, 0, false
	}

	offset++

	values := make([]string, 0, count)

	for range count {
		if offset >= len(data) {
			return nil, 0, false
		}

		if data[offset] == 0xFF || data[offset] == 0xFD {
			return nil, 0, false
		}

		raw, n := readCLRVariant(data[offset:], bigChunks)
		if n == 0 {
			return nil, 0, false
		}

		values = append(values, decodeBindValue(raw))
		offset += n
	}

	return values, offset, true
}

// findBindRowsFromTail locates the bind rows at the end of an execute message
// whose start is not known — the parse, where bind metadata of no fixed size
// stands between the statement and the values — and reads exactly want rows.
//
// The candidate start is walked back from the tail, so the tightest run is
// found before any metadata byte that happens to be a 0x07; rows from the tail
// are memoized, so a large batch costs one pass rather than one per candidate.
// lo bounds the scan so it never walks into the statement text.
func findBindRowsFromTail(payload []byte, lo, count, want int) (execBinds, bool) {
	if count <= 0 || want <= 0 || lo < 0 || lo >= len(payload) {
		return execBinds{}, false
	}

	for _, bigChunks := range [...]bool{true, false} {
		if start, ok := bindRowsStartFromTail(payload, lo, count, want, bigChunks); ok {
			return readBindRowsAs(payload[start:], count, bigChunks)
		}
	}

	return execBinds{}, false
}

// bindRowsStartFromTail returns the offset at which exactly want rows of count
// values run to the end of payload.
func bindRowsStartFromTail(payload []byte, lo, count, want int, bigChunks bool) (int, bool) {
	// rowsFrom[i] is the number of rows that run from payload[lo+i] exactly to
	// the end, 0 when none does.
	rowsFrom := make([]int, len(payload)-lo+1)

	for p := len(payload) - 1; p >= lo; p-- {
		if payload[p] != ttcMsgRowData {
			continue
		}

		_, next, ok := readBindRow(payload, p, count, bigChunks)
		if !ok {
			continue
		}

		switch {
		case next == len(payload):
			rowsFrom[p-lo] = 1
		case rowsFrom[next-lo] > 0:
			rowsFrom[p-lo] = rowsFrom[next-lo] + 1
		default:
			continue
		}

		if rowsFrom[p-lo] == want {
			return p, true
		}
	}

	return 0, false
}

// execIterations reads how many iterations a thin `03 5e` execute declares
// (al8i4[1], the array the header points at, which follows the statement text)
// and whether the call also fetches. body begins at the exec op header, and
// sqlEnd is the offset just past the statement text in it.
//
// It is the compressed (thin) encoding only: an OCI header is fixed-width, and
// a compressed walk over it reads nothing that means anything.
func execIterations(body []byte, sqlEnd int, wide64 bool) (int, bool, bool) {
	if wide64 || sqlEnd <= 0 || sqlEnd >= len(body) {
		return 0, false, false
	}

	if _, wide := execSQLLengthWideField(body); wide {
		return 0, false, false
	}

	header, ok := execThinHeader(body)
	if !ok {
		return 0, false, false
	}

	// al8i4[0] is the parse flag; al8i4[1] the execution count.
	_, n := readCompressedInt(body[sqlEnd:])
	if n == 0 {
		return 0, false, false
	}

	iterations, m := readCompressedInt(body[sqlEnd+n:])
	if m == 0 || iterations <= 0 {
		return 0, false, false
	}

	return iterations, header.options&execOptionFetch != 0, true
}

// execNoStatementTailFields is how many single-zero fields stand between the
// 12.2 oaccolid pointers' head (the dml-rowcounts pair) and the end of the thin
// exec header, in the shape a plain execution writes them. See
// execNoStatementIterations.
const execNoStatementTailFields = 8

// execNoStatementIterations is execIterations for a thin `03 5e` that declares
// no statement: with no text to anchor on, the header is walked field by field
// to its end — python-oracledb `_write_execute_message`: options, cursor id,
// the cursor flag, the sql length (0), the al8i4 pointer and its length 13, the
// al8o4 pair, the prefetch sizes, the binds pointer and count, the al8app..al8kvl
// pointers, the defines, registration, object, blv and dnam fields, the
// dml-rowcounts pair and the 12.2 tail — and al8i4[0] (the parse flag) and
// al8i4[1] (the execution count) are read right behind it.
//
// It returns the iterations, whether the call also fetches (then al8i4[1] is a
// prefetch size, not a batch), and the offset just past al8i4[1], where the
// bind rows can start no earlier than.
//
// It answers false for anything that does not walk exactly as written above (a
// non-zero field where an empty one is expected is a shape this reading was not
// built for), and the caller falls back to the one row it can find.
func execNoStatementIterations(body []byte) (execNoStatementBatch, bool) {
	header, ok := execThinHeader(body)
	if !ok || header.sqlLen.value != 0 {
		return execNoStatementBatch{}, false
	}

	pos := header.sqlLen.at + header.sqlLen.width

	// ptr marks a one-byte pointer flag, u a compressed integer.
	type field struct {
		ptr  bool
		want int // expected value, -1 for any
	}

	fields := []field{
		{true, 1}, {false, 13}, // al8i4 pointer, length
		{true, 0}, {true, 0}, // al8o4, al8o4l
		{false, -1}, {false, -1}, {false, -1}, // prefetch buffer, prefetch rows, max long
		{true, -1}, {false, -1}, // binds pointer, count
		{true, 0}, {true, 0}, {true, 0}, {true, 0}, {true, 0}, // al8app, al8txn, al8txl, al8kv, al8kvl
		{true, 0}, {false, 0}, {false, 0}, // al8doac, defines, registration id
		{true, 0}, {true, 1}, {true, 0}, {false, 0}, // objlist, objlen, blv, blvl
		{true, 0}, {false, 0}, {false, 0}, // dnam, dnaml, regid_msb
		{true, 0}, {false, 0}, // al8pidmlrc, its length
	}

	for range execNoStatementTailFields {
		fields = append(fields, field{true, 0})
	}

	for _, f := range fields {
		if pos >= len(body) {
			return execNoStatementBatch{}, false
		}

		value, n := int(body[pos]), 1
		if !f.ptr {
			value, n = readCompressedInt(body[pos:])
			if n == 0 {
				return execNoStatementBatch{}, false
			}
		}

		if f.want >= 0 && value != f.want {
			return execNoStatementBatch{}, false
		}

		pos += n
	}

	_, n := readCompressedInt(body[pos:])
	if n == 0 {
		return execNoStatementBatch{}, false
	}

	pos += n

	iterations, m := readCompressedInt(body[pos:])
	if m == 0 || iterations <= 0 {
		return execNoStatementBatch{}, false
	}

	return execNoStatementBatch{iterations: iterations, fetch: header.options&execOptionFetch != 0, rowsAt: pos + m}, true
}

// execNoStatementBatch is what execNoStatementIterations reads off a header.
type execNoStatementBatch struct {
	iterations int
	fetch      bool // the call also fetches: iterations is a prefetch size
	rowsAt     int  // offset just past al8i4[1], where the bind rows start
}

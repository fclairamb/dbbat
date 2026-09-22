package oracle

// The client's define block, and the one thing dbbat reads out of it: whether
// this fetch's LOB columns come back as locators or as their own bytes.
//
// A LOB column's row value has two shapes on the thin dialect, and **nothing in
// the server's own frames tells them apart** — the column records of a locator
// fetch and an inlined one are byte-identical, because the difference was asked
// for on the client's side of the wire. Measured on three recordings of one
// query:
//
//	go-ora, default          testdata/go_ora_lob.pcapng         the LOB's own bytes
//	go-ora, lob fetch=post   testdata/go_ora_lob_stream.pcapng  a locator
//	python-oracledb thin     testdata/python_thin_lob.pcapng    a locator
//	JDBC thin, default       testdata/jdbc_thin_lob.pcapng      a locator, prefetched
//
// The fourth is the one that costs the rule its exact-match shape: ojdbc
// re-declares the ordinary CHAR columns as VARCHAR2 although it is changing
// none of them, so defineTypeAgrees carries that pair as a second measured
// substitution (one-way, 96 → 1). What the frame then says is *locators* —
// JDBC keeps every LOB column the LOB type it already was — which is the same
// answer an unread define falls back to, so the relaxation changed what dbbat
// learns and not what it captures. Its rows carry the LOB's head in front of
// the locator as well, which is a row-walk concern rather than a define one;
// see skipPrefetchedLOBValue.
//
// The ask is a **define block**: an execute that declares no statement and
// carries one entry per column of the cursor already described. A client that
// wants the bodies re-declares each LOB column as a LONG type — go-ora's
// queryLobPrefetch turns CLOB into LONG VARCHAR (94) and BLOB into LONG RAW
// (24) — and what comes back is then a LONG column's value, not a locator at
// all. A client that wants locators either re-declares them as the LOB types
// they already are (python-oracledb thin) or sends no define whatsoever
// (go-ora's `lob fetch=post`).
//
// So the locator is what the protocol sends unless it was asked otherwise, and
// that asymmetry is what makes this readable: **inlining is something a client
// has to request, and dbbat reads the request rather than the rows.** A define
// dbbat cannot walk leaves the fetch on the locator reading, where a wrong walk
// costs the row (rowEndsAtMarker) rather than filling it with framing bytes.
//
// The walk is bounded the way every other reading in this package is: the
// entries have to be exactly as many as the describe said, they have to fill
// the frame to its last byte, and each one's type has to be the type the
// describe gave or one of the two measured substitutions defineTypeAgrees
// lists. Exactly one start offset may satisfy all three, or nothing is learned.

// lobRowShape is how a LOB column's value is laid out in this session's rows on
// the compressed dialect. The zero value is the locator, which is what the
// server sends when the client asked for nothing.
type lobRowShape int

const (
	// lobRowLocator is the default: a size, optionally the LOB's own size and
	// chunk size, and then the 40-byte locator as a CLR. See
	// readCompressedLOBLocatorColumn.
	lobRowLocator lobRowShape = iota

	// lobRowInline is what a client that re-declared its LOB columns as LONG
	// gets: the value itself, followed by the column's indicator and return
	// code. See readInlineLongColumn.
	lobRowInline
)

// TTC type codes a client substitutes for a LOB column when it wants the bytes
// inlined. None of them is a code a describe reports for a LOB, which is what
// makes the substitution legible.
const (
	tnsTypeLONG        = 8
	tnsTypeLongVarChar = 94
)

// defineEntryFixedPrefix is the DataType/Flag/Precision/Scale quartet every
// define entry opens with. Only the first byte is read here; the other three
// are stepped over.
const defineEntryFixedPrefix = 4

// execDefineLOBShape reads the LOB reading a thin client asked for out of its
// own define block, given the column types the describe reported for the cursor
// this define re-declares.
//
// It reports false when the frame is not a define, when it does not walk, or
// when the cursor has no LOB column to have an opinion about — all of which
// leave the caller on the locator reading.
func execDefineLOBShape(ttcPayload []byte, describeTypes []int) (lobRowShape, bool) {
	body, start, ok := execDefineEntriesStart(ttcPayload)
	if !ok {
		return lobRowLocator, false
	}

	defined, ok := execDefineColumnTypes(body, start, describeTypes)
	if !ok {
		return lobRowLocator, false
	}

	return lobShapeFromDefinedTypes(describeTypes, defined)
}

// lobShapeFromDefinedTypes turns a walked define block into the one bit the row
// walk needs.
//
// It requires the LOB columns to agree with each other. A client that inlined
// some and not others would need a per-column reading, and no recording shows
// one: go-ora substitutes for every LOB column of the select list or for none.
// Rather than guess at the mixed case it is left unlearned, which is the
// locator reading and therefore a refused row rather than a wrong value.
func lobShapeFromDefinedTypes(describeTypes, defined []int) (lobRowShape, bool) {
	var inlined, kept int

	for i, describeType := range describeTypes {
		if !isLOBTypeCode(describeType) {
			continue
		}

		if isLongTypeCode(defined[i]) {
			inlined++
		} else {
			kept++
		}
	}

	switch {
	case inlined > 0 && kept == 0:
		return lobRowInline, true
	case kept > 0 && inlined == 0:
		return lobRowLocator, true
	}

	return lobRowLocator, false
}

// execDefineEntriesStart locates the exec op of a frame that declares no
// statement, and returns it with the offset its header ends at — the earliest
// byte a define entry could start on.
//
// Only the thin encoding is read. The two OCI dialects spell a LOB column with
// a header of their own (readFixedLOBColumn), so nothing there depends on this,
// and offering their frames a second header layout to be mistaken for is the
// near-miss every reading in this package is bounded against.
func execDefineEntriesStart(ttcPayload []byte) ([]byte, int, bool) {
	if body, start, ok := execDefineEntriesStartAt(ttcPayload); ok {
		return body, start, true
	}

	if end, ok := closeCursorsEnd(ttcPayload); ok && end < len(ttcPayload) {
		body, start, ok := execDefineEntriesStartAt(ttcPayload[end:])

		return body, start, ok
	}

	return nil, 0, false
}

// execDefineEntriesStartAt is execDefineEntriesStart for a payload that must
// already begin at the exec op header.
func execDefineEntriesStartAt(body []byte) ([]byte, int, bool) {
	if !isPiggybackExecHeader(body) || len(body) < execHeaderMinLen {
		return nil, 0, false
	}

	// A frame carrying a statement is a parse, not a define: go-ora's own
	// prefetch re-execution clears stmt.parse before it sets stmt.define, and
	// python-oracledb thin defines on a cursor it has already described. The
	// wide headers are refused here by construction — execThinHeader walks the
	// thin one only.
	header, ok := execThinHeader(body)
	if !ok || header.sqlLen.value != 0 {
		return nil, 0, false
	}

	return body, header.sqlLen.at + header.sqlLen.width, true
}

// execDefineColumnTypes walks the define entries and returns the TTC type code
// each one declares, in column order.
//
// The start offset is found rather than computed: the fields between the
// statement-length field and the entries are the execute's own options, and
// their widths vary with what the client is asking for. So every offset from
// the header's end on is tried, and one is accepted only when the entries
// walked from it are exactly as many as the describe named, end exactly on the
// frame's last byte, and each declare a type the describe agrees with. A frame
// where two offsets manage that is refused rather than resolved — see
// TestDefineBlockIsRefusedWhenMoreThanOneOffsetWalks.
func execDefineColumnTypes(body []byte, from int, describeTypes []int) ([]int, bool) {
	numCols := len(describeTypes)
	if numCols == 0 || from >= len(body) {
		return nil, false
	}

	var (
		found []int
		hits  int
	)

	for start := from; start < len(body); start++ {
		types, ok := walkDefineEntries(body, start, describeTypes)
		if !ok {
			continue
		}

		hits++
		if hits > 1 {
			return nil, false
		}

		found = types
	}

	return found, hits == 1
}

// walkDefineEntries reads len(describeTypes) entries from body[start:] and
// requires them to end on the frame's last byte.
func walkDefineEntries(body []byte, start int, describeTypes []int) ([]int, bool) {
	types := make([]int, len(describeTypes))
	at := start

	for i, describeType := range describeTypes {
		defined, next, ok := readDefineEntry(body, at)
		if !ok || !defineTypeAgrees(describeType, defined) {
			return nil, false
		}

		types[i] = defined
		at = next
	}

	if at != len(body) {
		return nil, false
	}

	return types, true
}

// defineTypeAgrees reports whether a define entry's type is one the describe's
// type can turn into.
//
// The rule is deliberately tight. A client mostly echoes the describe back for
// every column it is not changing, so anything other than the same code is
// usually a walk that landed on the wrong bytes. The exceptions are a **table**
// rather than a rule, and each pair in it was read off a recording:
//
//   - a LOB column re-declared as a LONG (the substitution this whole reading
//     exists to see), measured on testdata/go_ora_lob.pcapng;
//   - a CHAR (96) column re-declared as a VARCHAR2 (1), measured on
//     testdata/jdbc_thin_lob.pcapng, where ojdbc spells every ordinary CHAR
//     column that way although it is changing none of them.
//
// Both are **one-way**. A CHAR declared as a LONG is not a LOB being inlined,
// and a VARCHAR2 column re-declared as a CHAR is something no recording shows —
// accepting either direction would widen the walk's per-byte search back toward
// the near-miss the exact-match rule exists to close.
func defineTypeAgrees(describeType, defined int) bool {
	if describeType == defined {
		return true
	}

	if isLOBTypeCode(describeType) && isLongTypeCode(defined) {
		return true
	}

	// ojdbc's scalar spelling, one-way: 96 may be declared 1, never the reverse.
	return describeType == tnsTypeCHAR && defined == tnsTypeVARCHAR
}

// isLOBTypeCode reports whether a describe's type code is one of the three
// whose row value is a locator rather than a datum.
func isLOBTypeCode(t int) bool {
	switch t {
	case tnsTypeCLOB, tnsTypeBLOB, tnsTypeBFILE:
		return true
	}

	return false
}

// isLongTypeCode reports whether a define entry's type code is a LONG — the
// substitution a client makes when it wants a LOB's bytes in the row.
func isLongTypeCode(t int) bool {
	switch t {
	case tnsTypeLONG, tnsTypeLONGRAW, tnsTypeLongVarChar:
		return true
	}

	return false
}

// readDefineEntry reads one define entry and returns the type code it declares
// with the offset behind it.
//
// The layout is the column-definition record a thin client writes, and it is
// the same one on both drivers measured (go-ora's ParameterInfo.write,
// python-oracledb thin's _write_column_metadata):
//
//	byte   DataType          <- the only field read
//	byte   Flag
//	byte   Precision
//	byte   Scale
//	cint   MaxLen
//	cint   ArraySize
//	cint   ContFlag
//	cint   len(ToID)         <- 0 is the single byte a nil ToID writes
//	CLR    ToID              <- present only when that length is non-zero
//	cint   Version
//	cint   CharsetID
//	byte   CharsetForm
//	cint   MaxCharLen
//	cint   oaccollid
func readDefineEntry(body []byte, at int) (int, int, bool) {
	if at < 0 || at+defineEntryFixedPrefix > len(body) {
		return 0, 0, false
	}

	dataType := int(body[at])
	at += defineEntryFixedPrefix

	// MaxLen, ArraySize, ContFlag
	for range 3 {
		next, ok := skipCompressedInt(body, at)
		if !ok {
			return 0, 0, false
		}

		at = next
	}

	toIDLen, n := readCompressedInt(body[at:])
	if n == 0 {
		return 0, 0, false
	}

	at += n

	if toIDLen > 0 {
		_, consumed := readCLR(body[at:])
		if consumed == 0 {
			return 0, 0, false
		}

		at += consumed
	}

	// Version, CharsetID
	for range 2 {
		next, ok := skipCompressedInt(body, at)
		if !ok {
			return 0, 0, false
		}

		at = next
	}

	if at >= len(body) {
		return 0, 0, false
	}

	at++ // CharsetForm

	// MaxCharLen, oaccollid
	for range 2 {
		next, ok := skipCompressedInt(body, at)
		if !ok {
			return 0, 0, false
		}

		at = next
	}

	return dataType, at, true
}

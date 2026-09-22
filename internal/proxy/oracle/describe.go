package oracle

// Oracle TTC internal type codes (TNSType) carried in a column's describe
// record. Only the ones dbbat distinguishes are named; the value is what the
// describe parser returns as columnDef.TypeCode.
const (
	tnsTypeVARCHAR     = 1
	tnsTypeNUMBER      = 2
	tnsTypeDATE        = 12
	tnsTypeRAW         = 23
	tnsTypeLONGRAW     = 24
	tnsTypeCHAR        = 96
	tnsTypeBINFLOAT    = 100 // IBFloat
	tnsTypeBINDOUBLE   = 101 // IBDouble
	tnsTypeTSDTY       = 180 // TimeStampDTY
	tnsTypeTSTZDTY     = 181 // TimeStampTZ_DTY
	tnsTypeIntervalDSD = 183 // IntervalDS_DTY
	tnsTypeTIMESTAMP   = 187
	tnsTypeTIMESTAMPTZ = 188
	tnsTypeIntervalDS  = 190
	tnsTypeTSLTZDTY    = 231 // TimeStampLTZ_DTY
	tnsTypeTSLTZ       = 232 // TimeStampeLTZ
)

// numberScaleTypes are the data types whose describe record encodes scale as a
// compressed int (2 bytes max) rather than a single byte — see
// ParameterInfo.load in go-ora.
var numberScaleTypes = map[int]bool{
	tnsTypeNUMBER:      true,
	tnsTypeTSDTY:       true,
	tnsTypeTSTZDTY:     true,
	tnsTypeIntervalDSD: true,
	tnsTypeTIMESTAMP:   true,
	tnsTypeTIMESTAMPTZ: true,
	tnsTypeIntervalDS:  true,
	tnsTypeTSLTZDTY:    true,
	tnsTypeTSLTZ:       true,
}

// columnDesc is a column's identity from its describe record: its name and TTC
// type code (see the tnsType* constants).
type columnDesc struct {
	Name string
	Type int
}

// parseColumnDescribes decodes the per-column definition records from a describe
// (func 0x10) payload and returns one columnDesc per column. It is conservative:
// it returns nil — so callers fall back to heuristic name scanning — whenever
// the payload is not a describe, the column count is implausible, a record runs
// off the end, or a decoded type code is not a known TNSType (a strong signal of
// a misaligned parse). A clean parse therefore yields trustworthy names+types.
//
// The encoding comes from `shape` — the session's learned oerShape — and never
// from anything in the payload. `fixedWidth` is the OCI encoding and
// `fixedWidth64` its 64-bit variant, which is a third layout rather than the
// second one at wider offsets; see parseColumnDescribeWide64.
func parseColumnDescribes(ttcPayload []byte, shape oerShape) []columnDesc {
	// The 64-bit dialect has one layout, not two. The classic/modern retry below
	// exists to tell apart two spellings of the record's *version-gated* trailing
	// fields, and this dialect ends its record in a single measured span instead
	// (wide64ColumnRecordTailLen) — so there is nothing to retry it with, and
	// offering the payload a second reading would only be a second chance at a
	// plausible-looking column list.
	if shape.fixedWidth64 {
		return parseColumnDescribesMode(ttcPayload, false, shape)
	}

	// The per-column record has version-dependent trailing fields. Classic
	// servers / lower-TTCVersion clients (go-ora, python-oracledb thin) end the
	// record after two trailing ints; modern ones (Oracle 23ai negotiating a high
	// TTCVersion with SQLcl/ojdbc 26.x) append data-use-case domain DLCs, an
	// annotations block, and three further ints. Try the classic layout first
	// (so thin clients never regress); if it misaligns — a record runs off the
	// end or yields an unknown TTC type — retry with the modern layout.
	if cols := parseColumnDescribesMode(ttcPayload, false, shape); cols != nil {
		return cols
	}

	return parseColumnDescribesMode(ttcPayload, true, shape)
}

// parseColumnDescribesMode decodes the column records using either the classic
// (modern=false) or modern (modern=true) per-record trailing layout. Returns nil
// — so the caller falls back / retries — whenever the payload is not a describe,
// the count is implausible, a record runs off the end, or a decoded type code is
// not a known TNSType (a strong signal of a misaligned parse).
func parseColumnDescribesMode(ttcPayload []byte, modern bool, shape oerShape) []columnDesc {
	count, start, ok := describeWireLayout(ttcPayload, shape)
	if !ok || count <= 0 || count > 1000 {
		return nil
	}

	c := &dcursor{buf: ttcPayload, pos: start, wide: shape.fixedWidth, wide64: shape.fixedWidth64}
	cols := make([]columnDesc, 0, count)

	for range count {
		name, typ := parseColumnDescribe(c, modern)
		if c.err || !isKnownTNSType(typ) {
			return nil
		}

		cols = append(cols, columnDesc{Name: name, Type: typ})
	}

	return cols
}

// describeWireLayout is describeColumnLayout for whichever encoding the session
// speaks.
func describeWireLayout(ttc []byte, shape oerShape) (int, int, bool) {
	switch {
	case shape.fixedWidth64:
		return describeColumnLayoutWide64(ttc)
	case shape.fixedWidth:
		return describeColumnLayoutWide(ttc)
	default:
		return describeColumnLayout(ttc)
	}
}

// describeColumnLayoutWide is describeColumnLayout for the fixed-width OCI
// encoding:
//
//	[0x10] [size ub4] [size bytes] [maxRowSize ub4] [colCount ub4] [1 skip byte] [records...]
//
// Everything it does differently is measured rather than assumed, on sqlplus
// describes recorded through the capture relay against 23ai
// (testdata/oci_describe.hex): the prefix's own length is a four-byte
// little-endian field where the compressed header has a single byte, and so are
// the two integers after it. The prefix stays raw bytes rather than a CLR — the
// 23 bytes it carried were a 16-byte identifier followed by a 7-byte Oracle
// DATE, with no length marker in front of them.
func describeColumnLayoutWide(ttc []byte) (int, int, bool) {
	if len(ttc) < 3 || ttc[0] != byte(TTCFuncQueryResult) {
		return 0, 0, false
	}

	c := &dcursor{buf: ttc, pos: 1, wide: true}

	size := c.intw(4)
	if c.err || size < 0 || c.pos+size > len(ttc) {
		return 0, 0, false
	}

	c.pos += size

	c.intw(4) // maxRowSize

	count := c.intw(4)
	if c.err || count <= 0 {
		return 0, 0, false
	}

	c.pos++ // the byte after colCount precedes the first record

	if c.err || c.pos > len(ttc) {
		return 0, 0, false
	}

	return count, c.pos, true
}

// describeColumnLayoutWide64 is describeColumnLayout for the **64-bit** OCI
// encoding, and it is deliberately the 4-byte one unchanged:
//
//	[0x10] [size ub4] [size bytes] [maxRowSize ub4] [colCount ub4] [1 skip byte] [records...]
//
// The extra byte this dialect writes before the first column record is real —
// it was the first thing measured about it — but it is **not** a header field.
// It is the first record's own lead byte, and every record has one (see
// parseColumnDescribeWide64). That is not a preference between two framings: it
// is the only one under which every record in the recording is the same shape.
// Read as a header byte plus a *trailing* flag per record, the eight records of
// testdata/oci64_describe.hex come out with two different tail lengths — 25
// bytes on the seven scalar columns, 23 on the object one — for no reason any
// field could supply. Read as a lead byte, all eight tails are 24 and the whole
// record differs only in its DLC payloads, which is what a wire format looks
// like.
//
// So this function exists to say that, and to keep the three encodings' entry
// points side by side; it delegates rather than duplicating the walk.
func describeColumnLayoutWide64(ttc []byte) (int, int, bool) {
	return describeColumnLayoutWide(ttc)
}

// describeColumnLayout walks the describe header and returns the column count
// and the offset of the first column-definition record:
//
//	[0x10] [size] [size bytes] [maxRowSize cint] [colCount cint] [1 skip byte] [records...]
func describeColumnLayout(ttc []byte) (int, int, bool) {
	if len(ttc) < 3 || ttc[0] != byte(TTCFuncQueryResult) {
		return 0, 0, false
	}

	pos := 1
	size := int(ttc[pos])
	pos += 1 + size // skip the size byte and the size-bytes prefix

	if pos >= len(ttc) {
		return 0, 0, false
	}

	_, n1 := readCompressedInt(ttc[pos:]) // maxRowSize
	if n1 == 0 {
		return 0, 0, false
	}

	pos += n1

	count, n2 := readCompressedInt(ttc[pos:])
	if n2 == 0 || count <= 0 {
		return 0, 0, false
	}

	pos += n2
	pos++ // the byte after colCount precedes the first record

	if pos > len(ttc) {
		return 0, 0, false
	}

	return count, pos, true
}

// tnsTypeOPAQUE is the type code 23ai reports for an opaque type — a
// `SYS.XMLTYPE` column is one. It sits in a gap the ranges below leave open,
// and it is named rather than folded into one of them because it is the only
// value measured in there: it came out of the `X` column of
// testdata/oci64_describe.hex, whose record also carries `SYS`, `XMLTYPE` and a
// 16-byte type id, all read at the offsets the surrounding columns pin.
//
// Until that column was recorded, a describe naming it failed isKnownTNSType
// and the *whole* describe was discarded — under every one of the three
// encodings, not just this one — so a query with an XMLTYPE column anywhere in
// its select list captured its rows under scanAndPadColumnNames' guesses.
const tnsTypeOPAQUE = 58

// isKnownTNSType reports whether t is a defined TTC type code. The ranges cover
// the full TNSType enumeration; an out-of-range value means the record parse
// drifted and the result must not be trusted.
func isKnownTNSType(t int) bool {
	switch {
	case t >= 1 && t <= 24:
		return true
	case t == tnsTypeOPAQUE:
		return true
	case t >= 60 && t <= 127:
		return true
	case t >= 155 && t <= 156:
		return true
	case t >= 180 && t <= 232:
		return true
	case t == 0xFC: // Boolean
		return true
	default:
		return false
	}
}

// dcursor is a forward, fail-safe byte cursor over a describe payload. Any
// out-of-bounds read sets err and makes subsequent reads no-ops, so a malformed
// record degrades to a parse failure instead of a panic.
//
// wide selects the **fixed-width OCI encoding**: the same field list, marshaled
// as little-endian integers of a per-call-site width instead of TTC compressed
// ones. It is set from the session's learned oerShape and never from the bytes
// being read — see refCursorIDsInBindOutput.
// wide64 narrows that to the **64-bit** variant of the same encoding. It is a
// third reading rather than the second one at wider offsets — see
// parseColumnDescribeWide64 — and like wide it is set from the session's learned
// oerShape (fixedWidth64) and never from the bytes.
type dcursor struct {
	buf    []byte
	pos    int
	err    bool
	wide   bool
	wide64 bool
}

// skip advances past n bytes that are read by no field, failing the cursor if
// they are not there. It is how a measured span is consumed — a run whose field
// boundaries no recording separates, but whose total is pinned by what sits on
// either side of it.
func (c *dcursor) skip(n int) {
	if c.err || n < 0 || c.pos+n > len(c.buf) {
		c.err = true

		return
	}

	c.pos += n
}

func (c *dcursor) byte() int {
	if c.err || c.pos >= len(c.buf) {
		c.err = true

		return 0
	}

	v := int(c.buf[c.pos])
	c.pos++

	return v
}

// cint reads a TTC compressed integer: a length byte then that many big-endian
// value bytes. A length byte with the high bit set marks a negative value (e.g.
// the -127 NUMBER float-scale sentinel) — the low 7 bits are the byte count and
// the result is negated, matching the driver's GetInt.
func (c *dcursor) cint() int {
	if c.err || c.pos >= len(c.buf) {
		c.err = true

		return 0
	}

	size := int(c.buf[c.pos])
	c.pos++

	neg := false
	if size&0x80 != 0 {
		neg = true
		size &= 0x7f
	}

	if size == 0 {
		return 0
	}

	if size > 8 || c.pos+size > len(c.buf) {
		c.err = true

		return 0
	}

	v := 0
	for i := 0; i < size; i++ {
		v = v<<8 | int(c.buf[c.pos+i])
	}

	c.pos += size

	if neg {
		v = -v
	}

	return v
}

// intw reads an integer of the stated **wire width**: that many little-endian
// bytes in the fixed-width OCI encoding, a self-sizing TTC compressed integer
// otherwise (where width is ignored, exactly as go-ora's `GetInt(size, …)`
// ignores its size argument once the session negotiated compression).
//
// The width therefore has to come from the call site, and every caller below
// names the one go-ora reads that field with. It is the whole difference between
// the two encodings: a compressed int carries its own length, a fixed-width one
// does not, so nothing here can be inferred from the payload.
func (c *dcursor) intw(width int) int {
	if !c.wide {
		return c.cint()
	}

	if c.err || width <= 0 || width > 8 || c.pos+width > len(c.buf) {
		c.err = true

		return 0
	}

	v := 0
	for i := width - 1; i >= 0; i-- {
		v = v<<8 | int(c.buf[c.pos+i])
	}

	c.pos += width

	return v
}

// dlc reads a TTC data-length-coded value: a length then the CLR-encoded bytes
// (truncated to that length). Returns nil for an empty field.
//
// The length is go-ora's `GetDlc`, i.e. `GetInt(4, …)` — a compressed integer on
// a thin session, a four-byte little-endian one on an OCI session. The CLR that
// follows it is the same in both: measured on a describe record carrying a
// 16-byte object type OID, where the four-byte length 16 was followed by a
// 0x10 CLR marker and then the OID.
func (c *dcursor) dlc() []byte {
	length := c.intw(4)
	if c.err || length <= 0 {
		// A negative length means the parse has drifted (e.g. a NUMBER scale's
		// -127 sentinel was read where a length was expected). Treat it as an
		// empty/invalid field rather than slicing with a negative bound, which
		// would panic. parseColumnDescribes validates alignment and bails out.
		return nil
	}

	data, n := readCLR(c.buf[c.pos:])
	if n == 0 {
		c.err = true

		return nil
	}

	c.pos += n

	if len(data) > length {
		data = data[:length]
	}

	return data
}

// parseColumnDescribe parses one column-definition record (ParameterInfo.load)
// at the cursor, advancing past it, and returns the column name and TTC type
// code. The field order is fixed up to the name + the two trailing ints (the
// classic layout). When modern is true, the additional TTCVersion ≥ 17/20 fields
// are consumed too — data-use-case domain schema/name, an annotations block, and
// three further ints — which sets where the next record starts.
func parseColumnDescribe(c *dcursor, modern bool) (string, int) {
	if c.wide64 {
		return parseColumnDescribeWide64(c)
	}

	dataType := c.byte()
	c.byte() // flag
	c.byte() // precision

	switch {
	case c.wide:
		// One signed byte, whatever the type — the numberScaleTypes split below
		// exists because a compressed int is the only way to carry the -127
		// float sentinel, and the fixed-width encoding has no such trouble.
		// Measured on a NUMBER(10,2) (precision 10, scale 2 in consecutive
		// bytes) and on `1/3`, whose scale byte is 0x81 = -127.
		c.byte()
	case numberScaleTypes[dataType]:
		c.intw(2) // scale (compressed)
	default:
		c.byte() // scale
	}

	c.intw(4) // maxLen
	c.intw(4) // maxNoOfArrayElements

	// contFlag. go-ora reads it as `GetInt(8|4)`, but the OCI encoding spends
	// exactly **five** bytes on it and maxNoOfArrayElements together, and which
	// of the two owns the fifth cannot be decided from any recording: both are
	// zero in every column ever captured. What is decided is the total, and it
	// is decided by the fields on either side — the maxLen before it (4000 on a
	// VARCHAR2(4000), 22 on a NUMBER) and the type OID DLC after it, whose
	// four-byte length and 16-byte payload pin where it must end.
	c.intw(1)

	c.dlc()   // toID
	c.intw(2) // version
	c.intw(2) // charsetID
	c.byte()  // charsetForm (go-ora reads it uncompressed: one byte either way)
	c.intw(4) // maxCharLen
	c.intw(4) // oaccollid (TTCVersion ≥ 8, always true for modern servers)
	c.byte()  // allowNull
	c.byte()  // v7 name length (unused; the DLC below carries the real length)

	name := c.dlc() // column name
	c.dlc()         // schema name
	c.dlc()         // type name

	// Trailing version ints (TTCVersion ≥ 3 and ≥ 6). The classic layout ends
	// here; go-ora / python-oracledb thin negotiate this with the server.
	c.intw(2)
	c.intw(4)

	if modern {
		parseColumnDescribeModernTail(c)
	}

	return string(name), dataType
}

// The 64-bit OCI column record's three measured spans. Each is pinned by what
// sits on either side of it — a field whose value is non-zero in some recorded
// column — and none is decomposed further than the recordings decompose it.
const (
	// wide64ColumnRecordLeadLen is the one-byte flag every record opens with.
	// See describeColumnLayoutWide64 for why it belongs to the record rather
	// than to the header it first showed up in.
	//
	// Its **value** is set on every recorded column without a type OID and
	// clear on every column with one, all thirteen of them — but the walk reads
	// no meaning into it. The pad below keys on the OID's own length field
	// instead, which is a value this walk reads and then validates a CLR
	// against; a flag whose meaning is a correlation over one recording is not.
	wide64ColumnRecordLeadLen = 1

	// wide64ColumnArrayAndContFlagLen is maxNoOfArrayElements and contFlag
	// together, the way the 4-byte dialect spends five bytes on the pair
	// without saying which owns the fifth. Twelve here, and pinned: the type
	// OID's four-byte length sits exactly twelve bytes past maxLen on the
	// object column, whose maxLen (2000) and OID (16 bytes behind a 0x10 CLR
	// marker) are both real values.
	wide64ColumnArrayAndContFlagLen = 12

	// wide64ColumnEmptyTypeOIDPad is what an **absent** type OID costs beyond
	// its four-byte length: eleven bytes a present one does not spend. See
	// wide64TypeOID.
	wide64ColumnEmptyTypeOIDPad = 11

	// wide64ColumnRecordTailLen is everything after the type name: the two
	// version-gated trailing integers, the data-use-case domain schema and name,
	// and the annotation count — 18 bytes in the 4-byte dialect, 24 here. It is
	// a span rather than five fields because only its first is ever non-zero in
	// the corpus (the object column's `07`), so the rest have no boundaries to
	// measure. A column carrying a 23ai SQL domain or an annotation would extend
	// it and misalign the walk, which costs the describe (the caller falls back
	// to scanAndPadColumnNames) and never a wrong name.
	wide64ColumnRecordTailLen = 24
)

// parseColumnDescribeWide64 is parseColumnDescribe for the **64-bit** OCI
// dialect: the same field list as ParameterInfo.load, at this dialect's widths.
//
// It is a function of its own rather than a wider `intw` on the one above
// because five things differ, and four of them differ in a direction no width
// argument expresses:
//
//   - every record opens with a one-byte flag (describeColumnLayoutWide64);
//   - maxNoOfArrayElements and contFlag spend twelve bytes where the 4-byte
//     dialect spends five;
//   - `version` is **one** byte where the 4-byte dialect spends two, and
//     `charsetForm` is **two** where it spends one — measured on the object
//     column, whose version is 1, and on the VARCHAR2(4000), whose charset id
//     (873), maximum character length (4000) and collation id (16382) pin every
//     boundary around them;
//   - an absent type OID costs eleven bytes more than its length field
//     (wide64TypeOID);
//   - and the record ends in a 24-byte span rather than in the version-gated
//     fields the classic/modern retry exists to tell apart.
//
// Every width above is read off a column whose value for that field is
// **non-zero**. Runs of zeros decide nothing and were not asked to: they are
// bounded by the non-zero fields on either side of them, which is why the two
// spans that could not be split are spelled out as spans.
func parseColumnDescribeWide64(c *dcursor) (string, int) {
	c.skip(wide64ColumnRecordLeadLen)

	dataType := c.byte()
	c.byte() // flag
	c.byte() // precision
	c.byte() // scale — one signed byte, as in the 4-byte dialect

	c.intw(4) // maxLen
	c.skip(wide64ColumnArrayAndContFlagLen)

	wide64TypeOID(c)

	c.byte()  // version (one byte here; two in the 4-byte dialect)
	c.intw(2) // charsetID
	c.intw(2) // charsetForm (two bytes here; one in the 4-byte dialect)
	c.intw(4) // maxCharLen
	c.intw(4) // oaccollid
	c.byte()  // allowNull
	c.byte()  // v7 name length (unused; the DLC below carries the real length)

	name := c.dlc() // column name
	c.dlc()         // schema name
	c.dlc()         // type name

	c.skip(wide64ColumnRecordTailLen)

	return string(name), dataType
}

// wide64TypeOID consumes the record's type OID: a four-byte length, then either
// the CLR carrying the OID or — when the length is zero — eleven further bytes.
//
// That conditional is the one part of this record that is not a field list, and
// it is measured rather than chosen. The distance from maxLen to `version` is 33
// bytes on a column carrying a 16-byte OID and 27 on one carrying none, while
// the OID's own CLR is 17 — so the absent case spends **six more** bytes than
// removing the CLR would account for, and the length field cannot simply move,
// because the object column pins it twelve bytes past maxLen. No fixed layout
// fits both; this one fits both and nothing in the corpus contradicts it.
//
// What the corpus cannot say is *why*, and the walk does not pretend to: the
// eleven bytes are zero wherever they appear, so "padding an absent DLC" and
// "an inline area a present OID displaces" are the same bytes. It matters only
// that the record advances correctly, and a record it advanced wrongly fails
// the next one's isKnownTNSType check rather than producing a name.
func wide64TypeOID(c *dcursor) {
	length := c.intw(4)
	if c.err {
		return
	}

	if length <= 0 {
		c.skip(wide64ColumnEmptyTypeOIDPad)

		return
	}

	data, n := readCLR(c.buf[c.pos:])
	if n == 0 || len(data) < length {
		c.err = true

		return
	}

	c.pos += n
}

// parseColumnDescribeModernTail consumes the extra per-column fields a modern
// Oracle server (23ai negotiating a high TTCVersion with SQLcl / ojdbc 26.x)
// appends after the classic two trailing ints: the TTCVersion ≥ 17 data-use-case
// domain schema and name (DLCs), the ≥ 20 annotations count and — when non-zero
// — the annotation key/value block (mirrors go-ora ParameterInfo.load), plus
// three further trailing ints that the 23ai server emits but go-ora v2.9.0 does
// not yet model (observed all-zero for non-domain columns). Without consuming
// these the next column record would start mid-field and misalign.
func parseColumnDescribeModernTail(c *dcursor) {
	c.dlc() // data-use-case domain schema (TTCVersion ≥ 17)
	c.dlc() // data-use-case domain name (TTCVersion ≥ 17)

	if numAnnotations := c.intw(4); numAnnotations > 0 { // TTCVersion ≥ 20
		c.byte()
		numAnnotations = c.intw(4) // re-read count
		c.byte()

		for i := 0; i < numAnnotations && !c.err; i++ {
			c.dlc()   // annotation key
			c.dlc()   // annotation value
			c.intw(4) // annotation flag
		}

		c.intw(4) // trailing length
	}

	if c.wide {
		// The three ints below are **not** sent to an OCI client: the same 23ai
		// server that appends them for a thin one ends the record at the
		// annotation count here. Measured, and not by inference — the records
		// of an eight-column REF cursor land exactly on the descriptor's own
		// 7-byte DATE field, which three more integers of any width would
		// overshoot.
		return
	}

	// Three further version ints the 23ai server appends.
	c.cint()
	c.cint()
	c.cint()
}

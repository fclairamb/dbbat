package oracle

// Oracle TTC internal type codes (TNSType) carried in a column's describe
// record. Only the ones dbbat distinguishes are named; the value is what the
// describe parser returns as columnDef.TypeCode.
const (
	tnsTypeVARCHAR     = 1
	tnsTypeNUMBER      = 2
	tnsTypeDATE        = 12
	tnsTypeRAW         = 23
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
func parseColumnDescribes(ttcPayload []byte) []columnDesc {
	count, start, ok := describeColumnLayout(ttcPayload)
	if !ok || count <= 0 || count > 1000 {
		return nil
	}

	c := &dcursor{buf: ttcPayload, pos: start}
	cols := make([]columnDesc, 0, count)

	for range count {
		name, typ := parseColumnDescribe(c)
		if c.err || !isKnownTNSType(typ) {
			return nil
		}

		cols = append(cols, columnDesc{Name: name, Type: typ})
	}

	return cols
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

// isKnownTNSType reports whether t is a defined TTC type code. The ranges cover
// the full TNSType enumeration; an out-of-range value means the record parse
// drifted and the result must not be trusted.
func isKnownTNSType(t int) bool {
	switch {
	case t >= 1 && t <= 24:
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
type dcursor struct {
	buf []byte
	pos int
	err bool
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

// dlc reads a TTC data-length-coded value: a compressed-int length then the
// CLR-encoded bytes (truncated to that length). Returns nil for an empty field.
func (c *dcursor) dlc() []byte {
	length := c.cint()
	if c.err || length == 0 {
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
// code. The field order is fixed up to the name; the trailing fields are the
// modern-server (TTCVersion ≥ 20) layout — domain schema/name plus an
// annotations count — which sets where the next record starts.
func parseColumnDescribe(c *dcursor) (string, int) {
	dataType := c.byte()
	c.byte() // flag
	c.byte() // precision

	if numberScaleTypes[dataType] {
		c.cint() // scale (compressed)
	} else {
		c.byte() // scale
	}

	c.cint() // maxLen
	c.cint() // maxNoOfArrayElements
	c.cint() // contFlag
	c.dlc()  // toID
	c.cint() // version
	c.cint() // charsetID
	c.byte() // charsetForm
	c.cint() // maxCharLen
	c.cint() // oaccollid (TTCVersion ≥ 8, always true for modern servers)
	c.byte() // allowNull
	c.byte() // v7 name length (unused; the DLC below carries the real length)

	name := c.dlc() // column name
	c.dlc()         // schema name
	c.dlc()         // type name

	// Trailing version ints (TTCVersion ≥ 3 and ≥ 6). Newer servers append
	// domain-name and annotation fields here; the observed Oracle Free server
	// does not, and the record ends after these two — see parseColumnDescribes,
	// which validates alignment and bails out rather than trust a bad parse.
	c.cint()
	c.cint()

	return string(name), dataType
}

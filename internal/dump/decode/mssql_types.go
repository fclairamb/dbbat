package decode

import (
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"unicode/utf16"
)

// TDS data type ids, mirroring internal/proxy/mssql/typeinfo.go. They appear in
// two places sharing one grammar — an RPC parameter's TYPE_INFO and a
// COLMETADATA column's — so one decoder frames both.
const (
	tdsTypeNull     = 0x1F
	tdsTypeInt1     = 0x30
	tdsTypeBit      = 0x32
	tdsTypeInt2     = 0x34
	tdsTypeInt4     = 0x38
	tdsTypeDateTim4 = 0x3A
	tdsTypeFlt4     = 0x3B
	tdsTypeMoney    = 0x3C
	tdsTypeDateTime = 0x3D
	tdsTypeFlt8     = 0x3E
	tdsTypeMoney4   = 0x7A
	tdsTypeInt8     = 0x7F

	tdsTypeGUID      = 0x24
	tdsTypeIntN      = 0x26
	tdsTypeDecimal   = 0x37
	tdsTypeNumeric   = 0x3F
	tdsTypeBitN      = 0x68
	tdsTypeDecimalN  = 0x6A
	tdsTypeNumericN  = 0x6C
	tdsTypeFltN      = 0x6D
	tdsTypeMoneyN    = 0x6E
	tdsTypeDateTimeN = 0x6F
	tdsTypeChar      = 0x2F
	tdsTypeVarChar   = 0x27
	tdsTypeBinary    = 0x2D
	tdsTypeVarBinary = 0x25

	tdsTypeDateN           = 0x28
	tdsTypeTimeN           = 0x29
	tdsTypeDateTime2N      = 0x2A
	tdsTypeDateTimeOffsetN = 0x2B

	tdsTypeBigVarBinary = 0xA5
	tdsTypeBigVarChar   = 0xA7
	tdsTypeBigBinary    = 0xAD
	tdsTypeBigChar      = 0xAF
	tdsTypeNVarChar     = 0xE7
	tdsTypeNChar        = 0xEF

	tdsTypeText    = 0x23
	tdsTypeImage   = 0x22
	tdsTypeNText   = 0x63
	tdsTypeVariant = 0x62
	tdsTypeXML     = 0xF1
	tdsTypeUDT     = 0xF0
)

// tdsValueKind is how a value of a given type is framed. Getting it right is
// the whole job: a wrong length loses the token stream, whereas a wrong
// interpretation only makes one printed cell ugly.
type tdsValueKind byte

const (
	tdsKindFixed tdsValueKind = iota
	tdsKindByte
	tdsKindUShort
	tdsKindTextPtr
	tdsKindVariant
	tdsKindPLP
)

// tdsCollationLen is the COLLATION structure that follows the length of every
// character type.
const tdsCollationLen = 5

// tdsPLPNull is the sentinel total length of a NULL partially-length-prefixed
// value.
const tdsPLPNull uint64 = 0xFFFFFFFFFFFFFFFF

// tdsMaxDeclaredLen is the declared length that marks the (max) form, whose
// values are PLP rather than length-prefixed.
const tdsMaxDeclaredLen = 0xFFFF

// mssqlTypeInfo is a parsed TYPE_INFO: enough to frame a value and to name it.
type mssqlTypeInfo struct {
	id   byte
	kind tdsValueKind
	size int
}

// mssqlColumn is one COLMETADATA entry.
type mssqlColumn struct {
	name string
	info mssqlTypeInfo
}

// tdsFixedTypeSizes is the wire size of every fixed-length type.
var tdsFixedTypeSizes = map[byte]int{
	tdsTypeNull: 0, tdsTypeInt1: 1, tdsTypeBit: 1, tdsTypeInt2: 2, tdsTypeInt4: 4,
	tdsTypeDateTim4: 4, tdsTypeFlt4: 4, tdsTypeMoney: 8, tdsTypeDateTime: 8,
	tdsTypeFlt8: 8, tdsTypeMoney4: 4, tdsTypeInt8: 8,
}

var tdsTypeNames = map[byte]string{
	tdsTypeNull: "null", tdsTypeInt1: "tinyint", tdsTypeBit: "bit", tdsTypeInt2: "smallint",
	tdsTypeInt4: "int", tdsTypeDateTim4: "smalldatetime", tdsTypeFlt4: "real",
	tdsTypeMoney: "money", tdsTypeDateTime: "datetime", tdsTypeFlt8: "float",
	tdsTypeMoney4: "smallmoney", tdsTypeInt8: "bigint", tdsTypeGUID: "uniqueidentifier",
	tdsTypeIntN: "int", tdsTypeDecimal: "decimal", tdsTypeNumeric: "numeric",
	tdsTypeBitN: "bit", tdsTypeDecimalN: "decimal", tdsTypeNumericN: "numeric",
	tdsTypeFltN: "float", tdsTypeMoneyN: "money", tdsTypeDateTimeN: "datetime",
	tdsTypeChar: "char", tdsTypeVarChar: "varchar", tdsTypeBinary: "binary",
	tdsTypeVarBinary: "varbinary", tdsTypeDateN: "date", tdsTypeTimeN: "time",
	tdsTypeDateTime2N: "datetime2", tdsTypeDateTimeOffsetN: "datetimeoffset",
	tdsTypeBigVarBinary: "varbinary", tdsTypeBigVarChar: "varchar",
	tdsTypeBigBinary: "binary", tdsTypeBigChar: "char", tdsTypeNVarChar: "nvarchar",
	tdsTypeNChar: "nchar", tdsTypeText: "text", tdsTypeImage: "image",
	tdsTypeNText: "ntext", tdsTypeVariant: "sql_variant", tdsTypeXML: "xml",
	tdsTypeUDT: "udt",
}

func tdsIsUCS2Type(id byte) bool {
	switch id {
	case tdsTypeNVarChar, tdsTypeNChar, tdsTypeNText, tdsTypeXML:
		return true
	default:
		return false
	}
}

func tdsIsASCIIType(id byte) bool {
	switch id {
	case tdsTypeChar, tdsTypeVarChar, tdsTypeBigChar, tdsTypeBigVarChar, tdsTypeText:
		return true
	default:
		return false
	}
}

// parseTDSTypeInfo decodes one TYPE_INFO starting at pos and returns the
// position just past it.
func parseTDSTypeInfo(buf []byte, pos int) (mssqlTypeInfo, int, bool) {
	if pos >= len(buf) {
		return mssqlTypeInfo{}, 0, false
	}

	info := mssqlTypeInfo{id: buf[pos]}
	pos++

	if size, ok := tdsFixedTypeSizes[info.id]; ok {
		info.kind = tdsKindFixed
		info.size = size

		return info, pos, true
	}

	switch info.id {
	case tdsTypeDateN:
		info.kind = tdsKindByte

		return info, pos, true
	case tdsTypeTimeN, tdsTypeDateTime2N, tdsTypeDateTimeOffsetN:
		info.kind = tdsKindByte

		return info, pos + 1, pos < len(buf)
	case tdsTypeGUID, tdsTypeIntN, tdsTypeBitN, tdsTypeFltN, tdsTypeMoneyN,
		tdsTypeDateTimeN, tdsTypeChar, tdsTypeVarChar, tdsTypeBinary, tdsTypeVarBinary:
		if pos >= len(buf) {
			return mssqlTypeInfo{}, 0, false
		}

		info.kind = tdsKindByte
		info.size = int(buf[pos])

		return info, pos + 1, true
	case tdsTypeDecimal, tdsTypeNumeric, tdsTypeDecimalN, tdsTypeNumericN:
		if pos+3 > len(buf) {
			return mssqlTypeInfo{}, 0, false
		}

		info.kind = tdsKindByte
		info.size = int(buf[pos])

		return info, pos + 3, true
	case tdsTypeBigVarBinary, tdsTypeBigBinary, tdsTypeBigVarChar, tdsTypeBigChar,
		tdsTypeNVarChar, tdsTypeNChar:
		return parseTDSUShortLenType(info, buf, pos)
	case tdsTypeText, tdsTypeNText, tdsTypeImage:
		return parseTDSLongLenType(info, buf, pos)
	case tdsTypeVariant:
		if pos+4 > len(buf) {
			return mssqlTypeInfo{}, 0, false
		}

		info.kind = tdsKindVariant

		return info, pos + 4, true
	default:
		// XML, UDT and anything unmodelled: their declarations are variable
		// and guessing a length would lose the stream.
		return mssqlTypeInfo{}, 0, false
	}
}

// parseTDSUShortLenType handles the (var)char/(var)binary family, whose
// declared length of 0xFFFF marks the (max) form.
func parseTDSUShortLenType(info mssqlTypeInfo, buf []byte, pos int) (mssqlTypeInfo, int, bool) {
	if pos+2 > len(buf) {
		return mssqlTypeInfo{}, 0, false
	}

	info.size = int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	pos += 2

	if info.size == tdsMaxDeclaredLen {
		info.kind = tdsKindPLP
	} else {
		info.kind = tdsKindUShort
	}

	if tdsIsUCS2Type(info.id) || tdsIsASCIIType(info.id) {
		if pos+tdsCollationLen > len(buf) {
			return mssqlTypeInfo{}, 0, false
		}

		pos += tdsCollationLen
	}

	return info, pos, true
}

// parseTDSLongLenType handles the legacy LOBs, whose values are TEXTPTR-framed.
func parseTDSLongLenType(info mssqlTypeInfo, buf []byte, pos int) (mssqlTypeInfo, int, bool) {
	if pos+4 > len(buf) {
		return mssqlTypeInfo{}, 0, false
	}

	info.kind = tdsKindTextPtr
	pos += 4

	if info.id == tdsTypeText || info.id == tdsTypeNText {
		if pos+tdsCollationLen > len(buf) {
			return mssqlTypeInfo{}, 0, false
		}

		pos += tdsCollationLen
	}

	return info, pos, true
}

// readTDSValue reads one value, returning its bytes (nil when NULL), whether it
// was NULL, and the position just past it.
func readTDSValue(info mssqlTypeInfo, buf []byte, pos int) ([]byte, bool, int, bool) {
	switch info.kind {
	case tdsKindFixed:
		if pos+info.size > len(buf) {
			return nil, false, 0, false
		}

		return buf[pos : pos+info.size], false, pos + info.size, true
	case tdsKindByte:
		return readTDSSized(buf, pos, 1, 0)
	case tdsKindUShort:
		return readTDSSized(buf, pos, 2, tdsMaxDeclaredLen)
	case tdsKindVariant:
		return readTDSSized(buf, pos, 4, 0)
	case tdsKindTextPtr:
		return readTDSTextPtrValue(buf, pos)
	case tdsKindPLP:
		return readTDSPLPValue(buf, pos)
	default:
		return nil, false, 0, false
	}
}

// readTDSSized reads a value whose length sits in prefixLen bytes ahead of it,
// with nullMarker standing for NULL.
func readTDSSized(buf []byte, pos, prefixLen, nullMarker int) ([]byte, bool, int, bool) {
	if pos+prefixLen > len(buf) {
		return nil, false, 0, false
	}

	var size int

	switch prefixLen {
	case 1:
		size = int(buf[pos])
	case 2:
		size = int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	default:
		size = int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	}

	pos += prefixLen

	if size == nullMarker || (nullMarker == 0 && size == 0) {
		return nil, true, pos, true
	}

	if pos+size > len(buf) {
		return nil, false, 0, false
	}

	return buf[pos : pos+size], false, pos + size, true
}

// readTDSTextPtrValue reads a legacy LOB: a text pointer, a timestamp, then a
// DWORD-prefixed body. A zero-length pointer is the NULL encoding.
func readTDSTextPtrValue(buf []byte, pos int) ([]byte, bool, int, bool) {
	const timestampLen = 8

	if pos >= len(buf) {
		return nil, false, 0, false
	}

	ptrLen := int(buf[pos])
	pos++

	if ptrLen == 0 {
		return nil, true, pos, true
	}

	if pos+ptrLen+timestampLen+4 > len(buf) {
		return nil, false, 0, false
	}

	pos += ptrLen + timestampLen

	size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4

	if size < 0 || pos+size > len(buf) {
		return nil, false, 0, false
	}

	return buf[pos : pos+size], false, pos + size, true
}

// readTDSPLPValue reassembles a partially-length-prefixed value: an 8-byte
// total, then chunks of a DWORD length each, closed by a zero-length chunk.
func readTDSPLPValue(buf []byte, pos int) ([]byte, bool, int, bool) {
	const totalLen = 8

	if pos+totalLen > len(buf) {
		return nil, false, 0, false
	}

	if binary.LittleEndian.Uint64(buf[pos:pos+totalLen]) == tdsPLPNull {
		return nil, true, pos + totalLen, true
	}

	pos += totalLen

	var value []byte

	for {
		if pos+4 > len(buf) {
			return nil, false, 0, false
		}

		size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		pos += 4

		if size == 0 {
			return value, false, pos, true
		}

		if pos+size > len(buf) {
			return nil, false, 0, false
		}

		value = append(value, buf[pos:pos+size]...)
		pos += size
	}
}

// renderTDSValue turns a value's bytes into something readable. Only --rows
// ever calls it.
func renderTDSValue(info mssqlTypeInfo, raw []byte, isNull bool) string {
	if isNull {
		return "NULL"
	}

	if tdsIsUCS2Type(info.id) {
		return quote(truncate(ucs2String(raw)))
	}

	if tdsIsASCIIType(info.id) {
		return quote(truncate(string(raw)))
	}

	if number, ok := tdsIntegerValue(info, raw); ok {
		return number
	}

	if len(raw) > maxValueLen/2 {
		raw = raw[:maxValueLen/2]
	}

	return "0x" + hex.EncodeToString(raw)
}

// tdsIntegerValue renders the signed integer family, which is most of what a
// non-text column actually holds.
func tdsIntegerValue(info mssqlTypeInfo, raw []byte) (string, bool) {
	switch info.id {
	case tdsTypeInt1, tdsTypeBit, tdsTypeInt2, tdsTypeInt4, tdsTypeInt8, tdsTypeIntN, tdsTypeBitN:
	default:
		return "", false
	}

	var value int64

	switch len(raw) {
	case 1:
		value = int64(raw[0])
	case 2:
		value = int64(int16(binary.LittleEndian.Uint16(raw)))
	case 4:
		value = int64(int32(binary.LittleEndian.Uint32(raw)))
	case 8:
		value = int64(binary.LittleEndian.Uint64(raw))
	default:
		return "", false
	}

	return strconv.FormatInt(value, 10), true
}

// ucs2String decodes UTF-16LE, which is how TDS spells every name and every
// nvarchar on the wire.
func ucs2String(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}

	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[i*2:])
	}

	return string(utf16.Decode(units))
}

// readTDSBVarchar reads a single-byte character count then that many UCS-2
// characters.
func readTDSBVarchar(buf []byte, pos int) (string, int, bool) {
	if pos >= len(buf) {
		return "", 0, false
	}

	chars := int(buf[pos])
	pos++

	if pos+chars*2 > len(buf) {
		return "", 0, false
	}

	return ucs2String(buf[pos : pos+chars*2]), pos + chars*2, true
}

// readTDSUSVarchar reads a USHORT character count then that many UCS-2
// characters.
func readTDSUSVarchar(buf []byte, pos int) (string, int, bool) {
	if pos+2 > len(buf) {
		return "", 0, false
	}

	chars := int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	pos += 2

	if pos+chars*2 > len(buf) {
		return "", 0, false
	}

	return ucs2String(buf[pos : pos+chars*2]), pos + chars*2, true
}

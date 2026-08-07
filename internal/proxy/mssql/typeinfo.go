package mssql

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TDS data type ids (MS-TDS 2.2.5.4 and 2.2.5.5). They appear in two places
// that share one grammar: the TYPE_INFO of an RPC parameter, and the TYPE_INFO
// of a COLMETADATA column. Parsing them once is what lets the request hook and
// the response accountant share a decoder.
const (
	// Fixed-length types carry no length at all, anywhere.
	typeNull     byte = 0x1F
	typeInt1     byte = 0x30
	typeBit      byte = 0x32
	typeInt2     byte = 0x34
	typeInt4     byte = 0x38
	typeDateTim4 byte = 0x3A
	typeFlt4     byte = 0x3B
	typeMoney    byte = 0x3C
	typeDateTime byte = 0x3D
	typeFlt8     byte = 0x3E
	typeMoney4   byte = 0x7A
	typeInt8     byte = 0x7F

	// BYTELEN types: a byte of declared length in TYPE_INFO, a byte of actual
	// length on the value, where 0 means NULL.
	typeGUID      byte = 0x24
	typeIntN      byte = 0x26
	typeDecimal   byte = 0x37
	typeNumeric   byte = 0x3F
	typeBitN      byte = 0x68
	typeDecimalN  byte = 0x6A
	typeNumericN  byte = 0x6C
	typeFltN      byte = 0x6D
	typeMoneyN    byte = 0x6E
	typeDateTimeN byte = 0x6F
	typeChar      byte = 0x2F
	typeVarChar   byte = 0x27
	typeBinary    byte = 0x2D
	typeVarBinary byte = 0x25

	// The date/time family added in TDS 7.3. DATE carries no TYPE_INFO length
	// at all; the other three carry a scale instead of one.
	typeDateN           byte = 0x28
	typeTimeN           byte = 0x29
	typeDateTime2N      byte = 0x2A
	typeDateTimeOffsetN byte = 0x2B

	// USHORTLEN types. A declared length of 0xFFFF means the (max) form, whose
	// value is PLP-encoded rather than length-prefixed.
	typeBigVarBinary byte = 0xA5
	typeBigVarChar   byte = 0xA7
	typeBigBinary    byte = 0xAD
	typeBigChar      byte = 0xAF
	typeNVarChar     byte = 0xE7
	typeNChar        byte = 0xEF

	// LONGLEN types: the legacy LOBs (TEXTPTR-encoded values), sql_variant,
	// and the two whose values are always PLP.
	typeText    byte = 0x23
	typeImage   byte = 0x22
	typeNText   byte = 0x63
	typeVariant byte = 0x62
	typeXML     byte = 0xF1
	typeUDT     byte = 0xF0
)

// valueKind is how a value of a given type is framed on the wire. It is the
// only part of TYPE_INFO the walk *must* get right: a wrong length loses the
// token stream, whereas a wrong interpretation only makes one captured cell
// ugly.
type valueKind byte

const (
	kindFixed   valueKind = iota // no prefix, size known from the type
	kindByte                     // BYTE length, 0 = NULL
	kindUShort                   // USHORT length, 0xFFFF = NULL
	kindTextPtr                  // TEXT / NTEXT / IMAGE
	kindVariant                  // sql_variant: LONG length, 0 = NULL
	kindPLP                      // ULONGLONG length then chunks
)

// collationLen is the size of the COLLATION structure that follows the length
// of every character type (MS-TDS 2.2.5.1.2).
const collationLen = 5

// plpNull / plpUnknown are the two sentinel PLP lengths.
const (
	plpNull    uint64 = 0xFFFFFFFFFFFFFFFF
	plpUnknown uint64 = 0xFFFFFFFFFFFFFFFE
)

// ErrUnknownDataType — a TYPE_INFO named a type this decoder does not model.
// It stops the walk rather than guessing a length.
var ErrUnknownDataType = errors.New("mssql: unmodelled TDS data type")

// typeInfo is a parsed TYPE_INFO: enough to frame a value, plus enough to
// render it.
type typeInfo struct {
	id byte
	// kind is how the value is framed.
	kind valueKind
	// size is the byte size for kindFixed, and the declared maximum otherwise.
	size int
	// precision / scale for the decimal family and the date/time family.
	precision byte
	scale     byte
}

// name renders the type for diagnostics and for the placeholder a value dbbat
// cannot interpret is captured as.
func (t typeInfo) name() string {
	if name, ok := typeNames[t.id]; ok {
		return name
	}

	return fmt.Sprintf("0x%02x", t.id)
}

var typeNames = map[byte]string{
	typeNull: "null", typeInt1: "tinyint", typeBit: "bit", typeInt2: "smallint",
	typeInt4: "int", typeDateTim4: "smalldatetime", typeFlt4: "real", typeMoney: "money",
	typeDateTime: "datetime", typeFlt8: "float", typeMoney4: "smallmoney", typeInt8: "bigint",
	typeGUID: "uniqueidentifier", typeIntN: "int", typeDecimal: "decimal", typeNumeric: "numeric",
	typeBitN: "bit", typeDecimalN: "decimal", typeNumericN: "numeric", typeFltN: "float",
	typeMoneyN: "money", typeDateTimeN: "datetime", typeChar: "char", typeVarChar: "varchar",
	typeBinary: "binary", typeVarBinary: "varbinary", typeDateN: "date", typeTimeN: "time",
	typeDateTime2N: "datetime2", typeDateTimeOffsetN: "datetimeoffset",
	typeBigVarBinary: "varbinary", typeBigVarChar: "varchar", typeBigBinary: "binary",
	typeBigChar: "char", typeNVarChar: "nvarchar", typeNChar: "nchar",
	typeText: "text", typeImage: "image", typeNText: "ntext", typeVariant: "sql_variant",
	typeXML: "xml", typeUDT: "udt",
}

// fixedTypeSizes is the wire size of every fixed-length type.
var fixedTypeSizes = map[byte]int{
	typeNull: 0, typeInt1: 1, typeBit: 1, typeInt2: 2, typeInt4: 4,
	typeDateTim4: 4, typeFlt4: 4, typeMoney: 8, typeDateTime: 8, typeFlt8: 8,
	typeMoney4: 4, typeInt8: 8,
}

// isUCS2Type reports whether a type's payload is UCS-2LE text.
func isUCS2Type(id byte) bool {
	switch id {
	case typeNVarChar, typeNChar, typeNText, typeXML:
		return true
	default:
		return false
	}
}

// isASCIIType reports whether a type's payload is single-byte text.
func isASCIIType(id byte) bool {
	switch id {
	case typeChar, typeVarChar, typeBigChar, typeBigVarChar, typeText:
		return true
	default:
		return false
	}
}

// parseTypeInfo decodes one TYPE_INFO starting at pos, returning it and the
// position just past it.
func parseTypeInfo(buf []byte, pos int) (typeInfo, int, error) {
	if pos >= len(buf) {
		return typeInfo{}, 0, ErrTokenTruncated
	}

	info := typeInfo{id: buf[pos]}
	pos++

	if size, ok := fixedTypeSizes[info.id]; ok {
		info.kind = kindFixed
		info.size = size

		return info, pos, nil
	}

	switch info.id {
	case typeDateN:
		// The one nullable type with no length byte in TYPE_INFO at all.
		info.kind = kindByte

		return info, pos, nil

	case typeTimeN, typeDateTime2N, typeDateTimeOffsetN:
		if pos >= len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		info.kind = kindByte
		info.scale = buf[pos]

		return info, pos + 1, nil

	case typeGUID, typeIntN, typeBitN, typeFltN, typeMoneyN, typeDateTimeN,
		typeChar, typeVarChar, typeBinary, typeVarBinary:
		if pos >= len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		info.kind = kindByte
		info.size = int(buf[pos])

		return info, pos + 1, nil

	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		if pos+3 > len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		info.kind = kindByte
		info.size = int(buf[pos])
		info.precision = buf[pos+1]
		info.scale = buf[pos+2]

		return info, pos + 3, nil

	case typeBigVarBinary, typeBigBinary, typeBigVarChar, typeBigChar, typeNVarChar, typeNChar:
		return parseUShortLenType(info, buf, pos)

	case typeText, typeNText, typeImage:
		return parseLongLenType(info, buf, pos)

	case typeVariant:
		if pos+4 > len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		info.kind = kindVariant
		info.size = int(binary.LittleEndian.Uint32(buf[pos : pos+4]))

		return info, pos + 4, nil

	case typeXML:
		return parseXMLType(info, buf, pos)

	case typeUDT:
		return parseUDTType(info, buf, pos)

	default:
		return typeInfo{}, 0, fmt.Errorf("%w: 0x%02x", ErrUnknownDataType, info.id)
	}
}

// parseUShortLenType handles the (var)char/(var)binary family whose TYPE_INFO
// carries a USHORT maximum length — 0xFFFF meaning the (max) form, whose values
// are PLP rather than length-prefixed.
func parseUShortLenType(info typeInfo, buf []byte, pos int) (typeInfo, int, error) {
	if pos+2 > len(buf) {
		return typeInfo{}, 0, ErrTokenTruncated
	}

	info.size = int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	pos += 2

	if info.size == 0xFFFF {
		info.kind = kindPLP
	} else {
		info.kind = kindUShort
	}

	if isUCS2Type(info.id) || isASCIIType(info.id) {
		if pos+collationLen > len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		pos += collationLen
	}

	return info, pos, nil
}

// parseLongLenType handles TEXT / NTEXT / IMAGE: a LONG maximum length, a
// collation for the two character forms, and TEXTPTR-framed values.
func parseLongLenType(info typeInfo, buf []byte, pos int) (typeInfo, int, error) {
	if pos+4 > len(buf) {
		return typeInfo{}, 0, ErrTokenTruncated
	}

	info.kind = kindTextPtr
	info.size = int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4

	if info.id == typeText || info.id == typeNText {
		if pos+collationLen > len(buf) {
			return typeInfo{}, 0, ErrTokenTruncated
		}

		pos += collationLen
	}

	return info, pos, nil
}

// parseXMLType skips the XML schema declaration. XML values are always PLP.
func parseXMLType(info typeInfo, buf []byte, pos int) (typeInfo, int, error) {
	if pos >= len(buf) {
		return typeInfo{}, 0, ErrTokenTruncated
	}

	info.kind = kindPLP
	schemaPresent := buf[pos]
	pos++

	if schemaPresent == 0 {
		return info, pos, nil
	}

	var err error

	for range 2 { // dbname, owning schema
		if _, pos, err = readBVarchar(buf, pos); err != nil {
			return typeInfo{}, 0, err
		}
	}

	if _, pos, err = readUSVarchar(buf, pos); err != nil { // schema collection
		return typeInfo{}, 0, err
	}

	return info, pos, nil
}

// parseUDTType skips a CLR user-defined type's declaration. UDT values are PLP.
func parseUDTType(info typeInfo, buf []byte, pos int) (typeInfo, int, error) {
	if pos+2 > len(buf) {
		return typeInfo{}, 0, ErrTokenTruncated
	}

	info.kind = kindPLP
	info.size = int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
	pos += 2

	var err error

	for range 3 { // dbname, schema name, type name
		if _, pos, err = readBVarchar(buf, pos); err != nil {
			return typeInfo{}, 0, err
		}
	}

	if _, pos, err = readUSVarchar(buf, pos); err != nil { // assembly qualified name
		return typeInfo{}, 0, err
	}

	return info, pos, nil
}

// readValue reads one TYPE_VARBYTE value, returning its bytes (nil when NULL),
// whether it was NULL, and the position just past it.
func readValue(info typeInfo, buf []byte, pos int) ([]byte, bool, int, error) {
	switch info.kind {
	case kindFixed:
		if pos+info.size > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		return buf[pos : pos+info.size], false, pos + info.size, nil

	case kindByte:
		if pos >= len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		size := int(buf[pos])
		pos++

		if size == 0 {
			return nil, true, pos, nil
		}

		if pos+size > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		return buf[pos : pos+size], false, pos + size, nil

	case kindUShort:
		if pos+2 > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		size := int(binary.LittleEndian.Uint16(buf[pos : pos+2]))
		pos += 2

		if size == 0xFFFF {
			return nil, true, pos, nil
		}

		if pos+size > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		return buf[pos : pos+size], false, pos + size, nil

	case kindVariant:
		if pos+4 > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		pos += 4

		if size <= 0 {
			return nil, true, pos, nil
		}

		if pos+size > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		return buf[pos : pos+size], false, pos + size, nil

	case kindTextPtr:
		return readTextPtrValue(buf, pos)

	case kindPLP:
		return readPLPValue(buf, pos)

	default:
		return nil, false, 0, ErrUnknownDataType
	}
}

// readTextPtrValue reads a legacy LOB: a text pointer, a timestamp, then a
// LONG-prefixed body. A zero-length text pointer is the NULL encoding.
func readTextPtrValue(buf []byte, pos int) ([]byte, bool, int, error) {
	const timestampLen = 8

	if pos >= len(buf) {
		return nil, false, 0, ErrTokenTruncated
	}

	ptrLen := int(buf[pos])
	pos++

	if ptrLen == 0 {
		return nil, true, pos, nil
	}

	if pos+ptrLen+timestampLen+4 > len(buf) {
		return nil, false, 0, ErrTokenTruncated
	}

	pos += ptrLen + timestampLen

	size := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
	pos += 4

	if size < 0 || pos+size > len(buf) {
		return nil, false, 0, ErrTokenTruncated
	}

	return buf[pos : pos+size], false, pos + size, nil
}

// readPLPValue reassembles a partially-length-prefixed value: an 8-byte total
// (which may be "unknown"), then chunks of a LONG length each, closed by a
// zero-length chunk.
func readPLPValue(buf []byte, pos int) ([]byte, bool, int, error) {
	if pos+8 > len(buf) {
		return nil, false, 0, ErrTokenTruncated
	}

	total := binary.LittleEndian.Uint64(buf[pos : pos+8])
	pos += 8

	if total == plpNull {
		return nil, true, pos, nil
	}

	var out []byte

	if total != plpUnknown && total < uint64(maxMessageSize) {
		out = make([]byte, 0, total)
	}

	for {
		if pos+4 > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		chunk := int(binary.LittleEndian.Uint32(buf[pos : pos+4]))
		pos += 4

		if chunk == 0 {
			return out, false, pos, nil
		}

		if chunk < 0 || pos+chunk > len(buf) {
			return nil, false, 0, ErrTokenTruncated
		}

		out = append(out, buf[pos:pos+chunk]...)
		pos += chunk
	}
}

// textValue renders a value as text, for the two places that want a string
// rather than a JSON cell: an RPC's statement parameter, and the parameter
// list captured on a query row.
func textValue(info typeInfo, raw []byte, isNull bool) string {
	if isNull {
		return "NULL"
	}

	switch v := jsonValueFor(info, raw, false).(type) {
	case nil:
		return "NULL"
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// jsonValueFor converts one decoded value into something json.Marshal renders
// usefully. It is best-effort by design: the framing above is what has to be
// exact, and a value this cannot interpret becomes a tagged base64 blob rather
// than a decoding failure.
func jsonValueFor(info typeInfo, raw []byte, isNull bool) any {
	if isNull || raw == nil {
		return nil
	}

	switch info.id {
	case typeNull:
		return nil

	case typeBit, typeBitN:
		return len(raw) > 0 && raw[0] != 0

	case typeInt1, typeInt2, typeInt4, typeInt8, typeIntN:
		return integerValue(raw)

	case typeFlt4, typeFlt8, typeFltN:
		return floatValue(raw)

	case typeMoney, typeMoney4, typeMoneyN:
		return moneyValue(raw)

	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		return decimalValue(raw, info.scale)

	case typeGUID:
		return guidValue(raw)

	case typeDateTime, typeDateTim4, typeDateTimeN:
		return legacyDateTimeValue(raw)

	case typeDateN:
		return dateValue(raw)

	case typeTimeN:
		return timeOfDayValue(raw, info.scale)

	case typeDateTime2N:
		return dateTime2Value(raw, info.scale)

	case typeDateTimeOffsetN:
		return dateTimeOffsetValue(raw, info.scale)

	default:
		return jsonTextOrBytes(info, raw)
	}
}

// jsonTextOrBytes handles the character and binary families, plus anything the
// decoder does not model.
func jsonTextOrBytes(info typeInfo, raw []byte) any {
	switch {
	case isUCS2Type(info.id):
		return ucs2ToString(raw)

	case isASCIIType(info.id):
		if utf8.Valid(raw) {
			return string(raw)
		}

		return latin1ToString(raw)

	default:
		// Binary, sql_variant, UDT and anything unmodelled: tagged base64, the
		// same shape the MySQL proxy uses for a non-UTF-8 blob.
		return map[string]string{
			"$bytes": base64.StdEncoding.EncodeToString(raw),
			"$type":  info.name(),
		}
	}
}

// integerValue decodes a signed little-endian integer. A single byte is
// SQL Server's tinyint, which is unsigned.
func integerValue(raw []byte) any {
	switch len(raw) {
	case 1:
		return int64(raw[0])
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(raw)))
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(raw)))
	case 8:
		return int64(binary.LittleEndian.Uint64(raw))
	default:
		return nil
	}
}

// floatValue decodes a 4- or 8-byte IEEE float.
func floatValue(raw []byte) any {
	switch len(raw) {
	case 4:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(raw)))
	case 8:
		return math.Float64frombits(binary.LittleEndian.Uint64(raw))
	default:
		return nil
	}
}

// moneyValue decodes MONEY / SMALLMONEY, both scaled by 10^-4. The 8-byte form
// puts its *high* half first, which is the classic money-decoding bug.
func moneyValue(raw []byte) any {
	const moneyScale = 4

	switch len(raw) {
	case 4:
		return scaledIntString(int64(int32(binary.LittleEndian.Uint32(raw))), moneyScale)
	case 8:
		high := int64(int32(binary.LittleEndian.Uint32(raw[0:4])))
		low := int64(binary.LittleEndian.Uint32(raw[4:8]))

		return scaledIntString(high<<32|low, moneyScale)
	default:
		return nil
	}
}

// decimalValue decodes DECIMAL / NUMERIC: a sign byte then a little-endian
// magnitude of up to 16 bytes.
func decimalValue(raw []byte, scale byte) any {
	if len(raw) < 2 {
		return nil
	}

	negative := raw[0] == 0
	magnitude := raw[1:]

	// The magnitude can be 128 bits, which no Go integer holds — render it as
	// text by long division rather than losing the top bits.
	digits := bigDecimalDigits(magnitude)
	if negative && digits != "0" {
		digits = "-" + digits
	}

	return insertDecimalPoint(digits, scale)
}

// bigDecimalDigits renders a little-endian magnitude of arbitrary width as a
// base-10 string.
func bigDecimalDigits(magnitude []byte) string {
	// Repeated division by 10^9 over a big-endian working copy.
	work := make([]byte, len(magnitude))
	for i, b := range magnitude {
		work[len(magnitude)-1-i] = b
	}

	var out []string

	for {
		var (
			remainder uint32
			nonZero   bool
		)

		for i := range work {
			acc := remainder<<8 | uint32(work[i])
			work[i] = byte(acc / 10)
			remainder = acc % 10

			if work[i] != 0 {
				nonZero = true
			}
		}

		out = append(out, strconv.Itoa(int(remainder)))

		if !nonZero {
			break
		}
	}

	var sb strings.Builder

	for i := len(out) - 1; i >= 0; i-- {
		sb.WriteString(out[i])
	}

	trimmed := strings.TrimLeft(sb.String(), "0")
	if trimmed == "" {
		return "0"
	}

	return trimmed
}

// insertDecimalPoint places the decimal point in a digit string.
func insertDecimalPoint(digits string, scale byte) string {
	if scale == 0 {
		return digits
	}

	negative := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")

	for len(digits) <= int(scale) {
		digits = "0" + digits
	}

	cut := len(digits) - int(scale)
	out := digits[:cut] + "." + digits[cut:]

	if negative {
		return "-" + out
	}

	return out
}

// scaledIntString renders an integer scaled by 10^-scale.
func scaledIntString(v int64, scale byte) string {
	negative := v < 0
	if negative {
		v = -v
	}

	out := insertDecimalPoint(strconv.FormatInt(v, 10), scale)
	if negative {
		return "-" + out
	}

	return out
}

// guidValue renders a uniqueidentifier. The first three groups are
// little-endian on the wire and big-endian in the text form.
func guidValue(raw []byte) any {
	if len(raw) != 16 {
		return nil
	}

	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x",
		raw[3], raw[2], raw[1], raw[0],
		raw[5], raw[4],
		raw[7], raw[6],
		raw[8], raw[9],
		raw[10:16])
}

// sqlEpoch is day zero for DATETIME and SMALLDATETIME.
var sqlEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// dateEpoch is day zero for the TDS 7.3 DATE family.
var dateEpoch = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

// legacyDateTimeValue decodes DATETIME (8 bytes) and SMALLDATETIME (4).
func legacyDateTimeValue(raw []byte) any {
	switch len(raw) {
	case 4:
		days := int(binary.LittleEndian.Uint16(raw[0:2]))
		minutes := int(binary.LittleEndian.Uint16(raw[2:4]))

		return sqlEpoch.AddDate(0, 0, days).Add(time.Duration(minutes) * time.Minute).
			Format("2006-01-02T15:04:05")

	case 8:
		days := int(int32(binary.LittleEndian.Uint32(raw[0:4])))
		ticks := int64(binary.LittleEndian.Uint32(raw[4:8]))

		// A tick is 1/300 of a second.
		nanos := ticks * int64(time.Second) / 300

		return sqlEpoch.AddDate(0, 0, days).Add(time.Duration(nanos)).
			Format("2006-01-02T15:04:05.999999999")

	default:
		return nil
	}
}

// dayNumber decodes the 3-byte little-endian day count the DATE family uses.
func dayNumber(raw []byte) int {
	return int(raw[0]) | int(raw[1])<<8 | int(raw[2])<<16
}

// dateValue decodes a DATE (3 bytes).
func dateValue(raw []byte) any {
	if len(raw) != 3 {
		return nil
	}

	return dateEpoch.AddDate(0, 0, dayNumber(raw)).Format("2006-01-02")
}

// timeTicks decodes the little-endian scaled tick count of a TIME value.
func timeTicks(raw []byte) int64 {
	var ticks int64
	for i := len(raw) - 1; i >= 0; i-- {
		ticks = ticks<<8 | int64(raw[i])
	}

	return ticks
}

// timeDuration converts scaled ticks into a duration.
func timeDuration(ticks int64, scale byte) time.Duration {
	divisor := int64(1)
	for range scale {
		divisor *= 10
	}

	if divisor == 0 {
		return 0
	}

	return time.Duration(ticks * int64(time.Second) / divisor)
}

// timeLen is how many bytes a TIME of the given scale occupies.
func timeLen(scale byte) int {
	switch {
	case scale <= 2:
		return 3
	case scale <= 4:
		return 4
	default:
		return 5
	}
}

// timeOfDayValue decodes a TIME(n).
func timeOfDayValue(raw []byte, scale byte) any {
	if len(raw) == 0 {
		return nil
	}

	d := timeDuration(timeTicks(raw), scale)

	return dateEpoch.Add(d).Format("15:04:05.999999999")
}

// dateTime2Value decodes a DATETIME2(n): the time part then the 3-byte date.
func dateTime2Value(raw []byte, scale byte) any {
	tl := timeLen(scale)
	if len(raw) < tl+3 {
		return nil
	}

	d := timeDuration(timeTicks(raw[:tl]), scale)

	return dateEpoch.AddDate(0, 0, dayNumber(raw[tl:tl+3])).Add(d).
		Format("2006-01-02T15:04:05.999999999")
}

// dateTimeOffsetValue decodes a DATETIMEOFFSET(n): a DATETIME2 in UTC followed
// by the offset in minutes.
func dateTimeOffsetValue(raw []byte, scale byte) any {
	tl := timeLen(scale)
	if len(raw) < tl+5 {
		return nil
	}

	d := timeDuration(timeTicks(raw[:tl]), scale)
	offset := int(int16(binary.LittleEndian.Uint16(raw[tl+3 : tl+5])))

	moment := dateEpoch.AddDate(0, 0, dayNumber(raw[tl:tl+3])).Add(d)

	return moment.In(time.FixedZone("", offset*60)).Format("2006-01-02T15:04:05.999999999-07:00")
}

// latin1ToString maps single-byte text that is not valid UTF-8 through
// Latin-1, which is the right guess for the default SQL Server collations and
// never fails.
func latin1ToString(raw []byte) string {
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}

	return string(runes)
}

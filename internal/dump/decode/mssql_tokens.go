package decode

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// TDS token types, mirroring internal/proxy/mssql/tokens.go.
const (
	tdsTokenOffset        = 0x78
	tdsTokenReturnStatus  = 0x79
	tdsTokenColMetadata   = 0x81
	tdsTokenTabName       = 0xA4
	tdsTokenColInfo       = 0xA5
	tdsTokenOrder         = 0xA9
	tdsTokenError         = 0xAA
	tdsTokenInfo          = 0xAB
	tdsTokenReturnValue   = 0xAC
	tdsTokenLoginAck      = 0xAD
	tdsTokenFeatureExtAck = 0xAE
	tdsTokenRow           = 0xD1
	tdsTokenNBCRow        = 0xD2
	tdsTokenEnvChange     = 0xE3
	tdsTokenSessionState  = 0xE4
	tdsTokenSSPI          = 0xED
	tdsTokenFedAuthInfo   = 0xEE
	tdsTokenDone          = 0xFD
	tdsTokenDoneProc      = 0xFE
	tdsTokenDoneInProc    = 0xFF
)

const (
	// tdsDoneTokenLen is the token byte plus its fixed 12-byte body.
	tdsDoneTokenLen = 13
	// tdsDoneCount marks the row count in a DONE body as meaningful.
	tdsDoneCount = 0x0010
	// tdsFeatureExtTerminator closes a feature-acknowledgement list.
	tdsFeatureExtTerminator = 0xFF
	// tdsNoMetadata is the COLMETADATA column count meaning "keep what was in
	// force".
	tdsNoMetadata = 0xFFFF
)

// walkTokens turns a response message into one line per token. A token the
// walk cannot frame stops it: the bytes after an unmodelled length are not
// tokens any more, and printing a guess would be worse than saying so.
func (m *mssqlSplitter) walkTokens(body []byte) []string {
	var out []string

	for pos := 0; pos < len(body); {
		text, used, ok := m.token(body[pos:])
		if !ok || used <= 0 {
			return append(out, fmt.Sprintf("... %d bytes not decoded (token 0x%02x)", len(body)-pos, body[pos]))
		}

		if text != "" {
			out = append(out, text)
		}

		pos += used
	}

	return out
}

// token decodes the one token at the front of b, returning its rendering and
// how many bytes it took.
func (m *mssqlSplitter) token(b []byte) (string, int, bool) {
	switch b[0] {
	case tdsTokenError, tdsTokenInfo:
		return tdsUShortToken(b, formatTDSMessageToken(b[0]))
	case tdsTokenLoginAck:
		return tdsUShortToken(b, func([]byte) string { return "LoginAck" })
	case tdsTokenEnvChange:
		return tdsUShortToken(b, formatTDSEnvChange)
	case tdsTokenSSPI:
		// An SSPI blob is an authentication payload: named, never printed.
		return tdsUShortToken(b, func(body []byte) string {
			return fmt.Sprintf("SSPI(%d bytes, redacted)", len(body))
		})
	case tdsTokenTabName, tdsTokenColInfo, tdsTokenOrder:
		return tdsUShortToken(b, func(body []byte) string {
			return fmt.Sprintf("%s(%d bytes)", tdsTokenName(b[0]), len(body))
		})
	case tdsTokenSessionState, tdsTokenFedAuthInfo:
		return tdsULongToken(b)
	case tdsTokenReturnStatus:
		if len(b) < 5 {
			return "", 0, false
		}

		return "ReturnStatus " + strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(b[1:5]))), 10), 5, true
	case tdsTokenOffset:
		return "Offset", 5, len(b) >= 5
	case tdsTokenFeatureExtAck:
		return tdsFeatureExtAckToken(b)
	case tdsTokenColMetadata:
		return m.colMetadata(b)
	case tdsTokenRow:
		return m.row(b, false)
	case tdsTokenNBCRow:
		return m.row(b, true)
	case tdsTokenReturnValue:
		return m.returnValue(b)
	case tdsTokenDone, tdsTokenDoneProc, tdsTokenDoneInProc:
		return tdsDoneToken(b)
	default:
		return "", 0, false
	}
}

// tdsUShortToken frames the tokens shaped as a type byte, a USHORT length and
// a body, and renders the body with render.
func tdsUShortToken(b []byte, render func([]byte) string) (string, int, bool) {
	if len(b) < 3 {
		return "", 0, false
	}

	total := 3 + int(binary.LittleEndian.Uint16(b[1:3]))
	if len(b) < total {
		return "", 0, false
	}

	return render(b[3:total]), total, true
}

// tdsULongToken frames the two tokens carrying a DWORD length.
func tdsULongToken(b []byte) (string, int, bool) {
	if len(b) < 5 {
		return "", 0, false
	}

	total := 5 + int(binary.LittleEndian.Uint32(b[1:5]))
	if total < 5 || len(b) < total {
		return "", 0, false
	}

	return fmt.Sprintf("%s(%d bytes)", tdsTokenName(b[0]), total-5), total, true
}

// tdsFeatureExtAckToken walks a feature-acknowledgement list, which is framed
// as its own run of entries rather than by one length.
func tdsFeatureExtAckToken(b []byte) (string, int, bool) {
	pos := 1
	features := 0

	for {
		if pos >= len(b) {
			return "", 0, false
		}

		if b[pos] == tdsFeatureExtTerminator {
			return fmt.Sprintf("FeatureExtAck(%d features)", features), pos + 1, true
		}

		pos++

		if pos+4 > len(b) {
			return "", 0, false
		}

		size := int(binary.LittleEndian.Uint32(b[pos : pos+4]))
		pos += 4

		if size < 0 || pos+size > len(b) {
			return "", 0, false
		}

		pos += size
		features++
	}
}

// tdsDoneToken renders a DONE-family token, whose row count is what a trace
// reader is usually after.
func tdsDoneToken(b []byte) (string, int, bool) {
	if len(b) < tdsDoneTokenLen {
		return "", 0, false
	}

	status := binary.LittleEndian.Uint16(b[1:3])
	text := fmt.Sprintf("%s status=0x%04x", tdsTokenName(b[0]), status)

	if status&tdsDoneCount != 0 {
		text += fmt.Sprintf(" rows=%d", binary.LittleEndian.Uint64(b[5:13]))
	}

	return text, tdsDoneTokenLen, true
}

// colMetadata parses the column shape of a result set, which is what lets the
// rows that follow be framed at all.
func (m *mssqlSplitter) colMetadata(b []byte) (string, int, bool) {
	if len(b) < 3 {
		return "", 0, false
	}

	count := int(binary.LittleEndian.Uint16(b[1:3]))
	pos := 3

	if count == tdsNoMetadata {
		return "ColMetaData(unchanged)", pos, true
	}

	columns := make([]mssqlColumn, 0, count)

	for range count {
		column, next, ok := parseTDSColumn(b, pos)
		if !ok {
			return "", 0, false
		}

		columns = append(columns, column)
		pos = next
	}

	m.columns = columns

	return formatTDSColMetadata(columns, m.opts), pos, true
}

// parseTDSColumn decodes one COLMETADATA entry.
func parseTDSColumn(b []byte, pos int) (mssqlColumn, int, bool) {
	const userTypeAndFlags = 6

	if pos+userTypeAndFlags > len(b) {
		return mssqlColumn{}, 0, false
	}

	pos += userTypeAndFlags

	info, pos, ok := parseTDSTypeInfo(b, pos)
	if !ok {
		return mssqlColumn{}, 0, false
	}

	// The legacy LOBs name the table they came from before their own name.
	if info.id == tdsTypeText || info.id == tdsTypeNText || info.id == tdsTypeImage {
		if pos >= len(b) {
			return mssqlColumn{}, 0, false
		}

		parts := int(b[pos])
		pos++

		for range parts {
			if _, pos, ok = readTDSUSVarchar(b, pos); !ok {
				return mssqlColumn{}, 0, false
			}
		}
	}

	name, pos, ok := readTDSBVarchar(b, pos)
	if !ok {
		return mssqlColumn{}, 0, false
	}

	return mssqlColumn{name: name, info: info}, pos, true
}

// row is the redaction that matters most: a capture is full of these and every
// one of them is customer data.
func (m *mssqlSplitter) row(b []byte, nbc bool) (string, int, bool) {
	if len(m.columns) == 0 {
		return "", 0, false
	}

	pos := 1

	var bitmap []byte

	if nbc {
		size := (len(m.columns) + 7) / 8
		if pos+size > len(b) {
			return "", 0, false
		}

		bitmap = b[pos : pos+size]
		pos += size
	}

	rendered := make([]string, 0, len(m.columns))

	for i, column := range m.columns {
		if nbc && bitmap[i/8]&(1<<(i%8)) != 0 {
			rendered = append(rendered, "NULL")

			continue
		}

		raw, isNull, next, ok := readTDSValue(column.info, b, pos)
		if !ok {
			return "", 0, false
		}

		rendered = append(rendered, renderTDSValue(column.info, raw, isNull))
		pos = next
	}

	head := fmt.Sprintf("Row(%d cols)", len(m.columns))
	if !m.opts.ShowRows {
		return head, pos, true
	}

	return head + " [" + strings.Join(rendered, ", ") + "]", pos, true
}

// returnValue decodes an output parameter, which is how a prepared handle and
// an OUTPUT argument come back.
func (m *mssqlSplitter) returnValue(b []byte) (string, int, bool) {
	const ordinalLen = 2

	pos := 1 + ordinalLen

	name, pos, ok := readTDSBVarchar(b, pos)
	if !ok {
		return "", 0, false
	}

	const statusUserTypeAndFlags = 7 // status (1) + user type (4) + flags (2)

	pos += statusUserTypeAndFlags
	if pos > len(b) {
		return "", 0, false
	}

	info, pos, ok := parseTDSTypeInfo(b, pos)
	if !ok {
		return "", 0, false
	}

	raw, isNull, pos, ok := readTDSValue(info, b, pos)
	if !ok {
		return "", 0, false
	}

	head := "ReturnValue " + name
	if !m.opts.ShowRows {
		return head, pos, true
	}

	return head + " = " + renderTDSValue(info, raw, isNull), pos, true
}

package decode

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// tdsPacketTypeNames names the packet types, mirroring
// internal/proxy/mssql/packet.go.
var tdsPacketTypeNames = map[byte]string{
	tdsTypeSQLBatch:   "SQLBatch",
	tdsTypeLegacyAuth: "LegacyAuth",
	tdsTypeRPC:        "RPC",
	tdsTypeReply:      "TabularResult",
	tdsTypeAttention:  "Attention",
	tdsTypeBulkLoad:   "BulkLoad",
	tdsTypeFedAuth:    "FedAuthToken",
	tdsTypeTransMgr:   "TransactionManagerRequest",
	tdsTypeLogin7:     "LOGIN7",
	tdsTypeSSPI:       "SSPI",
	tdsTypePrelogin:   "PRELOGIN",
}

var tdsTokenNames = map[byte]string{
	tdsTokenOffset:        "Offset",
	tdsTokenReturnStatus:  "ReturnStatus",
	tdsTokenColMetadata:   "ColMetaData",
	tdsTokenTabName:       "TabName",
	tdsTokenColInfo:       "ColInfo",
	tdsTokenOrder:         "Order",
	tdsTokenError:         "Error",
	tdsTokenInfo:          "Info",
	tdsTokenReturnValue:   "ReturnValue",
	tdsTokenLoginAck:      "LoginAck",
	tdsTokenFeatureExtAck: "FeatureExtAck",
	tdsTokenRow:           "Row",
	tdsTokenNBCRow:        "NBCRow",
	tdsTokenEnvChange:     "EnvChange",
	tdsTokenSessionState:  "SessionState",
	tdsTokenSSPI:          "SSPI",
	tdsTokenFedAuthInfo:   "FedAuthInfo",
	tdsTokenDone:          "Done",
	tdsTokenDoneProc:      "DoneProc",
	tdsTokenDoneInProc:    "DoneInProc",
}

// tdsPreloginOptionNames names the PRELOGIN option tokens.
var tdsPreloginOptionNames = map[byte]string{
	0x00: "version",
	0x01: "encryption",
	0x02: "instopt",
	0x03: "threadid",
	0x04: "mars",
	0x05: "traceid",
	0x06: "fedauthrequired",
	0x07: "nonceopt",
}

// tdsEncryptionNames names the four answers to the encryption option.
var tdsEncryptionNames = map[byte]string{
	0x00: "off",
	0x01: "on",
	0x02: "not_supported",
	0x03: "required",
}

// tdsStatementProcs are the system procedures whose leading string arguments
// are a statement and its parameter declaration rather than data. Their text
// is printed, the way a PostgreSQL Query's is; every argument after them is a
// value and is counted.
var tdsStatementProcs = map[uint16]bool{
	3:  true, // sp_cursorprepare
	5:  true, // sp_cursorprepexec
	10: true, // sp_executesql
	11: true, // sp_prepare
	13: true, // sp_prepexec
	14: true, // sp_prepexecrpc
}

var tdsWellKnownProcNames = map[uint16]string{
	1: "sp_cursor", 2: "sp_cursoropen", 3: "sp_cursorprepare", 4: "sp_cursorexecute",
	5: "sp_cursorprepexec", 6: "sp_cursorunprepare", 7: "sp_cursorfetch",
	8: "sp_cursoroption", 9: "sp_cursorclose", 10: "sp_executesql", 11: "sp_prepare",
	12: "sp_execute", 13: "sp_prepexec", 14: "sp_prepexecrpc", 15: "sp_unprepare",
}

// tdsStatementArgs is how many leading text arguments of a statement procedure
// are metadata: the statement itself and the parameter declaration.
const tdsStatementArgs = 2

// tdsProcIDByName is the NameLenOrProcID marker saying an id follows instead
// of a name.
const tdsProcIDByName = 0xFFFF

// tdsBatchSeparators end one RPC request inside a batch of them.
const (
	tdsBatchFlagTransaction = 0xFF
	tdsBatchFlagNoExec      = 0xFE
)

func tdsPacketTypeName(msgType byte) string {
	if name, ok := tdsPacketTypeNames[msgType]; ok {
		return name
	}

	return fmt.Sprintf("TDS(0x%02x)", msgType)
}

func tdsTokenName(token byte) string {
	if name, ok := tdsTokenNames[token]; ok {
		return name
	}

	return fmt.Sprintf("Token(0x%02x)", token)
}

// formatTDSPrelogin renders the option list both peers open with. The one
// option worth reading is the encryption answer, which is what says whether
// the rest of a capture taken below TLS is readable at all.
func formatTDSPrelogin(body []byte) string {
	if len(body) > 0 && body[0] == 0x16 {
		// A TLS record: the encapsulated handshake travels under the same
		// packet type once encryption is agreed.
		return fmt.Sprintf("PRELOGIN(TLS handshake, %d bytes)", len(body))
	}

	names, encryption, ok := parseTDSPreloginOptions(body)
	if !ok {
		return fmt.Sprintf("PRELOGIN(unreadable, %d bytes)", len(body))
	}

	head := fmt.Sprintf("PRELOGIN(%d options: %s)", len(names), strings.Join(names, ", "))
	if encryption == "" {
		return head
	}

	return head + " encryption=" + encryption
}

// parseTDSPreloginOptions walks the option table: a token, a big-endian offset
// and length, closed by a 0xFF terminator.
func parseTDSPreloginOptions(body []byte) ([]string, string, bool) {
	const entryLen = 5

	var (
		names      []string
		encryption string
	)

	for pos := 0; ; pos += entryLen {
		if pos >= len(body) {
			return nil, "", false
		}

		token := body[pos]
		if token == 0xFF {
			return names, encryption, true
		}

		if pos+entryLen > len(body) {
			return nil, "", false
		}

		offset := int(binary.BigEndian.Uint16(body[pos+1 : pos+3]))
		length := int(binary.BigEndian.Uint16(body[pos+3 : pos+5]))

		if offset+length > len(body) {
			return nil, "", false
		}

		names = append(names, tdsPreloginOptionName(token))

		if token == 0x01 && length > 0 {
			encryption = tdsEncryptionName(body[offset])
		}
	}
}

func tdsPreloginOptionName(token byte) string {
	if name, ok := tdsPreloginOptionNames[token]; ok {
		return name
	}

	return fmt.Sprintf("0x%02x", token)
}

func tdsEncryptionName(value byte) string {
	if name, ok := tdsEncryptionNames[value]; ok {
		return name
	}

	return fmt.Sprintf("0x%02x", value)
}

// LOGIN7 offset-table positions, mirroring internal/proxy/mssql/login7.go.
const (
	tdsLogin7OffsetTable = 36
	tdsLogin7FixedLen    = 94

	tdsPairHostName = 0
	tdsPairUserName = 1
	tdsPairAppName  = 3
	tdsPairDatabase = 8
)

// formatTDSLogin7 prints the connection's identity — host, user, application,
// database — the way the PostgreSQL splitter prints a StartupMessage.
//
// The password pair is deliberately not among them. LOGIN7 obfuscates it with
// a nibble swap and a fixed XOR, which is not encryption: a trace that printed
// it would be a trace that leaks it.
func formatTDSLogin7(body []byte) string {
	if len(body) < tdsLogin7FixedLen {
		return fmt.Sprintf("LOGIN7(unreadable, %d bytes, credentials redacted)", len(body))
	}

	parts := []string{"LOGIN7"}

	for _, field := range []struct {
		label string
		pair  int
	}{
		{"host", tdsPairHostName},
		{"user", tdsPairUserName},
		{"app", tdsPairAppName},
		{"database", tdsPairDatabase},
	} {
		if value, ok := tdsLogin7Field(body, field.pair); ok && value != "" {
			parts = append(parts, field.label+"="+quote(value))
		}
	}

	return strings.Join(parts, " ") + " (credentials redacted)"
}

// tdsLogin7Field resolves one offset/length pair of the LOGIN7 table. Lengths
// are in characters; offsets are from the start of the message.
func tdsLogin7Field(body []byte, pair int) (string, bool) {
	pos := tdsLogin7OffsetTable + pair*4
	if pos+4 > len(body) {
		return "", false
	}

	offset := int(binary.LittleEndian.Uint16(body[pos : pos+2]))
	chars := int(binary.LittleEndian.Uint16(body[pos+2 : pos+4]))

	if chars == 0 {
		return "", true
	}

	if offset < tdsLogin7FixedLen || offset+chars*2 > len(body) {
		return "", false
	}

	return ucs2String(body[offset : offset+chars*2]), true
}

// formatTDSTransactionManager names the transaction operation a request
// carries.
func formatTDSTransactionManager(body []byte) string {
	if len(body) < 2 {
		return "TransactionManagerRequest"
	}

	return fmt.Sprintf("TransactionManagerRequest(type=%d)", binary.LittleEndian.Uint16(body[:2]))
}

// formatTDSRPC renders an RPC request — one line per procedure call, since a
// batch may carry several.
func formatTDSRPC(body []byte, opts Options) []string {
	var out []string

	for pos := 0; pos < len(body); {
		text, next, ok := formatTDSRPCRequest(body, pos, opts)
		if !ok {
			return append(out, fmt.Sprintf("RPC(%d bytes not decoded)", len(body)-pos))
		}

		out = append(out, text)
		pos = next

		// A batch separates its requests with a flag byte.
		if pos < len(body) && (body[pos] == tdsBatchFlagTransaction || body[pos] == tdsBatchFlagNoExec) {
			pos++
		}
	}

	if len(out) == 0 {
		return []string{"RPC(empty)"}
	}

	return out
}

// formatTDSRPCRequest decodes one procedure call: its name or well-known id,
// then its parameters.
func formatTDSRPCRequest(body []byte, pos int, opts Options) (string, int, bool) {
	if pos+2 > len(body) {
		return "", 0, false
	}

	nameLen := binary.LittleEndian.Uint16(body[pos : pos+2])
	pos += 2

	var (
		name   string
		procID uint16
	)

	if nameLen == tdsProcIDByName {
		if pos+2 > len(body) {
			return "", 0, false
		}

		procID = binary.LittleEndian.Uint16(body[pos : pos+2])
		pos += 2
		name = tdsProcName(procID)
	} else {
		if pos+int(nameLen)*2 > len(body) {
			return "", 0, false
		}

		name = ucs2String(body[pos : pos+int(nameLen)*2])
		pos += int(nameLen) * 2
	}

	// Option flags.
	pos += 2
	if pos > len(body) {
		return "", 0, false
	}

	statements, count, next, ok := readTDSRPCParams(body, pos, procID, opts)
	if !ok {
		return "", 0, false
	}

	head := "RPC " + name
	if len(statements) > 0 {
		head += " " + strings.Join(statements, " ")
	}

	return fmt.Sprintf("%s (%d params)", head, count), next, true
}

// readTDSRPCParams walks the parameters, returning the ones that are printable
// statement text and the total count.
func readTDSRPCParams(body []byte, pos int, procID uint16, opts Options) ([]string, int, int, bool) {
	var printed []string

	count := 0

	for pos < len(body) {
		if body[pos] == tdsBatchFlagTransaction || body[pos] == tdsBatchFlagNoExec {
			return printed, count, pos, true
		}

		name, next, ok := readTDSBVarchar(body, pos)
		if !ok {
			return nil, 0, 0, false
		}

		// Status flags byte.
		pos = next + 1
		if pos > len(body) {
			return nil, 0, 0, false
		}

		info, pos2, ok := parseTDSTypeInfo(body, pos)
		if !ok {
			return nil, 0, 0, false
		}

		raw, isNull, pos3, ok := readTDSValue(info, body, pos2)
		if !ok {
			return nil, 0, 0, false
		}

		if text, ok := tdsPrintableParam(procID, count, name, info, raw, isNull, opts); ok {
			printed = append(printed, text)
		}

		count++
		pos = pos3
	}

	return printed, count, pos, true
}

// tdsPrintableParam decides whether a parameter is statement text — printed —
// or a value — counted. Only the leading string arguments of the statement
// procedures qualify, which is exactly where sp_executesql and its relatives
// put the batch and its parameter declaration.
func tdsPrintableParam(
	procID uint16, index int, name string, info mssqlTypeInfo, raw []byte, isNull bool, opts Options,
) (string, bool) {
	if opts.ShowRows {
		text := renderTDSValue(info, raw, isNull)
		if name != "" {
			text = name + "=" + text
		}

		return text, true
	}

	if !tdsStatementProcs[procID] || index >= tdsStatementArgs {
		return "", false
	}

	if !tdsIsUCS2Type(info.id) && !tdsIsASCIIType(info.id) {
		return "", false
	}

	return quote(truncate(ucs2OrASCII(info, raw))), true
}

func ucs2OrASCII(info mssqlTypeInfo, raw []byte) string {
	if tdsIsUCS2Type(info.id) {
		return ucs2String(raw)
	}

	return string(raw)
}

func tdsProcName(procID uint16) string {
	if name, ok := tdsWellKnownProcNames[procID]; ok {
		return name
	}

	return "procid=" + strconv.FormatUint(uint64(procID), 10)
}

// formatTDSColMetadata prints the column count, and the names only when --rows
// opted into content — exactly as RowDescription does on PostgreSQL.
func formatTDSColMetadata(columns []mssqlColumn, opts Options) string {
	head := fmt.Sprintf("ColMetaData(%d cols)", len(columns))

	if !opts.ShowRows {
		return head
	}

	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.name+" "+tdsTypeName(column.info.id))
	}

	return head + " [" + strings.Join(names, ", ") + "]"
}

func tdsTypeName(id byte) string {
	if name, ok := tdsTypeNames[id]; ok {
		return name
	}

	return fmt.Sprintf("0x%02x", id)
}

// formatTDSMessageToken renders an ERROR or INFO token: the server's own
// message, as the PostgreSQL splitter prints an ErrorResponse.
func formatTDSMessageToken(token byte) func([]byte) string {
	return func(body []byte) string {
		const fixedHead = 6 // Number (4) + State (1) + Class (1)

		if len(body) < fixedHead {
			return tdsTokenName(token)
		}

		number := int32(binary.LittleEndian.Uint32(body[:4]))
		class := body[5]

		text, _, ok := readTDSUSVarchar(body, fixedHead)
		if !ok {
			return fmt.Sprintf("%s %d (%d)", tdsTokenName(token), number, class)
		}

		return fmt.Sprintf("%s %d (%d): %s", tdsTokenName(token), number, class, collapse(text))
	}
}

// formatTDSEnvChange names the environment change a server reports — a
// database switch, a packet-size change, a transaction beginning.
func formatTDSEnvChange(body []byte) string {
	if len(body) == 0 {
		return "EnvChange"
	}

	return fmt.Sprintf("EnvChange(type=%d)", body[0])
}

package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// ALL_HEADERS (MS-TDS 2.2.5.2) prefixes every SQLBatch, RPC and Transaction
// Manager request on TDS 7.2+. It is a DWORD total length (counting itself)
// followed by that many bytes of headers.
const (
	allHeadersLenSize = 4
	// A single header is at least a DWORD length, a USHORT type and one byte
	// of data.
	minHeaderLen = 7
	// The three header types MS-TDS defines. Anything else means the leading
	// DWORD was not a header block at all.
	headerQueryNotification uint16 = 1
	headerTransDescriptor   uint16 = 2
	headerTraceActivity     uint16 = 3
)

// RPC parsing errors.
var (
	// ErrRPCTruncated — an RPC request ran off the end of its message.
	ErrRPCTruncated = errors.New("mssql: truncated RPC request")
	// ErrRPCNoRequest — the RPC message carried no request at all.
	ErrRPCNoRequest = errors.New("mssql: RPC message carries no request")
)

// Well-known procedure ids (MS-TDS 2.2.6.6). A client may send these instead
// of the procedure's name, and a proxy that only understood names would miss
// every statement a driver sends this way — which is most of them.
const (
	spCursor           uint16 = 1
	spCursorOpen       uint16 = 2
	spCursorPrepare    uint16 = 3
	spCursorExecute    uint16 = 4
	spCursorPrepExec   uint16 = 5
	spCursorUnprepare  uint16 = 6
	spCursorFetch      uint16 = 7
	spCursorOption     uint16 = 8
	spCursorClose      uint16 = 9
	spExecuteSQL       uint16 = 10
	spPrepare          uint16 = 11
	spExecute          uint16 = 12
	spPrepExec         uint16 = 13
	spPrepExecRPC      uint16 = 14
	spUnprepare        uint16 = 15
	procIDByNameMarker uint16 = 0xFFFF
)

// wellKnownProcNames maps a procedure id onto the name it stands for, so the
// logged statement reads the same whichever form the client used.
var wellKnownProcNames = map[uint16]string{
	spCursor: "sp_cursor", spCursorOpen: "sp_cursoropen", spCursorPrepare: "sp_cursorprepare",
	spCursorExecute: "sp_cursorexecute", spCursorPrepExec: "sp_cursorprepexec",
	spCursorUnprepare: "sp_cursorunprepare", spCursorFetch: "sp_cursorfetch",
	spCursorOption: "sp_cursoroption", spCursorClose: "sp_cursorclose",
	spExecuteSQL: "sp_executesql", spPrepare: "sp_prepare", spExecute: "sp_execute",
	spPrepExec: "sp_prepexec", spPrepExecRPC: "sp_prepexecrpc", spUnprepare: "sp_unprepare",
}

// rpcParam is one decoded RPC parameter.
type rpcParam struct {
	name   string
	status byte
	info   typeInfo
	raw    []byte
	isNull bool
}

// text renders the parameter's value for logging.
func (p rpcParam) text() string {
	return textValue(p.info, p.raw, p.isNull)
}

// rpcRequest is one call inside an RPC message. A single message may carry
// several, separated by a batch flag.
type rpcRequest struct {
	// name is the procedure name, filled in for the by-id form too.
	name string
	// procID is the well-known id, or procIDByNameMarker when the client sent
	// a name.
	procID uint16
	params []rpcParam
}

// parseAllHeaders returns the offset just past the ALL_HEADERS block.
//
// It is defensive on purpose. Every TDS 7.2+ client sends the block, but the
// consequence of misreading it is that the SQL text starts two bytes off and a
// pattern-based control silently stops matching — so the block is only skipped
// when it really looks like one.
func parseAllHeaders(payload []byte) int {
	if len(payload) < allHeadersLenSize {
		return 0
	}

	total := int(binary.LittleEndian.Uint32(payload[:allHeadersLenSize]))
	if total < allHeadersLenSize || total > len(payload) {
		return 0
	}

	// An empty block (just the length) is legal.
	if total == allHeadersLenSize {
		return total
	}

	if total < allHeadersLenSize+minHeaderLen {
		return 0
	}

	headerLen := int(binary.LittleEndian.Uint32(payload[4:8]))
	headerType := binary.LittleEndian.Uint16(payload[8:10])

	if headerLen < minHeaderLen || headerLen > total-allHeadersLenSize {
		return 0
	}

	switch headerType {
	case headerQueryNotification, headerTransDescriptor, headerTraceActivity:
		return total
	default:
		return 0
	}
}

// parseSQLBatch extracts the statement text from a SQLBatch (0x01) message.
//
// The text is UCS-2LE, not UTF-8. Reading it as bytes would mangle every
// non-ASCII identifier and — the part that matters — could let a statement
// slip past a pattern-based control that is a security control.
func parseSQLBatch(payload []byte) (string, error) {
	offset := parseAllHeaders(payload)

	body := payload[offset:]
	if len(body)%2 != 0 {
		return "", fmt.Errorf("%w: SQL text is not a whole number of UCS-2 characters", ErrMalformedRequest)
	}

	return ucs2ToString(body), nil
}

// parseRPC decodes an RPC (0x03) message into its requests.
func parseRPC(payload []byte) ([]rpcRequest, error) {
	pos := parseAllHeaders(payload)

	var requests []rpcRequest

	for pos < len(payload) {
		req, next, err := parseRPCRequest(payload, pos)
		if err != nil {
			return nil, err
		}

		requests = append(requests, req)
		pos = next
	}

	if len(requests) == 0 {
		return nil, ErrRPCNoRequest
	}

	return requests, nil
}

// batchFlag values separate consecutive requests inside one RPC message. They
// sit exactly where the next parameter's name-length byte would be, which is
// how every implementation tells the two apart: a 254- or 255-character
// parameter name does not occur.
const (
	batchFlagTransaction byte = 0xFF
	batchFlagNoExec      byte = 0xFE
)

// parseRPCRequest decodes one request, returning the position of the next.
func parseRPCRequest(payload []byte, pos int) (rpcRequest, int, error) {
	var req rpcRequest

	if pos+2 > len(payload) {
		return req, 0, ErrRPCTruncated
	}

	nameLen := binary.LittleEndian.Uint16(payload[pos : pos+2])
	pos += 2

	if nameLen == procIDByNameMarker {
		if pos+2 > len(payload) {
			return req, 0, ErrRPCTruncated
		}

		req.procID = binary.LittleEndian.Uint16(payload[pos : pos+2])
		pos += 2
		req.name = wellKnownProcNames[req.procID]
	} else {
		size := int(nameLen) * 2
		if pos+size > len(payload) {
			return req, 0, ErrRPCTruncated
		}

		req.procID = procIDByNameMarker
		req.name = ucs2ToString(payload[pos : pos+size])
		pos += size
	}

	// OptionFlags.
	if pos+2 > len(payload) {
		return req, 0, ErrRPCTruncated
	}

	pos += 2

	for pos < len(payload) {
		if payload[pos] == batchFlagTransaction || payload[pos] == batchFlagNoExec {
			return req, pos + 1, nil
		}

		param, next, err := parseRPCParam(payload, pos)
		if err != nil {
			return req, 0, err
		}

		req.params = append(req.params, param)
		pos = next
	}

	return req, pos, nil
}

// parseRPCParam decodes one ParameterData entry: a name, a status byte, the
// TYPE_INFO and the value.
func parseRPCParam(payload []byte, pos int) (rpcParam, int, error) {
	var param rpcParam

	name, pos, err := readBVarchar(payload, pos)
	if err != nil {
		return param, 0, ErrRPCTruncated
	}

	param.name = name

	if pos >= len(payload) {
		return param, 0, ErrRPCTruncated
	}

	param.status = payload[pos]
	pos++

	info, pos, err := parseTypeInfo(payload, pos)
	if err != nil {
		return param, 0, err
	}

	param.info = info

	raw, isNull, pos, err := readValue(info, payload, pos)
	if err != nil {
		return param, 0, err
	}

	param.raw = raw
	param.isNull = isNull

	return param, pos, nil
}

// stringParam returns the text of the parameter at index i, or "" when it is
// absent, NULL, or not a character type.
func (r rpcRequest) stringParam(i int) string {
	if i >= len(r.params) {
		return ""
	}

	p := r.params[i]
	if p.isNull || p.raw == nil {
		return ""
	}

	switch {
	case isUCS2Type(p.info.id):
		return ucs2ToString(p.raw)
	case isASCIIType(p.info.id):
		return string(p.raw)
	default:
		return ""
	}
}

// intParam returns the integer value of the parameter at index i.
func (r rpcRequest) intParam(i int) (int64, bool) {
	if i >= len(r.params) {
		return 0, false
	}

	p := r.params[i]
	if p.isNull || p.raw == nil {
		return 0, false
	}

	v, ok := integerValue(p.raw).(int64)

	return v, ok
}

// statementParamIndex is where each SQL-carrying RPC form puts its statement.
//
// These are the documented parameter orders of the well-known procedures
// (MS-TDS 3.2.5.4 and the sp_* reference). Every entry here is a form that
// really does carry a statement dbbat can enforce a grant on; forms that carry
// only a handle are handled by handleParamIndex instead.
var statementParamIndex = map[uint16]int{
	spExecuteSQL:     0, // @stmt, @params, values…
	spPrepare:        2, // @handle OUT, @params, @stmt, @options
	spPrepExec:       2, // @handle OUT, @params, @stmt, values…
	spCursorPrepare:  2, // @handle OUT, @params, @stmt, @options, …
	spCursorOpen:     1, // @cursor OUT, @stmt, @scrollopt, …
	spCursorPrepExec: 3, // @cursor OUT, @handle OUT, @params, @stmt, …
	spPrepExecRPC:    1, // @handle OUT, @rpccall
}

// handleParamIndex is where the forms that execute something prepared earlier
// put the handle they execute.
var handleParamIndex = map[uint16]int{
	spExecute:       0, // @handle, values…
	spCursorExecute: 1, // @cursor OUT, @handle, …
	spUnprepare:     0,
}

// preparingProcs are the forms whose first RETURNVALUE is a prepared-statement
// handle worth remembering against the statement they prepared.
var preparingProcs = map[uint16]bool{
	spPrepare: true, spPrepExec: true, spCursorPrepare: true, spCursorPrepExec: true,
}

// harmlessProcs carry no statement and change nothing a grant control is about:
// cursor bookkeeping and releasing a handle. They are logged and forwarded even
// under a restrictive grant, because the statement they act on was already
// checked when it was prepared or opened.
var harmlessProcs = map[uint16]bool{
	spCursor: true, spCursorFetch: true, spCursorOption: true,
	spCursorClose: true, spCursorUnprepare: true, spUnprepare: true,
}

// isWellKnown reports whether the request used the procedure-id shorthand.
func (r rpcRequest) isWellKnown() bool {
	return r.procID != procIDByNameMarker
}

// statementParamNames are the names SQL Server accepts for the statement
// parameter of the SQL-carrying system procedures. The empty name is included
// because that is what every driver sends: these are positional calls.
var statementParamNames = map[string]bool{
	"": true, "@stmt": true, "@statement": true, "@tsql": true, "@rpccall": true,
}

// isStatementParamName reports whether a parameter name could be the statement.
func isStatementParamName(name string) bool {
	return statementParamNames[strings.ToLower(name)]
}

// statementTexts returns *every* parameter of this request that could be the
// statement SQL Server ends up running.
//
// Usually that is exactly one: the documented positional slot, sent unnamed.
// The set exists because SQL Server accepts these system procedures both
// positionally and by name, and dbbat must not be able to disagree with the
// server about which parameter is the statement — a client that could engineer
// such a disagreement would have dbbat validate one string while the server ran
// another, which is a read_only bypass. So when a request is anything other
// than the plain positional form, every candidate is enforced.
func (r rpcRequest) statementTexts() []string {
	if !r.isWellKnown() {
		return nil
	}

	idx, ok := statementParamIndex[r.procID]
	if !ok {
		return nil
	}

	var candidates []string

	// Anything explicitly named as a statement parameter.
	for i, p := range r.params {
		if p.name == "" || !isStatementParamName(p.name) {
			continue
		}

		if text := r.stringParam(i); text != "" {
			candidates = append(candidates, text)
		}
	}

	// And the positional slot, which is what the server reads for a call sent
	// the way every driver sends one.
	if text := r.stringParam(idx); text != "" {
		candidates = append(candidates, text)
	}

	return candidates
}

// statementText returns the statement this request is logged as: the positional
// one when it is there, otherwise the first named candidate. Enforcement uses
// statementTexts, not this.
func (r rpcRequest) statementText() (string, bool) {
	candidates := r.statementTexts()
	if len(candidates) == 0 {
		return "", false
	}

	return candidates[len(candidates)-1], true
}

// handle returns the prepared-statement handle this request executes, if any.
func (r rpcRequest) handle() (int64, bool) {
	if !r.isWellKnown() {
		return 0, false
	}

	idx, ok := handleParamIndex[r.procID]
	if !ok {
		return 0, false
	}

	return r.intParam(idx)
}

// logText renders the request the way it is stored and shown: the inline
// statement when there is one, otherwise an EXEC of the procedure.
func (r rpcRequest) logText() string {
	if stmt, ok := r.statementText(); ok {
		return stmt
	}

	name := r.name
	if name == "" {
		name = fmt.Sprintf("procedure #%d", r.procID)
	}

	var args []string

	for _, p := range r.params {
		if p.name != "" {
			args = append(args, p.name+" = "+p.text())

			continue
		}

		args = append(args, p.text())
	}

	if len(args) == 0 {
		return "EXEC " + name
	}

	return "EXEC " + name + " " + strings.Join(args, ", ")
}

// parameterValues renders the request's parameters for the query row.
func (r rpcRequest) parameterValues() []string {
	if len(r.params) == 0 {
		return nil
	}

	values := make([]string, 0, len(r.params))

	for _, p := range r.params {
		if p.name != "" {
			values = append(values, p.name+"="+p.text())

			continue
		}

		values = append(values, p.text())
	}

	return values
}

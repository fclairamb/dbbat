package oracle

import "fmt"

// TTCFunctionCode represents a TTC function code inside a TNS Data packet.
// TTC (Two-Task Common) is Oracle's RPC protocol layered inside TNS Data packets.
//
// Layout of a TNS Data packet payload:
//
//	Offset  Size  Field
//	0       2     Data flags (usually 0x0000)
//	2       1     TTC function code
//	3       ...   Function-specific payload
type TTCFunctionCode byte

// TTC function codes for Oracle's Two-Task Common protocol.
// In modern Oracle (v315+), function 0x03 is a generic "piggyback" that
// carries sub-operations (auth, execute, close, etc.) identified by byte 1.
const (
	TTCFuncSetProtocol  TTCFunctionCode = 0x01 // OSETPRO — session init
	TTCFuncSetDataTypes TTCFunctionCode = 0x02 // ODTYPES — session init
	TTCFuncPiggyback    TTCFunctionCode = 0x03 // Generic piggyback (sub-op at byte 1)
	TTCFuncOCLOSE       TTCFunctionCode = 0x05 // OCLOSE — close cursor (legacy)
	TTCFuncResponse     TTCFunctionCode = 0x08 // Server response
	TTCFuncOClosev2     TTCFunctionCode = 0x09 // OCLOSE — close cursor (v315+)
	TTCFuncOVersion     TTCFunctionCode = 0x0B // OVERSION — version request
	TTCFuncOALL8        TTCFunctionCode = 0x0E // OALL8 — parse+execute (legacy)
	TTCFuncQueryResult  TTCFunctionCode = 0x10 // Query result with row data
	TTCFuncOFETCH       TTCFunctionCode = 0x11 // OFETCH — fetch rows
	TTCFuncOCANCEL      TTCFunctionCode = 0x14 // OCANCEL — cancel query
)

// Piggyback sub-operation codes (byte 1 when func=0x03).
const (
	PiggybackSubClose   byte = 0x09 // Close cursor
	PiggybackSubExecSQL byte = 0x5e // Execute with SQL (OALL8 equivalent)
	PiggybackSubAuth1   byte = 0x76 // AUTH Phase 1
	PiggybackSubAuth2   byte = 0x73 // AUTH Phase 2
)

// Execute-with-SQL sub-operation codes for func=0x11.
// Different Oracle client drivers use different sub-ops.
const (
	execSubOpJDBC   byte = 0x69 // DBeaver, JDBC thin driver
	execSubOpPython byte = 0x98 // Python oracledb thin driver
)

// ttcDataFlagsSize is the size of the data flags prefix in a TNS Data payload.
const ttcDataFlagsSize = 2

// parseTTCFunctionCode extracts the TTC function code from a TNS Data packet payload.
// The payload must have at least 3 bytes: 2 bytes data flags + 1 byte function code.
func parseTTCFunctionCode(tnsDataPayload []byte) (TTCFunctionCode, error) {
	if len(tnsDataPayload) < ttcDataFlagsSize+1 {
		return 0, ErrTTCPayloadTooShort
	}

	return TTCFunctionCode(tnsDataPayload[ttcDataFlagsSize]), nil
}

// extractTTCPayload returns the TTC payload after the data flags and function code.
// Returns nil if the payload is too short.
func extractTTCPayload(tnsDataPayload []byte) []byte {
	if len(tnsDataPayload) < ttcDataFlagsSize+1 {
		return nil
	}

	return tnsDataPayload[ttcDataFlagsSize:]
}

// String returns a human-readable name for the TTC function code.
func (fc TTCFunctionCode) String() string {
	switch fc {
	case TTCFuncSetProtocol:
		return "OSETPRO"
	case TTCFuncSetDataTypes:
		return "ODTYPES"
	case TTCFuncPiggyback:
		return "PIGGYBACK"
	case TTCFuncOCLOSE:
		return "OCLOSE"
	case TTCFuncResponse:
		return "Response"
	case TTCFuncOClosev2:
		return "OCLOSEv2"
	case TTCFuncOVersion:
		return "OVERSION"
	case TTCFuncOALL8:
		return "OALL8"
	case TTCFuncQueryResult:
		return "QRESULT"
	case TTCFuncOFETCH:
		return "OFETCH"
	case TTCFuncOCANCEL:
		return "OCANCEL"
	default:
		return fmt.Sprintf("0x%02x", byte(fc))
	}
}

// IsPiggybackExecSQL checks if a piggyback payload is an execute-with-SQL message.
func IsPiggybackExecSQL(ttcPayload []byte) bool {
	// ttcPayload starts at the function code byte
	// [0] = 0x03 (piggyback), [1] = sub-op
	return len(ttcPayload) > 1 && ttcPayload[1] == PiggybackSubExecSQL
}

// IsPiggybackClose checks if a piggyback payload is a close cursor message.
func IsPiggybackClose(ttcPayload []byte) bool {
	return len(ttcPayload) > 1 && ttcPayload[1] == PiggybackSubClose
}

// IsExecSQL checks if a func=0x11 payload is an execute-with-SQL message
// rather than a plain OFETCH. Different clients use different sub-ops.
func IsExecSQL(ttcPayload []byte) bool {
	if len(ttcPayload) < 2 {
		return false
	}

	switch ttcPayload[1] {
	case execSubOpJDBC, execSubOpPython:
		return true
	default:
		return false
	}
}

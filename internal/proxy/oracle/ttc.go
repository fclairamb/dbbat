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
const (
	TTCFuncSetProtocol  TTCFunctionCode = 0x01 // OSETPRO — session init
	TTCFuncSetDataTypes TTCFunctionCode = 0x02 // ODTYPES — session init
	TTCFuncOOPEN        TTCFunctionCode = 0x03 // OOPEN — open cursor
	TTCFuncOCLOSE       TTCFunctionCode = 0x05 // OCLOSE — close cursor
	TTCFuncResponse     TTCFunctionCode = 0x08 // Server response
	TTCFuncOMarker      TTCFunctionCode = 0x09 // OMARKER — break/reset
	TTCFuncOVersion     TTCFunctionCode = 0x0B // OVERSION — version request
	TTCFuncOALL8        TTCFunctionCode = 0x0E // OALL8 — parse+execute (primary query message)
	TTCFuncOFETCH       TTCFunctionCode = 0x11 // OFETCH — fetch rows
	TTCFuncOCANCEL      TTCFunctionCode = 0x14 // OCANCEL — cancel query
	TTCFuncOLOBOPS      TTCFunctionCode = 0x44 // OLOBOPS — LOB operations
	TTCFuncOSQL7        TTCFunctionCode = 0x47 // OSQL7 — legacy SQL
	TTCFuncOAUTH        TTCFunctionCode = 0x5E // OAUTH — authentication
	TTCFuncOSESSKEY     TTCFunctionCode = 0x73 // OSESSKEY — session key exchange
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
	case TTCFuncOOPEN:
		return "OOPEN"
	case TTCFuncOCLOSE:
		return "OCLOSE"
	case TTCFuncResponse:
		return "Response"
	case TTCFuncOMarker:
		return "OMARKER"
	case TTCFuncOVersion:
		return "OVERSION"
	case TTCFuncOALL8:
		return "OALL8"
	case TTCFuncOFETCH:
		return "OFETCH"
	case TTCFuncOCANCEL:
		return "OCANCEL"
	case TTCFuncOLOBOPS:
		return "OLOBOPS"
	case TTCFuncOSQL7:
		return "OSQL7"
	case TTCFuncOAUTH:
		return "OAUTH"
	case TTCFuncOSESSKEY:
		return "OSESSKEY"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(fc))
	}
}

// IsKnown returns true if this function code is a recognized TTC function.
func (fc TTCFunctionCode) IsKnown() bool {
	switch fc {
	case TTCFuncSetProtocol, TTCFuncSetDataTypes, TTCFuncOOPEN, TTCFuncOCLOSE,
		TTCFuncResponse, TTCFuncOMarker, TTCFuncOVersion, TTCFuncOALL8,
		TTCFuncOFETCH, TTCFuncOCANCEL, TTCFuncOLOBOPS, TTCFuncOSQL7,
		TTCFuncOAUTH, TTCFuncOSESSKEY:
		return true
	default:
		return false
	}
}

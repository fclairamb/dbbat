package decode

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// TTC message types, mirroring internal/proxy/oracle/ttc.go. They are the byte
// right after the Data packet's flags. Note 0x11 is the piggyback message
// type, not a fetch — a real fetch is message type 0x03 with function 0x05,
// and the proxy carries a long comment on the cost of getting that wrong.
const (
	ttcMsgSetProtocol  = 0x01
	ttcMsgSetDataTypes = 0x02
	ttcMsgPiggyback    = 0x03
	ttcMsgOERR         = 0x04
	ttcMsgOClose       = 0x05
	ttcMsgContinuation = 0x06
	ttcMsgResponse     = 0x08
	ttcMsgOClosev2     = 0x09
	ttcMsgOVersion     = 0x0B
	ttcMsgOALL8        = 0x0E
	ttcMsgQueryResult  = 0x10
	ttcMsgPiggyback2   = 0x11
	ttcMsgOCancel      = 0x14
	ttcMsgFastAuth     = 0x22
)

var ttcMessageNames = map[byte]string{
	ttcMsgSetProtocol:  "OSETPRO",
	ttcMsgSetDataTypes: "ODTYPES",
	ttcMsgPiggyback:    "PIGGYBACK",
	ttcMsgOERR:         "OERR",
	ttcMsgOClose:       "OCLOSE",
	ttcMsgContinuation: "CONTINUATION",
	ttcMsgResponse:     "Response",
	ttcMsgOClosev2:     "OCLOSEv2",
	ttcMsgOVersion:     "OVERSION",
	ttcMsgOALL8:        "OALL8",
	ttcMsgQueryResult:  "QRESULT",
	ttcMsgPiggyback2:   "PIGGYBACK2",
	ttcMsgOCancel:      "OCANCEL",
	ttcMsgFastAuth:     "FASTAUTH",
}

// TTC function codes, the byte after a piggyback message type.
const (
	ttcFuncReexecDML     = 0x04
	ttcFuncFetch         = 0x05
	ttcFuncLogoff        = 0x09
	ttcFuncReexecSelect  = 0x4e
	ttcFuncExecSQL       = 0x5e
	ttcFuncCloseCursors  = 0x69
	ttcFuncAuthPhase2    = 0x73
	ttcFuncAuthPhase1    = 0x76
	ttcFuncOCISession    = 0x6b
	ttcFuncEndToEndAttrs = 0x87
	ttcFuncPythonExec    = 0x98
)

var ttcFunctionNames = map[byte]string{
	ttcFuncReexecDML:     "reexec-dml",
	ttcFuncFetch:         "fetch",
	ttcFuncLogoff:        "logoff",
	ttcFuncReexecSelect:  "reexec-select",
	ttcFuncExecSQL:       "exec-sql",
	ttcFuncCloseCursors:  "close-cursors",
	ttcFuncAuthPhase2:    "auth-phase-2",
	ttcFuncAuthPhase1:    "auth-phase-1",
	ttcFuncOCISession:    "oci-session",
	ttcFuncEndToEndAttrs: "set-end-to-end-attrs",
	ttcFuncPythonExec:    "exec-sql-python",
}

var tnsPacketNames = map[byte]string{
	tnsTypeConnect:   "Connect",
	tnsTypeAccept:    "Accept",
	tnsTypeAck:       "Ack",
	tnsTypeRefuse:    "Refuse",
	tnsTypeRedirect:  "Redirect",
	tnsTypeData:      "Data",
	tnsTypeNull:      "Null",
	tnsTypeAbort:     "Abort",
	tnsTypeResend:    "Resend",
	tnsTypeMarker:    "Marker",
	tnsTypeAttention: "Attention",
	tnsTypeControl:   "Control",
}

// Marker sub-codes, mirroring internal/proxy/oracle/markers.go.
var tnsMarkerNames = map[byte]string{
	0x01: "break",
	0x02: "reset",
	0x03: "interrupt",
}

// tnsDataFlagEOF is the flag a peer sets on the empty Data packet that ends a
// session, which is the one flag value worth naming in a trace.
const tnsDataFlagEOF = 0x0040

// tnsAcceptVersionOffset is where an Accept reports the negotiated TNS
// version, which is what decides the packet-length encoding for the rest of
// the session.
const tnsAcceptVersionOffset = 0

func ttcMessageName(msgType byte) string {
	if name, ok := ttcMessageNames[msgType]; ok {
		return name
	}

	return fmt.Sprintf("TTC(0x%02x)", msgType)
}

func ttcFunctionName(code byte) string {
	if name, ok := ttcFunctionNames[code]; ok {
		return name
	}

	return fmt.Sprintf("op=0x%02x", code)
}

// formatTNSPacket names a non-Data packet. Connect and Accept carry the
// version negotiation, which is what a reader wants from them.
func formatTNSPacket(packetType byte, payload []byte) string {
	name, ok := tnsPacketNames[packetType]
	if !ok {
		name = fmt.Sprintf("TNS(0x%02x)", packetType)
	}

	switch packetType {
	case tnsTypeConnect, tnsTypeAccept:
		if len(payload) >= tnsAcceptVersionOffset+2 {
			version := binary.BigEndian.Uint16(payload[tnsAcceptVersionOffset : tnsAcceptVersionOffset+2])

			return fmt.Sprintf("%s version=%d (%d bytes)", name, version, len(payload)+tnsHeaderLen)
		}
	case tnsTypeMarker:
		if len(payload) > 0 {
			return fmt.Sprintf("Marker(%s)", tnsMarkerName(payload[0]))
		}
	}

	return fmt.Sprintf("%s(%d bytes)", name, len(payload)+tnsHeaderLen)
}

func tnsMarkerName(code byte) string {
	if name, ok := tnsMarkerNames[code]; ok {
		return name
	}

	return fmt.Sprintf("0x%02x", code)
}

// ttcNativeServicesMagic opens the Native Security / Native Services
// negotiation Oracle runs between the Accept and the first TTC call. It is not
// a TTC message at all, so it is named rather than misread as one.
var ttcNativeServicesMagic = []byte{0xDE, 0xAD, 0xBE, 0xEF}

// formatTTCCall names the call a Data packet carries, and reports whether it
// is an authentication step — which is what keeps the answer to it redacted
// too.
//
// ttc[0] is the message type; for the two piggyback types ttc[1] is the
// function code, and that is where the meaning is.
func formatTTCCall(ttc []byte) (string, bool) {
	if bytes.HasPrefix(ttc, ttcNativeServicesMagic) {
		return fmt.Sprintf("NativeServices(%d bytes)", len(ttc)), false
	}

	name := ttcMessageName(ttc[0])

	if ttc[0] != ttcMsgPiggyback && ttc[0] != ttcMsgPiggyback2 {
		if ttc[0] == ttcMsgFastAuth {
			return fmt.Sprintf("%s (%d bytes, redacted)", name, len(ttc)), true
		}

		return fmt.Sprintf("%s (%d bytes)", name, len(ttc)), false
	}

	if len(ttc) < 2 {
		return fmt.Sprintf("%s (%d bytes)", name, len(ttc)), false
	}

	function := ttc[1]

	if function == ttcFuncAuthPhase1 || function == ttcFuncAuthPhase2 {
		// O5LOGON: the session key exchange and the verifier. Named, never
		// printed, and --rows does not lift this.
		return fmt.Sprintf("%s %s (%d bytes, redacted)", name, ttcFunctionName(function), len(ttc)), true
	}

	return fmt.Sprintf("%s %s (%d bytes)", name, ttcFunctionName(function), len(ttc)), false
}

// formatTNSFlagSuffix names the data flags worth seeing in a trace.
func formatTNSFlagSuffix(flags uint16) string {
	if flags&tnsDataFlagEOF != 0 {
		return " [eof]"
	}

	if flags != 0 {
		return fmt.Sprintf(" [flags=0x%04x]", flags)
	}

	return ""
}

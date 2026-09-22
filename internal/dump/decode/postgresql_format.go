package decode

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// maxValueLen bounds how much of a single value --rows prints. A trace is one
// message per line; an unbounded bytea would make that unreadable.
const maxValueLen = 120

// formatFrontend renders one client-to-server message.
//
// Statement text is printed: reading it is the point of the tool, and the
// queries table holds it anyway. Bind parameters are not, unless asked for.
// Authentication payloads are never printed.
func formatFrontend(msg pgproto3.FrontendMessage, opts Options) string {
	if text, ok := formatStartupPacket(msg); ok {
		return text
	}

	if text, ok := formatFrontendAuth(msg); ok {
		return text
	}

	switch m := msg.(type) {
	case *pgproto3.Query:
		return "Query " + quote(m.String)
	case *pgproto3.Parse:
		return fmt.Sprintf("Parse stmt=%s %s (%d params)", quote(m.Name), quote(m.Query), len(m.ParameterOIDs))
	case *pgproto3.Bind:
		return formatBind(m, opts)
	case *pgproto3.Describe:
		return "Describe " + objectRef(m.ObjectType, m.Name)
	case *pgproto3.Execute:
		return fmt.Sprintf("Execute portal=%s maxRows=%d", quote(m.Portal), m.MaxRows)
	case *pgproto3.Close:
		return "Close " + objectRef(m.ObjectType, m.Name)
	case *pgproto3.Sync:
		return "Sync"
	case *pgproto3.Flush:
		return "Flush"
	case *pgproto3.Terminate:
		return "Terminate"
	case *pgproto3.CopyData:
		return fmt.Sprintf("CopyData(%d bytes)", len(m.Data))
	case *pgproto3.CopyDone:
		return "CopyDone"
	case *pgproto3.CopyFail:
		return "CopyFail " + quote(m.Message)
	case *pgproto3.FunctionCall:
		return fmt.Sprintf("FunctionCall oid=%d (%d args)", m.Function, len(m.Arguments))
	default:
		return fmt.Sprintf("%T", msg)
	}
}

// formatStartupPacket renders the untyped prologue a client sends before the
// typed stream begins.
func formatStartupPacket(msg pgproto3.FrontendMessage) (string, bool) {
	switch m := msg.(type) {
	case *pgproto3.StartupMessage:
		return formatStartup(m), true
	case *pgproto3.SSLRequest:
		return "SSLRequest", true
	case *pgproto3.GSSEncRequest:
		return "GSSEncRequest", true
	case *pgproto3.CancelRequest:
		return "CancelRequest pid=" + strconv.FormatUint(uint64(m.ProcessID), 10), true
	default:
		return "", false
	}
}

// formatFrontendAuth names the client's authentication replies. Their payloads
// are passwords, SCRAM proofs and GSS tokens: named, never printed, and --rows
// does not lift this.
func formatFrontendAuth(msg pgproto3.FrontendMessage) (string, bool) {
	switch m := msg.(type) {
	case *pgproto3.PasswordMessage:
		return "PasswordMessage (redacted)", true
	case *pgproto3.SASLInitialResponse:
		return "SASLInitialResponse " + quote(m.AuthMechanism) + " (redacted)", true
	case *pgproto3.SASLResponse:
		return "SASLResponse (redacted)", true
	case *pgproto3.GSSResponse:
		return "GSSResponse (redacted)", true
	default:
		return "", false
	}
}

// formatBackend renders one server-to-client message.
func formatBackend(msg pgproto3.BackendMessage, opts Options) string {
	if text, ok := formatAuthentication(msg); ok {
		return text
	}

	if text, ok := formatBackendAck(msg); ok {
		return text
	}

	if text, ok := formatBackendCopy(msg); ok {
		return text
	}

	switch m := msg.(type) {
	case *pgproto3.BackendKeyData:
		// The secret key is a live cancellation token; only the pid is printed.
		return "BackendKeyData pid=" + strconv.FormatUint(uint64(m.ProcessID), 10)
	case *pgproto3.ParameterStatus:
		return fmt.Sprintf("ParameterStatus %s=%s", m.Name, quote(m.Value))
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery " + string(rune(m.TxStatus))
	case *pgproto3.RowDescription:
		return formatRowDescription(m, opts)
	case *pgproto3.DataRow:
		return formatDataRow(m, opts)
	case *pgproto3.CommandComplete:
		return "CommandComplete " + quote(string(m.CommandTag))
	case *pgproto3.ErrorResponse:
		return "ErrorResponse " + formatProblem(m.Severity, m.Code, m.Message)
	case *pgproto3.NoticeResponse:
		return "NoticeResponse " + formatProblem(m.Severity, m.Code, m.Message)
	case *pgproto3.ParameterDescription:
		return fmt.Sprintf("ParameterDescription(%d params)", len(m.ParameterOIDs))
	case *pgproto3.NotificationResponse:
		return fmt.Sprintf("NotificationResponse pid=%d channel=%s (%d byte payload)",
			m.PID, quote(m.Channel), len(m.Payload))
	case *pgproto3.FunctionCallResponse:
		return fmt.Sprintf("FunctionCallResponse(%d bytes)", len(m.Result))
	case *pgproto3.NegotiateProtocolVersion:
		return fmt.Sprintf("NegotiateProtocolVersion(minor=%d, %d unrecognized)",
			m.NewestMinorProtocol, len(m.UnrecognizedOptions))
	default:
		return fmt.Sprintf("%T", msg)
	}
}

// formatBackendAck covers the server messages that carry nothing but their own
// name.
func formatBackendAck(msg pgproto3.BackendMessage) (string, bool) {
	switch msg.(type) {
	case *pgproto3.ParseComplete:
		return "ParseComplete", true
	case *pgproto3.BindComplete:
		return "BindComplete", true
	case *pgproto3.CloseComplete:
		return "CloseComplete", true
	case *pgproto3.NoData:
		return "NoData", true
	case *pgproto3.EmptyQueryResponse:
		return "EmptyQueryResponse", true
	case *pgproto3.PortalSuspended:
		return "PortalSuspended", true
	case *pgproto3.CopyDone:
		return "CopyDone", true
	default:
		return "", false
	}
}

// formatBackendCopy renders the COPY subprotocol. A CopyData payload is table
// data, so it is counted and never printed — not even under --rows, which opts
// into individual values, not into a bulk export.
func formatBackendCopy(msg pgproto3.BackendMessage) (string, bool) {
	switch m := msg.(type) {
	case *pgproto3.CopyInResponse:
		return fmt.Sprintf("CopyInResponse(%d cols, %s)", len(m.ColumnFormatCodes), copyFormat(m.OverallFormat)), true
	case *pgproto3.CopyOutResponse:
		return fmt.Sprintf("CopyOutResponse(%d cols, %s)", len(m.ColumnFormatCodes), copyFormat(m.OverallFormat)), true
	case *pgproto3.CopyBothResponse:
		return fmt.Sprintf("CopyBothResponse(%d cols, %s)", len(m.ColumnFormatCodes), copyFormat(m.OverallFormat)), true
	case *pgproto3.CopyData:
		return fmt.Sprintf("CopyData(%d bytes)", len(m.Data)), true
	default:
		return "", false
	}
}

// formatAuthentication names an authentication message without ever printing
// its payload — salts, SCRAM nonces and server signatures stay out of a trace
// that is meant to be pasteable into a bug report.
func formatAuthentication(msg pgproto3.BackendMessage) (string, bool) {
	switch msg.(type) {
	case *pgproto3.AuthenticationOk:
		return "AuthenticationOk", true
	case *pgproto3.AuthenticationCleartextPassword:
		return "AuthenticationCleartextPassword", true
	case *pgproto3.AuthenticationMD5Password:
		return "AuthenticationMD5Password (redacted)", true
	case *pgproto3.AuthenticationGSS:
		return "AuthenticationGSS", true
	case *pgproto3.AuthenticationGSSContinue:
		return "AuthenticationGSSContinue (redacted)", true
	case *pgproto3.AuthenticationSASL:
		return "AuthenticationSASL", true
	case *pgproto3.AuthenticationSASLContinue:
		return "AuthenticationSASLContinue (redacted)", true
	case *pgproto3.AuthenticationSASLFinal:
		return "AuthenticationSASLFinal (redacted)", true
	default:
		return "", false
	}
}

// formatStartup prints the connection parameters in a stable order. They are
// names (user, database, application_name), not query data, and they are the
// first thing anyone reading a session trace wants.
func formatStartup(msg *pgproto3.StartupMessage) string {
	keys := make([]string, 0, len(msg.Parameters))
	for key := range msg.Parameters {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, fmt.Sprintf("StartupMessage protocol=%d.%d",
		msg.ProtocolVersion>>16, msg.ProtocolVersion&0xFFFF))

	for _, key := range keys {
		parts = append(parts, key+"="+quote(msg.Parameters[key]))
	}

	return strings.Join(parts, " ")
}

// formatBind collapses the parameter values to a count unless --rows was given.
func formatBind(msg *pgproto3.Bind, opts Options) string {
	head := fmt.Sprintf("Bind portal=%s stmt=%s", quote(msg.DestinationPortal), quote(msg.PreparedStatement))

	if !opts.ShowRows {
		return fmt.Sprintf("%s (%d params)", head, len(msg.Parameters))
	}

	return fmt.Sprintf("%s (%d params) %s", head, len(msg.Parameters), formatValues(msg.Parameters))
}

// formatRowDescription prints the field count, and the field names only when
// --rows opted into content.
func formatRowDescription(msg *pgproto3.RowDescription, opts Options) string {
	head := fmt.Sprintf("RowDescription(%d fields)", len(msg.Fields))

	if !opts.ShowRows {
		return head
	}

	names := make([]string, 0, len(msg.Fields))
	for _, field := range msg.Fields {
		names = append(names, string(field.Name))
	}

	return head + " [" + strings.Join(names, ", ") + "]"
}

// formatDataRow is the redaction that matters most: a capture is full of these
// and every one of them is customer data.
func formatDataRow(msg *pgproto3.DataRow, opts Options) string {
	head := fmt.Sprintf("DataRow(%d cols)", len(msg.Values))

	if !opts.ShowRows {
		return head
	}

	return head + " " + formatValues(msg.Values)
}

// formatValues renders a list of wire values, NULL included, each truncated.
func formatValues(values [][]byte) string {
	rendered := make([]string, 0, len(values))

	for _, value := range values {
		if value == nil {
			rendered = append(rendered, "NULL")

			continue
		}

		rendered = append(rendered, quote(truncate(string(value))))
	}

	return "[" + strings.Join(rendered, ", ") + "]"
}

func truncate(s string) string {
	if len(s) <= maxValueLen {
		return s
	}

	return s[:maxValueLen] + "…"
}

// objectRef names what a Describe or Close targets.
func objectRef(objectType byte, name string) string {
	kind := "statement"
	if objectType == 'P' {
		kind = "portal"
	}

	return kind + "=" + quote(name)
}

func copyFormat(overall byte) string {
	if overall == 1 {
		return "binary"
	}

	return "text"
}

// formatProblem renders the parts of an error or notice a trace needs: the
// severity, the SQLSTATE and the primary message.
func formatProblem(severity, code, message string) string {
	return fmt.Sprintf("%s %s: %s", severity, code, collapse(message))
}

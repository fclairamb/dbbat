package decode

import (
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongoMaxDocLen bounds how much extended JSON --rows prints for one document.
// A trace is one message per line and an aggregation pipeline is not.
const mongoMaxDocLen = 400

// mongoOpCodeNames names the opcodes, mirroring internal/proxy/mongodb/wire.go.
var mongoOpCodeNames = map[int32]string{
	1:    "OP_REPLY",
	2001: "OP_UPDATE",
	2002: "OP_INSERT",
	2004: "OP_QUERY",
	2005: "OP_GET_MORE",
	2006: "OP_DELETE",
	2007: "OP_KILL_CURSORS",
	2012: "OP_COMPRESSED",
	2013: "OP_MSG",
}

// mongoCredentialCommands are the commands whose body is a credential or a
// step of proving one. They are named and never printed, and --rows does not
// lift this — in either direction, since the reply carries the server's half
// of the same exchange.
var mongoCredentialCommands = map[string]bool{
	"saslStart":        true,
	"saslContinue":     true,
	"authenticate":     true,
	"getnonce":         true,
	"copydbsaslstart":  true,
	"copydbgetnonce":   true,
	"createUser":       true,
	"updateUser":       true,
	"setParameter":     true,
	"createUserFromDB": true,
}

// mongoSummarizedFields are the command fields that hold query or document
// data. They are reported as a shape — a key count, a stage count, a document
// count — never as content, unless --rows asks.
var mongoSummarizedFields = []struct {
	name string
	noun string
}{
	{"filter", "keys"},
	{"query", "keys"},
	{"pipeline", "stages"},
	{"documents", "docs"},
	{"updates", "updates"},
	{"deletes", "deletes"},
	{"sort", "keys"},
	{"projection", "keys"},
}

func mongoOpCodeName(opCode int32) string {
	if name, ok := mongoOpCodeNames[opCode]; ok {
		return name
	}

	return "opcode " + strconv.FormatInt(int64(opCode), 10)
}

// mongoCommandBody returns the kind-0 section's document, which is the command
// itself.
func mongoCommandBody(sections []mongoSection) (bson.Raw, bool) {
	for _, section := range sections {
		if section.kind == 0 && len(section.documents) == 1 {
			return section.documents[0], true
		}
	}

	return nil, false
}

// mongoCommandName is the first key of the command document, which is where
// MongoDB puts the verb.
func mongoCommandName(doc bson.Raw) string {
	elems, err := doc.Elements()
	if err != nil || len(elems) == 0 {
		return ""
	}

	return elems[0].Key()
}

// mongoIsCredentialCommand reports whether a command must never be printed. A
// hello carrying a speculative authentication counts: the credential rides
// inside the handshake.
func mongoIsCredentialCommand(name string, doc bson.Raw) bool {
	if mongoCredentialCommands[name] {
		return true
	}

	return !doc.Lookup("speculativeAuthenticate").IsZero()
}

// The two redacted renderings below take no Options, and the two constants
// take nothing at all. Every credential-bearing command and every reply to one
// goes through them, so "--rows does not lift this" holds because there is no
// Options in scope to consult — not because a reviewer remembered.

// mongoRedactedReply / mongoRedactedOpReply stand in for the server's half of
// a credential exchange. A reply carries no command name of its own, so it is
// redacted by the request it answers and there is nothing left to render.
const (
	mongoRedactedReply   = "Reply (redacted)"
	mongoRedactedOpReply = "OP_REPLY (redacted)"
)

// formatMongoRedactedCommand names a credential-bearing command and stops
// there: its body is a password, a SCRAM step or a key exchange.
func formatMongoRedactedCommand(name string) string {
	return name + " (redacted)"
}

// formatMongoCommand renders a client command: the verb, what it acts on, and
// the *shape* of the data it carries.
func formatMongoCommand(name string, doc bson.Raw, sections []mongoSection, opts Options) string {
	if name == "" {
		return "OP_MSG(empty command document)"
	}

	parts := []string{name}

	if target, ok := mongoCommandTarget(doc); ok {
		parts = append(parts, target)
	}

	if database, ok := doc.Lookup("$db").StringValueOK(); ok {
		parts = append(parts, "db="+database)
	}

	if opts.ShowRows {
		return strings.Join(parts, " ") + " " + mongoDocumentJSON(doc)
	}

	if summary := summarizeMongoCommand(doc, sections); summary != "" {
		parts = append(parts, "("+summary+")")
	}

	return strings.Join(parts, " ")
}

// mongoCommandTarget names the collection the command runs against: the
// command's own value when it is a string, and the `collection` field for the
// cursor commands whose value is a cursor id instead.
func mongoCommandTarget(doc bson.Raw) (string, bool) {
	elems, err := doc.Elements()
	if err != nil || len(elems) == 0 {
		return "", false
	}

	if value, ok := elems[0].Value().StringValueOK(); ok {
		return value, true
	}

	return doc.Lookup("collection").StringValueOK()
}

// summarizeMongoCommand is the redaction that matters most on MongoDB: a
// document *is* the data, so it is reported by shape and not by content.
func summarizeMongoCommand(doc bson.Raw, sections []mongoSection) string {
	var parts []string

	for _, field := range mongoSummarizedFields {
		count, ok := mongoShapeCount(doc.Lookup(field.name))
		if !ok {
			continue
		}

		parts = append(parts, fmt.Sprintf("%s: %d %s", field.name, count, field.noun))
	}

	for _, section := range sections {
		if section.kind != 1 {
			continue
		}

		parts = append(parts, fmt.Sprintf("%s: %d docs", section.identifier, len(section.documents)))
	}

	return strings.Join(parts, ", ")
}

// mongoShapeCount counts a value's elements: the keys of a document or the
// entries of an array. It never looks at what they hold.
func mongoShapeCount(value bson.RawValue) (int, bool) {
	if value.IsZero() {
		return 0, false
	}

	if doc, ok := value.DocumentOK(); ok {
		elems, err := doc.Elements()
		if err != nil {
			return 0, false
		}

		return len(elems), true
	}

	if array, ok := value.ArrayOK(); ok {
		values, err := array.Values()
		if err != nil {
			return 0, false
		}

		return len(values), true
	}

	return 0, false
}

// formatMongoReply renders the server's answer: whether it worked, and how many
// documents are coming back — never the documents themselves unless --rows.
func formatMongoReply(doc bson.Raw, sections []mongoSection, opts Options) string {
	parts := []string{"Reply"}

	if status, ok := mongoNumber(doc.Lookup("ok")); ok {
		parts = append(parts, "ok="+strconv.FormatInt(status, 10))
	}

	if code, ok := mongoNumber(doc.Lookup("code")); ok {
		parts = append(parts, "code="+strconv.FormatInt(code, 10))
	}

	if errmsg, ok := doc.Lookup("errmsg").StringValueOK(); ok {
		// A server error message, printed as the PostgreSQL splitter prints an
		// ErrorResponse.
		parts = append(parts, quote(errmsg))
	}

	if opts.ShowRows {
		return strings.Join(parts, " ") + " " + mongoDocumentJSON(doc)
	}

	if summary := summarizeMongoReply(doc, sections); summary != "" {
		parts = append(parts, "("+summary+")")
	}

	return strings.Join(parts, " ")
}

// summarizeMongoReply reports a reply by shape: the cursor it opened and how
// many documents its batch holds.
func summarizeMongoReply(doc bson.Raw, sections []mongoSection) string {
	var parts []string

	if cursor, ok := doc.Lookup("cursor").DocumentOK(); ok {
		if id, ok := mongoNumber(cursor.Lookup("id")); ok {
			parts = append(parts, "cursor: "+strconv.FormatInt(id, 10))
		}

		for _, batch := range []string{"firstBatch", "nextBatch"} {
			if count, ok := mongoShapeCount(cursor.Lookup(batch)); ok {
				parts = append(parts, fmt.Sprintf("%s: %d docs", batch, count))
			}
		}
	}

	if count, ok := mongoNumber(doc.Lookup("n")); ok {
		parts = append(parts, "n: "+strconv.FormatInt(count, 10))
	}

	for _, section := range sections {
		if section.kind != 1 {
			continue
		}

		parts = append(parts, fmt.Sprintf("%s: %d docs", section.identifier, len(section.documents)))
	}

	if len(parts) == 0 {
		elems, err := doc.Elements()
		if err == nil {
			parts = append(parts, fmt.Sprintf("%d keys", len(elems)))
		}
	}

	return strings.Join(parts, ", ")
}

// mongoNumber reads an integer out of whichever numeric type BSON used.
func mongoNumber(value bson.RawValue) (int64, bool) {
	if v, ok := value.Int32OK(); ok {
		return int64(v), true
	}

	if v, ok := value.Int64OK(); ok {
		return v, true
	}

	if v, ok := value.DoubleOK(); ok {
		return int64(v), true
	}

	return 0, false
}

// mongoDocumentJSON is what --rows opts into: the document as extended JSON,
// collapsed onto one line and truncated.
func mongoDocumentJSON(doc bson.Raw) string {
	text := collapse(doc.String())
	if len(text) > mongoMaxDocLen {
		text = text[:mongoMaxDocLen] + "…"
	}

	return text
}

// mongoFlagSuffix names the OP_MSG flags worth seeing in a trace.
func mongoFlagSuffix(flags uint32) string {
	if flags&mongoFlagMoreToCome != 0 {
		return " [moreToCome]"
	}

	return ""
}

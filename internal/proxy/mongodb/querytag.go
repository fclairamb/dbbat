package mongodb

import (
	"encoding/binary"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MongoDB has no statement text and therefore no SQL comment to prepend the
// dbbat identity to — but it has the field that convention was invented for:
// `comment`, accepted on the data-bearing commands and echoed back verbatim by
// `system.profile`, the Atlas profiler and `db.currentOp()`. So the same tag
// the PostgreSQL and MySQL proxies put in a comment rides here instead:
//
//	{ find: "widgets", filter: {...}, comment: "dbbat='0.28.1',user='florent',conn='3f9a1c7b2e4d',grant='diag-paris'" }
//
// The value is `shared.QueryTagger.Tag()` — the exact bytes of the SQL
// comment's body, minus the `/* */`. A BSON sub-document of the same four
// values was the alternative and was rejected: the string is what sqlcommenter
// consumers parse, one grep finds a connection across all three protocols, and
// the Atlas profiler shows a string comment inline where it collapses a
// sub-document.
//
// Everything the SQL proxies guarantee holds here too: the tag is applied to
// the bytes going upstream and to nothing else, after every grant control, the
// `$db` check and any approval hold have run on the client's command, so the
// `queries` row, the audit chain, the approval patterns and the `.pcapng`
// capture all keep the command the client sent.

// bsonTypeString is the BSON element type byte for a UTF-8 string, which is
// what `comment` is written as. Spelled out for the same reason
// bsonTypeInt64 is: the element is built by hand rather than by round-tripping
// the forwarded command through a Go struct.
const bsonTypeString = 0x02

// commentKey is the generic MongoDB command option carrying an arbitrary
// string (or any BSON value) that the server attaches to the operation and
// reports back in the profiler, the slow-query log line and currentOp.
const commentKey = "comment"

// commentTaggableCommands are the commands dbbat tags.
//
// It is an allowlist rather than the exemption list maxTimeMS uses, and
// deliberately so: `comment` is documented on the data-bearing commands (the
// ones MongoDB profiles and the ones a DBA hunting load is looking at), while
// several other commands reject fields they do not recognize. A tag is an
// observability nicety — it must never be the reason a command fails — so the
// set is exactly the commands whose `comment` support MongoDB documents, which
// is also the set whose entries in `system.profile` are worth attributing.
//
// The handshake, auth and teardown chatter (hello, ping, saslStart,
// killCursors, endSessions, …) is absent for the same reason it is exempt from
// maxTimeMS: none of it is a statement a user wrote.
var commentTaggableCommands = map[string]bool{
	"aggregate":     true,
	"bulkWrite":     true,
	"count":         true,
	"delete":        true,
	"distinct":      true,
	"find":          true,
	"findAndModify": true,
	"findandmodify": true,
	"getMore":       true,
	"insert":        true,
	"mapReduce":     true,
	"mapreduce":     true,
	"update":        true,
}

// applyQueryTag returns body with the dbbat identity in `comment`, and reports
// whether anything changed.
//
// Inert — returns (body, false, nil) — when DBB_QUERY_TAGGING is off, when the
// command is not one of the taggable ones, and, importantly, when the client
// already sent a `comment`.
//
// **A client-supplied comment wins, and the command is then left untouched.**
// Unlike a SQL comment, which is dead text nothing else owns, `comment` is a
// single-valued field drivers and ORMs set for their own tracing, and whoever
// set it also wrote the consumer that parses it. Appending dbbat's tag into it
// (or promoting it to a sub-document) would hand that consumer a value it never
// agreed to; overwriting it would lose their trace id outright. Skipping is the
// one resolution that breaks nothing — and it costs little here, because on
// this protocol the profiler *also* records `appName`, which dbbat already tags
// with `dbbat/<version> @<user> c=<uid suffix>` on every session
// (shared.BuildUpstreamName). So a command dbbat declines to tag is still
// attributable; it just takes the neighboring column.
func (s *Session) applyQueryTag(body bson.Raw, cmd string) (bson.Raw, bool, error) {
	if !s.queryTag.Active() || !commentTaggableCommands[cmd] {
		return body, false, nil
	}

	return withComment(body, s.queryTag.Tag())
}

// withComment returns doc with `comment` set, and reports whether anything
// changed. A doc that already carries a `comment` of any type is returned
// untouched.
//
// Like withMaxTimeMS, the document is rebuilt element by element from the raw
// bytes, so every value but the added one is forwarded byte-identical, and the
// new element is appended last — the command name stays the first field, which
// is the one ordering rule OP_MSG has.
func withComment(doc bson.Raw, comment string) (bson.Raw, bool, error) {
	if comment == "" {
		return doc, false, nil
	}

	elements, err := doc.Elements()
	if err != nil {
		return doc, false, fmt.Errorf("mongodb: read command elements: %w", err)
	}

	out := make([]byte, 4, len(doc)+len(commentKey)+len(comment)+8)

	for _, e := range elements {
		if e.Key() == commentKey {
			return doc, false, nil
		}

		out = append(out, e...)
	}

	out = append(out, bsonTypeString)
	out = append(out, commentKey...)
	out = append(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(comment)+1))
	out = append(out, comment...)
	out = append(out, 0)
	out = append(out, 0)

	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))

	return out, true, nil
}

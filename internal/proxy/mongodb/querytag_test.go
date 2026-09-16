package mongodb

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/fclairamb/dbbat/internal/proxy/shared"
)

const testTagConnUID = "3f9a1c7b-0000-4000-8000-1c7b2e4d0001"

// taggedSession builds a session carrying an active tag and nothing else, so
// the rewrite path can be exercised without a socket, a store or an upstream.
func taggedSession(t *testing.T) *Session {
	t.Helper()

	uid, err := uuid.Parse(testTagConnUID)
	if err != nil {
		t.Fatalf("parse uid: %v", err)
	}

	return &Session{queryTag: shared.NewQueryTagger("0.28.1", "florent", uid, "diag-paris")}
}

// wantTag is what the fixture session's tag must be: the sqlcommenter comment
// body, with no delimiters, no timestamp and no per-command id.
const wantTag = "dbbat='0.28.1',user='florent',conn='1c7b2e4d0001',grant='diag-paris'"

func TestQueryTagger_TagIsThePrefixWithoutTheDelimiters(t *testing.T) {
	t.Parallel()

	uid, err := uuid.Parse(testTagConnUID)
	if err != nil {
		t.Fatalf("parse uid: %v", err)
	}

	tagger := shared.NewQueryTagger("0.28.1", "florent", uid, "diag-paris")

	if got := tagger.Tag(); got != wantTag {
		t.Fatalf("Tag() = %q, want %q", got, wantTag)
	}

	if got, want := tagger.Prefix(), "/*"+wantTag+"*/ "; got != want {
		t.Fatalf("Prefix() = %q, want %q", got, want)
	}

	// The zero value is inert on this accessor too.
	if got := (shared.QueryTagger{}).Tag(); got != "" {
		t.Fatalf("zero-value Tag() = %q, want \"\"", got)
	}
}

func TestWithComment_InjectsAndPreservesEverythingElse(t *testing.T) {
	t.Parallel()

	in := mustRaw(t, bson.D{
		{Key: "find", Value: "widgets"},
		{Key: "filter", Value: bson.D{{Key: "colour", Value: "red"}}},
		{Key: "$db", Value: "app"},
	})

	out, changed, err := withComment(in, wantTag)
	if err != nil {
		t.Fatalf("withComment: %v", err)
	}

	if !changed {
		t.Fatal("changed = false, want true")
	}

	got, ok := out.Lookup(commentKey).StringValueOK()
	if !ok {
		t.Fatalf("comment missing or not a string in %v", out)
	}

	if got != wantTag {
		t.Fatalf("comment = %q, want %q", got, wantTag)
	}

	// The command name stays first — the one ordering rule OP_MSG has.
	if name := commandName(out); name != "find" {
		t.Fatalf("command name = %q, want find", name)
	}

	if want, got := in.Lookup("$db"), out.Lookup("$db"); want.String() != got.String() {
		t.Fatalf("$db changed: %v -> %v", want, got)
	}

	if want, got := in.Lookup("filter"), out.Lookup("filter"); want.String() != got.String() {
		t.Fatalf("filter changed: %v -> %v", want, got)
	}

	// The rebuilt document declares its own real length.
	if declared := int32(binary.LittleEndian.Uint32(out[0:4])); int(declared) != len(out) {
		t.Fatalf("declared length %d, actual %d", declared, len(out))
	}
}

// TestWithComment_ClientCommentWins is the rule that separates this from a SQL
// comment: `comment` is single-valued and a driver may already own it, so a
// command that carries one is forwarded byte-for-byte.
func TestWithComment_ClientCommentWins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		doc  bson.D
	}{
		{
			name: "string comment",
			doc: bson.D{
				{Key: "find", Value: "widgets"},
				{Key: "comment", Value: "trace-id=abc123"},
			},
		},
		{
			// `comment` accepts any BSON value, so a client's document-shaped
			// one has to be respected exactly like a string.
			name: "document comment",
			doc: bson.D{
				{Key: "aggregate", Value: "widgets"},
				{Key: "comment", Value: bson.D{{Key: "trace", Value: "abc123"}}},
			},
		},
		{
			// An empty string is still the client's choice.
			name: "empty comment",
			doc: bson.D{
				{Key: "update", Value: "widgets"},
				{Key: "comment", Value: ""},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := mustRaw(t, tc.doc)

			out, changed, err := withComment(in, wantTag)
			if err != nil {
				t.Fatalf("withComment: %v", err)
			}

			if changed {
				t.Fatal("changed = true, want false: the client's comment must win")
			}

			if !bytesEqual(out, in) {
				t.Fatalf("document was rewritten: %v -> %v", in, out)
			}
		})
	}
}

func TestApplyQueryTag_OnlyTaggableCommands(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)

	cases := []struct {
		cmd     string
		doc     bson.D
		wantTag bool
	}{
		{cmd: "find", doc: bson.D{{Key: "find", Value: "widgets"}}, wantTag: true},
		{cmd: "aggregate", doc: bson.D{{Key: "aggregate", Value: "widgets"}}, wantTag: true},
		{cmd: "getMore", doc: bson.D{{Key: "getMore", Value: int64(7)}}, wantTag: true},
		{cmd: "update", doc: bson.D{{Key: "update", Value: "widgets"}}, wantTag: true},
		{cmd: "delete", doc: bson.D{{Key: "delete", Value: "widgets"}}, wantTag: true},
		{cmd: "insert", doc: bson.D{{Key: "insert", Value: "widgets"}}, wantTag: true},
		// Handshake / teardown chatter: not a statement anyone wrote, and the
		// commands most likely to reject a field they do not know.
		{cmd: "hello", doc: bson.D{{Key: "hello", Value: int32(1)}}},
		{cmd: "ping", doc: bson.D{{Key: "ping", Value: int32(1)}}},
		{cmd: "endSessions", doc: bson.D{{Key: "endSessions", Value: bson.A{}}}},
		{cmd: "createIndexes", doc: bson.D{{Key: "createIndexes", Value: "widgets"}}},
	}

	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			t.Parallel()

			in := mustRaw(t, tc.doc)

			out, changed, err := s.applyQueryTag(in, tc.cmd)
			if err != nil {
				t.Fatalf("applyQueryTag: %v", err)
			}

			if changed != tc.wantTag {
				t.Fatalf("changed = %v, want %v", changed, tc.wantTag)
			}

			if !tc.wantTag && !bytesEqual(out, in) {
				t.Fatal("an untaggable command was rewritten")
			}

			if tc.wantTag {
				if got, _ := out.Lookup(commentKey).StringValueOK(); got != wantTag {
					t.Fatalf("comment = %q, want %q", got, wantTag)
				}
			}
		})
	}
}

// TestApplyQueryTag_DisabledChangesNothing: the zero-value tagger is what a
// session gets with DBB_QUERY_TAGGING off, and it must not move a byte.
func TestApplyQueryTag_DisabledChangesNothing(t *testing.T) {
	t.Parallel()

	s := &Session{}
	in := mustRaw(t, bson.D{{Key: "find", Value: "widgets"}, {Key: "$db", Value: "app"}})

	out, changed, err := s.applyQueryTag(in, "find")
	if err != nil {
		t.Fatalf("applyQueryTag: %v", err)
	}

	if changed {
		t.Fatal("changed = true with tagging off")
	}

	if !bytesEqual(out, in) {
		t.Fatal("the command was rewritten with tagging off")
	}
}

// TestApplyQueryTag_RepeatedCommandsAreByteIdentical pins the no-timestamp,
// no-per-command-id property: two executions of the same command on the same
// session produce the same bytes, so the target keeps aggregating them.
func TestApplyQueryTag_RepeatedCommandsAreByteIdentical(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	in := mustRaw(t, bson.D{{Key: "find", Value: "widgets"}, {Key: "$db", Value: "app"}})

	first, _, err := s.applyQueryTag(in, "find")
	if err != nil {
		t.Fatalf("applyQueryTag: %v", err)
	}

	time.Sleep(2 * time.Millisecond)

	second, _, err := s.applyQueryTag(in, "find")
	if err != nil {
		t.Fatalf("applyQueryTag: %v", err)
	}

	if !bytesEqual(first, second) {
		t.Fatalf("two executions differ:\n%v\n%v", first, second)
	}
}

// TestPrepareForwarded_ComposesWithMaxTimeMS: both dbbat-owned options land in
// one re-serialized message, and what dbbat *records* — buildSQLText over the
// client's body — never sees either of them.
func TestPrepareForwarded_ComposesWithMaxTimeMS(t *testing.T) {
	t.Parallel()

	s := taggedSession(t)
	s.statementLimit = 3 * time.Second

	body := mustRaw(t, bson.D{{Key: "find", Value: "widgets"}, {Key: "$db", Value: "app"}})
	recorded := buildSQLText("find", body)

	m := opMsgMessage(t, body)

	parsed, err := parseOpMsg(m.body)
	if err != nil {
		t.Fatalf("parseOpMsg: %v", err)
	}

	forwarded, err := s.prepareForwarded(m, parsed, body, "find")
	if err != nil {
		t.Fatalf("prepareForwarded: %v", err)
	}

	roundTripped, err := parseOpMsg(forwarded.body)
	if err != nil {
		t.Fatalf("parseOpMsg(forwarded): %v", err)
	}

	out, ok := roundTripped.commandBody()
	if !ok {
		t.Fatal("forwarded message has no command body")
	}

	if got, _ := asInt64(out.Lookup(maxTimeMSKey)); got != 3000 {
		t.Fatalf("maxTimeMS = %d, want 3000 — the deadline was lost by the tag rewrite", got)
	}

	if got, _ := out.Lookup(commentKey).StringValueOK(); got != wantTag {
		t.Fatalf("comment = %q, want %q", got, wantTag)
	}

	if forwarded.requestID != m.requestID {
		t.Fatalf("requestID = %d, want %d", forwarded.requestID, m.requestID)
	}

	// The recorded text is the client's command: no tag, no injected deadline.
	if strings.Contains(recorded, "dbbat=") || strings.Contains(recorded, maxTimeMSKey) {
		t.Fatalf("the recorded statement carries a dbbat rewrite: %q", recorded)
	}
}

// TestPrepareForwarded_UntouchedCommandKeepsItsBytes: with nothing to inject,
// the very message that arrived is forwarded.
func TestPrepareForwarded_UntouchedCommandKeepsItsBytes(t *testing.T) {
	t.Parallel()

	s := &Session{}
	body := mustRaw(t, bson.D{{Key: "ping", Value: int32(1)}, {Key: "$db", Value: "admin"}})
	m := opMsgMessage(t, body)

	parsed, err := parseOpMsg(m.body)
	if err != nil {
		t.Fatalf("parseOpMsg: %v", err)
	}

	forwarded, err := s.prepareForwarded(m, parsed, body, "ping")
	if err != nil {
		t.Fatalf("prepareForwarded: %v", err)
	}

	if forwarded != m {
		t.Fatal("an untouched command was re-serialized instead of forwarded as it arrived")
	}
}

// opMsgMessage wraps a command document in a minimal OP_MSG message.
func opMsgMessage(t *testing.T, body bson.Raw) *message {
	t.Helper()

	payload := make([]byte, 0, 5+len(body))
	payload = append(payload, 0, 0, 0, 0)
	payload = append(payload, 0)
	payload = append(payload, body...)

	raw := make([]byte, headerLen+len(payload))
	writeHeader(raw, int32(len(raw)), 42, 0, opCodeMsg)
	copy(raw[headerLen:], payload)

	return &message{
		length:    int32(len(raw)),
		requestID: 42,
		opCode:    opCodeMsg,
		body:      raw[headerLen:],
		raw:       raw,
	}
}

func bytesEqual(a, b bson.Raw) bool {
	return string(a) == string(b)
}

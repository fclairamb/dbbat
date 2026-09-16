package mongodb

import (
	"encoding/binary"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestWithMaxTimeMS(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		doc         bson.D
		limitMS     int64
		wantChanged bool
		wantValue   int64
	}{
		{
			name:        "absent is injected",
			doc:         bson.D{{Key: "find", Value: "t"}, {Key: "$db", Value: "app"}},
			limitMS:     1000,
			wantChanged: true,
			wantValue:   1000,
		},
		{
			name:        "larger is clamped",
			doc:         bson.D{{Key: "find", Value: "t"}, {Key: "maxTimeMS", Value: int64(60000)}},
			limitMS:     1000,
			wantChanged: true,
			wantValue:   1000,
		},
		{
			// 0 is MongoDB's "no limit", so it is the widest value a client
			// can send and must be clamped like any other over-limit one.
			name:        "zero is clamped",
			doc:         bson.D{{Key: "find", Value: "t"}, {Key: "maxTimeMS", Value: int32(0)}},
			limitMS:     1000,
			wantChanged: true,
			wantValue:   1000,
		},
		{
			name:        "smaller client value is kept",
			doc:         bson.D{{Key: "find", Value: "t"}, {Key: "maxTimeMS", Value: int32(250)}},
			limitMS:     1000,
			wantChanged: false,
			wantValue:   250,
		},
		{
			name:        "equal client value is kept",
			doc:         bson.D{{Key: "find", Value: "t"}, {Key: "maxTimeMS", Value: float64(1000)}},
			limitMS:     1000,
			wantChanged: false,
			wantValue:   1000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := mustRaw(t, tc.doc)

			out, changed, err := withMaxTimeMS(in, tc.limitMS)
			if err != nil {
				t.Fatalf("withMaxTimeMS: %v", err)
			}

			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}

			got, ok := asInt64(out.Lookup("maxTimeMS"))
			if !ok {
				t.Fatalf("maxTimeMS missing from %v", out)
			}

			if got != tc.wantValue {
				t.Fatalf("maxTimeMS = %d, want %d", got, tc.wantValue)
			}

			// Every other element survives byte-identically.
			if name := commandName(out); name != commandName(in) {
				t.Fatalf("command name changed: %q -> %q", commandName(in), name)
			}

			if want := in.Lookup("$db"); want.Type != 0 {
				if got := out.Lookup("$db"); got.String() != want.String() {
					t.Fatalf("$db changed: %v -> %v", want, got)
				}
			}

			// The rebuilt document declares its own real length.
			if declared := int32(binary.LittleEndian.Uint32(out[0:4])); int(declared) != len(out) {
				t.Fatalf("declared length %d, actual %d", declared, len(out))
			}
		})
	}
}

func TestRebuildOpMsg_PreservesDocumentSequences(t *testing.T) {
	t.Parallel()

	body := mustRaw(t, bson.D{{Key: "insert", Value: "t"}, {Key: "$db", Value: "app"}})
	doc1 := mustRaw(t, bson.D{{Key: "_id", Value: int32(1)}})
	doc2 := mustRaw(t, bson.D{{Key: "_id", Value: int32(2)}})

	// header + flags + kind0 body + kind1 sequence("documents", doc1, doc2)
	payload := make([]byte, 4)
	payload = append(payload, 0)
	payload = append(payload, body...)
	payload = append(payload, 1)

	start := len(payload)
	payload = append(payload, 0, 0, 0, 0)
	payload = append(payload, "documents"...)
	payload = append(payload, 0)
	payload = append(payload, doc1...)
	payload = append(payload, doc2...)
	binary.LittleEndian.PutUint32(payload[start:start+4], uint32(len(payload)-start))

	raw := make([]byte, headerLen+len(payload))
	writeHeader(raw, int32(len(raw)), 7, 0, opCodeMsg)
	copy(raw[headerLen:], payload)

	m := &message{length: int32(len(raw)), requestID: 7, opCode: opCodeMsg, body: raw[headerLen:], raw: raw}

	parsed, err := parseOpMsg(m.body)
	if err != nil {
		t.Fatalf("parseOpMsg: %v", err)
	}

	rewritten, changed, err := withMaxTimeMS(body, 1500)
	if err != nil || !changed {
		t.Fatalf("withMaxTimeMS: changed=%v err=%v", changed, err)
	}

	out, err := rebuildOpMsg(m, parsed, rewritten)
	if err != nil {
		t.Fatalf("rebuildOpMsg: %v", err)
	}

	roundTripped, err := parseOpMsg(out[headerLen:])
	if err != nil {
		t.Fatalf("parseOpMsg(rebuilt): %v", err)
	}

	newBody, ok := roundTripped.commandBody()
	if !ok {
		t.Fatal("rebuilt message has no command body")
	}

	if got, ok := asInt64(newBody.Lookup("maxTimeMS")); !ok || got != 1500 {
		t.Fatalf("maxTimeMS = %d (ok=%v), want 1500", got, ok)
	}

	docs, ok := roundTripped.sequence("documents")
	if !ok || len(docs) != 2 {
		t.Fatalf("document sequence lost: ok=%v len=%d", ok, len(docs))
	}

	if docs[0].Lookup("_id").String() != doc1.Lookup("_id").String() {
		t.Fatal("first bulk document changed")
	}

	// The request id has to survive: it is the key the reply is correlated on.
	if got := int32(binary.LittleEndian.Uint32(out[4:8])); got != 7 {
		t.Fatalf("requestID = %d, want 7", got)
	}

	if declared := int32(binary.LittleEndian.Uint32(out[0:4])); int(declared) != len(out) {
		t.Fatalf("declared length %d, actual %d", declared, len(out))
	}
}

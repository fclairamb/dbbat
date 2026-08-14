package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCanonicalNumber(t *testing.T) {
	t.Parallel()

	cases := []struct {
		literal string
		want    string
	}{
		{"0", "0"},
		{"-0", "0"},
		{"0.0", "0"},
		{"1", "1"},
		{"1.0", "1"},
		{"1.50", "1.5"},
		{"00.5", "0.5"},
		{"1e2", "100"},
		{"1E2", "100"},
		{"1e+2", "100"},
		{"1.5e1", "15"},
		{"1.5e-1", "0.15"},
		{"-1.5e-1", "-0.15"},
		{"0.1e-3", "0.0001"},
		{"12345678901234567890", "12345678901234567890"},
		{"-42", "-42"},
		{"1e-7", "0.0000001"},
	}

	for _, tc := range cases {
		got, err := canonicalNumber(tc.literal)
		if err != nil {
			t.Errorf("canonicalNumber(%q) error = %v", tc.literal, err)

			continue
		}

		if got != tc.want {
			t.Errorf("canonicalNumber(%q) = %q, want %q", tc.literal, got, tc.want)
		}
	}
}

// TestCanonicalNumberIsIdempotent is the property the jsonb round trip needs:
// re-normalizing an already normalized literal must not move it.
func TestCanonicalNumberIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, literal := range []string{"0", "-0", "1.50", "1e2", "1.5e-1", "0.1e-3", "-42"} {
		once, err := canonicalNumber(literal)
		if err != nil {
			t.Fatalf("canonicalNumber(%q) error = %v", literal, err)
		}

		twice, err := canonicalNumber(once)
		if err != nil {
			t.Fatalf("canonicalNumber(%q) error = %v", once, err)
		}

		if once != twice {
			t.Errorf("canonicalNumber is not idempotent for %q: %q then %q", literal, once, twice)
		}
	}
}

func TestCanonicalNumberRejectsAbsurdExponent(t *testing.T) {
	t.Parallel()

	if _, err := canonicalNumber("1e999999"); !errors.Is(err, errCanonicalNumberRange) {
		t.Errorf("canonicalNumber(1e999999) error = %v, want errCanonicalNumberRange", err)
	}
}

// TestCanonicalJSONNormalizesOrderAndWhitespace covers what jsonb does to a
// document on the way in: keys sorted, whitespace dropped, duplicates
// collapsed to the last one.
func TestCanonicalJSONNormalizesOrderAndWhitespace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"key order", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"whitespace", "{\n  \"a\" : 1 }", `{"a":1}`},
		{"nested", `{"z":{"y":2,"x":1},"a":[3,2,1]}`, `{"a":[3,2,1],"z":{"x":1,"y":2}}`},
		{"duplicate keys", `{"a":1,"a":2}`, `{"a":2}`},
		{"numbers", `{"a":1.0,"b":1e2}`, `{"a":1,"b":100}`},
		{"scalars", `{"a":null,"b":true,"c":"x"}`, `{"a":null,"b":true,"c":"x"}`},
		{"array", `[1, 2,  3]`, `[1,2,3]`},
	}

	for _, tc := range cases {
		got, err := canonicalJSON([]byte(tc.input))
		if err != nil {
			t.Errorf("%s: canonicalJSON error = %v", tc.name, err)

			continue
		}

		if string(got) != tc.want {
			t.Errorf("%s: canonicalJSON(%s) = %s, want %s", tc.name, tc.input, got, tc.want)
		}
	}
}

// TestCanonicalJSONEquivalentDocuments is the point of the whole exercise: two
// spellings of the same document must hash the same, or verification reports a
// break every time PostgreSQL re-renders a row.
func TestCanonicalJSONEquivalentDocuments(t *testing.T) {
	t.Parallel()

	a, err := canonicalJSON([]byte(`{"count": 1e2, "name":"x", "flag":true}`))
	if err != nil {
		t.Fatalf("canonicalJSON error = %v", err)
	}

	b, err := canonicalJSON([]byte(`{"flag":true,"name":"x","count":100}`))
	if err != nil {
		t.Fatalf("canonicalJSON error = %v", err)
	}

	if string(a) != string(b) {
		t.Errorf("equivalent documents canonicalized differently: %s vs %s", a, b)
	}
}

func TestCanonicalJSONAbsentVersusEmpty(t *testing.T) {
	t.Parallel()

	got, err := canonicalJSON(nil)
	if err != nil {
		t.Fatalf("canonicalJSON(nil) error = %v", err)
	}

	if got != nil {
		t.Errorf("canonicalJSON(nil) = %q, want nil", got)
	}

	object, err := canonicalJSON([]byte(`{}`))
	if err != nil {
		t.Fatalf("canonicalJSON({}) error = %v", err)
	}

	if string(object) != "{}" {
		t.Errorf("canonicalJSON({}) = %q, want {}", object)
	}
}

func TestCanonicalTimeTruncatesToMicroseconds(t *testing.T) {
	t.Parallel()

	// timestamptz stores microseconds. A nanosecond that survives into the MAC
	// makes the row unverifiable the moment it comes back from PostgreSQL.
	withNanos := time.Date(2026, 8, 10, 12, 34, 56, 123456789, time.UTC)
	truncated := time.Date(2026, 8, 10, 12, 34, 56, 123456000, time.UTC)

	if canonicalTime(withNanos) != canonicalTime(truncated) {
		t.Errorf("canonicalTime kept sub-microsecond precision: %q vs %q",
			canonicalTime(withNanos), canonicalTime(truncated))
	}

	if got, want := canonicalTime(withNanos), "2026-08-10T12:34:56.123456Z"; got != want {
		t.Errorf("canonicalTime() = %q, want %q", got, want)
	}

	// A zone other than UTC must render identically once normalized, because
	// the driver is free to hand back either.
	local := truncated.In(time.FixedZone("somewhere", 3*3600))
	if canonicalTime(local) != canonicalTime(truncated) {
		t.Errorf("canonicalTime is zone-sensitive: %q vs %q",
			canonicalTime(local), canonicalTime(truncated))
	}
}

// TestCanonicalPayloadIsUnambiguous checks the framing: no two different field
// values may produce the same byte string by sliding the boundary between them.
func TestCanonicalPayloadIsUnambiguous(t *testing.T) {
	t.Parallel()

	a := newCanonicalPayload("d")
	a.writeString("one", "ab")
	a.writeString("two", "c")

	b := newCanonicalPayload("d")
	b.writeString("one", "a")
	b.writeString("two", "bc")

	if string(a.bytes()) == string(b.bytes()) {
		t.Error("two distinct field sequences produced the same payload")
	}

	absent := newCanonicalPayload("d")
	absent.writeOptional("x", nil)

	empty := newCanonicalPayload("d")
	empty.writeOptional("x", []byte{})

	if string(absent.bytes()) == string(empty.bytes()) {
		t.Error("an absent field hashed the same as an empty one")
	}
}

// chainKATFixtures are the fixed inputs the known-answer tests below are built
// from. Changing any of them changes the MACs, which is exactly what the tests
// are here to catch: the serialization is part of the on-disk format.
var (
	katChainKey     = []byte("0123456789abcdef0123456789abcdef")
	katRowUID       = uuid.MustParse("018f0000-0000-7000-8000-00000000aaaa")
	katUserUID      = uuid.MustParse("018f0000-0000-7000-8000-00000000bbbb")
	katConnUID      = uuid.MustParse("018f0000-0000-7000-8000-00000000cccc")
	katCreatedAt    = time.Date(2026, 8, 10, 12, 34, 56, 123456000, time.UTC)
	katPreviousMAC  = []byte{0x01, 0x02, 0x03, 0x04}
	katAuditDetails = json.RawMessage(`{"b":1,"a":"x"}`)
)

// TestAuditChainPayloadKnownAnswer pins the audit serialization.
func TestAuditChainPayloadKnownAnswer(t *testing.T) {
	t.Parallel()

	const want = "b4c51581658b00ee6090ce4122764938dffe92226f91075a8316e51cefba8981"

	payload, err := auditChainPayload(
		7, katRowUID, "grant_created", &katUserUID, nil, katAuditDetails, katCreatedAt, katPreviousMAC)
	if err != nil {
		t.Fatalf("auditChainPayload() error = %v", err)
	}

	s := &Store{chainKey: katChainKey}
	if got := hex.EncodeToString(s.chainMAC(payload)); got != want {
		t.Errorf("audit chain MAC = %s, want %s", got, want)
	}
}

// TestQueryChainPayloadKnownAnswer pins the query serialization.
func TestQueryChainPayloadKnownAnswer(t *testing.T) {
	t.Parallel()

	const want = "cdd3e4858bf87bc14af11dd72447eb28edcb9ba2d0d4580fefaa05e6f34be687"

	payload, err := queryChainPayload(
		3, katRowUID, katConnUID, "SELECT 1", json.RawMessage(`{"values":["a"]}`),
		katCreatedAt, katPreviousMAC)
	if err != nil {
		t.Fatalf("queryChainPayload() error = %v", err)
	}

	s := &Store{chainKey: katChainKey}
	if got := hex.EncodeToString(s.chainMAC(payload)); got != want {
		t.Errorf("query chain MAC = %s, want %s", got, want)
	}
}

// TestChainPayloadRespondsToEveryField makes sure no covered field is silently
// dropped from the serialization: flipping any one of them must move the MAC.
func TestChainPayloadRespondsToEveryField(t *testing.T) {
	t.Parallel()

	s := &Store{chainKey: katChainKey}

	base, err := auditChainPayload(
		7, katRowUID, "grant_created", &katUserUID, nil, katAuditDetails, katCreatedAt, katPreviousMAC)
	if err != nil {
		t.Fatalf("auditChainPayload() error = %v", err)
	}

	otherUID := uuid.MustParse("018f0000-0000-7000-8000-00000000dddd")
	variants := map[string]func() ([]byte, error){
		"chain_seq": func() ([]byte, error) {
			return auditChainPayload(8, katRowUID, "grant_created", &katUserUID, nil,
				katAuditDetails, katCreatedAt, katPreviousMAC)
		},
		"uid": func() ([]byte, error) {
			return auditChainPayload(7, otherUID, "grant_created", &katUserUID, nil,
				katAuditDetails, katCreatedAt, katPreviousMAC)
		},
		"event_type": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_revoked", &katUserUID, nil,
				katAuditDetails, katCreatedAt, katPreviousMAC)
		},
		"user_id": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_created", &otherUID, nil,
				katAuditDetails, katCreatedAt, katPreviousMAC)
		},
		"performed_by": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_created", &katUserUID, &otherUID,
				katAuditDetails, katCreatedAt, katPreviousMAC)
		},
		"details": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_created", &katUserUID, nil,
				json.RawMessage(`{"b":2,"a":"x"}`), katCreatedAt, katPreviousMAC)
		},
		"created_at": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_created", &katUserUID, nil,
				katAuditDetails, katCreatedAt.Add(time.Microsecond), katPreviousMAC)
		},
		"prev_mac": func() ([]byte, error) {
			return auditChainPayload(7, katRowUID, "grant_created", &katUserUID, nil,
				katAuditDetails, katCreatedAt, []byte{0x09})
		},
	}

	baseMAC := s.chainMAC(base)

	for field, build := range variants {
		payload, err := build()
		if err != nil {
			t.Errorf("%s: auditChainPayload() error = %v", field, err)

			continue
		}

		if string(s.chainMAC(payload)) == string(baseMAC) {
			t.Errorf("changing %s did not change the MAC", field)
		}
	}
}

// TestChainDomainSeparation stops an audit payload from ever being replayed as
// a query payload, or a genesis from being reused across connections.
func TestChainDomainSeparation(t *testing.T) {
	t.Parallel()

	s := &Store{chainKey: katChainKey}

	other := uuid.MustParse("018f0000-0000-7000-8000-00000000eeee")
	if string(s.queryGenesisMAC(katConnUID)) == string(s.queryGenesisMAC(other)) {
		t.Error("two connections share a genesis MAC")
	}

	if string(s.auditGenesisMAC()) == string(s.queryGenesisMAC(katConnUID)) {
		t.Error("the audit genesis equals a query genesis")
	}
}

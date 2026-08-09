package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Canonical serialization for the tamper-evident chains.
//
// Determinism is the entire feature: a MAC computed when the row is written
// has to be reproducible from the row read back out of PostgreSQL, forever. Two
// things make that non-obvious, and both are handled here rather than left to
// the caller.
//
//  1. timestamptz keeps microseconds, Go keeps nanoseconds. A time.Time that
//     is not truncated before it is hashed will never hash the same again after
//     a round trip. canonicalTime truncates and normalizes the zone.
//
//  2. jsonb is not text. PostgreSQL parses it, sorts the keys, drops the
//     whitespace, keeps only the last of duplicate keys and re-renders the
//     numbers, so the bytes that come back are not the bytes that went in.
//     canonicalJSON is written so that canonicalJSON(jsonb(x)) ==
//     canonicalJSON(x): it applies the same normalizations itself, which makes
//     it a fixed point of the round trip instead of trying to predict
//     PostgreSQL's exact output.
//
// TestCanonicalJSONSurvivesJSONB pins property 2 against a real PostgreSQL.

// errCanonicalNumberRange rejects a JSON number whose exponent would expand to
// an absurd plain-decimal rendering. PostgreSQL's numeric would refuse it long
// before this, so the only thing such a literal can do here is make us allocate
// gigabytes.
var errCanonicalNumberRange = errors.New("json number exponent out of range")

// canonicalNumberMaxDigits bounds the plain-decimal expansion of a number.
const canonicalNumberMaxDigits = 100_000

// canonicalPayload accumulates the fields a MAC covers.
//
// Every field is written length-prefixed and tagged, so no two different field
// sequences can produce the same byte string: an event type of "a" with a
// detail of "bc" cannot collide with an event type of "ab" and a detail of "c".
// Optional fields write a presence byte, which keeps NULL distinct from empty.
type canonicalPayload struct {
	buf bytes.Buffer
}

func newCanonicalPayload(domain string) *canonicalPayload {
	p := &canonicalPayload{}
	p.writeBytes("domain", []byte(domain))

	return p
}

func (p *canonicalPayload) writeBytes(field string, value []byte) {
	var length [8]byte

	p.buf.WriteByte(1) // present

	binary.BigEndian.PutUint64(length[:], uint64(len(field)))
	p.buf.Write(length[:])
	p.buf.WriteString(field)

	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	p.buf.Write(length[:])
	p.buf.Write(value)
}

func (p *canonicalPayload) writeString(field, value string) {
	p.writeBytes(field, []byte(value))
}

func (p *canonicalPayload) writeInt(field string, value int64) {
	p.writeString(field, strconv.FormatInt(value, 10))
}

// writeOptional writes an absence marker for a nil value, so a NULL column and
// an empty one hash differently.
func (p *canonicalPayload) writeOptional(field string, value []byte) {
	if value == nil {
		var length [8]byte

		p.buf.WriteByte(0) // absent

		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		p.buf.Write(length[:])
		p.buf.WriteString(field)

		return
	}

	p.writeBytes(field, value)
}

func (p *canonicalPayload) bytes() []byte {
	return p.buf.Bytes()
}

// canonicalTime renders a timestamp the way it will still render after a round
// trip through a timestamptz column: UTC, truncated to the microsecond
// PostgreSQL actually stores, in a fixed-width layout that does not drop
// trailing zeros the way RFC 3339 formatting does.
func canonicalTime(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000Z")
}

// normalizeStoredTime is canonicalTime's counterpart for values on their way
// *into* the database: the row must store exactly what was hashed.
func normalizeStoredTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// canonicalJSON re-serializes a JSON document into a form that is stable across
// a jsonb round trip: object keys sorted, no insignificant whitespace, numbers
// reduced to a plain decimal that depends only on their value.
//
// nil input (a NULL column) stays nil, which the payload writer turns into an
// absence marker.
func canonicalJSON(raw []byte) ([]byte, error) {
	if raw == nil {
		return nil, nil
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		// An empty jsonb column is stored as NULL, so an empty byte slice can
		// only come from a caller handing us nothing. Treat it as absent
		// rather than failing to parse it.
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("failed to parse json for canonicalization: %w", err)
	}

	var out bytes.Buffer
	if err := writeCanonicalValue(&out, value); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

func writeCanonicalValue(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		return writeCanonicalString(out, v)
	case json.Number:
		normalized, err := canonicalNumber(v.String())
		if err != nil {
			return err
		}

		out.WriteString(normalized)
	case []any:
		out.WriteByte('[')

		for i, item := range v {
			if i > 0 {
				out.WriteByte(',')
			}

			if err := writeCanonicalValue(out, item); err != nil {
				return err
			}
		}

		out.WriteByte(']')
	case map[string]any:
		return writeCanonicalObject(out, v)
	default:
		return fmt.Errorf("%w: %T", errCanonicalUnsupported, value)
	}

	return nil
}

var errCanonicalUnsupported = errors.New("unsupported json value")

func writeCanonicalObject(out *bytes.Buffer, obj map[string]any) error {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	out.WriteByte('{')

	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}

		if err := writeCanonicalString(out, k); err != nil {
			return err
		}

		out.WriteByte(':')

		if err := writeCanonicalValue(out, obj[k]); err != nil {
			return err
		}
	}

	out.WriteByte('}')

	return nil
}

// writeCanonicalString escapes a string with Go's encoder. Which escapes it
// picks does not matter — only that it picks the same ones on both sides, and
// the decoded string is identical on both sides because jsonb preserves string
// values exactly.
func writeCanonicalString(out *bytes.Buffer, s string) error {
	encoded, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to encode json string: %w", err)
	}

	out.Write(encoded)

	return nil
}

// canonicalNumber renders a JSON number literal as a plain decimal that depends
// only on its numeric value: no exponent, no leading zeros, no trailing
// fractional zeros, no negative zero.
//
// That is what makes it survive jsonb. PostgreSQL parses a number into numeric
// and re-renders it — "1e2" comes back as "100", "1.50" keeps its scale — so
// hashing the literal would report a false break. Reducing both the literal and
// whatever PostgreSQL echoes to the same normal form does not.
func canonicalNumber(literal string) (string, error) {
	s := literal
	negative := false

	if s != "" && (s[0] == '-' || s[0] == '+') {
		negative = s[0] == '-'
		s = s[1:]
	}

	mantissa := s
	exponent := 0

	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]

		exp, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil {
			return "", fmt.Errorf("failed to parse json number exponent %q: %w", literal, err)
		}

		exponent = exp
	}

	intPart, fracPart := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart, fracPart = mantissa[:i], mantissa[i+1:]
	}

	digits := intPart + fracPart
	// Position of the decimal point within digits, counted from the left.
	point := len(intPart) + exponent

	lead := 0
	for lead < len(digits) && digits[lead] == '0' {
		lead++
	}

	digits = digits[lead:]
	point -= lead

	digits = strings.TrimRight(digits, "0")

	if digits == "" {
		return "0", nil
	}

	if point > canonicalNumberMaxDigits || point < -canonicalNumberMaxDigits {
		return "", fmt.Errorf("%w: %q", errCanonicalNumberRange, literal)
	}

	var b strings.Builder

	if negative {
		b.WriteByte('-')
	}

	switch {
	case point <= 0:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -point))
		b.WriteString(digits)
	case point >= len(digits):
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", point-len(digits)))
	default:
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	}

	return b.String(), nil
}

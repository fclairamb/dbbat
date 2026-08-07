package store

import (
	"database/sql/driver"
	"fmt"
	"strings"
)

// StringArray is a []string that round-trips through a PostgreSQL `text[]`
// column without going through bun's own array codec.
//
// Why dbbat carries its own: bun's PostgreSQL array *parser*
// (`dialect/pgdialect`, `arrayParser.readNext`) treats an element whose first
// byte is `(` or `[` as a range/composite literal and terminates it at the
// matching bracket, so the rest of the element becomes a *second* element.
// PostgreSQL emits array elements unquoted whenever they need no quoting, so
// a perfectly-stored `{(?i)^DELETE}` reads back as `["(?i)", "^DELETE"]`.
//
// That is not cosmetic for `grant_definitions.approval_patterns`: `(?i)` on
// its own is a regexp that matches *every* statement, so a definition whose
// only pattern is the documented `(?i)^DELETE` form would put an approval
// hold on every query the grant runs — and the surviving `^DELETE` half is
// case-sensitive, exactly what the `(?i)` was there to prevent.
//
// Fields using this type must NOT carry bun's `,array` tag option: the option
// is what selects bun's codec, and the whole point here is to use
// driver.Valuer / sql.Scanner instead. Encoding quotes every element, so the
// literal dbbat writes is never ambiguous; decoding implements the PostgreSQL
// `array_out` grammar (quoted or bare elements, `\` escapes inside quotes)
// and nothing else.
//
// Round-tripping is lossy in exactly one place: a NULL *element* (which
// PostgreSQL renders as a bare `NULL`) decodes to the empty string, because
// []string cannot represent it. No dbbat column stores NULL elements.
type StringArray []string

var (
	_ driver.Valuer = StringArray(nil)
	_ interface {
		Scan(src any) error
	} = (*StringArray)(nil)
)

// Value renders the slice as a PostgreSQL array literal with every element
// double-quoted. A nil slice becomes the empty array rather than NULL: every
// column dbbat uses this type for is `notnull default '{}'`, and an empty
// array is the honest representation of "no entries" anyway.
func (a StringArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.Grow(len(a)*16 + 2)
	b.WriteByte('{')

	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}

		b.WriteByte('"')

		for j := 0; j < len(s); j++ {
			if c := s[j]; c == '"' || c == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(s[j])
		}

		b.WriteByte('"')
	}

	b.WriteByte('}')

	return b.String(), nil
}

// Scan parses a PostgreSQL array literal as emitted by `array_out`.
func (a *StringArray) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*a = nil

		return nil
	case string:
		return a.parse(v)
	case []byte:
		return a.parse(string(v))
	default:
		return fmt.Errorf("store: cannot scan %T into StringArray", src)
	}
}

func (a *StringArray) parse(s string) error {
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("store: cannot parse PostgreSQL array %q", s)
	}

	body := s[1 : len(s)-1]
	if body == "" {
		*a = StringArray{}

		return nil
	}

	out := make(StringArray, 0, 4)

	for i := 0; i < len(body); {
		var (
			elem string
			err  error
		)

		if body[i] == '"' {
			if elem, i, err = parseQuotedArrayElem(body, i); err != nil {
				return fmt.Errorf("store: cannot parse PostgreSQL array %q: %w", s, err)
			}
		} else {
			elem, i = parseBareArrayElem(body, i)
		}

		out = append(out, elem)

		// Every element but the last is followed by the separator.
		if i < len(body) {
			if body[i] != ',' {
				return fmt.Errorf("store: cannot parse PostgreSQL array %q: expected ',' at offset %d", s, i+1)
			}

			i++
		}
	}

	*a = out

	return nil
}

// parseQuotedArrayElem reads a double-quoted element starting at body[i] (the
// opening quote) and returns it unescaped plus the offset just past the
// closing quote.
func parseQuotedArrayElem(body string, i int) (string, int, error) {
	var b strings.Builder

	for i++; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
			if i >= len(body) {
				return "", 0, fmt.Errorf("unterminated escape")
			}

			b.WriteByte(body[i])
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteByte(body[i])
		}
	}

	return "", 0, fmt.Errorf("unterminated quoted element")
}

// parseBareArrayElem reads an unquoted element starting at body[i] and returns
// it plus the offset of the following separator (or the end of the body). A
// bare NULL is a NULL element, which []string renders as the empty string.
func parseBareArrayElem(body string, i int) (string, int) {
	start := i
	for i < len(body) && body[i] != ',' {
		i++
	}

	if raw := body[start:i]; raw != "NULL" {
		return raw, i
	}

	return "", i
}

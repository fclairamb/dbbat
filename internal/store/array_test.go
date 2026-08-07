package store

import (
	"testing"
)

func TestStringArrayValueQuotesEveryElement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   StringArray
		want string
	}{
		{"nil", nil, `{}`},
		{"empty", StringArray{}, `{}`},
		{"plain", StringArray{"a", "b"}, `{"a","b"}`},
		{"leading paren", StringArray{`(?i)^DELETE`}, `{"(?i)^DELETE"}`},
		{"leading bracket", StringArray{`[abc]`}, `{"[abc]"}`},
		{"comma inside", StringArray{`a,b`}, `{"a,b"}`},
		{"quote inside", StringArray{`he said "hi"`}, `{"he said \"hi\""}`},
		{"backslash inside", StringArray{`a\b`}, `{"a\\b"}`},
		{"empty element", StringArray{""}, `{""}`},
		{"literal NULL text", StringArray{"NULL"}, `{"NULL"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := tc.in.Value()
			if err != nil {
				t.Fatalf("Value() error: %v", err)
			}

			if got != tc.want {
				t.Fatalf("Value() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStringArrayScanParsesPostgresOutput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", `{}`, []string{}},
		{"bare elements", `{a,b}`, []string{"a", "b"}},
		// The regression this type exists for: PostgreSQL emits an element
		// needing no quoting bare, and bun's parser splits it at the matching
		// paren. `(?i)^DELETE` must stay one element.
		{"bare leading paren", `{(?i)^DELETE}`, []string{`(?i)^DELETE`}},
		{"bare leading bracket", `{[abc]}`, []string{`[abc]`}},
		{"bare alternation group", `{(a|b)}`, []string{`(a|b)`}},
		{"several bare parens", `{(?i)FOO,X(?i)Y,A)^B,?i)Z,(?s)^X}`, []string{
			`(?i)FOO`, `X(?i)Y`, `A)^B`, `?i)Z`, `(?s)^X`,
		}},
		{"quoted elements", `{"a,b","c"}`, []string{"a,b", "c"}},
		{"escaped quote", `{"he said \"hi\""}`, []string{`he said "hi"`}},
		{"escaped backslash", `{"a\\b"}`, []string{`a\b`}},
		{"empty quoted element", `{""}`, []string{""}},
		{"null element", `{a,NULL,b}`, []string{"a", "", "b"}},
		{"quoted NULL is text", `{"NULL"}`, []string{"NULL"}},
		{"mixed", `{(?i)^DELETE,"a,b",plain}`, []string{`(?i)^DELETE`, "a,b", "plain"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, src := range []any{tc.in, []byte(tc.in)} {
				var got StringArray
				if err := got.Scan(src); err != nil {
					t.Fatalf("Scan(%T) error: %v", src, err)
				}

				if len(got) != len(tc.want) {
					t.Fatalf("Scan(%T) = %q, want %q", src, got, tc.want)
				}

				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("Scan(%T) = %q, want %q", src, got, tc.want)
					}
				}
			}
		})
	}
}

func TestStringArrayRoundTripsThroughItsOwnEncoding(t *testing.T) {
	t.Parallel()

	in := StringArray{
		`(?i)^DELETE`, `[abc]`, `(a|b)`, `a,b`, `he said "hi"`, `a\b`, ``, `NULL`,
		`{nested}`, `(?i)^DROP\s+TABLE`,
	}

	encoded, err := in.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	var out StringArray
	if err := out.Scan(encoded); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if len(out) != len(in) {
		t.Fatalf("round trip = %q, want %q", out, in)
	}

	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("round trip[%d] = %q, want %q", i, out[i], in[i])
		}
	}
}

func TestStringArrayScanNullAndGarbage(t *testing.T) {
	t.Parallel()

	var got StringArray
	if err := got.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}

	if got != nil {
		t.Fatalf("Scan(nil) = %q, want nil", got)
	}

	for _, bad := range []any{`not an array`, `{unterminated`, 42} {
		var got StringArray
		if err := got.Scan(bad); err == nil {
			t.Fatalf("Scan(%v) expected an error, got %q", bad, got)
		}
	}
}

package shared

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateOnRune(t *testing.T) {
	t.Parallel()

	t.Run("short text unchanged", func(t *testing.T) {
		t.Parallel()

		if got := TruncateOnRune("café", 10); got != "café" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("ascii cut at exactly the limit", func(t *testing.T) {
		t.Parallel()

		got := TruncateOnRune(strings.Repeat("a", 20), 8)
		if got != strings.Repeat("a", 8) {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("multi-byte rune straddling the limit", func(t *testing.T) {
		t.Parallel()

		// "é" is two bytes: bytes 7 and 8, so a byte cut at 8 splits it.
		s := strings.Repeat("a", 7) + "é" + "bbb"

		got := TruncateOnRune(s, 8)
		if !utf8.ValidString(got) {
			t.Fatalf("invalid UTF-8: %q", got)
		}

		if got != strings.Repeat("a", 7) {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("non-positive limit", func(t *testing.T) {
		t.Parallel()

		if got := TruncateOnRune("abc", 0); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

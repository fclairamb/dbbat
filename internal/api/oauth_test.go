package api

import (
	"strings"
	"testing"
)

func TestCanonicalizeUsername(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		displayName string
		email       string
		want        string
	}{
		// Acceptance criteria from the spec.
		{
			name:        "single accent in display name",
			displayName: "mélanie.samedi",
			want:        "melanie.samedi",
		},
		{
			name:        "mixed accents and capitals",
			displayName: "José.García",
			want:        "jose.garcia",
		},
		{
			name:        "space and accent",
			displayName: "François Müller",
			want:        "francois.muller",
		},
		{
			name:        "pure ASCII passes through unchanged",
			displayName: "alice.smith",
			want:        "alice.smith",
		},

		// Edge cases.
		{
			name:        "empty display name falls back to user",
			displayName: "",
			email:       "",
			want:        "user",
		},
		{
			name:        "whitespace-only display name falls back to user",
			displayName: "   ",
			email:       "",
			want:        "user",
		},
		{
			name:        "email fallback when display empty",
			displayName: "",
			email:       "alice@example.com",
			want:        "alice",
		},
		{
			name:        "email fallback folds accents in local part",
			displayName: "",
			email:       "josé@example.com",
			want:        "jose",
		},

		// Non-Latin scripts should fall through to the regex strip and the
		// fallback path (no panic, no empty username escaping). The CJK cases
		// are present intentionally to exercise the fallback path.
		{
			name: "CJK display name with email fallback",
			//nolint:gosmopolitan // testing CJK fallback is the point
			displayName: "山田太郎",
			email:       "yamada@example.com",
			// CJK strips to empty → falls back to "user" in canonicalize.
			// Note: when display is non-empty, email is *not* tried in this function.
			want: "user",
		},
		{
			name:        "Cyrillic display name with email fallback",
			displayName: "Иван",
			email:       "ivan@example.com",
			want:        "user",
		},
		{
			name: "all non-Latin and no email at all",
			//nolint:gosmopolitan // testing CJK fallback is the point
			displayName: "山田",
			email:       "",
			want:        "user",
		},

		// Length cap (30 chars).
		{
			name:        "long name truncates at 30",
			displayName: strings.Repeat("a", 50),
			want:        strings.Repeat("a", 30),
		},

		// Mixed content keeps allowed characters.
		{
			name:        "dots and dashes preserved",
			displayName: "alice-smith.jr",
			want:        "alice-smith.jr",
		},
		{
			name:        "underscore preserved",
			displayName: "alice_smith",
			want:        "alice_smith",
		},

		// Letters that don't decompose under NFD: ø, ß, æ, ł — these strip out.
		// The fallback path covers the all-non-decomposable case.
		{
			name:        "non-decomposable letters strip out",
			displayName: "Bjørn",
			want:        "bjrn", // ø strips, b/j/r/n remain
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := canonicalizeUsername(tt.displayName, tt.email)
			if got != tt.want {
				t.Errorf("canonicalizeUsername(%q, %q) = %q, want %q",
					tt.displayName, tt.email, got, tt.want)
			}
		})
	}
}

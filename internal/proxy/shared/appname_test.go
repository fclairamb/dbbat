package shared

import (
	"strings"
	"testing"
)

func TestBuildUpstreamName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		version       string
		username      string
		clientAppName string
		maxLen        int
		want          string
	}{
		{
			name:     "no client app name",
			version:  "1.2.3",
			username: "florent",
			maxLen:   63,
			want:     "dbbat/1.2.3 @florent",
		},
		{
			name:          "whitespace only app name treated as absent",
			version:       "1.2.3",
			username:      "florent",
			clientAppName: "   ",
			maxLen:        63,
			want:          "dbbat/1.2.3 @florent",
		},
		{
			name:          "with client app name",
			version:       "1.2.3",
			username:      "florent",
			clientAppName: "psql",
			maxLen:        63,
			want:          "dbbat/1.2.3 @florent for psql",
		},
		{
			name:          "client app name is trimmed",
			version:       "1.2.3",
			username:      "florent",
			clientAppName: "  psql  ",
			maxLen:        63,
			want:          "dbbat/1.2.3 @florent for psql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := BuildUpstreamName(tt.version, tt.username, tt.clientAppName, tt.maxLen)
			if got != tt.want {
				t.Errorf("BuildUpstreamName(%q, %q, %q, %d) = %q, want %q",
					tt.version, tt.username, tt.clientAppName, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestBuildUpstreamName_TruncatesAppNameFirst(t *testing.T) {
	t.Parallel()

	base := "dbbat/1.2.3 @florent"
	longAppName := strings.Repeat("x", 100)

	got := BuildUpstreamName("1.2.3", "florent", longAppName, 63)

	if len(got) != 63 {
		t.Fatalf("len(got) = %d, want 63", len(got))
	}

	if !strings.HasPrefix(got, base+" for ") {
		t.Errorf("got %q, want prefix %q", got, base+" for ")
	}
}

func TestBuildUpstreamName_PrefixTruncatedWhenTooLong(t *testing.T) {
	t.Parallel()

	// A maxLen shorter than the bare "dbbat/$version @$username" prefix forces
	// truncation of the prefix itself, even with an app name supplied.
	got := BuildUpstreamName("1.2.3", "a-very-long-username-indeed", "psql", 10)

	if len(got) != 10 {
		t.Fatalf("len(got) = %d, want 10", len(got))
	}

	want := "dbbat/1.2.3 @a-very-long-username-indeed"[:10]
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildUpstreamName_ExactlyMaxLength(t *testing.T) {
	t.Parallel()

	base := "dbbat/1.2.3 @florent"
	sep := " for "
	maxLen := len(base) + len(sep) + 5

	got := BuildUpstreamName("1.2.3", "florent", "abcde", maxLen)

	want := base + sep + "abcde"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if len(got) != maxLen {
		t.Errorf("len(got) = %d, want %d", len(got), maxLen)
	}
}

func TestBuildUpstreamName_ZeroMaxLen(t *testing.T) {
	t.Parallel()

	if got := BuildUpstreamName("1.2.3", "florent", "", 0); got != "" {
		t.Errorf("got %q, want empty string", got)
	}

	if got := BuildUpstreamName("1.2.3", "florent", "psql", 0); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

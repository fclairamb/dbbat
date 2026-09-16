package shared

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// testConnUID has a first group ("aaaaaaaa") deliberately different from its
// last group ("0123456789ab"), so any test asserting on the "c=" tag proves
// BuildUpstreamName took the *last* 12 hex characters, not the first.
var testConnUID = uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-0123456789ab")

func TestBuildUpstreamName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		version       string
		username      string
		connUID       uuid.UUID
		clientAppName string
		maxLen        int
		want          string
	}{
		{
			name:     "no connUID, no client app name",
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
			name:          "no connUID, with client app name",
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
		{
			name:     "connUID adds the c= tag, no app name",
			version:  "1.2.3",
			username: "florent",
			connUID:  testConnUID,
			maxLen:   63,
			want:     "dbbat/1.2.3 @florent c=0123456789ab",
		},
		{
			name:          "connUID and app name together, per the spec example",
			version:       "0.28.1",
			username:      "florent",
			connUID:       testConnUID,
			clientAppName: "psql",
			maxLen:        63,
			want:          "dbbat/0.28.1 @florent c=0123456789ab for psql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := BuildUpstreamName(tt.version, tt.username, tt.connUID, tt.clientAppName, tt.maxLen)
			if got != tt.want {
				t.Errorf("BuildUpstreamName(%q, %q, %v, %q, %d) = %q, want %q",
					tt.version, tt.username, tt.connUID, tt.clientAppName, tt.maxLen, got, tt.want)
			}
		})
	}
}

// TestBuildUpstreamName_SuffixIsLastTwelveNotFirst pins down the "c=" tag
// against a uuid whose first and last 12 hex characters are unmistakably
// different, guarding against a regression to the timestamp-carrying prefix
// of a UUIDv7 (which collides across connections opened in the same
// millisecond — the whole reason the suffix is taken from the tail).
func TestBuildUpstreamName_SuffixIsLastTwelveNotFirst(t *testing.T) {
	t.Parallel()

	got := BuildUpstreamName("1.2.3", "florent", testConnUID, "", 63)

	if !strings.Contains(got, "c=0123456789ab") {
		t.Errorf("got %q, want it to contain the last 12 hex chars %q", got, "0123456789ab")
	}

	if strings.Contains(got, "c=aaaaaaaabbbb") {
		t.Errorf("got %q, tag used the first 12 hex chars instead of the last", got)
	}
}

func TestBuildUpstreamName_NilUID(t *testing.T) {
	t.Parallel()

	got := BuildUpstreamName("1.2.3", "florent", uuid.Nil, "psql", 63)

	if strings.Contains(got, "c=") {
		t.Errorf("got %q, want no c= tag for the zero uuid", got)
	}

	want := "dbbat/1.2.3 @florent for psql"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildUpstreamName_TruncationOrder verifies the spec's truncation
// order: the client app name is cut first (down to nothing if there's no
// room at all for it), then the version; "@user" and "c=" are never cut.
func TestBuildUpstreamName_TruncationOrder(t *testing.T) {
	t.Parallel()

	t.Run("app name truncated first, base survives intact", func(t *testing.T) {
		t.Parallel()

		base := "dbbat/1.2.3 @florent c=0123456789ab"
		longAppName := strings.Repeat("x", 100)

		got := BuildUpstreamName("1.2.3", "florent", testConnUID, longAppName, 63)

		if len(got) != 63 {
			t.Fatalf("len(got) = %d, want 63", len(got))
		}

		if !strings.HasPrefix(got, base+" for ") {
			t.Errorf("got %q, want prefix %q", got, base+" for ")
		}
	})

	t.Run("app name dropped entirely when there is no room for any of it", func(t *testing.T) {
		t.Parallel()

		base := "dbbat/1.2.3 @florent c=0123456789ab"

		got := BuildUpstreamName("1.2.3", "florent", testConnUID, "psql", len(base))

		if got != base {
			t.Errorf("got %q, want %q (app name dropped, base untouched)", got, base)
		}
	})

	t.Run("version shrinks next, user and tag never cut", func(t *testing.T) {
		t.Parallel()

		// No room for the full version once the tag is accounted for, but
		// enough room to keep "@user c=<12 hex>" intact.
		userPart := "@florent c=0123456789ab"
		maxLen := len("dbbat/") + 2 + 1 + len(userPart) // 2-char version budget

		got := BuildUpstreamName("1.2.3", "florent", testConnUID, "", maxLen)

		if !strings.HasSuffix(got, " "+userPart) {
			t.Errorf("got %q, want it to end with intact %q", got, " "+userPart)
		}

		if len(got) != maxLen {
			t.Errorf("len(got) = %d, want %d", len(got), maxLen)
		}

		if !strings.HasPrefix(got, "dbbat/1.") {
			t.Errorf("got %q, want version truncated to \"1.\"", got)
		}
	})
}

func TestBuildUpstreamName_ExactlyMaxLength(t *testing.T) {
	t.Parallel()

	base := "dbbat/1.2.3 @florent"
	sep := " for "
	maxLen := len(base) + len(sep) + 5

	got := BuildUpstreamName("1.2.3", "florent", uuid.Nil, "abcde", maxLen)

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

	if got := BuildUpstreamName("1.2.3", "florent", testConnUID, "", 0); got != "" {
		t.Errorf("got %q, want empty string", got)
	}

	if got := BuildUpstreamName("1.2.3", "florent", testConnUID, "psql", 0); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// TestBuildUpstreamName_ProtocolCaps exercises the format against every
// protocol's actual cap, with a realistic app name, and checks the "c="
// suffix always survives untouched — the whole point of the feature.
func TestBuildUpstreamName_ProtocolCaps(t *testing.T) {
	t.Parallel()

	caps := map[string]int{
		"postgresql": 63,
		"oracle":     48,
		"mysql":      256,
		"mssql":      128,
		"mongodb":    128,
	}

	for name, maxLen := range caps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := BuildUpstreamName("0.28.1", "florent", testConnUID, "psql", maxLen)

			if len(got) > maxLen {
				t.Fatalf("len(got) = %d, exceeds cap %d: %q", len(got), maxLen, got)
			}

			if !strings.Contains(got, "c=0123456789ab") {
				t.Errorf("got %q, want the c= tag to survive under the %d-byte cap", got, maxLen)
			}
		})
	}
}

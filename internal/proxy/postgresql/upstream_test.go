package postgresql

import (
	"strings"
	"testing"

	"github.com/fclairamb/dbbat/internal/version"
)

func TestBuildApplicationName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		username      string
		clientAppName string
		want          string
	}{
		{
			name:     "empty client app name",
			username: "florent",
			want:     "dbbat/" + version.Version + " @florent",
		},
		{
			name:          "whitespace only",
			username:      "florent",
			clientAppName: "   ",
			want:          "dbbat/" + version.Version + " @florent",
		},
		{
			name:          "psql client",
			username:      "florent",
			clientAppName: "psql",
			want:          "dbbat/" + version.Version + " @florent for psql",
		},
		{
			name:          "custom app name",
			username:      "florent",
			clientAppName: "myapp",
			want:          "dbbat/" + version.Version + " @florent for myapp",
		},
		{
			name:          "app name with spaces",
			username:      "florent",
			clientAppName: "My Application",
			want:          "dbbat/" + version.Version + " @florent for My Application",
		},
		{
			name:          "app name with leading/trailing spaces",
			username:      "florent",
			clientAppName: "  trimmed  ",
			want:          "dbbat/" + version.Version + " @florent for trimmed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildApplicationName(tt.username, tt.clientAppName)
			if got != tt.want {
				t.Errorf("buildApplicationName(%q, %q) = %q, want %q", tt.username, tt.clientAppName, got, tt.want)
			}
		})
	}
}

func TestBuildApplicationName_MaxLength(t *testing.T) {
	t.Parallel()

	// Test that result never exceeds maxAppNameLen (63 characters)
	longAppName := strings.Repeat("x", 100)
	result := buildApplicationName("florent", longAppName)

	if len(result) > maxAppNameLen {
		t.Errorf("buildApplicationName() returned %d chars, want <= %d", len(result), maxAppNameLen)
	}

	// Verify it still starts with the dbbat/version @username prefix
	expectedPrefix := "dbbat/" + version.Version + " @florent"
	if !strings.HasPrefix(result, expectedPrefix) {
		t.Errorf("buildApplicationName() = %q, want prefix %q", result, expectedPrefix)
	}
}

func TestBuildApplicationName_ExactlyMaxLength(t *testing.T) {
	t.Parallel()

	// Calculate how long the client app name can be to hit exactly 63 chars
	base := "dbbat/" + version.Version + " @florent"
	separator := " for "
	maxClientLen := maxAppNameLen - len(base) - len(separator)

	clientAppName := strings.Repeat("a", maxClientLen)
	result := buildApplicationName("florent", clientAppName)

	expected := base + separator + clientAppName
	if result != expected {
		t.Errorf("buildApplicationName(%q) = %q, want %q", clientAppName, result, expected)
	}

	if len(result) != maxAppNameLen {
		t.Errorf("len(buildApplicationName()) = %d, want exactly %d", len(result), maxAppNameLen)
	}
}

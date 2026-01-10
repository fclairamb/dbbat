package proxy

import (
	"strings"
	"testing"

	"github.com/fclairamb/dbbat/internal/version"
)

func TestBuildApplicationName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		clientAppName string
		want          string
	}{
		{
			name:          "empty client app name",
			clientAppName: "",
			want:          "dbbat-" + version.Version,
		},
		{
			name:          "whitespace only",
			clientAppName: "   ",
			want:          "dbbat-" + version.Version,
		},
		{
			name:          "psql client",
			clientAppName: "psql",
			want:          "dbbat-" + version.Version + " / psql",
		},
		{
			name:          "custom app name",
			clientAppName: "myapp",
			want:          "dbbat-" + version.Version + " / myapp",
		},
		{
			name:          "app name with spaces",
			clientAppName: "My Application",
			want:          "dbbat-" + version.Version + " / My Application",
		},
		{
			name:          "app name with leading/trailing spaces",
			clientAppName: "  trimmed  ",
			want:          "dbbat-" + version.Version + " / trimmed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildApplicationName(tt.clientAppName)
			if got != tt.want {
				t.Errorf("buildApplicationName(%q) = %q, want %q", tt.clientAppName, got, tt.want)
			}
		})
	}
}

func TestBuildApplicationName_MaxLength(t *testing.T) {
	t.Parallel()

	// Test that result never exceeds maxAppNameLen (63 characters)
	longAppName := strings.Repeat("x", 100)
	result := buildApplicationName(longAppName)

	if len(result) > maxAppNameLen {
		t.Errorf("buildApplicationName() returned %d chars, want <= %d", len(result), maxAppNameLen)
	}

	// Verify it still starts with dbbat prefix
	expectedPrefix := "dbbat-" + version.Version
	if !strings.HasPrefix(result, expectedPrefix) {
		t.Errorf("buildApplicationName() = %q, want prefix %q", result, expectedPrefix)
	}
}

func TestBuildApplicationName_ExactlyMaxLength(t *testing.T) {
	t.Parallel()

	// Calculate how long the client app name can be to hit exactly 63 chars
	dbbatName := "dbbat-" + version.Version
	separator := " / "
	maxClientLen := maxAppNameLen - len(dbbatName) - len(separator)

	clientAppName := strings.Repeat("a", maxClientLen)
	result := buildApplicationName(clientAppName)

	expected := dbbatName + separator + clientAppName
	if result != expected {
		t.Errorf("buildApplicationName(%q) = %q, want %q", clientAppName, result, expected)
	}

	if len(result) != maxAppNameLen {
		t.Errorf("len(buildApplicationName()) = %d, want exactly %d", len(result), maxAppNameLen)
	}
}

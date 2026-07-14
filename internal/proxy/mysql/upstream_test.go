package mysql

import (
	"strings"
	"testing"

	"github.com/fclairamb/dbbat/internal/version"
)

func TestBuildUpstreamProgramName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		username          string
		clientProgramName string
		want              string
	}{
		{
			name:     "no client program name",
			username: "florent",
			want:     "dbbat/" + version.Version + " @florent",
		},
		{
			name:              "whitespace only",
			username:          "florent",
			clientProgramName: "   ",
			want:              "dbbat/" + version.Version + " @florent",
		},
		{
			name:              "mysql client",
			username:          "florent",
			clientProgramName: "mysql",
			want:              "dbbat/" + version.Version + " @florent for mysql",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildUpstreamProgramName(tt.username, tt.clientProgramName)
			if got != tt.want {
				t.Errorf("buildUpstreamProgramName(%q, %q) = %q, want %q",
					tt.username, tt.clientProgramName, got, tt.want)
			}
		})
	}
}

func TestBuildUpstreamProgramName_MaxLength(t *testing.T) {
	t.Parallel()

	longAppName := strings.Repeat("x", 1000)
	result := buildUpstreamProgramName("florent", longAppName)

	if len(result) > maxProgramNameLen {
		t.Errorf("buildUpstreamProgramName() returned %d chars, want <= %d", len(result), maxProgramNameLen)
	}

	expectedPrefix := "dbbat/" + version.Version + " @florent"
	if !strings.HasPrefix(result, expectedPrefix) {
		t.Errorf("buildUpstreamProgramName() = %q, want prefix %q", result, expectedPrefix)
	}
}

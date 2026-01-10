// Package version contains build version information set via ldflags.
package version

// Build information variables (set via ldflags during build).
// Example:
//
//	go build -ldflags "-X 'github.com/fclairamb/dbbat/internal/version.Version=1.2.3' \
//	                   -X 'github.com/fclairamb/dbbat/internal/version.Commit=$(git rev-parse --short HEAD)' \
//	                   -X 'github.com/fclairamb/dbbat/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)'"
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

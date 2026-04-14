// Package version provides build-time version metadata.
package version

import "fmt"

// Build-time variables injected via ldflags.
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// String returns a formatted version string.
func String() string {
	return fmt.Sprintf("git-bridge %s (commit: %s, built: %s)", Version, GitCommit, BuildDate)
}

// Package version holds build metadata injected at link time via
// -ldflags "-X github.com/cooloo9871/local-apiserver-lb/internal/version.Version=..."
package version

import "fmt"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String returns a single-line human-readable version description.
func String() string {
	return fmt.Sprintf("local-apiserver-lb %s (commit %s, built %s)", Version, Commit, BuildDate)
}

// Package buildinfo carries version metadata stamped in at link time by the Makefile.
package buildinfo

import "fmt"

// Set with -ldflags "-X github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo.Version=...".
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a one-line description of the build, for --version output.
func String(binary string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s)", binary, Version, Commit, Date)
}

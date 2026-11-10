// Package buildinfo exposes version information injected at build time.
package buildinfo

import "runtime"

// Set via -ldflags "-X github.com/unmadethings/unthrecover/internal/buildinfo.Version=..."
var (
	Version = "0.1.0-alpha.dev"
	Commit  = "unknown"
	Date    = "unknown"

	// Google OAuth client of the project ("Desktop app" type), embedded by the
	// release build. For installed apps Google does not treat the client
	// secret as confidential. Overridable with UNTH_GOOGLE_CLIENT_ID/_SECRET.
	GoogleClientID     = ""
	GoogleClientSecret = ""
)

// GoVersion returns the Go toolchain version used to build the binary.
func GoVersion() string { return runtime.Version() }

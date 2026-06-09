// Package version exposes build metadata, injected via -ldflags at build time.
package version

// These are overridden at build time with -X flags by the mise build task.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}

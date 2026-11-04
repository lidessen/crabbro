// Package version is set at build time with -ldflags.
package version

// Version is the marketing version. install.sh passes the git description.
var Version = "0.1.0"

// Commit is the git commit the binary was built from.
var Commit = "unknown"

// String is the version line the daemon logs.
func String() string {
	if Commit == "" || Commit == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ")"
}

// Package version exposes the remmote release version.
package version

// Version is updated from the versioned commit history at release builds.
// The Makefile injects the latest history version with -ldflags.
var Version = "0.12.2"

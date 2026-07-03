// Package version holds the build-time version metadata. Version is
// overridden at release time via
// -ldflags "-X github.com/ctourriere/codebeam/internal/version.Version=v1.2.3".
package version

var Version = "dev"

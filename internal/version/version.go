// Package version holds the build version, set at link time:
//
//	go build -ldflags "-X github.com/aist-dev/nostromo/internal/version.Version=v1.2.3"
package version

// Version is the semantic version of this build, with or without a leading "v".
// Development builds report "dev".
var Version = "dev"

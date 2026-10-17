// Package version reports the server's build version.
package version

import "runtime/debug"

// version is set at link time:
//
//	go build -ldflags "-X github.com/biga-sh/server/internal/version.version=1.2.3"
var version = "1.0.0"

// String is the server version: the link-time value, else the module version
// `go install` recorded, else "dev".
func String() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

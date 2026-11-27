// Package version is the single source of etcd-walker's user-facing version.
//
// Version below is the one place a release bumps (DESIGN §9.3). The Makefile
// and build-deb.sh read it out of this file rather than declaring their own,
// and pass it back in with -ldflags "-X …/pkg/version.Version=…", which is
// also how `make VERSION=0.12.0-rc1` stamps a one-off build without editing
// source. A plain `go build` needs no flags and still reports the right
// version.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is the release version. A var, not a const, so -ldflags -X can
// override it.
var Version = "0.11.0"

// Commit is the VCS revision the binary was built from. Normally left empty:
// String falls back to what the Go toolchain stamped into the build info
// (vcs.revision), which `go build` records for a module built inside its git
// checkout. Packaging that builds outside a checkout can set it via -X.
var Commit = ""

// commitFromBuildInfo returns the short VCS revision recorded by the
// toolchain, with a "-dirty" suffix when the tree had local modifications.
func commitFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// String is what `etcd-walker -version` prints: version, commit (when known),
// Go toolchain and target platform — enough to identify a binary from a bug
// report.
func String() string {
	commit := Commit
	if commit == "" {
		commit = commitFromBuildInfo()
	}
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("etcd-walker %s (commit %s, %s, %s/%s)",
		Version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

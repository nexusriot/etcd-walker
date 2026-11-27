package version

import (
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestStringCarriesVersionAndPlatform(t *testing.T) {
	s := String()
	for _, want := range []string{"etcd-walker " + Version, runtime.GOOS + "/" + runtime.GOARCH, runtime.Version()} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}

func TestExplicitCommitWins(t *testing.T) {
	old := Commit
	defer func() { Commit = old }()
	Commit = "abc123"
	if s := String(); !strings.Contains(s, "commit abc123") {
		t.Errorf("String() = %q, want the -X commit", s)
	}
}

// The Makefile and build-deb.sh extract the version with a sed over this exact
// line shape; reformatting it (const, alignment, a comment on the line) would
// silently give them an empty version.
func TestVersionLineShapeIsWhatThePackagingParses(t *testing.T) {
	raw, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^var Version = "(.*)"$`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal(`version.go lost its 'var Version = "x.y.z"' line that the Makefile seds out`)
	}
	if m[1] != Version {
		t.Errorf("parsed %q, Version is %q", m[1], Version)
	}
}

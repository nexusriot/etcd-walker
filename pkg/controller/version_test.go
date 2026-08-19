package controller

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The user-facing version is duplicated in three files (DESIGN §9.3) until
// F-3 single-sources it via ldflags. They have drifted before — the header
// said 0.8.0 while the roadmap and Makefile had moved on — and a stale
// version in a shipped .deb is the kind of thing nobody notices until a bug
// report cites the wrong build. Pin them together.
func TestVersionIsConsistentAcrossTheRepo(t *testing.T) {
	// Tests run in their package directory; the repo root is two levels up.
	const root = "../.."

	cases := []struct {
		file    string
		pattern string
	}{
		{root + "/Makefile", `(?m)^VERSION\s*\?=\s*(\S+)`},
		{root + "/build-deb.sh", `(?m)^version=(\S+)`},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("reading %s: %v", tc.file, err)
		}
		m := regexp.MustCompile(tc.pattern).FindStringSubmatch(string(raw))
		if m == nil {
			t.Errorf("%s: no version line matching %s — did the file's shape change?",
				tc.file, tc.pattern)
			continue
		}
		if got := strings.TrimSpace(m[1]); got != appVersion {
			t.Errorf("%s declares version %q, but controller.appVersion is %q — "+
				"DESIGN §9.3: all three must be bumped together", tc.file, got, appVersion)
		}
	}
}

// DEBIAN/control must keep its placeholder: the Makefile and build-deb.sh both
// sed it to the real version at package time, so a literal there would ship a
// stale version no matter what the other three say.
func TestDebianControlUsesVersionPlaceholder(t *testing.T) {
	raw, err := os.ReadFile("../../DEBIAN/control")
	if err != nil {
		t.Fatalf("reading DEBIAN/control: %v", err)
	}
	if !strings.Contains(string(raw), "Version: _version_") {
		t.Errorf("DEBIAN/control should carry the _version_ placeholder the "+
			"packaging scripts substitute, got:\n%s", raw)
	}
}

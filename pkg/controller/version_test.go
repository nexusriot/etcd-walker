package controller

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// F-3 single-sourced the version in pkg/version (DESIGN §9.3). Before that it
// was repeated in three files and drifted more than once. Pin the packaging
// scripts to *derive* it: a literal version reappearing in either one is the
// drift coming back.
func TestPackagingDerivesVersionFromSource(t *testing.T) {
	const root = "../.."
	for _, file := range []string{root + "/Makefile", root + "/build-deb.sh"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		s := string(raw)
		if !strings.Contains(s, "pkg/version/version.go") {
			t.Errorf("%s no longer reads the version from pkg/version/version.go", file)
		}
		if !strings.Contains(s, ".Version=") {
			t.Errorf("%s no longer stamps the version via -ldflags -X", file)
		}
		if m := regexp.MustCompile(`(?m)^(VERSION\s*\?=|version=)\s*[0-9]`).FindString(s); m != "" {
			t.Errorf("%s hard-codes a version again (%q) — read it from pkg/version", file, m)
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

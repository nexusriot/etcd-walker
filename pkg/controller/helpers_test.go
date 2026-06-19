package controller

import "testing"

func TestParseTTLInput(t *testing.T) {
	ok := map[string]int64{
		"0":      0,
		"3600":   3600,
		"  90 ":  90,
		"45s":    45,
		"90m":    5400,
		"1h30m":  5400,
		"2h":     7200,
		"1500ms": 1, // rounds down to whole seconds
	}
	for in, want := range ok {
		got, err := parseTTLInput(in)
		if err != nil {
			t.Errorf("parseTTLInput(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseTTLInput(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"-1", "-5m", "abc", "1h2x", "500ms"}
	for _, in := range bad {
		if got, err := parseTTLInput(in); err == nil {
			t.Errorf("parseTTLInput(%q) = %d, want error", in, got)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	cases := map[int64]string{
		0:    "none",
		-1:   "none",
		45:   "45s (45s)",
		90:   "1m30s (90s)",
		3723: "1h2m3s (3723s)",
	}
	for secs, want := range cases {
		if got := formatTTL(secs); got != want {
			t.Errorf("formatTTL(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestNormAbs(t *testing.T) {
	cases := map[string]string{
		"":             "/",
		"   ":          "/",
		"/":            "/",
		"foo":          "/foo",
		"/foo/":        "/foo",
		"/foo//bar":    "/foo/bar",
		"foo/bar/":     "/foo/bar",
		"///a///b///":  "/a/b",
		"  /foo/bar  ": "/foo/bar",
	}
	for in, want := range cases {
		if got := normAbs(in); got != want {
			t.Errorf("normAbs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParentOf(t *testing.T) {
	cases := map[string]string{
		"/":            "/",
		"/foo":         "/",
		"/foo/":        "/",
		"/foo/bar":     "/foo",
		"/foo/bar/baz": "/foo/bar",
		"foo/bar":      "/foo",
	}
	for in, want := range cases {
		if got := parentOf(in); got != want {
			t.Errorf("parentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseOf(t *testing.T) {
	cases := map[string]string{
		"/":         "/",
		"/foo":      "foo",
		"/foo/":     "foo",
		"/foo/bar":  "bar",
		"/foo/bar/": "bar",
		"foo":       "foo",
	}
	for in, want := range cases {
		if got := baseOf(in); got != want {
			t.Errorf("baseOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDepthOf(t *testing.T) {
	cases := map[string]int{
		"/":            0,
		"/foo":         1,
		"/foo/bar":     2,
		"/foo/bar/":    2,
		"/foo/bar/baz": 3,
	}
	for in, want := range cases {
		if got := depthOf(in); got != want {
			t.Errorf("depthOf(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestValueStats(t *testing.T) {
	type want struct {
		bytes     int
		lines     int
		printable bool
	}
	cases := []struct {
		in string
		w  want
	}{
		{"", want{0, 1, true}},
		{"abc", want{3, 1, true}},
		{"a\nb", want{3, 2, true}},
		{"a\nb\n", want{4, 3, true}},
		{"\xff\xfe", want{2, 1, false}},
	}
	for _, c := range cases {
		b, l, p := valueStats(c.in)
		if b != c.w.bytes || l != c.w.lines || p != c.w.printable {
			t.Errorf("valueStats(%q) = (%d,%d,%t), want (%d,%d,%t)",
				c.in, b, l, p, c.w.bytes, c.w.lines, c.w.printable)
		}
	}
}

func TestShortHash(t *testing.T) {
	// 8 bytes hex-encoded => 16 chars, deterministic, value-sensitive.
	h1 := shortHash("hello")
	if len(h1) != 16 {
		t.Errorf("shortHash len = %d, want 16", len(h1))
	}
	if h1 != shortHash("hello") {
		t.Error("shortHash not deterministic")
	}
	if h1 == shortHash("world") {
		t.Error("shortHash collision for distinct inputs")
	}
}

func TestMakeMapKeyAndDisplayName(t *testing.T) {
	if makeMapKey("foo", true) != "foo|dir" {
		t.Errorf("makeMapKey dir = %q", makeMapKey("foo", true))
	}
	if makeMapKey("foo", false) != "foo|file" {
		t.Errorf("makeMapKey file = %q", makeMapKey("foo", false))
	}
	if makeMapKey("foo", true) == makeMapKey("foo", false) {
		t.Error("dir and file map keys must differ for the same basename")
	}
	if displayName("foo", true) != "foo/" {
		t.Errorf("displayName dir = %q", displayName("foo", true))
	}
	if displayName("foo", false) != "foo" {
		t.Errorf("displayName file = %q", displayName("foo", false))
	}
}

func TestSplitFunc(t *testing.T) {
	if !splitFunc('/') || splitFunc('a') {
		t.Error("splitFunc should split only on '/'")
	}
}

func TestGetPosition(t *testing.T) {
	c := &Controller{}
	s := []string{"a", "b", "c"}
	if c.getPosition("b", s) != 1 {
		t.Errorf("getPosition(b) = %d, want 1", c.getPosition("b", s))
	}
	if c.getPosition("missing", s) != 0 {
		t.Errorf("getPosition(missing) = %d, want 0 (fallback)", c.getPosition("missing", s))
	}
}

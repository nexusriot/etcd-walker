package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/model"
)

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

// Jumping to "/" hands injectNode a node named "/"; that used to panic with
// an index-out-of-range slicing the empty basename.
func TestInjectNodeRootIsSafe(t *testing.T) {
	c := &Controller{injected: make(map[string]map[string]*model.Node)}
	c.injectNode(&model.Node{Name: "/", IsDir: true})
	if len(c.injected) != 0 {
		t.Errorf("root must not be injected: %+v", c.injected)
	}
	c.removeInjected(&model.Node{Name: "/", IsDir: true}) // must not panic
}

func TestInjectAndRemoveNode(t *testing.T) {
	c := &Controller{injected: make(map[string]map[string]*model.Node)}
	nd := &model.Node{Name: "/a/_b", IsDir: false, Value: "v"}
	c.injectNode(nd)
	bucket := c.injected["/a/"]
	if bucket == nil || bucket["_b|file"] == nil {
		t.Fatalf("node not injected under /a/: %+v", c.injected)
	}
	c.removeInjected(nd)
	if len(c.injected) != 0 {
		t.Errorf("bucket not cleaned after removal: %+v", c.injected)
	}
}

func TestPrettyJSON(t *testing.T) {
	if got, ok := prettyJSON(`{"b":1,"a":[2,3]}`); !ok || !strings.Contains(got, "\n  \"b\": 1") {
		t.Errorf("object not prettified: ok=%t got=%q", ok, got)
	}
	if got, ok := prettyJSON(`[1,2]`); !ok || got != "[\n  1,\n  2\n]" {
		t.Errorf("array not prettified: ok=%t got=%q", ok, got)
	}
	if _, ok := prettyJSON(`  {"x": true} `); !ok {
		t.Error("surrounding whitespace should not defeat detection")
	}
	// Non-JSON and scalars keep their raw preview.
	for _, in := range []string{"", "42", `"str"`, "plain text", "{broken"} {
		if _, ok := prettyJSON(in); ok {
			t.Errorf("prettyJSON(%q) should not report JSON", in)
		}
	}
}

func TestHexDump(t *testing.T) {
	got := hexDump("AB\x00\xff", 256)
	want := "00000000  41 42 00 ff                                       |AB..|\n"
	if got != want {
		t.Errorf("hexDump = %q, want %q", got, want)
	}

	// Truncation at max bytes; 16 bytes per row.
	long := strings.Repeat("a", 40)
	rows := strings.Count(hexDump(long, 32), "\n")
	if rows != 2 {
		t.Errorf("hexDump(40 bytes, max 32) rows = %d, want 2", rows)
	}
	if hexDump("", 16) != "" {
		t.Error("hexDump of empty string should be empty")
	}
}

// The list must sort by basename, not by the "<base>|dir" map key — '|'
// sorts after alphanumerics, which used to put "app2/" before "app/" and
// made search() (which sorted by display name) select the wrong row.
func TestOrderedEntriesSortsByBasename(t *testing.T) {
	nodes := map[string]*Node{
		"app|dir":  {node: &model.Node{Name: "/x/app", IsDir: true}},
		"app2|dir": {node: &model.Node{Name: "/x/app2", IsDir: true}},
		"a|file":   {node: &model.Node{Name: "/x/a"}},
		"ab|file":  {node: &model.Node{Name: "/x/ab"}},
	}
	mks, display := orderedEntries(nodes)
	wantMks := []string{"app|dir", "app2|dir", "a|file", "ab|file"}
	wantDisplay := []string{"app/", "app2/", "a", "ab"}
	if !reflect.DeepEqual(mks, wantMks) {
		t.Errorf("mks = %v, want %v", mks, wantMks)
	}
	if !reflect.DeepEqual(display, wantDisplay) {
		t.Errorf("display = %v, want %v", display, wantDisplay)
	}
}

package controller

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
)

// fakeModel is an in-memory modelAPI: listings and gets are served from maps,
// mutations are no-ops (SetKeepTTL records its call so restore tests can
// assert TTL preservation). Directory keys must carry their trailing slash,
// the way the controller passes currentDir around.
type fakeModel struct {
	nodes     map[string][]*model.Node     // "/dir/" -> children
	gets      map[string]*model.Node       // "/dir/key" -> node
	lsErr     map[string]error             // "/dir/" -> forced Ls failure
	hist      map[string][]*model.Revision // "/dir/key" -> canned history
	histTrunc bool
	histErr   error
	histCalls int

	setKeepCalls []setKeepCall
}

type setKeepCall struct {
	key, value string
	leaseID    int64
	ttl        int64
}

func (f *fakeModel) ProtocolVersion() string { return "v3" }
func (f *fakeModel) AuthLabel() string       { return "OFF" }

func (f *fakeModel) Ls(dir string) ([]*model.Node, error) {
	if err := f.lsErr[dir]; err != nil {
		return nil, err
	}
	return f.nodes[dir], nil
}

func (f *fakeModel) Get(key string) (*model.Node, error) {
	if n, ok := f.gets[key]; ok {
		return n, nil
	}
	return nil, fmt.Errorf("not found: %s", key)
}

func (f *fakeModel) Set(string, string) error           { return nil }
func (f *fakeModel) SetTTL(string, string, int64) error { return nil }
func (f *fakeModel) SetKeepTTL(key, value string, leaseID, ttl int64) error {
	f.setKeepCalls = append(f.setKeepCalls, setKeepCall{key, value, leaseID, ttl})
	return nil
}
func (f *fakeModel) MkDir(string) error                               { return nil }
func (f *fakeModel) Del(string) error                                 { return nil }
func (f *fakeModel) DelDir(string) error                              { return nil }
func (f *fakeModel) RenameDir(string, string) error                   { return nil }
func (f *fakeModel) RenameKey(string, string) error                   { return nil }
func (f *fakeModel) CopyKey(string, string) error                     { return nil }
func (f *fakeModel) CopyDir(string, string) error                     { return nil }
func (f *fakeModel) Export(string) (map[string]string, error)         { return nil, nil }
func (f *fakeModel) Import(map[string]string, bool) (int, int, error) { return 0, 0, nil }
func (f *fakeModel) Search(string, string, bool, int) ([]*model.Node, bool, error) {
	return nil, false, nil
}
func (f *fakeModel) History(key string, limit int) ([]*model.Revision, bool, error) {
	f.histCalls++
	if f.histErr != nil {
		return nil, false, f.histErr
	}
	return f.hist[key], f.histTrunc, nil
}

// newTestController wires a Controller to headless tview widgets (no screen
// needed as long as the app never runs) and the given fake model.
func newTestController(m modelAPI) *Controller {
	v := &view.View{
		App:     tview.NewApplication(),
		Pages:   tview.NewPages(),
		List:    tview.NewList().ShowSecondaryText(false),
		Details: tview.NewTextView().SetDynamicColors(true),
		ModalEdit: func(p tview.Primitive, width, height int) tview.Primitive {
			return p
		},
	}
	return &Controller{
		view:        v,
		model:       m,
		currentDir:  "/",
		lastGoodDir: "/",
		position:    make(map[string]int),
		injected:    make(map[string]map[string]*model.Node),
	}
}

// currentMapKey returns the secondary text (mapKey) of the focused list row.
func currentMapKey(c *Controller) string {
	_, sec := c.view.List.GetItemText(c.view.List.GetCurrentItem())
	return strings.TrimSpace(sec)
}

func TestMakeNodeMapMergesInjected(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {
			{Name: "/a", IsDir: false, Value: "server"},
			{Name: "/b", IsDir: true},
		},
	}})
	// Hidden key visible only via the injected cache…
	c.injectNode(&model.Node{Name: "/_h", Value: "hidden"})
	// …and an injected duplicate of /a that must lose to the server listing.
	c.injectNode(&model.Node{Name: "/a", Value: "stale"})

	if err := c.makeNodeMap(); err != nil {
		t.Fatal(err)
	}
	if len(c.currentNodes) != 3 {
		t.Fatalf("nodes = %v, want a|file, b|dir, _h|file", c.currentNodes)
	}
	if c.currentNodes["_h|file"] == nil {
		t.Error("injected hidden key missing from node map")
	}
	if got := c.currentNodes["a|file"].node.Value; got != "server" {
		t.Errorf("server listing must win over injected duplicate, got %q", got)
	}
}

func TestUpdateListOrdersAndCaches(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {
			{Name: "/app", IsDir: true},
			{Name: "/app2", IsDir: true},
			{Name: "/k"},
		},
	}})
	ordered := c.updateList()
	want := []string{"app/", "app2/", "k"}
	if strings.Join(ordered, ",") != strings.Join(want, ",") {
		t.Errorf("ordered = %v, want %v", ordered, want)
	}
	if strings.Join(c.ordered, ",") != strings.Join(want, ",") {
		t.Errorf("c.ordered not cached: %v", c.ordered)
	}
	// [..] on top, then the three entries.
	if n := c.view.List.GetItemCount(); n != 4 {
		t.Errorf("list rows = %d, want 4", n)
	}
	if c.lastGoodDir != "/" {
		t.Errorf("lastGoodDir = %q, want /", c.lastGoodDir)
	}
}

// A failing listing must not kill the session: the controller reports the
// error and falls back to the last directory that listed cleanly.
func TestUpdateListFallsBackOnLsError(t *testing.T) {
	c := newTestController(&fakeModel{
		nodes: map[string][]*model.Node{
			"/": {{Name: "/sub", IsDir: true}},
		},
		lsErr: map[string]error{"/sub/": errors.New("boom")},
	})
	c.updateList() // establish lastGoodDir = "/"

	c.Down("sub") // navigates into /sub/, whose listing fails

	if c.currentDir != "/" {
		t.Errorf("currentDir = %q, want fallback to /", c.currentDir)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("error modal should be shown for the failed listing")
	}
	if len(c.ordered) != 1 || c.ordered[0] != "sub/" {
		t.Errorf("fallback listing = %v, want [sub/]", c.ordered)
	}
}

func TestDownUpNavigation(t *testing.T) {
	c := newTestController(&fakeModel{})

	c.Down("x")
	if c.currentDir != "/x/" {
		t.Errorf("Down(x) -> %q, want /x/", c.currentDir)
	}
	c.Down("y")
	if c.currentDir != "/x/y/" {
		t.Errorf("Down(y) -> %q, want /x/y/", c.currentDir)
	}
	c.Up()
	if c.currentDir != "/x/" {
		t.Errorf("Up -> %q, want /x/", c.currentDir)
	}
	c.Up()
	if c.currentDir != "/" {
		t.Errorf("Up -> %q, want /", c.currentDir)
	}
	c.Up() // at root: stays put
	if c.currentDir != "/" {
		t.Errorf("Up at root -> %q, want /", c.currentDir)
	}
}

func TestNavigateToKeySelectsIt(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/a/": {
			{Name: "/a/j"},
			{Name: "/a/k"},
		},
	}})
	c.navigateTo(&model.Node{Name: "/a/k"})

	if c.currentDir != "/a/" {
		t.Errorf("currentDir = %q, want /a/", c.currentDir)
	}
	if mk := currentMapKey(c); mk != "k|file" {
		t.Errorf("focused row = %q, want k|file", mk)
	}
}

func TestNavigateToDir(t *testing.T) {
	c := newTestController(&fakeModel{})

	c.navigateTo(&model.Node{Name: "/a/b", IsDir: true})
	if c.currentDir != "/a/b/" {
		t.Errorf("currentDir = %q, want /a/b/", c.currentDir)
	}

	// Root must not become "//" (regression for the jump-to-/ path).
	c.navigateTo(&model.Node{Name: "/", IsDir: true})
	if c.currentDir != "/" {
		t.Errorf("currentDir = %q, want /", c.currentDir)
	}
}

func TestFillDetailsKeyRendersRichInfo(t *testing.T) {
	rich := &model.Node{
		Name:      "/a/k",
		Value:     `{"b":1}`,
		TTL:       90,
		CreateRev: 10,
		ModRev:    20,
		Version:   2,
		ClusterId: "42",
	}
	c := newTestController(&fakeModel{gets: map[string]*model.Node{"/a/k": rich}})
	// List cache carries a bare node (keys-only listing); the fresh get must
	// fill in value, TTL and revisions.
	c.currentNodes = map[string]*Node{"k|file": {node: &model.Node{Name: "/a/k"}}}

	c.fillDetails("k|file")
	text := c.view.Details.GetText(true)

	for _, want := range []string{
		"Type: Key",
		"TTL: 1m30s (90s)",
		"Create rev: 10",
		"Mod rev: 20",
		"Version: 2",
		"Preview (JSON",
		`"b": 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("details missing %q in:\n%s", want, text)
		}
	}
}

func TestFillDetailsBinaryValueHexDump(t *testing.T) {
	c := newTestController(&fakeModel{gets: map[string]*model.Node{
		"/a/bin": {Name: "/a/bin", Value: "\xff\xfeAB"},
	}})
	c.currentNodes = map[string]*Node{"bin|file": {node: &model.Node{Name: "/a/bin"}}}

	c.fillDetails("bin|file")
	text := c.view.Details.GetText(true)

	if !strings.Contains(text, "Preview (hex") {
		t.Errorf("binary value should get a hex preview:\n%s", text)
	}
	if !strings.Contains(text, "ff fe 41 42") {
		t.Errorf("hex bytes missing:\n%s", text)
	}
}

func TestFillDetailsDirCounts(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/d/": {
			{Name: "/d/sub", IsDir: true},
			{Name: "/d/k1"},
			{Name: "/d/k2"},
		},
	}})
	c.currentNodes = map[string]*Node{"d|dir": {node: &model.Node{Name: "/d", IsDir: true}}}

	c.fillDetails("d|dir")
	text := c.view.Details.GetText(true)

	for _, want := range []string{"Type: Directory", "Children: 3", "Subdirs: 1", "Keys: 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("details missing %q in:\n%s", want, text)
		}
	}
}

func TestResolveImportKey(t *testing.T) {
	c := newTestController(&fakeModel{})

	c.currentDir = "/a/"
	if got := c.resolveImportKey("x"); got != "/a/x" {
		t.Errorf("relative under /a/ = %q, want /a/x", got)
	}
	if got := c.resolveImportKey("/abs/key"); got != "/abs/key" {
		t.Errorf("absolute = %q, want /abs/key", got)
	}
	c.currentDir = "/"
	if got := c.resolveImportKey("y"); got != "/y" {
		t.Errorf("relative under / = %q, want /y", got)
	}
}

func TestColorize(t *testing.T) {
	c := newTestController(&fakeModel{})
	if got := c.colorize("_hidden", false, "lbl"); got != "[yellow]lbl[-]" {
		t.Errorf("underscore entry = %q, want yellow-wrapped", got)
	}
	if got := c.colorize("plain", false, "lbl"); got != "lbl" {
		t.Errorf("plain entry = %q, want unchanged", got)
	}
}

// Ctrl+E on a directory must fall through to the rename dialog — the old
// edit() path (whose file branch was dead code that dropped TTLs) is gone.
func TestEditMultilineOnDirOpensRenameDialog(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/d", IsDir: true}},
	}})
	c.updateList()
	c.view.List.SetCurrentItem(1) // row 0 is [..]

	c.editMultiline()

	if !c.view.Pages.HasPage("modal") {
		t.Error("rename dialog should open when Ctrl+E is pressed on a directory")
	}
}

func TestReinjectRename(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.injectNode(&model.Node{Name: "/a/_old", Value: "v"})

	c.reinjectRename("/a/_old", "/a/_new", false, "42", "v")

	bucket := c.injected["/a/"]
	if bucket["_old|file"] != nil {
		t.Error("old injected entry should be gone after rename")
	}
	if bucket["_new|file"] == nil {
		t.Error("new injected entry missing after rename")
	}
}

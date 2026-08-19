package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
)

// fakeModel is an in-memory modelAPI: listings and gets are served from maps,
// mutations are no-ops (SetKeepTTL records its call so restore tests can
// assert TTL preservation). Directory keys must carry their trailing slash,
// the way the controller passes currentDir around.
type fakeModel struct {
	nodes      map[string][]*model.Node     // "/dir/" -> children
	gets       map[string]*model.Node       // "/dir/key" -> node
	lsErr      map[string]error             // "/dir/" -> forced Ls failure
	hist       map[string][]*model.Revision // "/dir/key" -> canned history
	histTrunc  bool
	histErr    error
	histCalls  int
	exportData map[string]string // served by Export
	setErr     error             // forced Set failure

	setKeepCalls []setKeepCall
	delDirCalls  []string
	// calls records every mutating call as "op arg1 arg2", so a flow test can
	// assert that a rejected input reached the model not at all.
	calls []string
}

func (f *fakeModel) note(format string, a ...interface{}) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
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

func (f *fakeModel) Set(k, v string) error              { f.note("set %s %s", k, v); return f.setErr }
func (f *fakeModel) SetTTL(string, string, int64) error { return nil }
func (f *fakeModel) SetKeepTTL(key, value string, leaseID, ttl int64) error {
	f.setKeepCalls = append(f.setKeepCalls, setKeepCall{key, value, leaseID, ttl})
	return nil
}
func (f *fakeModel) MkDir(d string) error { f.note("mkdir %s", d); return nil }
func (f *fakeModel) Del(string) error     { return nil }
func (f *fakeModel) DelDir(key string) error {
	f.delDirCalls = append(f.delDirCalls, key)
	return nil
}
func (f *fakeModel) RenameDir(string, string) error           { return nil }
func (f *fakeModel) RenameKey(string, string) error           { return nil }
func (f *fakeModel) CopyKey(s1, d string) error               { f.note("copykey %s %s", s1, d); return nil }
func (f *fakeModel) CopyDir(s1, d string) error               { f.note("copydir %s %s", s1, d); return nil }
func (f *fakeModel) Export(string) (map[string]string, error) { return f.exportData, nil }
func (f *fakeModel) Import(items map[string]string, o bool) (int, int, error) {
	f.note("import %d %v", len(items), o)
	return len(items), 0, nil
}
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
		view:  v,
		model: m,
		// Present but not wired to the model: tests that care about journalling
		// wrap their fake in newJournaling explicitly, the way NewController
		// does. This just keeps the viewer from dereferencing nil.
		journal:     &Journal{},
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
	if got := c.colorize("_hidden", "lbl"); got != "[yellow]lbl[-]" {
		t.Errorf("underscore entry = %q, want yellow-wrapped", got)
	}
	if got := c.colorize("plain", "lbl"); got != "lbl" {
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

// B-7: v3 listings are keys-only, so a cached node carries no value until
// fillDetails fetches one — and fillDetails swallows its own Get error. The
// editor must therefore re-read the key, not open on the cached empty string:
// saving that would silently wipe the key.
func TestEditMultilineRereadsValue(t *testing.T) {
	fm := &fakeModel{
		nodes: map[string][]*model.Node{
			// Value empty, exactly as a keys-only v3 listing returns it.
			"/": {{Name: "/k", IsDir: false}},
		},
		gets: map[string]*model.Node{
			"/k": {Name: "/k", Value: "real value", LeaseID: 7, TTL: 30},
		},
	}
	c := newTestController(fm)
	c.updateList()
	c.view.List.SetCurrentItem(1) // row 0 is [..]

	c.editMultiline()

	got := c.currentNodes["k|file"].node
	if got.Value != "real value" {
		t.Errorf("editor opened on value %q, want %q — saving would wipe the key", got.Value, "real value")
	}
	if got.LeaseID != 7 || got.TTL != 30 {
		t.Errorf("lease/TTL not refreshed: lease=%d ttl=%d, want 7/30", got.LeaseID, got.TTL)
	}
}

// A key that cannot be re-read must not open an editor at all — the user would
// be editing a blank buffer over live data.
func TestEditMultilineRefusesWhenRereadFails(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/k", IsDir: false}},
	}}) // no gets entry -> Get fails
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.editMultiline()

	if !c.view.Pages.HasPage("modal") {
		t.Fatal("expected an error modal when the key cannot be re-read")
	}
	// The cached node must be left as-is rather than presented as current.
	if got := c.currentNodes["k|file"].node.Value; got != "" {
		t.Errorf("cached value mutated to %q on a failed re-read", got)
	}
}

// B-8: only keys a listing genuinely hides (underscore-prefixed, invisible in
// etcd v2) belong in the injected cache. Injecting every jump target left
// ghost rows behind once the key was deleted server-side.
func TestNavigateToDoesNotInjectOrdinaryKeys(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/a/": {{Name: "/a/k"}},
	}})

	c.navigateTo(&model.Node{Name: "/a/k"})

	if len(c.injected) != 0 {
		t.Errorf("ordinary key was injected: %+v", c.injected)
	}
}

func TestNavigateToInjectsHiddenKeys(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/a/": {}, // v2 hides underscore-prefixed keys from listings
	}})

	c.navigateTo(&model.Node{Name: "/a/_hidden"})

	if c.injected["/a/"]["_hidden|file"] == nil {
		t.Errorf("underscore key should still be injected: %+v", c.injected)
	}
}

// B-9: a search that matches nothing must leave the cursor alone rather than
// yanking it to the first row, which reads as a successful hit.
func TestSearchMissKeepsCursor(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/a"}, {Name: "/b"}, {Name: "/c"}},
	}})
	c.updateList()
	c.view.List.SetCurrentItem(3) // "c"
	before := c.view.List.GetCurrentItem()

	c.selectRow("nope", c.ordered)

	if got := c.view.List.GetCurrentItem(); got != before {
		t.Errorf("cursor moved to %d on a miss, want it to stay at %d", got, before)
	}

	c.selectRow("a", c.ordered)
	if got := c.view.List.GetCurrentItem(); got != 1 {
		t.Errorf("cursor = %d after selecting %q, want 1", got, "a")
	}
}

// B-10: a node name that splits into no path components must not panic the
// listing (the old FieldsFunc(...)[len-1] indexed an empty slice).
func TestMakeNodeMapToleratesDegenerateNames(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/"}, {Name: ""}, {Name: "/ok"}},
	}})

	if err := c.makeNodeMap(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.currentNodes["ok|file"]; !ok {
		t.Errorf("well-formed node missing from map: %+v", c.currentNodes)
	}
}

// frontPage returns the primitive on the topmost page. The test harness's
// ModalEdit is the identity function, so this is the widget itself.
func frontPage(c *Controller) tview.Primitive {
	_, p := c.view.Pages.GetFrontPage()
	return p
}

// pressEnter drives a widget's own input handler, the way the running app
// would, so SetDoneFunc callbacks fire without a screen.
func pressEnter(p tview.Primitive) {
	p.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
}

// O-1: exporting onto an existing file must ask first. Silently replacing a
// file the user may have exported minutes ago is not recoverable.
func TestExportPromptsBeforeOverwriting(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/export.json"
	if err := os.WriteFile(path, []byte("previous contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newTestController(&fakeModel{exportData: map[string]string{"/k": "v"}})
	c.export()
	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("expected the export input on top, got %T", frontPage(c))
	}
	inp.SetText(path)
	pressEnter(inp)

	if !c.view.Pages.HasPage("modal") {
		t.Fatal("expected an overwrite confirmation")
	}
	if _, isInput := frontPage(c).(*tview.InputField); isInput {
		t.Error("still on the filename input; no confirmation was raised")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous contents" {
		t.Errorf("file was overwritten before confirmation: %q", got)
	}
}

// A path that does not exist yet needs no confirmation.
func TestExportWritesNewFileWithoutPrompt(t *testing.T) {
	path := t.TempDir() + "/fresh.json"

	c := newTestController(&fakeModel{exportData: map[string]string{"/k": "v"}})
	c.export()
	inp := frontPage(c).(*tview.InputField)
	inp.SetText(path)
	pressEnter(inp)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("export did not write the file: %v", err)
	}
	if !strings.Contains(string(raw), `"/k": "v"`) {
		t.Errorf("unexpected export contents: %s", raw)
	}
}

func TestWriteExportReportsSuccess(t *testing.T) {
	path := t.TempDir() + "/out.json"
	c := newTestController(&fakeModel{exportData: map[string]string{"/a": "1", "/b": "2"}})

	c.writeExport(path)

	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected a success info modal")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("export is not valid JSON: %v", err)
	}
	if len(back) != 2 || back["/a"] != "1" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

func TestTruncateBytesKeepsRunesIntact(t *testing.T) {
	// "héllo" — the é is two bytes, straddling a naive cut at 2.
	s := "héllo"
	cut, truncated := truncateBytes(s, 2)
	if !truncated {
		t.Error("expected truncation")
	}
	if !utf8.ValidString(cut) {
		t.Errorf("cut %q is not valid UTF-8", cut)
	}
	if cut != "h" {
		t.Errorf("cut = %q, want %q", cut, "h")
	}

	if cut, truncated := truncateBytes(s, 99); truncated || cut != s {
		t.Errorf("short input should pass through: %q %v", cut, truncated)
	}
}

// N-11: a failed on-focus refresh must be visible. v3 listings are keys-only,
// so a cached node has no value at all — showing it as if it were live is what
// made B-7 destructive rather than merely annoying.
func TestFillDetailsFlagsFailedRefresh(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/k"}}, // no gets entry -> Get fails
	}})
	c.updateList()

	c.fillDetails("k|file")

	if !c.currentNodes["k|file"].stale {
		t.Error("node not flagged stale after a failed refresh")
	}
	details := c.view.Details.GetText(true)
	if !strings.Contains(details, "could not refresh") {
		t.Errorf("details pane hides the failure:\n%s", details)
	}
	if !strings.Contains(details, "out of date") {
		t.Errorf("details pane does not warn about the cached values:\n%s", details)
	}

	// The row itself is marked too, so the warning survives scrolling past.
	label, _ := c.view.List.GetItemText(1)
	if !strings.Contains(label, "cached") {
		t.Errorf("row label = %q, want a cached marker", label)
	}
}

// A refresh that succeeds leaves no trace of staleness behind.
func TestFillDetailsClearsStaleOnRecovery(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/k"}}}}
	c := newTestController(f)
	c.updateList()

	c.fillDetails("k|file") // fails
	if !c.currentNodes["k|file"].stale {
		t.Fatal("precondition: node should be stale")
	}

	f.gets = map[string]*model.Node{"/k": {Name: "/k", Value: "back"}}
	c.fillDetails("k|file") // recovers

	if c.currentNodes["k|file"].stale {
		t.Error("stale flag survived a successful refresh")
	}
	if details := c.view.Details.GetText(true); strings.Contains(details, "could not refresh") {
		t.Errorf("stale warning still rendered:\n%s", details)
	}
	if label, _ := c.view.List.GetItemText(1); strings.Contains(label, "cached") {
		t.Errorf("row label still marked cached: %q", label)
	}
}

// A key that turned into a directory under us is a failed refresh too, not a
// silent fallback to whatever was cached.
func TestFillDetailsFlagsTypeChange(t *testing.T) {
	c := newTestController(&fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		gets:  map[string]*model.Node{"/k": {Name: "/k", IsDir: true}},
	})
	c.updateList()

	c.fillDetails("k|file")

	if !c.currentNodes["k|file"].stale {
		t.Error("node should be stale when the path is no longer a key")
	}
}

func TestRowLabelMarkers(t *testing.T) {
	c := newTestController(&fakeModel{})

	plain := c.rowLabel(&Node{node: &model.Node{Name: "/k"}}, "k")
	if plain != "   k" {
		t.Errorf("plain row = %q, want %q", plain, "   k")
	}

	dir := c.rowLabel(&Node{node: &model.Node{Name: "/d", IsDir: true}}, "d/")
	if !strings.HasPrefix(dir, "📁 ") {
		t.Errorf("dir row = %q, want a folder glyph", dir)
	}

	hidden := c.rowLabel(&Node{node: &model.Node{Name: "/_h"}}, "_h")
	if !strings.Contains(hidden, "[yellow]") {
		t.Errorf("underscore row = %q, want it highlighted", hidden)
	}

	// Stale wins over the underscore highlight: it is the more urgent signal,
	// and nesting the two colour tags would leave the reset mismatched.
	stale := c.rowLabel(&Node{node: &model.Node{Name: "/_h"}, stale: true}, "_h")
	if !strings.Contains(stale, "[gray]") || !strings.Contains(stale, "cached") {
		t.Errorf("stale row = %q, want it greyed and tagged", stale)
	}
	if strings.Contains(stale, "[yellow]") {
		t.Errorf("stale row = %q, should not also carry the underscore colour", stale)
	}
}

package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// twoPaneController returns a controller in dual mode with both panes listed.
func twoPaneController(f *fakeModel) *Controller {
	c := newTestController(f)
	c.updateList()
	c.SetDualPane(true)
	return c
}

// The swap is one struct assignment precisely so no per-pane field can be left
// behind. Reflection makes that a property rather than a promise: give every
// field in paneState a distinguishable value, swap twice, and nothing may have
// moved.
func TestSwapPanesMovesEveryField(t *testing.T) {
	c := newTestController(&fakeModel{})
	left := paneState{
		currentDir:   "/left",
		currentNodes: map[string]*Node{"a|file": {node: &model.Node{Name: "/left/a"}}},
		position:     map[string]int{"/left": 3},
		ordered:      []string{"a"},
		lastGoodDir:  "/left-good",
		rev:          11,
	}
	right := paneState{
		currentDir:   "/right",
		currentNodes: map[string]*Node{"b|file": {node: &model.Node{Name: "/right/b"}}},
		position:     map[string]int{"/right": 7},
		ordered:      []string{"b"},
		lastGoodDir:  "/right-good",
		rev:          22,
	}
	// Any field left out of the swap would keep its old value below.
	if reflect.TypeOf(paneState{}).NumField() != 6 {
		t.Fatalf("paneState grew to %d fields — confirm swapPanes still moves them all",
			reflect.TypeOf(paneState{}).NumField())
	}

	c.paneState, c.inactive = left, right
	c.swapPanes()

	if c.currentDir != "/right" || c.inactive.currentDir != "/left" {
		t.Errorf("dirs did not swap: active=%s inactive=%s", c.currentDir, c.inactive.currentDir)
	}
	if c.rev != 22 || c.inactive.rev != 11 {
		t.Errorf("revisions did not swap: active=%d inactive=%d", c.rev, c.inactive.rev)
	}
	if c.active != 1 {
		t.Errorf("active pane index = %d, want 1", c.active)
	}
	if c.view.List != c.view.Lists[1] {
		t.Error("the view still points at the old pane's list")
	}

	c.swapPanes()
	if !reflect.DeepEqual(c.paneState, left) || !reflect.DeepEqual(c.inactive, right) {
		t.Errorf("swapping twice did not restore both panes:\nactive=%+v\ninactive=%+v", c.paneState, c.inactive)
	}
	if c.active != 0 || c.view.List != c.view.Lists[0] {
		t.Errorf("swapping twice left active=%d", c.active)
	}
}

// Each pane keeps its own directory, cursor and revision. If they shared any
// of it, switching panes would silently move the other one.
func TestPanesKeepIndependentState(t *testing.T) {
	f := &fakeModel{
		rev: 100,
		nodes: map[string][]*model.Node{
			"/":     {{Name: "/a", IsDir: true}, {Name: "/k"}},
			"/a/":   {{Name: "/a/inner"}},
			"/live": nil,
		},
		nodesAt: map[string][]*model.Node{"/": {{Name: "/back-then"}}},
	}
	c := twoPaneController(f)

	// Left pane walks into /a; right pane pins itself to a revision.
	c.Down("a")
	if c.currentDir != "/a/" {
		t.Fatalf("left pane dir = %q, want /a/", c.currentDir)
	}
	c.switchPane()
	c.applyRevision(42)

	if c.currentDir != "/" || c.rev != 42 {
		t.Errorf("right pane = %q rev %d, want / at rev 42", c.currentDir, c.rev)
	}
	c.switchPane()
	if c.currentDir != "/a/" {
		t.Errorf("left pane moved to %q when the right one navigated", c.currentDir)
	}
	if c.rev != 0 {
		t.Errorf("left pane picked up the right pane's revision: %d", c.rev)
	}
}

// F5 copies into the other pane's directory without asking for a path: the
// other side IS the destination, which is the point of a two-pane browser.
func TestCrossPaneCopyUsesOtherPaneDir(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{
		"/src/": {{Name: "/src/key"}},
		"/dst/": nil,
	}}
	c := twoPaneController(f)
	c.currentDir = "/src/"
	c.updateList()
	c.inactive.currentDir = "/dst/"
	c.view.List.SetCurrentItem(1) // past "[..]"

	c.crossTransfer(false)

	want := []string{"copykey /src/key /dst/key"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("cross-pane copy did %v, want %v", f.calls, want)
	}
}

// F6 moves: a rename, so the source is gone afterwards.
func TestCrossPaneMoveRenames(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{
		"/src/": {{Name: "/src/sub", IsDir: true}},
		"/dst/": nil,
	}}
	c := twoPaneController(f)
	c.currentDir = "/src/"
	c.updateList()
	c.inactive.currentDir = "/dst/"
	c.view.List.SetCurrentItem(1)

	c.crossTransfer(true)

	if len(f.renameDirCalls) != 1 || f.renameDirCalls[0] != "/src/sub -> /dst/sub" {
		t.Errorf("cross-pane move did %v, want a dir rename /src/sub -> /dst/sub", f.renameDirCalls)
	}
}

// Writing into a pane that is showing the past would put the data somewhere
// that pane cannot display. Refuse instead.
func TestCrossPaneTransferRefusesPinnedTarget(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/src/": {{Name: "/src/key"}}}}
	c := twoPaneController(f)
	c.currentDir = "/src/"
	c.updateList()
	c.inactive.currentDir = "/dst/"
	c.inactive.rev = 42
	c.view.List.SetCurrentItem(1)

	c.crossTransfer(false)

	if len(f.calls) != 0 {
		t.Errorf("copied into a pane pinned to the past: %v", f.calls)
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanation of why nothing happened")
	}
}

// Both panes in the same directory means the copy would target its own source.
func TestCrossPaneTransferRefusesSameDir(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/same/": {{Name: "/same/key"}}}}
	c := twoPaneController(f)
	c.currentDir = "/same/"
	c.updateList()
	c.inactive.currentDir = "/same/"
	c.view.List.SetCurrentItem(1)

	c.crossTransfer(false)

	if len(f.calls) != 0 {
		t.Errorf("copied a key onto itself: %v", f.calls)
	}
}

// The cross-pane bindings are mutations like any other, so the policy guard
// covers them — including read-only sessions, which must refuse both.
func TestCrossPaneTransferRespectsReadOnly(t *testing.T) {
	for _, move := range []bool{false, true} {
		f := &fakeModel{nodes: map[string][]*model.Node{"/src/": {{Name: "/src/key"}}}}
		c := twoPaneController(f)
		c.policy = Policy{ReadOnly: true}
		c.currentDir = "/src/"
		c.updateList()
		c.inactive.currentDir = "/dst/"
		c.view.List.SetCurrentItem(1)

		c.crossTransfer(move)

		if len(f.calls) != 0 || len(f.renameKeyCalls) != 0 {
			t.Errorf("read-only session performed a cross-pane transfer (move=%t): %v %v",
				move, f.calls, f.renameKeyCalls)
		}
	}
}

// Turning the second pane off must leave the focused pane on screen. Dropping
// the user back into the other pane's directory would be a silent jump.
func TestSingleModeKeepsTheFocusedPane(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{
		"/":     {{Name: "/a", IsDir: true}},
		"/one/": nil,
		"/two/": nil,
	}}
	c := twoPaneController(f)
	c.currentDir = "/one/"
	c.switchPane()
	c.currentDir = "/two/"

	c.SetDualPane(false)

	if c.currentDir != "/two/" {
		t.Errorf("collapsing to one pane moved the user to %q, want /two/", c.currentDir)
	}
	if c.active != 0 || c.view.List != c.view.Lists[0] {
		t.Errorf("the surviving pane must be the visible one: active=%d", c.active)
	}
}

// Without the second pane there is nowhere to copy to; say so rather than
// doing something surprising.
func TestCrossPaneTransferNeedsDualMode(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/key"}}}}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.crossTransfer(false)

	if len(f.calls) != 0 {
		t.Errorf("single-pane copy reached the model: %v", f.calls)
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanation that dual-pane mode is needed")
	}
}

// switchPane is a no-op with one pane: Tab must not silently swap the user
// into a pane that is not on screen.
func TestSwitchPaneIsNoopInSingleMode(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.currentDir = "/here"
	c.switchPane()
	if c.currentDir != "/here" || c.active != 0 {
		t.Errorf("Tab moved a single-pane session: dir=%q active=%d", c.currentDir, c.active)
	}
}

// The bindings themselves: a handler nothing routes a key to is dead code, and
// an input capture only runs on the focused pane — which is exactly how the
// first build of this feature shipped with a keyboard that did nothing.
func TestNewBindingsReachTheirHandlers(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/k"}}}}
	c := newTestController(f)
	c.updateList()
	c.setInput()

	press := func(k tcell.Key) {
		c.view.List.InputHandler()(tcell.NewEventKey(k, 0, tcell.ModNone), func(tview.Primitive) {})
	}

	// Ctrl+B toggles the second pane, both ways.
	press(tcell.KeyCtrlB)
	if !c.dual {
		t.Fatal("Ctrl+B did not turn the second pane on")
	}
	press(tcell.KeyCtrlB)
	if c.dual {
		t.Error("Ctrl+B did not turn the second pane off again")
	}

	// Tab switches panes once there are two.
	press(tcell.KeyCtrlB)
	press(tcell.KeyTab)
	if c.active != 1 {
		t.Errorf("Tab left the active pane at %d", c.active)
	}
	press(tcell.KeyTab)
	if c.active != 0 {
		t.Errorf("Tab back left the active pane at %d", c.active)
	}

	// Ctrl+G opens the revision picker, Ctrl+U the snapshot list.
	press(tcell.KeyCtrlG)
	if _, ok := frontPage(c).(*tview.InputField); !ok {
		t.Errorf("Ctrl+G did not open the revision picker, got %T", frontPage(c))
	}
	c.view.Pages.RemovePage("modal")

	t.Setenv("ETCD_WALKER_STATE_DIR", t.TempDir())
	press(tcell.KeyCtrlU)
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("Ctrl+U did not reach the snapshot browser")
	}
	c.view.Pages.RemovePage("modal-info")

	// F5/F6 reach the cross-pane transfer: both panes sit in "/", so the
	// handler's own "same directory" refusal is the proof it ran.
	for _, k := range []tcell.Key{tcell.KeyF5, tcell.KeyF6} {
		c.view.List.SetCurrentItem(1)
		press(k)
		if !c.view.Pages.HasPage("modal-info") {
			t.Errorf("%v did not reach the cross-pane handler", k)
		}
		c.view.Pages.RemovePage("modal-info")
	}
}

// The hotkey panel must scroll: it lists more bindings than a short terminal
// can show. The old capture returned nil for every key so that any keystroke
// would close the panel — which also meant the arrows never reached the
// TextView and the bottom of the list was unreachable.
func TestHotkeysPanelScrollsAndCloses(t *testing.T) {
	open := func() (*Controller, *tview.TextView) {
		t.Helper()
		c := newTestController(&fakeModel{})
		c.updateList()
		c.setInput()
		c.view.List.InputHandler()(tcell.NewEventKey(tcell.KeyCtrlH, 0, tcell.ModNone), func(tview.Primitive) {})
		tv, ok := frontPage(c).(*tview.TextView)
		if !ok {
			t.Fatalf("Ctrl+H did not open the hotkey panel, got %T", frontPage(c))
		}
		return c, tv
	}

	// Down moves the view and leaves the panel open. The offset is compared
	// against itself because a fresh TextView starts at -1 ("not positioned"),
	// not 0.
	c, tv := open()
	before, _ := tv.GetScrollOffset()
	tv.InputHandler()(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone), func(tview.Primitive) {})
	if after, _ := tv.GetScrollOffset(); after != before+1 {
		t.Errorf("Down moved the panel from %d to %d, want %d", before, after, before+1)
	}
	if !c.view.Pages.HasPage("modal-help") {
		t.Fatal("Down closed the hotkey panel instead of scrolling it")
	}

	// The paging keys reach the TextView too. How far they move depends on the
	// panel's drawn height, which is why only their pass-through is asserted
	// here — TestHotkeysPanelFitsAShortTerminal renders the real thing.
	for _, key := range []tcell.Key{tcell.KeyPgDn, tcell.KeyPgUp, tcell.KeyEnd} {
		tv.InputHandler()(tcell.NewEventKey(key, 0, tcell.ModNone), func(tview.Primitive) {})
		if !c.view.Pages.HasPage("modal-help") {
			t.Fatalf("%v closed the hotkey panel instead of scrolling it", key)
		}
	}
	// …and Home comes back to the top rather than closing.
	tv.InputHandler()(tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone), func(tview.Primitive) {})
	if off, _ := tv.GetScrollOffset(); off != 0 {
		t.Errorf("Home left the panel at offset %d", off)
	}
	if !c.view.Pages.HasPage("modal-help") {
		t.Error("Home closed the hotkey panel")
	}

	// Every documented way out closes it.
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone),
		tcell.NewEventKey(tcell.KeyCtrlH, 0, tcell.ModNone),
		tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone),
	} {
		c, tv := open()
		tv.InputHandler()(ev, func(tview.Primitive) {})
		if c.view.Pages.HasPage("modal-help") {
			t.Errorf("%v did not close the hotkey panel", ev.Key())
		}
	}
}

// Every binding the app answers to should be findable in the panel; a hotkey
// that exists only in the source is one nobody discovers.
func TestHotkeysPanelListsTheBindings(t *testing.T) {
	text := newTestController(&fakeModel{}).view.NewHotkeysModal().GetText(true)
	for _, want := range []string{
		"Ctrl+N", "Ctrl+D", "Ctrl+E", "Ctrl+R", "Ctrl+T", "Ctrl+V", "Ctrl+G",
		"Ctrl+B", "Tab", "F5", "F6", "Ctrl+U", "Ctrl+A", "Ctrl+F", "Ctrl+J",
		"Ctrl+W", "Ctrl+O", "Ctrl+P", "Ctrl+Y", "Ctrl+H", "Ctrl+Q", "Del",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the hotkey panel never mentions %s", want)
		}
	}
	// The panel is scrollable, so it must say how to scroll it.
	if !strings.Contains(text, "scroll") {
		t.Error("the panel does not tell the reader it scrolls")
	}
}

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// ---- live refresh (F-1) --------------------------------------------------

func liveFake() *fakeModel {
	return &fakeModel{
		nodes: map[string][]*model.Node{
			"/":     {{Name: "/app", IsDir: true}, {Name: "/a"}, {Name: "/b"}},
			"/app/": {{Name: "/app/x"}},
		},
		gets: map[string]*model.Node{
			"/a": {Name: "/a", Value: "1"}, "/b": {Name: "/b", Value: "2"},
			"/app/x": {Name: "/app/x", Value: "x"},
		},
	}
}

func rowNames(c *Controller) []string {
	var out []string
	for i := 0; i < c.view.List.GetItemCount(); i++ {
		_, sec := c.view.List.GetItemText(i)
		out = append(out, strings.TrimSpace(sec))
	}
	return out
}

func TestLiveWatchFollowsNavigation(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	if len(f.watchDirs) != 0 {
		t.Fatalf("watching %v with live mode off", f.watchDirs)
	}
	c.SetLive(true)
	defer c.stopLive()
	if got := f.watchDirs; len(got) != 1 || got[0] != "/" {
		t.Fatalf("watches = %v, want [/]", got)
	}
	c.Down("app")
	if got := f.watchDirs; len(got) != 2 || got[1] != "/app" {
		t.Fatalf("watches = %v: entering /app must re-point the watch", got)
	}
	c.updateList() // same dir: no new watch
	if len(f.watchDirs) != 2 {
		t.Errorf("re-listing the same dir restarted the watch: %v", f.watchDirs)
	}
	c.SetLive(false)
	if c.watches[0] != nil {
		t.Error("turning live off must stop the watch")
	}
}

// A change event re-lists the pane and keeps the cursor on the same entry,
// even when rows are inserted above it.
func TestLiveRefreshKeepsCursorOnEntry(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	c.selectRow("b", c.ordered)
	if currentMapKeyOf(c.view.List) != "b|file" {
		t.Fatalf("setup: cursor on %q", currentMapKeyOf(c.view.List))
	}

	f.nodes["/"] = append([]*model.Node{{Name: "/0new"}}, f.nodes["/"]...)
	c.onWatch(0, c.watches[0].gen, nil)

	if got := rowNames(c); !contains(got, "0new|file") {
		t.Fatalf("rows = %v, the new key did not appear", got)
	}
	if got := currentMapKeyOf(c.view.List); got != "b|file" {
		t.Errorf("cursor on %q after refresh, want it to stay on b", got)
	}
}

// The focused key being deleted moves the cursor to a neighbour, not to the top.
func TestLiveRefreshFocusedKeyDeleted(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	c.selectRow("b", c.ordered) // last row
	f.nodes["/"] = f.nodes["/"][:2]
	c.onWatch(0, c.watches[0].gen, nil)
	if got := currentMapKeyOf(c.view.List); got != "a|file" {
		t.Errorf("cursor on %q, want the row that took b's place (a)", got)
	}
}

// A background refresh that fails must not pop a modal or jump directories —
// it shows in the header and leaves the listing alone.
func TestLiveRefreshFailureIsQuiet(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.Down("app")
	c.SetLive(true)
	defer c.stopLive()
	before := rowNames(c)
	f.lsErr = map[string]error{"/app/": errors.New("etcdserver: request timed out")}
	c.onWatch(0, c.watches[0].gen, nil)

	if c.view.Pages.HasPage("modal") {
		t.Error("a background refresh failure opened a modal")
	}
	if c.currentDir != "/app/" {
		t.Errorf("dir = %q, a failed refresh must not fall back elsewhere", c.currentDir)
	}
	if got := rowNames(c); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("rows changed on a failed refresh: %v → %v", before, got)
	}
	if c.liveErr == nil {
		t.Error("the failure should be recorded for the header")
	}
}

// An event from a watch that has since been replaced must be ignored.
func TestLiveStaleWatchEventIgnored(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	old := c.watches[0].gen
	c.Down("app")
	f.nodes["/app/"] = append(f.nodes["/app/"], &model.Node{Name: "/app/y"})
	c.onWatch(0, old, nil)
	if contains(rowNames(c), "y|file") {
		t.Error("a stale watch's event refreshed the pane")
	}
}

func TestLiveErrorAndRecovery(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()

	c.onPing(errors.New("context deadline exceeded"))
	if c.liveErr == nil {
		t.Fatal("ping failure not recorded")
	}
	// Changes made while disconnected are picked up on recovery even with no
	// watch event, because the watch may not be able to replay them.
	f.nodes["/"] = append(f.nodes["/"], &model.Node{Name: "/c"})
	gen := c.watches[0].gen
	c.onPing(nil)
	if c.watches[0].gen == gen {
		t.Error("recovery must restart the watch from the present")
	}
	if c.liveErr != nil {
		t.Error("recovery not recorded")
	}
	if !contains(rowNames(c), "c|file") {
		t.Error("recovery did not re-read the pane")
	}
}

// A pane pinned to a past revision never changes and must not be watched.
func TestLiveSkipsPinnedPane(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.rev = 5
	c.SetLive(true)
	defer c.stopLive()
	if c.watches[0] != nil {
		t.Error("a pane at a past revision was watched")
	}
	if !c.view.Pages.HasPage("modal-info") {
		c.toggleLive()
		c.toggleLive()
		if !c.view.Pages.HasPage("modal-info") {
			t.Error("turning live on in a pinned pane should explain why it is not watched")
		}
	}
	c.rev = 0
	c.updateList()
	if c.watches[0] == nil {
		t.Error("returning to live did not start the watch")
	}
}

// Two panes are watched independently, and the parked one refreshes without
// stealing the details pane.
func TestLiveDualPaneWatchesBoth(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	c.dual = true
	c.withOtherPane(func() { c.Down("app") })
	c.syncWatches()
	if c.watches[0] == nil || c.watches[1] == nil {
		t.Fatalf("watches = %+v, want both panes", c.watches)
	}
	if c.watches[1].dir != "/app" {
		t.Errorf("right pane watches %q, want /app", c.watches[1].dir)
	}
	c.selectRow("a", c.ordered)
	c.fillDetails(currentMapKeyOf(c.view.List))
	details := c.view.Details.GetText(true)

	f.nodes["/app/"] = append(f.nodes["/app/"], &model.Node{Name: "/app/y"})
	c.onWatch(1, c.watches[1].gen, nil)

	if c.active != 0 {
		t.Errorf("active pane changed to %d", c.active)
	}
	if !contains(mapKeysOf(c.inactive.currentNodes), "y|file") {
		t.Error("the parked pane was not refreshed")
	}
	if got := c.view.Details.GetText(true); got != details {
		t.Error("refreshing the parked pane redrew the details pane")
	}
}

// E2E round 1: turning live mode on must re-read what is on screen — a watch
// only reports changes from now on, so anything written while live mode was
// off stayed invisible until the next unrelated change.
func TestLiveTurnOnRefreshesImmediately(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	f.nodes["/"] = append(f.nodes["/"], &model.Node{Name: "/written-while-off"})
	c.SetLive(true)
	defer c.stopLive()
	if !contains(rowNames(c), "written-while-off|file") {
		t.Error("turning live on did not pick up a change made while it was off")
	}
}

// E2E round 1: a key named like a colour tag vanished from the listing.
func TestRowLabelEscapesKeyNames(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/[bracket]"}, {Name: "/[red]x"}}}}
	c := newTestController(f)
	c.updateList()
	for i := 1; i < c.view.List.GetItemCount(); i++ {
		main, _ := c.view.List.GetItemText(i)
		if !strings.Contains(main, "[bracket[]") && !strings.Contains(main, "[red[]x") {
			t.Errorf("row %d label %q is not escaped", i, main)
		}
	}
	c.currentDir = "/[red]/"
	if got := c.listTitle(); strings.Contains(got, "[red]") {
		t.Errorf("title %q is not escaped", got)
	}
}

func TestPumpWatchCoalescesBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan model.WatchEvent, 64)
	got := make(chan error, 64)
	go pumpWatch(ctx, ch, 100*time.Millisecond, func(err error) { got <- err })

	ch <- model.WatchEvent{}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("first change not delivered promptly")
	}
	for i := 0; i < 20; i++ {
		ch <- model.WatchEvent{}
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(got); n != 1 {
		t.Errorf("a burst of 20 changes produced %d refreshes, want 1", n)
	}
	<-got
	ch <- model.WatchEvent{Err: errors.New("boom")}
	select {
	case err := <-got:
		if err == nil {
			t.Error("errors must pass through")
		}
	case <-time.After(time.Second):
		t.Fatal("error not delivered")
	}
}

func TestLiveHeaderStates(t *testing.T) {
	if text, _ := liveHeader("base", tcell.ColorGreen, false, nil); text != "base" {
		t.Errorf("off: %q", text)
	}
	text, col := liveHeader("base", tcell.ColorGreen, true, nil)
	if !strings.Contains(text, "● LIVE") || col != tcell.ColorGreen {
		t.Errorf("healthy: %q %v", text, col)
	}
	text, col = liveHeader("base", tcell.ColorGreen, true, errors.New("dial tcp [::1]:2379: connection refused"))
	if col != tcell.ColorRed {
		t.Errorf("failing header colour %v, want red", col)
	}
	// Escaped, so the [::1] survives tview's tag parser.
	if !strings.Contains(text, "⚠ LIVE") || !strings.Contains(text, "[::1[]") {
		t.Errorf("failing: %q", text)
	}
	long := strings.Repeat("é", 200)
	if text, _ := liveHeader("", 0, true, errors.New(long)); len([]rune(text)) > 90 {
		t.Errorf("long error not truncated: %d runes", len([]rune(text)))
	}
}

func ctrlS() *tcell.EventKey { return tcell.NewEventKey(tcell.KeyCtrlS, 0, tcell.ModNone) }

func pressKeyOn(p tview.Primitive, ev *tcell.EventKey) {
	if h := p.InputHandler(); h != nil {
		if p, ok := p.(interface {
			GetInputCapture() func(*tcell.EventKey) *tcell.EventKey
		}); ok {
			if capture := p.GetInputCapture(); capture != nil {
				if ev = capture(ev); ev == nil {
					return
				}
			}
		}
		h(ev, func(tview.Primitive) {})
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func mapKeysOf(m map[string]*Node) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---- CAS saves (N-1) -----------------------------------------------------

func casFake() *fakeModel {
	return &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		gets: map[string]*model.Node{
			"/k": {Name: "/k", Value: "server", ModRev: 30, LeaseID: 4, TTL: 50},
		},
	}
}

func TestSaveValueIsConditionalOnBaseRevision(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	base := &model.Node{Name: "/k", Value: "orig", ModRev: 20, LeaseID: 4, TTL: 50}
	c.saveValue(val, base, "mine")
	if len(f.setKeepCalls) != 1 || f.setKeepCalls[0].rev != 20 {
		t.Fatalf("calls = %+v, want one CAS against rev 20", f.setKeepCalls)
	}
}

// Regression guard for the subtle half of N-1: the editor's base must be the
// node as read when the editor opened. The on-focus/live refresh replaces
// val.node while the editor is open, and comparing against *that* would wave
// a concurrent write straight through.
func TestEditorBaseIsSnapshotNotLiveNode(t *testing.T) {
	f := casFake()
	f.gets["/k"].ModRev = 20
	c := newTestController(f)
	c.updateList()
	c.selectRow("k", c.ordered)
	c.editMultiline()
	defer c.view.CloseEditor()

	// Someone writes the key; a refresh updates the listing's node.
	f.gets["/k"] = &model.Node{Name: "/k", Value: "theirs", ModRev: 30}
	c.fillDetails("k|file")
	if c.currentNodes["k|file"].node.ModRev != 30 {
		t.Fatal("setup: refresh did not update the node")
	}

	// Ctrl+S in the editor.
	pressKeyOn(c.view.App.GetFocus(), ctrlS())
	if len(f.setKeepCalls) != 1 {
		t.Fatalf("calls = %+v", f.setKeepCalls)
	}
	if got := f.setKeepCalls[0].rev; got != 20 {
		t.Errorf("save compared against rev %d, want 20 (the revision the edit was based on)", got)
	}
}

func TestConflictShowsDialogAndKeepsEdit(t *testing.T) {
	f := casFake()
	f.setKeepErrs = []error{&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}}
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.saveValue(val, &model.Node{Name: "/k", ModRev: 20}, "mine")
	if !c.view.Pages.HasPage("modal") {
		t.Fatal("a conflict must open the resolution dialog")
	}
	if c.view.Pages.HasPage("modal-info") {
		t.Error("a conflict must not report success")
	}
}

// Overwrite after a conflict is still a CAS — against the version the user
// was just shown — and keeps the *live* lease/TTL.
func TestConflictOverwriteUsesCurrentRevisionAndLease(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	base := &model.Node{Name: "/k", ModRev: 20, LeaseID: 1, TTL: 5}
	ce := &model.ConflictError{Key: "/k", Expected: 20, Actual: 30}
	c.conflictChoice(val, base, "mine", ce, "overwrite")
	if len(f.setKeepCalls) != 1 {
		t.Fatalf("calls = %+v", f.setKeepCalls)
	}
	got := f.setKeepCalls[0]
	if got.rev != 30 || got.leaseID != 4 || got.ttl != 50 || got.value != "mine" {
		t.Errorf("overwrite = %+v, want CAS at rev 30 with live lease 4 / ttl 50", got)
	}
}

// A third writer racing the overwrite is caught again rather than clobbered.
func TestConflictOverwriteCanConflictAgain(t *testing.T) {
	f := casFake()
	f.setKeepErrs = []error{&model.ConflictError{Key: "/k", Expected: 30, Actual: 31}}
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}, "overwrite")
	if !c.view.Pages.HasPage("modal") {
		t.Error("a second conflict must show the dialog again")
	}
}

// A write landing while the conflict dialog is open (server now at rev 31,
// dialog showed 30) must not be overwritten unseen: the CAS is against the
// revision the user was shown, so it fails and the dialog comes back.
func TestConflictOverwriteDoesNotClobberWriteMadeWhileDialogOpen(t *testing.T) {
	f := casFake()
	f.gets["/k"].ModRev = 31
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	ce := &model.ConflictError{Key: "/k", Expected: 20, Actual: 30}
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine", ce, "overwrite")
	if len(f.setKeepCalls) != 1 || f.setKeepCalls[0].rev != 30 {
		t.Fatalf("overwrite = %+v, want a CAS at the shown rev 30, not the re-read 31", f.setKeepCalls)
	}
}

// Looking at the diff makes the diffed revision the one an overwrite targets.
func TestConflictDiffAdvancesOverwriteRevision(t *testing.T) {
	f := casFake()
	f.gets["/k"].ModRev = 31
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	ce := &model.ConflictError{Key: "/k", Expected: 20, Actual: 30}
	base := &model.Node{Name: "/k", ModRev: 20}
	c.conflictChoice(val, base, "mine", ce, "diff")
	c.conflictChoice(val, base, "mine", ce, "overwrite")
	if len(f.setKeepCalls) != 1 || f.setKeepCalls[0].rev != 31 {
		t.Fatalf("overwrite after diff = %+v, want a CAS at the diffed rev 31", f.setKeepCalls)
	}
}

func TestConflictRecreateDeletedKeyIsUnconditional(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20, LeaseID: 9, TTL: 3}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Deleted: true}, "recreate")
	got := f.setKeepCalls[0]
	if got.rev != 0 || got.leaseID != 0 || got.ttl != 0 {
		t.Errorf("recreate = %+v, want an unconditional permanent write", got)
	}
}

func TestConflictDiffShowsBothSides(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}, "diff")
	name, p := c.view.Pages.GetFrontPage()
	if name != "modal" {
		t.Fatalf("front page %q", name)
	}
	tv, ok := p.(interface{ GetText(bool) string })
	if !ok {
		t.Fatalf("diff page is %T", p)
	}
	text := tv.GetText(true)
	if !strings.Contains(text, "-server") && !strings.Contains(text, "- server") {
		if !strings.Contains(text, "server") || !strings.Contains(text, "mine") {
			t.Errorf("diff text lacks both sides:\n%s", text)
		}
	}
}

// Esc on the conflict dialog must not throw away the user's text.
func TestConflictEscKeepsEditing(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "my precious text",
		&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}, "")
	defer c.view.CloseEditor()
	ta, ok := c.view.App.GetFocus().(interface{ GetText() string })
	if !ok || ta.GetText() != "my precious text" {
		t.Errorf("editor not reopened with the user's text (focus %T)", c.view.App.GetFocus())
	}
}

func TestRestoreRevisionIsCAS(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	c.restoreRevision(&model.Node{Name: "/k"}, &model.Revision{Rev: 5, Value: "old"})
	if len(f.setKeepCalls) != 1 || f.setKeepCalls[0].rev != 30 {
		t.Errorf("restore calls = %+v, want a CAS against the just-read rev 30", f.setKeepCalls)
	}
}

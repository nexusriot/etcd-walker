package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// Tests written from a coverage-gap review after F-1/N-1: the interactive
// flows below had no test at all (quick search, the TTL dialog, recursive
// find, the revision prompt), or only their happy path (conflict resolution).

func key(k tcell.Key) *tcell.EventKey { return tcell.NewEventKey(k, 0, tcell.ModNone) }

// ---- quick search --------------------------------------------------------

func TestQuickSearchMovesCursor(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.search()
	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("search page is %T", frontPage(c))
	}
	inp.SetText("b")
	pressEnter(inp)
	if got := currentMapKeyOf(c.view.List); got != "b|file" {
		t.Errorf("cursor on %q, want b", got)
	}
	if c.view.Pages.HasPage("modal") {
		t.Error("search dialog left open")
	}
}

// A miss must leave the cursor alone (B-9), and Esc must not move it either.
func TestQuickSearchMissAndEscKeepCursor(t *testing.T) {
	c := newTestController(liveFake())
	c.updateList()
	c.selectRow("a", c.ordered)
	for _, finish := range []func(tview.Primitive){
		pressEnter,
		func(p tview.Primitive) { p.InputHandler()(key(tcell.KeyEsc), func(tview.Primitive) {}) },
	} {
		c.search()
		inp := frontPage(c).(*tview.InputField)
		inp.SetText("zzz-nothing")
		finish(inp)
		if got := currentMapKeyOf(c.view.List); got != "a|file" {
			t.Errorf("cursor moved to %q", got)
		}
	}
}

// Gap-review bug: the search dialog captured the row order when it opened.
// A live refresh that inserts rows while it is open shifted every row down,
// and Enter put the cursor on whichever key now sat at the old index.
func TestQuickSearchSurvivesLiveRefreshWhileOpen(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()

	c.search()
	inp := frontPage(c).(*tview.InputField)

	f.nodes["/"] = append([]*model.Node{{Name: "/0"}, {Name: "/00"}}, f.nodes["/"]...)
	c.onWatch(0, c.watches[0].gen, nil)

	inp.SetText("b")
	pressEnter(inp)
	if got := currentMapKeyOf(c.view.List); got != "b|file" {
		t.Errorf("search for b after a live refresh landed on %q", got)
	}
}

// A row that appeared after the dialog opened is findable.
func TestQuickSearchFindsRowAddedWhileOpen(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.search()
	f.nodes["/"] = append(f.nodes["/"], &model.Node{Name: "/brand-new"})
	c.updateList()
	inp := frontPage(c).(*tview.InputField)
	inp.SetText("brand-new")
	pressEnter(inp)
	if got := currentMapKeyOf(c.view.List); got != "brand-new|file" {
		t.Errorf("cursor on %q", got)
	}
}

// ---- TTL dialog ------------------------------------------------------------

func ttlFake() *fakeModel {
	return &fakeModel{
		// The listing node has no value (v3 listings are keys-only); the
		// dialog must write the value it re-reads, never this one (B-7).
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		gets:  map[string]*model.Node{"/k": {Name: "/k", Value: "live-value", TTL: 0}},
	}
}

func TestSetTTLWritesFreshValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"90", "setttl /k live-value 90"},
		{"1h30m", "setttl /k live-value 5400"},
		{"", "setttl /k live-value 0"}, // empty clears the TTL
	} {
		f := ttlFake()
		c := newTestController(f)
		c.updateList()
		c.selectRow("k", c.ordered)
		c.setTTL()
		form := formOnScreen(t, c)
		form.GetFormItem(0).(*tview.InputField).SetText(tc.in)
		pressButton(t, form, "Save")
		if len(f.calls) != 1 || f.calls[0] != tc.want {
			t.Errorf("input %q: calls = %v, want [%s]", tc.in, f.calls, tc.want)
		}
	}
}

func TestSetTTLRejectsBadInputAndVanishedKey(t *testing.T) {
	f := ttlFake()
	c := newTestController(f)
	c.updateList()
	c.selectRow("k", c.ordered)
	c.setTTL()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("soon")
	pressButton(t, form, "Save")
	if len(f.calls) != 0 || !c.view.Pages.HasPage("modal") {
		t.Errorf("bad TTL: calls=%v, error modal shown=%v", f.calls, c.view.Pages.HasPage("modal"))
	}
	c.view.Pages.RemovePage("modal")

	c.setTTL()
	form = formOnScreen(t, c)
	delete(f.gets, "/k") // deleted while the dialog was open
	form.GetFormItem(0).(*tview.InputField).SetText("60")
	pressButton(t, form, "Save")
	if len(f.calls) != 0 {
		t.Errorf("TTL written to a key that no longer exists: %v", f.calls)
	}
}

func TestSetTTLRefusedReadOnlyAndAtRevision(t *testing.T) {
	f := ttlFake()
	c := newTestController(f)
	c.policy.ReadOnly = true
	c.updateList()
	c.selectRow("k", c.ordered)
	c.view.List.InputHandler()(key(tcell.KeyCtrlT), func(tview.Primitive) {})
	c.setInput()
	pressKeyOn(c.view.List, key(tcell.KeyCtrlT))
	if _, isForm := frontPage(c).(*tview.Form); isForm {
		t.Error("read-only session opened the TTL dialog")
	}
}

// ---- recursive find --------------------------------------------------------

func TestFindRecursiveShowsResultsAndNavigates(t *testing.T) {
	f := liveFake()
	f.searchHits = []*model.Node{{Name: "/app/x"}}
	c := newTestController(f)
	c.updateList()
	c.findRecursive()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("x")
	form.GetFormItem(1).(*tview.Checkbox).SetChecked(true)
	pressButton(t, form, "Search")
	if !contains(f.calls, "search / x true") {
		t.Errorf("calls = %v", f.calls)
	}
	list, ok := frontPage(c).(*tview.List)
	if !ok || list.GetItemCount() != 1 {
		t.Fatalf("results page = %T", frontPage(c))
	}
	pressEnter(list)
	if c.currentDir != "/app/" || currentMapKeyOf(c.view.List) != "x|file" {
		t.Errorf("navigated to %q / %q, want /app/ and x", c.currentDir, currentMapKeyOf(c.view.List))
	}
}

func TestFindRecursiveEmptyNoMatchAndError(t *testing.T) {
	f := liveFake()
	c := newTestController(f)
	c.updateList()

	c.findRecursive()
	pressButton(t, formOnScreen(t, c), "Search") // empty query: nothing happens
	if len(f.calls) != 0 || c.view.Pages.HasPage("modal") || c.view.Pages.HasPage("modal-info") {
		t.Errorf("empty query reached the model: %v", f.calls)
	}

	c.findRecursive()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("nope")
	pressButton(t, form, "Search")
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("no-match should say so")
	}
	c.view.Pages.RemovePage("modal-info")

	f.searchErr = errors.New("etcdserver: request timed out")
	c.findRecursive()
	form = formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("x")
	pressButton(t, form, "Search")
	if !c.view.Pages.HasPage("modal") {
		t.Error("a failed search must show an error")
	}
}

// Find in a pane pinned to the past must search the past.
func TestFindRecursiveCarriesRevision(t *testing.T) {
	f := liveFake()
	f.searchHits = []*model.Node{{Name: "/a"}}
	c := newTestController(f)
	c.updateList()
	c.rev = 77
	c.findRecursive()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("a")
	f.readRevs = nil
	pressButton(t, form, "Search")
	if len(f.readRevs) == 0 || f.readRevs[0] != 77 {
		t.Errorf("search read at revs %v, want 77", f.readRevs)
	}
}

// ---- revision prompt -------------------------------------------------------

func TestGoToRevisionPromptErrors(t *testing.T) {
	// v2: the cluster has no revisions; a number must explain, empty is fine.
	f := liveFake()
	c := newTestController(f)
	c.updateList()
	c.goToRevision()
	inp := frontPage(c).(*tview.InputField)
	inp.SetText("5")
	pressEnter(inp)
	if !c.view.Pages.HasPage("modal") || c.rev != 0 {
		t.Errorf("revision on a cluster without revisions: rev=%d", c.rev)
	}
	c.view.Pages.RemovePage("modal")

	// v3: garbage input is rejected, and the pane stays live.
	f.rev = 100
	c.goToRevision()
	inp = frontPage(c).(*tview.InputField)
	inp.SetText("yesterday")
	pressEnter(inp)
	if c.rev != 0 || !c.view.Pages.HasPage("modal") {
		t.Errorf("garbage revision accepted: rev=%d", c.rev)
	}
	c.view.Pages.RemovePage("modal")

	// A pinned pane's prompt is prefilled, and clearing it returns to live.
	f.nodesAt = map[string][]*model.Node{"/": {{Name: "/a"}}}
	c.applyRevision(50)
	c.view.Pages.RemovePage("modal-info")
	c.goToRevision()
	inp = frontPage(c).(*tview.InputField)
	if inp.GetText() != "50" {
		t.Errorf("prompt prefilled with %q, want 50", inp.GetText())
	}
	inp.SetText("")
	pressEnter(inp)
	if c.rev != 0 {
		t.Errorf("clearing the prompt left rev=%d", c.rev)
	}
}

// ---- conflict resolution branches -----------------------------------------

func TestConflictReloadOpensEditorOnServerValue(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}, "reload")
	defer c.view.CloseEditor()
	ta, ok := c.view.App.GetFocus().(interface{ GetText() string })
	if !ok || ta.GetText() != "server" {
		t.Fatalf("reload did not open the editor on the server value (focus %T)", c.view.App.GetFocus())
	}
	// …and its base is the reloaded revision, so saving now succeeds.
	pressKeyOn(c.view.App.GetFocus(), ctrlS())
	if len(f.setKeepCalls) != 1 || f.setKeepCalls[0].rev != 30 {
		t.Errorf("save after reload = %+v, want CAS at rev 30", f.setKeepCalls)
	}
}

func TestConflictOverwriteOrReloadOfVanishedKeyErrors(t *testing.T) {
	for _, choice := range []string{"overwrite", "reload", "diff"} {
		f := casFake()
		c := newTestController(f)
		c.updateList()
		val := c.currentNodes["k|file"]
		delete(f.gets, "/k")
		c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
			&model.ConflictError{Key: "/k", Expected: 20, Actual: 30}, choice)
		if len(f.setKeepCalls) != 0 {
			t.Errorf("%s wrote to a key that vanished: %+v", choice, f.setKeepCalls)
		}
		if !c.view.Pages.HasPage("modal") {
			t.Errorf("%s on a vanished key should explain", choice)
		}
	}
}

func TestConflictDiscardWritesNothing(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.conflictChoice(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Deleted: true}, "discard")
	if len(f.setKeepCalls) != 0 || c.view.Pages.HasPage("modal") {
		t.Errorf("discard: calls=%+v", f.setKeepCalls)
	}
}

func conflictDiffText(t *testing.T, server, mine string) string {
	t.Helper()
	f := casFake()
	f.gets["/k"].Value = server
	c := newTestController(f)
	c.updateList()
	c.showConflictDiff(c.currentNodes["k|file"], &model.Node{Name: "/k", ModRev: 20}, mine,
		&model.ConflictError{Key: "/k", Expected: 20, Actual: 30})
	tv, ok := frontPage(c).(interface{ GetText(bool) string })
	if !ok {
		t.Fatalf("diff page is %T", frontPage(c))
	}
	// Esc goes back to the conflict dialog, not to the list.
	pressKeyOn(frontPage(c), key(tcell.KeyEsc))
	if _, isModal := frontPage(c).(*tview.Modal); !isModal {
		t.Errorf("Esc from the diff went to %T, want the conflict dialog", frontPage(c))
	}
	return tv.GetText(true)
}

func TestConflictDiffBranches(t *testing.T) {
	if s := conflictDiffText(t, "a\nb\n", "a\nc\n"); !strings.Contains(s, "- b") || !strings.Contains(s, "+ c") {
		t.Errorf("text diff:\n%s", s)
	}
	if s := conflictDiffText(t, "same", "same"); !strings.Contains(s, "no differences") {
		t.Errorf("identical:\n%s", s)
	}
	if s := conflictDiffText(t, "\xff\xfe", "x"); !strings.Contains(s, "binary value") {
		t.Errorf("binary:\n%s", s)
	}
	big := strings.Repeat("line\n", 2000)
	if s := conflictDiffText(t, big+"x", big+"y"); !strings.Contains(s, "too large") {
		t.Errorf("huge:\n%.200s", s)
	}
}

// The deleted-key dialog has different buttons from the changed-key one.
func TestConflictDialogButtons(t *testing.T) {
	f := casFake()
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.resolveConflict(val, &model.Node{Name: "/k", ModRev: 20}, "mine",
		&model.ConflictError{Key: "/k", Expected: 20, Deleted: true})
	if _, ok := frontPage(c).(*tview.Modal); !ok {
		t.Fatalf("front page %T", frontPage(c))
	}
}

// ---- live mode plumbing ----------------------------------------------------

func TestCtrlLBindingTogglesLive(t *testing.T) {
	c := newTestController(liveFake())
	c.updateList()
	c.setInput()
	defer c.stopLive()
	pressKeyOn(c.view.List, key(tcell.KeyCtrlL))
	if !c.liveOn || c.watches[0] == nil {
		t.Fatal("Ctrl+L did not turn live mode on")
	}
	pressKeyOn(c.view.List, key(tcell.KeyCtrlL))
	if c.liveOn || c.watches[0] != nil {
		t.Error("second Ctrl+L did not turn it off")
	}
}

// Ctrl+L works in a read-only session: watching is not a write.
func TestCtrlLAllowedReadOnly(t *testing.T) {
	c := newTestController(liveFake())
	c.policy.ReadOnly = true
	c.updateList()
	c.setInput()
	defer c.stopLive()
	pressKeyOn(c.view.List, key(tcell.KeyCtrlL))
	if !c.liveOn {
		t.Error("read-only session refused live mode")
	}
}

func TestDualOffStopsSecondWatch(t *testing.T) {
	c := newTestController(liveFake())
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	c.dual = true
	c.syncWatches()
	if c.watches[1] == nil {
		t.Fatal("second pane not watched in dual mode")
	}
	c.dual = false
	c.syncWatches()
	if c.watches[1] != nil {
		t.Error("hidden pane still watched")
	}
}

func TestLiveWithoutModelIsNoop(t *testing.T) {
	c := newTestController(nil)
	c.model = nil
	c.SetLive(true)
	if c.liveOn || c.pingCancel != nil {
		t.Error("live mode started without a model (startup failed)")
	}
}

// A watch error flips the header; the next successful event clears it.
func TestWatchErrorThenEventClears(t *testing.T) {
	c := newTestController(liveFake())
	c.updateList()
	c.SetLive(true)
	defer c.stopLive()
	gen := c.watches[0].gen
	c.onWatch(0, gen, errors.New("etcdserver: no leader"))
	if c.liveErr == nil {
		t.Fatal("watch error not recorded")
	}
	c.onWatch(0, gen, nil)
	if c.liveErr != nil {
		t.Error("a successful event did not clear the error")
	}
}

// A refresh for the hidden second pane is a no-op, not a crash or a swap.
func TestRefreshHiddenPaneIsNoop(t *testing.T) {
	c := newTestController(liveFake())
	c.updateList()
	c.refreshPane(1)
	if c.active != 0 {
		t.Errorf("active pane changed to %d", c.active)
	}
}

// A save that fails for a non-conflict reason (timeout, lost connection,
// permission denied) must not throw away the user's text.
func TestSaveFailureKeepsText(t *testing.T) {
	f := casFake()
	f.setKeepErrs = []error{errors.New("context deadline exceeded")}
	c := newTestController(f)
	c.updateList()
	val := c.currentNodes["k|file"]
	c.saveValue(val, &model.Node{Name: "/k", ModRev: 30}, "hours of work")
	m, ok := frontPage(c).(*tview.Modal)
	if !ok {
		t.Fatalf("front page %T, want the error", frontPage(c))
	}
	pressModalButton(m)
	defer c.view.CloseEditor()
	ta, ok := c.view.App.GetFocus().(interface{ GetText() string })
	if !ok || ta.GetText() != "hours of work" {
		t.Errorf("after a failed save the editor holds %v", c.view.App.GetFocus())
	}
	// …and saving again is still conditional on the original base.
	pressKeyOn(c.view.App.GetFocus(), ctrlS())
	if len(f.setKeepCalls) != 2 || f.setKeepCalls[1].rev != 30 {
		t.Errorf("retry = %+v", f.setKeepCalls)
	}
}

// pressModalButton presses a modal's focused (first) button. A headless modal
// has no focus, so first let it hand focus down to its button, the way the
// running app's focus chain would.
func pressModalButton(m *tview.Modal) {
	var focus func(p tview.Primitive)
	focus = func(p tview.Primitive) { p.Focus(focus) }
	m.Focus(focus)
	m.InputHandler()(key(tcell.KeyEnter), focus)
}

func TestTitlePathShortensFromTheLeftAndEscapes(t *testing.T) {
	long := "/x/" + strings.Repeat("L", 300) + "/tail-key"
	got := titlePath(long)
	if n := len([]rune(got)); n != maxTitlePath {
		t.Errorf("len = %d, want %d", n, maxTitlePath)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "/tail-key") {
		t.Errorf("titlePath = %q", got)
	}
	if got := titlePath("/[red]k"); got != "/[red[]k" {
		t.Errorf("short path = %q, want escaped", got)
	}
}

// The editor title keeps its mode label and hints however long the key is.
func TestEditorTitleSurvivesLongKey(t *testing.T) {
	name := "/" + strings.Repeat("L", 300)
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: name}}},
		gets:  map[string]*model.Node{name: {Name: name, Value: "v"}},
	}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)
	c.editMultiline()
	defer c.view.CloseEditor()
	ta, ok := c.view.App.GetFocus().(*tview.TextArea)
	if !ok {
		t.Fatalf("focus %T", c.view.App.GetFocus())
	}
	if title := ta.GetTitle(); !strings.Contains(title, "Edit (multiline)") || len([]rune(title)) > 120 {
		t.Errorf("title = %q", title)
	}
}

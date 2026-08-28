package controller

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

func TestParseRevisionInput(t *testing.T) {
	const current = int64(1000)
	ok := map[string]int64{
		"":     0, // empty = back to live
		"  ":   0,
		"0":    0,
		"1":    1,
		"999":  999,
		"1000": 1000, // the current revision is readable
		"-100": 900,  // relative to current
		" -1 ": 999,
		"-999": 1,
	}
	for in, want := range ok {
		got, err := parseRevisionInput(in, current)
		if err != nil {
			t.Errorf("parseRevisionInput(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseRevisionInput(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"abc", "12x", "1.5", "1001", "-1000", "-2000"}
	for _, in := range bad {
		if got, err := parseRevisionInput(in, current); err == nil {
			t.Errorf("parseRevisionInput(%q) = %d, want an error", in, got)
		}
	}

	// Without a known current revision, absolute values still work (nothing to
	// bound them against) but relative ones cannot be resolved.
	if got, err := parseRevisionInput("42", 0); err != nil || got != 42 {
		t.Errorf("absolute revision with unknown current = %d/%v", got, err)
	}
	if _, err := parseRevisionInput("-42", 0); err == nil {
		t.Error("a relative revision with no current revision must fail, not guess")
	}
}

// The pane's revision must reach every read. A listing that quietly came back
// live while the title said "rev 42" is the whole failure mode this feature
// has to avoid.
func TestPinnedPaneReadsAtItsRevision(t *testing.T) {
	f := &fakeModel{
		rev: 100,
		nodes: map[string][]*model.Node{
			"/": {{Name: "/live-only", IsDir: false}},
		},
		nodesAt: map[string][]*model.Node{
			"/": {{Name: "/back-then", IsDir: false}},
		},
		getsAt: map[string]*model.Node{
			"/back-then": {Name: "/back-then", Value: "old value"},
		},
	}
	c := newTestController(f)
	c.updateList()

	c.applyRevision(42)
	if c.rev != 42 {
		t.Fatalf("pane rev = %d, want 42", c.rev)
	}

	// The listing is the historical one.
	if _, ok := c.currentNodes["back-then|file"]; !ok {
		t.Errorf("listing did not come from the pinned revision: %v", c.currentNodes)
	}
	if _, ok := c.currentNodes["live-only|file"]; ok {
		t.Error("a pinned pane listed the live tree")
	}

	// Every read since pinning carried the revision.
	for _, rev := range f.readRevs[len(f.readRevs)-1:] {
		if rev != 42 {
			t.Errorf("a read after pinning carried rev %d, want 42", rev)
		}
	}

	// Details, find and export follow the pane too.
	f.readRevs = nil
	c.fillDetails("back-then|file")
	c.export()
	c.writeExport(t.TempDir() + "/e.json")
	for _, rev := range f.readRevs {
		if rev != 42 {
			t.Errorf("read at rev %d after pinning, want 42 (readRevs=%v)", rev, f.readRevs)
		}
	}

	// The title says so — a historical listing that looks live is a trap.
	if title := c.listTitle(); !strings.Contains(title, "rev 42") {
		t.Errorf("list title = %q, want it to name the revision", title)
	}
}

// Returning to live must actually return to live.
func TestClearingRevisionGoesBackToLive(t *testing.T) {
	f := &fakeModel{
		rev:     100,
		nodes:   map[string][]*model.Node{"/": {{Name: "/live-only"}}},
		nodesAt: map[string][]*model.Node{"/": {{Name: "/back-then"}}},
	}
	c := newTestController(f)
	c.applyRevision(42)
	c.applyRevision(0)

	if c.rev != 0 {
		t.Fatalf("rev = %d, want 0", c.rev)
	}
	if _, ok := c.currentNodes["live-only|file"]; !ok {
		t.Errorf("clearing the revision did not restore the live listing: %v", c.currentNodes)
	}
	if strings.Contains(c.listTitle(), "rev") {
		t.Errorf("live title still mentions a revision: %q", c.listTitle())
	}
}

// A revision below the compaction point must leave the pane where it was. The
// tempting alternative — switch anyway and show the error — strands the user
// in a pane whose every read fails.
func TestCompactedRevisionLeavesPaneAlone(t *testing.T) {
	f := &fakeModel{
		rev:    100,
		nodes:  map[string][]*model.Node{"/": {{Name: "/k"}}},
		revErr: errors.New("etcdserver: mvcc: required revision has been compacted"),
	}
	c := newTestController(f)
	c.updateList()

	c.applyRevision(5)

	if c.rev != 0 {
		t.Errorf("pane switched to an unreadable revision: rev = %d", c.rev)
	}
	if _, ok := c.currentNodes["k|file"]; !ok {
		t.Error("the live listing was lost on a failed revision switch")
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("expected an error modal explaining the compaction")
	}
}

func TestReviseCompactionError(t *testing.T) {
	got := reviseCompactionError(errors.New("etcdserver: mvcc: required revision has been compacted"), 5)
	if !strings.Contains(got.Error(), "compacted away") {
		t.Errorf("compaction error not explained: %v", got)
	}
	other := errors.New("context deadline exceeded")
	if reviseCompactionError(other, 5) != other {
		t.Error("an unrelated error must pass through untouched")
	}
	if reviseCompactionError(nil, 5) != nil {
		t.Error("nil must stay nil")
	}
}

// A pinned pane is a snapshot: every mutation is refused, whichever route it
// takes. guarded is the funnel, so testing it there covers the restore and
// import paths that never touch the list's key bindings.
func TestPinnedPaneRefusesEveryMutation(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.rev = 42

	ran := false
	c.guarded(guard{action: "delete", paths: []string{"/k"}, do: func() { ran = true }})

	if ran {
		t.Error("a mutation ran from a pane pinned to a past revision")
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanation of why nothing happened")
	}
}

// The up-front binding check refuses the same actions, so the dialog never
// opens on a pane that could not save the result.
func TestPinnedPaneRefusesMutatingKeysUpFront(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/k"}}}}
	c := newTestController(f)
	c.updateList()
	c.setInput()
	c.rev = 42

	for key, action := range mutatingKeys {
		c.view.Pages.RemovePage("modal-info")
		c.view.List.InputHandler()(tcell.NewEventKey(key, 0, tcell.ModNone), func(tview.Primitive) {})
		if !c.view.Pages.HasPage("modal-info") {
			t.Errorf("%s was not refused on a pinned pane", action)
		}
		if c.view.Pages.HasPage("modal") {
			t.Errorf("%s opened its dialog on a pinned pane", action)
		}
	}
}

// Ctrl+E stays reachable while pinned — reading a historical value in full is
// the point — but the editor opens disabled so nobody types into a value that
// cannot be saved.
func TestEditorIsViewOnlyAtRevision(t *testing.T) {
	f := &fakeModel{
		rev:     100,
		nodesAt: map[string][]*model.Node{"/": {{Name: "/k"}}},
		getsAt:  map[string]*model.Node{"/k": {Name: "/k", Value: "as it was"}},
	}
	c := newTestController(f)
	c.applyRevision(42)
	c.view.List.SetCurrentItem(1) // past "[..]"

	c.editMultiline()

	ta, ok := c.view.App.GetFocus().(*tview.TextArea)
	if !ok {
		t.Fatalf("expected the editor on screen, got %T", c.view.App.GetFocus())
	}
	if !ta.GetDisabled() {
		t.Error("the editor must be read-only while the pane is pinned to a revision")
	}
	if ta.GetText() != "as it was" {
		t.Errorf("editor shows %q, want the value at the pinned revision", ta.GetText())
	}
}

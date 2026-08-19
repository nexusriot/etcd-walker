package controller

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

func TestPolicyProtects(t *testing.T) {
	p := Policy{ProtectedPrefixes: []string{"/registry", "  ", "/a/b/"}}
	cases := []struct {
		path       string
		wantPrefix string
		want       bool
	}{
		{"/registry", "/registry", true},
		{"/registry/pods/x", "/registry", true},
		{"/registry/", "/registry", true},
		{"/a/b/c", "/a/b", true},
		// A shared string prefix is not containment.
		{"/registryx", "", false},
		{"/reg", "", false},
		{"/a/bc", "", false},
		{"/other", "", false},
		{"/", "", false},
	}
	for _, tc := range cases {
		gotPrefix, got := p.protects(tc.path)
		if got != tc.want || gotPrefix != tc.wantPrefix {
			t.Errorf("protects(%q) = (%q,%v), want (%q,%v)", tc.path, gotPrefix, got, tc.wantPrefix, tc.want)
		}
	}

	// A bare "/" protects the whole keyspace.
	root := Policy{ProtectedPrefixes: []string{"/"}}
	if pfx, ok := root.protects("/anything/at/all"); !ok || pfx != "/" {
		t.Errorf(`protects under "/" = (%q,%v), want ("/",true)`, pfx, ok)
	}

	// No configuration protects nothing.
	if _, ok := (Policy{}).protects("/registry"); ok {
		t.Error("empty policy should protect nothing")
	}
}

// F-4: a read-only session refuses the mutation and never runs it.
func TestGuardedRefusesInReadOnly(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ReadOnly: true}

	ran := false
	c.guarded(guard{action: "delete", paths: []string{"/k"}, do: func() { ran = true }})

	if ran {
		t.Error("mutation ran in a read-only session")
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanatory modal")
	}
}

// Read-only must also stop the dialogs opening in the first place.
func TestReadOnlyBlocksMutatingHotkeys(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/k"}},
	}})
	c.policy = Policy{ReadOnly: true}
	c.updateList()
	c.view.List.SetCurrentItem(1)
	c.setInput()

	for key, action := range mutatingKeys {
		c.view.Pages.RemovePage("modal")
		c.view.Pages.RemovePage("modal-info")
		c.view.List.InputHandler()(tcell.NewEventKey(key, 0, tcell.ModNone), func(tview.Primitive) {})

		if c.view.Pages.HasPage("modal") {
			t.Errorf("%s dialog opened in a read-only session", action)
		}
		if !c.view.Pages.HasPage("modal-info") {
			t.Errorf("%s gave no feedback in a read-only session", action)
		}
	}
}

// Navigation and other read-only actions stay available.
func TestReadOnlyKeepsReadActions(t *testing.T) {
	for _, key := range []tcell.Key{tcell.KeyCtrlP, tcell.KeyCtrlY, tcell.KeyCtrlW, tcell.KeyCtrlF, tcell.KeyCtrlJ} {
		if action, blocked := mutatingKeys[key]; blocked {
			t.Errorf("read-only must not block %v (mapped to %q)", key, action)
		}
	}
}

// N-14: an unprotected path is written without ceremony.
func TestGuardedRunsUnprotectedImmediately(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	ran := false
	c.guarded(guard{action: "delete", paths: []string{"/apps/k"}, do: func() { ran = true }})

	if !ran {
		t.Error("unprotected mutation should run immediately")
	}
	if c.view.Pages.HasPage("modal") {
		t.Error("unprotected mutation should not prompt")
	}
}

// A protected path holds the write until the prefix basename is typed out.
func TestGuardedProtectedRequiresTypedConfirmation(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	ran := false
	c.guarded(guard{action: "delete", paths: []string{"/registry/pods/x"}, do: func() { ran = true }})

	if ran {
		t.Fatal("protected mutation ran before confirmation")
	}
	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("expected a confirmation input, got %T", frontPage(c))
	}
	// The prompt has to name the prefix that was crossed.
	if !strings.Contains(inp.GetTitle(), "registry") {
		t.Errorf("prompt does not name the protected prefix: %q", inp.GetTitle())
	}

	inp.SetText("registry")
	pressEnter(inp)

	if !ran {
		t.Error("mutation did not run after a correct confirmation")
	}
}

// The wrong word cancels, and hands control back through onCancel.
func TestGuardedProtectedWrongWordCancels(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	ran, cancelled := false, false
	c.guarded(guard{
		action:   "save",
		paths:    []string{"/registry/x"},
		do:       func() { ran = true },
		onCancel: func() { cancelled = true },
	})

	inp := frontPage(c).(*tview.InputField)
	inp.SetText("not-it")
	pressEnter(inp)

	if ran {
		t.Error("mutation ran despite a wrong confirmation")
	}
	if !cancelled {
		t.Error("onCancel was not invoked")
	}
}

// Any single protected target gates a whole batch — one import must not slip
// keys into a protected prefix because most of its keys were harmless.
func TestGuardedGatesBatchOnAnyProtectedTarget(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	ran := false
	c.guarded(guard{
		action: "import",
		paths:  []string{"/apps/a", "/apps/b", "/registry/sneaky"},
		do:     func() { ran = true },
	})

	if ran {
		t.Error("batch ran without confirming its protected target")
	}
	if _, ok := frontPage(c).(*tview.InputField); !ok {
		t.Error("expected a confirmation prompt for the batch")
	}
}

// The guard is reached through the restore flow too, which bypasses the list's
// key bindings entirely.
func TestRestoreRevisionRespectsReadOnly(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		gets:  map[string]*model.Node{"/k": {Name: "/k", Value: "current"}},
	}
	c := newTestController(f)
	c.policy = Policy{ReadOnly: true}
	c.updateList()

	c.restoreRevision(&model.Node{Name: "/k"}, &model.Revision{Rev: 5, Value: "old"})

	if len(f.setKeepCalls) != 0 {
		t.Errorf("restore wrote in a read-only session: %+v", f.setKeepCalls)
	}
}

func TestRestoreRevisionRespectsProtectedPrefix(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/registry/k"}}},
		gets:  map[string]*model.Node{"/registry/k": {Name: "/registry/k", Value: "current"}},
	}
	c := newTestController(f)
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}
	c.updateList()

	c.restoreRevision(&model.Node{Name: "/registry/k"}, &model.Revision{Rev: 5, Value: "old"})

	if len(f.setKeepCalls) != 0 {
		t.Fatalf("restore wrote before confirmation: %+v", f.setKeepCalls)
	}
	inp := frontPage(c).(*tview.InputField)
	inp.SetText("registry")
	pressEnter(inp)

	if len(f.setKeepCalls) != 1 {
		t.Errorf("restore did not run after confirmation: %+v", f.setKeepCalls)
	}
}

package controller

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// journalled wires a fake model behind the journalling decorator, the way
// NewController does.
func journalled(m modelAPI, dryRun bool) (*journaling, *Journal) {
	j := &Journal{dryRun: dryRun}
	return newJournaling(m, j, dryRun), j
}

// N-6: a successful mutation is recorded with its exact arguments.
func TestJournalRecordsMutations(t *testing.T) {
	w, j := journalled(&fakeModel{}, false)

	if err := w.Set("/a/k", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := w.Del("/a/gone"); err != nil {
		t.Fatal(err)
	}
	if err := w.DelDir("/a/sub"); err != nil {
		t.Fatal(err)
	}

	if j.Len() != 3 {
		t.Fatalf("recorded %d entries, want 3: %+v", j.Len(), j.Entries())
	}
	script := j.Script()
	for _, want := range []string{
		"etcdctl put '/a/k' 'hello'",
		"etcdctl del '/a/gone'",
		"etcdctl del --prefix '/a/sub/'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// A mutation that fails must not be recorded — a journal that lists changes
// which never happened is worse than no journal.
func TestJournalSkipsFailedMutations(t *testing.T) {
	w, j := journalled(&fakeModel{setErr: errors.New("etcdserver: permission denied")}, false)

	if err := w.Set("/a/k", "v"); err == nil {
		t.Fatal("expected the underlying error to propagate")
	}
	if j.Len() != 0 {
		t.Errorf("failed mutation was recorded: %+v", j.Entries())
	}
}

// N-21: dry-run records without touching the cluster.
func TestJournalDryRunDoesNotWrite(t *testing.T) {
	f := &fakeModel{}
	w, j := journalled(f, true)

	if err := w.Set("/a/k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetKeepTTL("/a/k", "v2", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Del("/a/k"); err != nil {
		t.Fatal(err)
	}

	if len(f.setKeepCalls) != 0 {
		t.Errorf("dry-run reached the model: %+v", f.setKeepCalls)
	}
	if j.Len() != 3 {
		t.Errorf("dry-run recorded %d entries, want 3", j.Len())
	}
	if !strings.Contains(j.Script(), "DRY RUN") {
		t.Error("dry-run script must say so in its header")
	}
}

// Even a mutation the cluster would reject is recorded under dry-run: nothing
// is sent, so there is no error to learn from.
func TestJournalDryRunIgnoresModelErrors(t *testing.T) {
	w, j := journalled(&fakeModel{setErr: errors.New("boom")}, true)

	if err := w.Set("/a/k", "v"); err != nil {
		t.Errorf("dry-run should not surface a model error it never triggered: %v", err)
	}
	if j.Len() != 1 {
		t.Error("dry-run did not record the rehearsed change")
	}
}

// Import holds its whole payload, so it renders as real commands — sorted, so
// the script does not shuffle between runs of the same session.
func TestJournalImportRendersSortedPuts(t *testing.T) {
	w, j := journalled(&fakeModel{}, true)

	written, _, err := w.Import(map[string]string{"/z": "3", "/a": "1", "/m": "2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if written != 3 {
		t.Errorf("dry-run import reported %d written, want 3", written)
	}
	script := j.Script()
	ia := strings.Index(script, "'/a'")
	im := strings.Index(script, "'/m'")
	iz := strings.Index(script, "'/z'")
	if ia < 0 || im < ia || iz < im {
		t.Errorf("import puts not in sorted order:\n%s", script)
	}
}

// Operations etcdctl cannot reproduce from what we recorded are comments, not
// commands that would quietly do the wrong thing.
func TestJournalUnreproducibleOpsAreComments(t *testing.T) {
	w, j := journalled(&fakeModel{}, false)

	if err := w.RenameKey("/a", "/b"); err != nil {
		t.Fatal(err)
	}
	if err := w.CopyKey("/a", "/c"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetTTL("/a", "v", 60); err != nil {
		t.Fatal(err)
	}

	for _, line := range strings.Split(j.Script(), "\n") {
		if strings.HasPrefix(line, "etcdctl") {
			t.Errorf("unreproducible op emitted as a runnable command: %q", line)
		}
	}
	if !strings.Contains(j.Script(), "# rename '/a' -> '/b'") {
		t.Errorf("rename not described:\n%s", j.Script())
	}
}

// A value that cannot survive a shell script must not be emitted as a put.
func TestJournalBinaryValueIsNotPut(t *testing.T) {
	w, j := journalled(&fakeModel{}, false)
	if err := w.Set("/bin", "\x00\xff binary"); err != nil {
		t.Fatal(err)
	}
	script := j.Script()
	if strings.Contains(script, "etcdctl put") {
		t.Errorf("binary value emitted as a put:\n%s", script)
	}
	if !strings.Contains(script, "binary/control data") {
		t.Errorf("binary value not explained:\n%s", script)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"":          "''",
		"with sp":   "'with sp'",
		`it's`:      `'it'\''s'`,
		"a\nb":      "'a\nb'",
		`$(rm -rf)`: `'$(rm -rf)'`, // stays inert inside single quotes
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScriptSafe(t *testing.T) {
	safe := []string{"", "plain", "two\nlines", "tab\there", "üñî"}
	for _, s := range safe {
		if !scriptSafe(s) {
			t.Errorf("scriptSafe(%q) = false, want true", s)
		}
	}
	unsafe := []string{"\x00", "bell\x07", "\xff\xfe"}
	for _, s := range unsafe {
		if scriptSafe(s) {
			t.Errorf("scriptSafe(%q) = true, want false", s)
		}
	}
}

// The decorator promotes read methods from the embedded interface, so a new
// *mutating* method on modelAPI would silently bypass the journal. Pin the
// surface: growing modelAPI must be a deliberate act.
func TestModelAPISurfaceIsJournalled(t *testing.T) {
	iface := reflect.TypeOf((*modelAPI)(nil)).Elem()

	const knownMethods = 18
	if got := iface.NumMethod(); got != knownMethods {
		t.Fatalf("modelAPI has %d methods, expected %d — if you added one, decide "+
			"whether it mutates: mutators must be overridden in journaling (and "+
			"listed in mutatingModelMethods), readers can be promoted",
			got, knownMethods)
	}

	// Every declared mutator must exist on the decorator.
	dec := reflect.TypeOf(&journaling{})
	for _, name := range mutatingModelMethods {
		if _, ok := iface.MethodByName(name); !ok {
			t.Errorf("mutatingModelMethods lists %q, which modelAPI does not have", name)
			continue
		}
		if _, ok := dec.MethodByName(name); !ok {
			t.Errorf("journaling does not implement mutator %q", name)
		}
	}
}

// Every mutator, driven through the decorator, must leave a journal entry —
// the completeness guarantee the feature rests on.
func TestEveryMutatorIsRecorded(t *testing.T) {
	calls := map[string]func(w *journaling) error{
		"Set":        func(w *journaling) error { return w.Set("/k", "v") },
		"SetTTL":     func(w *journaling) error { return w.SetTTL("/k", "v", 60) },
		"SetKeepTTL": func(w *journaling) error { return w.SetKeepTTL("/k", "v", 0, 0) },
		"MkDir":      func(w *journaling) error { return w.MkDir("/d") },
		"Del":        func(w *journaling) error { return w.Del("/k") },
		"DelDir":     func(w *journaling) error { return w.DelDir("/d") },
		"RenameDir":  func(w *journaling) error { return w.RenameDir("/d", "/e") },
		"RenameKey":  func(w *journaling) error { return w.RenameKey("/k", "/j") },
		"CopyKey":    func(w *journaling) error { return w.CopyKey("/k", "/j") },
		"CopyDir":    func(w *journaling) error { return w.CopyDir("/d", "/e") },
		"Import":     func(w *journaling) error { _, _, err := w.Import(map[string]string{"/k": "v"}, true); return err },
	}
	if len(calls) != len(mutatingModelMethods) {
		t.Fatalf("this test drives %d mutators, mutatingModelMethods lists %d",
			len(calls), len(mutatingModelMethods))
	}
	for name, call := range calls {
		w, j := journalled(&fakeModel{}, false)
		if err := call(w); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if j.Len() != 1 {
			t.Errorf("%s left %d journal entries, want 1", name, j.Len())
		}
	}
}

// The viewer offers an empty session an explanation rather than an empty list.
func TestShowJournalEmpty(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.journal = &Journal{}

	c.showJournal()

	if c.view.Pages.HasPage("modal") {
		t.Error("no picker should open for an empty journal")
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanatory info modal")
	}
}

func TestShowJournalListsEntries(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.journal = &Journal{}
	c.journal.add("set /a/k (5 bytes)", "etcdctl put '/a/k' 'hello'")
	c.journal.add("delete /a/j", "etcdctl del '/a/j'")

	c.showJournal()

	list, ok := frontPage(c).(*tview.List)
	if !ok {
		t.Fatalf("expected the journal list, got %T", frontPage(c))
	}
	if list.GetItemCount() != 2 {
		t.Errorf("journal list has %d rows, want 2", list.GetItemCount())
	}
	if main, _ := list.GetItemText(0); !strings.Contains(main, "set /a/k") {
		t.Errorf("first row = %q", main)
	}
}

func TestWriteJournalIsExecutable(t *testing.T) {
	path := t.TempDir() + "/session.sh"
	c := newTestController(&fakeModel{})
	c.journal = &Journal{}
	c.journal.add("set /a/k", "etcdctl put '/a/k' 'v'")

	c.writeJournal(path)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "#!/bin/sh") {
		t.Errorf("journal is not a script:\n%s", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("journal not executable: %v", info.Mode().Perm())
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected a success modal")
	}
}

// F-5: deleting a directory demands its name typed out; a stray Enter on an
// ok/cancel dialog must not be able to drop a subtree.
func TestDeleteDirRequiresTypedName(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/victim", IsDir: true}},
	}}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.delete()

	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("expected a typed confirmation, got %T", frontPage(c))
	}
	if !strings.Contains(inp.GetTitle(), "everything under it") {
		t.Errorf("prompt should say the delete is recursive: %q", inp.GetTitle())
	}

	// Enter on an empty field must not delete.
	pressEnter(inp)
	if len(f.delDirCalls) != 0 {
		t.Fatalf("bare Enter deleted the subtree: %+v", f.delDirCalls)
	}

	c.delete()
	inp = frontPage(c).(*tview.InputField)
	inp.SetText("victim")
	pressEnter(inp)
	if len(f.delDirCalls) != 1 || f.delDirCalls[0] != "/victim" {
		t.Errorf("typed confirmation did not delete: %+v", f.delDirCalls)
	}
}

// Deleting a single key keeps the lighter ok/cancel dialog.
func TestDeleteKeyKeepsSimpleConfirm(t *testing.T) {
	c := newTestController(&fakeModel{nodes: map[string][]*model.Node{
		"/": {{Name: "/k"}},
	}})
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.delete()

	if _, isInput := frontPage(c).(*tview.InputField); isInput {
		t.Error("a single key should not demand its name be typed out")
	}
	if _, isModal := frontPage(c).(*tview.Modal); !isModal {
		t.Errorf("expected the ok/cancel modal, got %T", frontPage(c))
	}
}

// A protected directory asks for the prefix, not the directory name, so the
// user is never asked to type two different words for one delete.
func TestDeleteProtectedDirAsksOnceForThePrefix(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{
		"/registry/": {{Name: "/registry/pods", IsDir: true}},
	}}
	c := newTestController(f)
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}
	c.currentDir = "/registry/"
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.delete()

	inp := frontPage(c).(*tview.InputField)
	if !strings.Contains(inp.GetTitle(), "protected") {
		t.Errorf("protection should supersede the recursive-delete word: %q", inp.GetTitle())
	}
	inp.SetText("registry")
	pressEnter(inp)

	if len(f.delDirCalls) != 1 {
		t.Errorf("delete did not run after the single confirmation: %+v", f.delDirCalls)
	}
	if c.view.Pages.HasPage("modal") {
		if _, second := frontPage(c).(*tview.InputField); second {
			t.Error("a second typed confirmation was demanded")
		}
	}
}

// B-14: declining a protected-path confirmation used to queue a "Cancelled"
// notice on Pages and THEN re-open the value editor, which swaps the
// application root. The notice sat hidden behind the editor and resurfaced
// when it closed — drawn over a list that already had the focus. The re-opened
// editor is the feedback; there must be no leftover modal.
func TestDeclinedProtectedSaveLeavesNoStaleModal(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	reopened := false
	c.guarded(guard{
		action:   "save",
		paths:    []string{"/registry/k"},
		do:       func() { t.Error("save ran despite a declined confirmation") },
		onCancel: func() { reopened = true },
	})

	inp := frontPage(c).(*tview.InputField)
	inp.SetText("wrong")
	pressEnter(inp)

	if !reopened {
		t.Fatal("onCancel did not run")
	}
	if c.view.Pages.HasPage("modal-info") {
		t.Error("a notice was left on Pages; it would resurface behind the editor")
	}
	if c.view.Pages.HasPage("modal") {
		t.Error("the confirmation dialog was not removed")
	}
}

// Without an onCancel there is no editor to hand back to, so the user still
// gets told why nothing happened.
func TestDeclinedConfirmationWithoutHandlerStillReports(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}

	c.guarded(guard{action: "delete", paths: []string{"/registry/k"},
		do: func() { t.Error("delete ran despite a declined confirmation") }})

	inp := frontPage(c).(*tview.InputField)
	inp.SetText("wrong")
	pressEnter(inp)

	if !c.view.Pages.HasPage("modal-info") {
		t.Error("expected an explanation when nothing takes the screen back")
	}
}

// B-15: a dry run must not touch the injected cache. Faking the row would show
// a key that was never created — the ghost-row class of B-8, reintroduced.
func TestDryRunDoesNotFakeInjectedRows(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.policy = Policy{DryRun: true}

	c.injectWritten(&model.Node{Name: "/_hidden", Value: "v"})
	if len(c.injected) != 0 {
		t.Errorf("dry run injected a phantom row: %+v", c.injected)
	}

	// …and must not drop a row for a key it never deleted.
	c.injectNode(&model.Node{Name: "/_real", Value: "v"}) // navigation: legitimate
	c.removeWritten(&model.Node{Name: "/_real"})
	if c.injected["/"]["_real|file"] == nil {
		t.Errorf("dry run removed a row for a key it never deleted: %+v", c.injected)
	}

	c.reinjectWritten("/_real", "/_moved", false, "", "v")
	if c.injected["/"]["_moved|file"] != nil {
		t.Errorf("dry run applied a rename to the cache: %+v", c.injected)
	}
}

// Outside dry-run the same helpers behave exactly as before.
func TestWrittenHelpersApplyNormally(t *testing.T) {
	c := newTestController(&fakeModel{})

	c.injectWritten(&model.Node{Name: "/_h", Value: "v"})
	if c.injected["/"]["_h|file"] == nil {
		t.Fatalf("injectWritten did not inject: %+v", c.injected)
	}
	c.reinjectWritten("/_h", "/_h2", false, "", "v")
	if c.injected["/"]["_h2|file"] == nil {
		t.Errorf("reinjectWritten did not rename: %+v", c.injected)
	}
	c.removeWritten(&model.Node{Name: "/_h2"})
	if c.injected["/"]["_h2|file"] != nil {
		t.Errorf("removeWritten did not remove: %+v", c.injected)
	}
}

// B-16: dry-run success modals must not claim the change happened.
func TestWriteHeaderIsHonestUnderDryRun(t *testing.T) {
	c := newTestController(&fakeModel{})
	if got := c.writeHeader("Copied"); got != "Copied" {
		t.Errorf("normal header = %q, want %q", got, "Copied")
	}
	c.policy = Policy{DryRun: true}
	if got := c.writeHeader("Copied"); !strings.Contains(got, "dry run") || !strings.Contains(got, "not written") {
		t.Errorf("dry-run header = %q, want it to disclaim the write", got)
	}
}

package controller

import (
	"os"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// pressButton fires a form button the way Enter on it would, so a dialog's
// Save/Cancel path runs without a screen.
func pressButton(t *testing.T, form *tview.Form, label string) {
	t.Helper()
	idx := form.GetButtonIndex(label)
	if idx < 0 {
		t.Fatalf("form has no %q button", label)
	}
	btn := form.GetButton(idx)
	btn.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
}

func formOnScreen(t *testing.T, c *Controller) *tview.Form {
	t.Helper()
	f, ok := frontPage(c).(*tview.Form)
	if !ok {
		t.Fatalf("expected a form on screen, got %T", frontPage(c))
	}
	return f
}

// setCreateForm fills the create dialog: name, value, is-directory.
func setCreateForm(f *tview.Form, name, value string, isDir bool) {
	f.GetFormItem(0).(*tview.InputField).SetText(name)
	f.GetFormItem(1).(*tview.InputField).SetText(value)
	f.GetFormItem(2).(*tview.Checkbox).SetChecked(isDir)
}

// create()'s validation was entirely untested — every branch here rejects an
// input that would otherwise reach the cluster.
func TestCreateRejectsBadNames(t *testing.T) {
	cases := []struct {
		name, input string
	}{
		{"empty", ""},
		{"only spaces", "   "},
		{"contains a slash", "a/b"},
		{"reserved marker", ".dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeModel{}
			c := newTestController(f)
			c.updateList()

			c.create()
			form := formOnScreen(t, c)
			setCreateForm(form, tc.input, "v", false)
			pressButton(t, form, "Save")

			if len(f.calls) != 0 {
				t.Errorf("invalid name %q reached the model: %v", tc.input, f.calls)
			}
			if !c.view.Pages.HasPage("modal") {
				t.Error("expected an error modal")
			}
		})
	}
}

// Creating over something that already exists must be refused, not silently
// overwrite it.
func TestCreateRefusesExistingPath(t *testing.T) {
	f := &fakeModel{gets: map[string]*model.Node{"/taken": {Name: "/taken", Value: "old"}}}
	c := newTestController(f)
	c.updateList()

	c.create()
	form := formOnScreen(t, c)
	setCreateForm(form, "taken", "new", false)
	pressButton(t, form, "Save")

	if len(f.calls) != 0 {
		t.Errorf("create overwrote an existing key: %v", f.calls)
	}
}

func TestCreateWritesKeyAndDir(t *testing.T) {
	f := &fakeModel{}
	c := newTestController(f)
	c.currentDir = "/app/"
	c.updateList()

	c.create()
	form := formOnScreen(t, c)
	setCreateForm(form, "k", "v", false)
	pressButton(t, form, "Save")
	if len(f.calls) != 1 || f.calls[0] != "set /app/k v" {
		t.Errorf("create key -> %v, want [set /app/k v]", f.calls)
	}

	f.calls = nil
	c.create()
	form = formOnScreen(t, c)
	setCreateForm(form, "d", "", true)
	pressButton(t, form, "Save")
	if len(f.calls) != 1 || f.calls[0] != "mkdir /app/d" {
		t.Errorf("create dir -> %v, want [mkdir /app/d]", f.calls)
	}
}

func TestCreateQuitWritesNothing(t *testing.T) {
	f := &fakeModel{}
	c := newTestController(f)
	c.updateList()

	c.create()
	form := formOnScreen(t, c)
	setCreateForm(form, "k", "v", false)
	pressButton(t, form, "Quit")

	if len(f.calls) != 0 {
		t.Errorf("cancelled create still wrote: %v", f.calls)
	}
	if c.view.Pages.HasPage("modal") {
		t.Error("dialog should be dismissed")
	}
}

// duplicate() resolves relative targets against the current directory and
// refuses the degenerate ones.
func TestDuplicateTargetValidation(t *testing.T) {
	cases := []struct {
		name, target string
		wantCalls    int
	}{
		{"empty target", "", 0},
		{"onto itself", "/app/src", 0},
		{"root", "/", 0},
		{"relative target", "copy", 1},
		{"absolute target", "/other/copy", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeModel{nodes: map[string][]*model.Node{
				"/app/": {{Name: "/app/src"}},
			}}
			c := newTestController(f)
			c.currentDir = "/app/"
			c.updateList()
			c.view.List.SetCurrentItem(1) // row 0 is [..]

			c.duplicate()
			form := formOnScreen(t, c)
			form.GetFormItem(0).(*tview.InputField).SetText(tc.target)
			pressButton(t, form, "Save")

			if len(f.calls) != tc.wantCalls {
				t.Errorf("target %q -> calls %v, want %d", tc.target, f.calls, tc.wantCalls)
			}
		})
	}
}

func TestDuplicateResolvesRelativeTarget(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/app/": {{Name: "/app/src"}}}}
	c := newTestController(f)
	c.currentDir = "/app/"
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.duplicate()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("copy")
	pressButton(t, form, "Save")

	if len(f.calls) != 1 || f.calls[0] != "copykey /app/src /app/copy" {
		t.Errorf("calls = %v, want [copykey /app/src /app/copy]", f.calls)
	}
}

// A directory duplicate must use CopyDir, not CopyKey.
func TestDuplicateDirUsesCopyDir(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {{Name: "/src", IsDir: true}}}}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.duplicate()
	form := formOnScreen(t, c)
	form.GetFormItem(0).(*tview.InputField).SetText("/dst")
	pressButton(t, form, "Save")

	if len(f.calls) != 1 || f.calls[0] != "copydir /src /dst" {
		t.Errorf("calls = %v, want [copydir /src /dst]", f.calls)
	}
}

// jump() resolves absolute and relative paths and honours the trailing-slash
// "this must be a directory" hint.
func TestJumpResolvesAndValidates(t *testing.T) {
	newC := func() (*fakeModel, *Controller) {
		f := &fakeModel{
			nodes: map[string][]*model.Node{
				"/":     {{Name: "/app", IsDir: true}},
				"/app/": {{Name: "/app/k"}},
			},
			gets: map[string]*model.Node{
				"/app":   {Name: "/app", IsDir: true},
				"/app/k": {Name: "/app/k"},
			},
		}
		c := newTestController(f)
		c.updateList()
		return f, c
	}

	// Absolute jump into a directory.
	_, c := newC()
	c.jump()
	inp := frontPage(c).(*tview.InputField)
	inp.SetText("/app/")
	pressEnter(inp)
	if c.currentDir != "/app/" {
		t.Errorf("jump(/app/) -> currentDir %q, want /app/", c.currentDir)
	}

	// A trailing slash on a key is a contradiction and must be refused.
	_, c = newC()
	c.jump()
	inp = frontPage(c).(*tview.InputField)
	inp.SetText("/app/k/")
	pressEnter(inp)
	if c.currentDir != "/" {
		t.Errorf("jump to a key with a dir hint moved to %q", c.currentDir)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("expected a 'not a folder' error")
	}

	// A path that does not resolve reports not-found and stays put.
	_, c = newC()
	c.jump()
	inp = frontPage(c).(*tview.InputField)
	inp.SetText("/nope")
	pressEnter(inp)
	if c.currentDir != "/" {
		t.Errorf("failed jump moved to %q", c.currentDir)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("expected a not-found error")
	}
}

// promptImportMode parses the file before asking anything, so a bad file is
// reported rather than turning into a confusing empty import.
func TestPromptImportModeRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()

	bad := dir + "/bad.json"
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeModel{}
	c := newTestController(f)
	c.promptImportMode(bad)
	if !c.view.Pages.HasPage("modal") {
		t.Error("invalid JSON should raise an error modal")
	}
	if len(f.calls) != 0 {
		t.Errorf("invalid JSON reached the model: %v", f.calls)
	}

	empty := dir + "/empty.json"
	if err := os.WriteFile(empty, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	c = newTestController(f)
	c.promptImportMode(empty)
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("an empty file should be explained, not imported")
	}

	c = newTestController(f)
	c.promptImportMode(dir + "/missing.json")
	if !c.view.Pages.HasPage("modal") {
		t.Error("an unreadable file should raise an error modal")
	}
}

// The import prompt resolves relative keys against the current directory
// before handing anything to the model.
func TestPromptImportModeResolvesKeys(t *testing.T) {
	path := t.TempDir() + "/in.json"
	if err := os.WriteFile(path, []byte(`{"rel":"1","/abs":"2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeModel{}
	c := newTestController(f)
	c.currentDir = "/base/"
	c.updateList()

	c.promptImportMode(path)
	if _, ok := frontPage(c).(*tview.Modal); !ok {
		t.Fatalf("expected the import-mode prompt, got %T", frontPage(c))
	}
	if got := c.resolveImportKey("rel"); got != "/base/rel" {
		t.Errorf("relative key resolved to %q, want /base/rel", got)
	}
	if got := c.resolveImportKey("/abs"); got != "/abs" {
		t.Errorf("absolute key resolved to %q, want /abs", got)
	}
}

// B-13 guard for the status line: every bracketed tag in the header has to
// survive tview's colour-tag parser, which ate the unescaped "[TLS]".
func TestHeaderTagsSurviveColorTagParsing(t *testing.T) {
	rendered := func(s string) string {
		tv := tview.NewTextView().SetDynamicColors(true)
		tv.SetText(s)
		return tv.GetText(true)
	}

	opts := model.Options{Host: "h", Port: "2379", TLSEnabled: true}
	policy := Policy{ReadOnly: true, DryRun: true, ProtectedPrefixes: []string{"/a", "/b"}}
	text, color := headerText(opts, "v3", "ON", policy)
	got := rendered(text)

	for _, want := range []string{"[TLS]", "[READ-ONLY]", "[DRY-RUN]", "protected: 2", "h:2379", "v3", "ON"} {
		if !strings.Contains(got, want) {
			t.Errorf("header lost %q:\n%s", want, got)
		}
	}
	if color != tcell.ColorYellow {
		t.Errorf("read-only header colour = %v, want yellow", color)
	}
}

func TestHeaderColorsAndOmissions(t *testing.T) {
	plain, color := headerText(model.Options{Host: "h", Port: "1"}, "v2", "OFF", Policy{})
	if color != tcell.ColorGreen {
		t.Errorf("plain header colour = %v, want green", color)
	}
	for _, absent := range []string{"TLS", "READ-ONLY", "DRY-RUN", "protected"} {
		if strings.Contains(plain, absent) {
			t.Errorf("plain header should not mention %q: %s", absent, plain)
		}
	}
	if _, c := headerText(model.Options{}, "v3", "?", Policy{DryRun: true}); c != tcell.ColorAqua {
		t.Errorf("dry-run header colour = %v, want aqua", c)
	}
	// The version in the header is the one the docs promise (DESIGN §9.3).
	if !strings.Contains(plain, appVersion) {
		t.Errorf("header %q does not carry appVersion %q", plain, appVersion)
	}
}

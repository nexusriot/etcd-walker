package controller

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// snapshotSandbox redirects the snapshot directory at a temp dir for one test.
func snapshotSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ETCD_WALKER_STATE_DIR", dir)
	return filepath.Join(dir, "snapshots")
}

// deleteDirController stages a controller with one directory ready to delete.
func deleteDirController(t *testing.T, f *fakeModel) *Controller {
	t.Helper()
	if f.nodes == nil {
		f.nodes = map[string][]*model.Node{"/": {{Name: "/doomed", IsDir: true}}}
	}
	c := newTestController(f)
	c.policy.SnapshotBeforeDelete = true
	c.updateList()
	c.view.List.SetCurrentItem(1) // past "[..]"
	return c
}

// confirmDelete drives the type-the-name confirmation a directory delete asks
// for, so the delete actually runs.
func confirmDelete(t *testing.T, c *Controller, name string) {
	t.Helper()
	c.delete()
	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("expected the typed confirmation, got %T", frontPage(c))
	}
	inp.SetText(name)
	pressEnter(inp)
}

func TestSnapshotFileName(t *testing.T) {
	at := time.Date(2026, 8, 28, 14, 3, 4, 0, time.UTC)
	cases := map[string]string{
		"/app/config": "20260828T140304-app_config.json",
		"/":           "20260828T140304-root.json",
		"/a b/c":      "20260828T140304-a_b_c.json",
		"/wei®d":      "20260828T140304-wei_d.json", // anything unusual becomes '_'
	}
	for in, want := range cases {
		if got := snapshotFileName(in, at); got != want {
			t.Errorf("snapshotFileName(%q) = %q, want %q", in, got, want)
		}
	}
	long := "/" + strings.Repeat("x", 200)
	if got := snapshotFileName(long, at); len(got) > 90 {
		t.Errorf("snapshotFileName did not bound the name: %d chars", len(got))
	}
}

// The whole point: after a recursive delete the subtree is on disk.
func TestRecursiveDeleteWritesSnapshot(t *testing.T) {
	dir := snapshotSandbox(t)
	f := &fakeModel{exportData: map[string]string{
		"/doomed/a": "1",
		"/doomed/b": "2",
	}}
	c := deleteDirController(t, f)

	confirmDelete(t, c, "doomed")

	if len(f.delDirCalls) != 1 {
		t.Fatalf("delete did not reach the model: %v", f.delDirCalls)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one snapshot in %s, got %v (%v)", dir, entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	held, err := snap.values()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(held, f.exportData) {
		t.Errorf("snapshot keys = %v, want %v", held, f.exportData)
	}
	if snap.Path != "/doomed" || snap.Version != snapshotVersion {
		t.Errorf("snapshot header = %+v", snap)
	}
}

// If the subtree cannot be read, the delete must not happen: proceeding would
// destroy data with no undo, which is exactly what the snapshot exists to stop.
func TestDeleteAbortsWhenSnapshotFails(t *testing.T) {
	snapshotSandbox(t)
	f := &fakeModel{exportErr: errors.New("etcdserver: request timed out")}
	c := deleteDirController(t, f)

	confirmDelete(t, c, "doomed")

	if len(f.delDirCalls) != 0 {
		t.Errorf("directory was deleted despite the snapshot failing: %v", f.delDirCalls)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("expected an error modal explaining the delete was cancelled")
	}
}

// Turning snapshots off must delete without one — the escape hatch the error
// message points at.
func TestDeleteWithoutSnapshotWhenDisabled(t *testing.T) {
	dir := snapshotSandbox(t)
	f := &fakeModel{exportData: map[string]string{"/doomed/a": "1"}}
	c := deleteDirController(t, f)
	c.policy.SnapshotBeforeDelete = false

	confirmDelete(t, c, "doomed")

	if len(f.delDirCalls) != 1 {
		t.Errorf("delete did not run with snapshots disabled: %v", f.delDirCalls)
	}
	if _, err := os.ReadDir(dir); !os.IsNotExist(err) {
		t.Errorf("a snapshot was written with snapshots disabled: %v", err)
	}
}

// A dry run deletes nothing, so there is nothing to undo — and writing a file
// would leave a rehearsal looking like a real deletion afterwards.
func TestDryRunTakesNoSnapshot(t *testing.T) {
	dir := snapshotSandbox(t)
	f := &fakeModel{exportData: map[string]string{"/doomed/a": "1"}}
	c := deleteDirController(t, f)
	c.policy.DryRun = true
	c.model = newJournaling(f, c.journal, true)

	confirmDelete(t, c, "doomed")

	if len(f.delDirCalls) != 0 {
		t.Fatalf("dry run reached the cluster: %v", f.delDirCalls)
	}
	if _, err := os.ReadDir(dir); !os.IsNotExist(err) {
		t.Errorf("dry run wrote an undo snapshot: %v", err)
	}
}

// Deleting a single key takes no snapshot: it is recoverable from the key's
// own revision history, and one file per key would bury the ones that matter.
func TestSingleKeyDeleteTakesNoSnapshot(t *testing.T) {
	dir := snapshotSandbox(t)
	f := &fakeModel{
		nodes:      map[string][]*model.Node{"/": {{Name: "/lonely"}}},
		exportData: map[string]string{"/lonely": "v"},
	}
	c := deleteDirController(t, f)
	val, ok := c.focusedNode()
	if !ok {
		t.Fatal("no node under the cursor")
	}

	c.deleteNode(val)

	if len(f.delCalls) != 1 {
		t.Errorf("key delete = %v, want one Del", f.delCalls)
	}
	if _, err := os.ReadDir(dir); !os.IsNotExist(err) {
		t.Errorf("a single-key delete wrote a snapshot: %v", err)
	}
}

func TestListSnapshotsNewestFirstAndSkipsJunk(t *testing.T) {
	dir := snapshotSandbox(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, snap snapshot) {
		raw, _ := json.Marshal(snap)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	write("a.json", snapshot{Version: 1, Path: "/old", Created: older, Keys: map[string]string{"/old/k": "v"}})
	write("b.json", snapshot{Version: 1, Path: "/new", Created: newer, Keys: map[string]string{"/new/k": "v"}})
	// One corrupt file must not hide the readable ones.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file from a future format is refused rather than misread.
	write("future.json", snapshot{Version: snapshotVersion + 1, Path: "/future", Created: newer})

	snaps, err := listSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("listed %d snapshots, want 2: %+v", len(snaps), snaps)
	}
	if snaps[0].path != "/new" || snaps[1].path != "/old" {
		t.Errorf("snapshots not newest-first: %+v", snaps)
	}
	if snaps[0].keys != 1 {
		t.Errorf("key count = %d, want 1", snaps[0].keys)
	}
}

// listSnapshots must not fail merely because nothing has been deleted yet.
func TestListSnapshotsWithNoDirectory(t *testing.T) {
	snapshotSandbox(t)
	snaps, err := listSnapshots()
	if err != nil {
		t.Errorf("missing snapshot dir should not be an error: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("got %d snapshots from an empty state dir", len(snaps))
	}
}

// Restoring goes through Import, so it lands in the journal and obeys the
// policy like any other bulk write.
func TestRestoreSnapshotImportsKeys(t *testing.T) {
	snapshotSandbox(t)
	keys := map[string]string{"/gone/a": "1", "/gone/b": "2"}
	file, err := writeSnapshot(snapshot{
		Version: snapshotVersion, Path: "/gone", Created: time.Now(), Keys: keys,
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeModel{}
	c := newTestController(f)

	// The picker asks overwrite/skip first; the modal's buttons cannot be
	// driven without a screen, so the flow is entered where that answer lands.
	c.restoreSnapshot(snapshotMeta{file: file, path: "/gone", keys: len(keys)})
	if _, ok := frontPage(c).(*tview.Modal); !ok {
		t.Fatalf("expected the overwrite/skip question, got %T", frontPage(c))
	}
	snap, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	c.applyRestore(snap, true)

	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "import 2 ") {
		t.Errorf("restore did not import the snapshot: %v", f.calls)
	}
}

func TestRestoreSnapshotRespectsReadOnly(t *testing.T) {
	snapshotSandbox(t)
	file, err := writeSnapshot(snapshot{
		Version: snapshotVersion, Path: "/gone", Created: time.Now(),
		Keys: map[string]string{"/gone/a": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeModel{}
	c := newTestController(f)
	c.policy = Policy{ReadOnly: true}
	c.applyRestore(snap, true)

	if len(f.calls) != 0 {
		t.Errorf("read-only session restored a snapshot: %v", f.calls)
	}
}

// A protected prefix gates a restore too — it is a bulk write into paths the
// operator asked to be deliberate about.
func TestRestoreSnapshotRespectsProtectedPrefix(t *testing.T) {
	snapshotSandbox(t)
	file, err := writeSnapshot(snapshot{
		Version: snapshotVersion, Path: "/registry", Created: time.Now(),
		Keys: map[string]string{"/registry/pods/x": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeModel{}
	c := newTestController(f)
	c.policy = Policy{ProtectedPrefixes: []string{"/registry"}}
	c.applyRestore(snap, true)

	inp, ok := frontPage(c).(*tview.InputField)
	if !ok {
		t.Fatalf("expected a typed confirmation for the protected prefix, got %T", frontPage(c))
	}
	if len(f.calls) != 0 {
		t.Fatalf("restore ran before the confirmation: %v", f.calls)
	}
	inp.SetText("registry")
	pressEnter(inp)

	if len(f.calls) != 1 {
		t.Errorf("confirmed restore did not run: %v", f.calls)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		2048:    "2.0 kB",
		3145728: "3.0 MB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// A value that is not valid UTF-8 must come back byte for byte. encoding/json
// rewrites invalid UTF-8 as U+FFFD, so a snapshot that kept binary values in
// the plain map would hand back different bytes than were deleted — an undo
// that corrupts what it restores. Found against a real cluster, pinned here.
func TestSnapshotPreservesBinaryValues(t *testing.T) {
	snapshotSandbox(t)
	data := map[string]string{
		"/d/text":   "plain",
		"/d/binary": "\x00\xff\xfe raw",
		"/d/utf8":   "üñî",
	}
	file, err := writeSnapshot(newSnapshot("/d", "host:2379", 9, time.Now(), data))
	if err != nil {
		t.Fatal(err)
	}

	snap, err := readSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, plain := snap.Keys["/d/binary"]; plain {
		t.Error("a binary value was stored in the plain map, where JSON mangles it")
	}
	if snap.count() != len(data) {
		t.Errorf("count = %d, want %d", snap.count(), len(data))
	}
	got, err := snap.values()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, data) {
		t.Errorf("round-trip = %q, want %q", got, data)
	}

	// Text values stay readable in the file, so a snapshot is still a plain
	// export anyone can inspect.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"/d/text": "plain"`) {
		t.Errorf("text values are no longer readable in the file:\n%s", raw)
	}
}

// A corrupt base64 payload must be reported, not restored as garbage.
func TestRestoreRejectsCorruptBinaryPayload(t *testing.T) {
	snapshotSandbox(t)
	f := &fakeModel{}
	c := newTestController(f)
	c.applyRestore(&snapshot{
		Version: snapshotVersion, Path: "/d",
		BinaryKeys: map[string]string{"/d/k": "not base64!!"},
	}, true)

	if len(f.calls) != 0 {
		t.Errorf("a corrupt snapshot was restored anyway: %v", f.calls)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("expected an error modal")
	}
}

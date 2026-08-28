package controller

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// snapshotVersion tags the on-disk format so a future change can be detected
// rather than silently misread as the current one.
const snapshotVersion = 1

// snapshot is what a recursive delete leaves behind: the whole subtree it
// removed, plus enough context to know which cluster and moment it came from.
// Text values keep the same key/value shape the JSON export and import use, so
// a snapshot is also a plain export anyone can read or replay by hand.
type snapshot struct {
	Version  int               `json:"version"`
	Path     string            `json:"path"`
	Endpoint string            `json:"endpoint"`
	Revision int64             `json:"revision"`
	Created  time.Time         `json:"created"`
	Keys     map[string]string `json:"keys"`
	// BinaryKeys holds base64 for every value that is not valid UTF-8.
	// encoding/json rewrites invalid UTF-8 as U+FFFD, so storing such a value
	// in Keys would hand back different bytes than were deleted — and an undo
	// that silently rewrites your data is not an undo. Verified against a real
	// cluster: a raw 0xff byte survives this and does not survive the plain
	// map (DESIGN §14.2).
	BinaryKeys map[string]string `json:"binary_keys,omitempty"`
}

// newSnapshot splits a subtree into the text values a human can read in the
// file and the binary ones that have to be encoded to survive JSON.
func newSnapshot(path, endpoint string, rev int64, at time.Time, data map[string]string) snapshot {
	snap := snapshot{
		Version:  snapshotVersion,
		Path:     normAbs(path),
		Endpoint: endpoint,
		Revision: rev,
		Created:  at,
		Keys:     make(map[string]string, len(data)),
	}
	for k, v := range data {
		if utf8.ValidString(v) {
			snap.Keys[k] = v
			continue
		}
		if snap.BinaryKeys == nil {
			snap.BinaryKeys = map[string]string{}
		}
		snap.BinaryKeys[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return snap
}

// values returns the whole subtree with the binary half decoded — what a
// restore writes back.
func (s *snapshot) values() (map[string]string, error) {
	out := make(map[string]string, len(s.Keys)+len(s.BinaryKeys))
	for k, v := range s.Keys {
		out[k] = v
	}
	for k, enc := range s.BinaryKeys {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, fmt.Errorf("%s: corrupt base64 value: %w", k, err)
		}
		out[k] = string(raw)
	}
	return out, nil
}

// count is how many keys the snapshot holds, both halves together.
func (s *snapshot) count() int { return len(s.Keys) + len(s.BinaryKeys) }

// snapshotMeta is one entry in the picker: the file plus the header fields the
// list shows, without keeping every value in memory for every snapshot.
type snapshotMeta struct {
	file    string
	path    string
	created time.Time
	keys    int
	bytes   int64
}

// snapshotsDir is where undo snapshots live: XDG state, because they are
// recoverable-but-disposable local data, not config and not a cache. The env
// override exists so tests (and anyone with an unusual home) can redirect it.
func snapshotsDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("ETCD_WALKER_STATE_DIR")); dir != "" {
		return filepath.Join(dir, "snapshots"), nil
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "etcd-walker", "snapshots"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("no home directory to store snapshots in (set ETCD_WALKER_STATE_DIR)")
	}
	return filepath.Join(home, ".local", "state", "etcd-walker", "snapshots"), nil
}

// snapshotFileName builds a name that sorts by time and still says what it
// holds. Every character that is not obviously safe in a filename becomes '_',
// because etcd keys may contain anything at all.
func snapshotFileName(path string, at time.Time) string {
	slug := strings.Trim(normAbs(path), "/")
	if slug == "" {
		slug = "root"
	}
	clean := make([]rune, 0, len(slug))
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			clean = append(clean, r)
		default:
			clean = append(clean, '_')
		}
	}
	slug = string(clean)
	const maxSlug = 60
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
	}
	return fmt.Sprintf("%s-%s.json", at.UTC().Format("20060102T150405"), slug)
}

// writeSnapshot saves a subtree to the snapshot directory and returns the file
// it wrote.
func writeSnapshot(snap snapshot) (string, error) {
	dir, err := snapshotsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, snapshotFileName(snap.Path, snap.Created))
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	return file, nil
}

// readSnapshot loads one snapshot file.
func readSnapshot(file string) (*snapshot, error) {
	raw, err := os.ReadFile(file) // #nosec G304 — the path comes from our own picker
	if err != nil {
		return nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	if snap.Version > snapshotVersion {
		return nil, fmt.Errorf("%s was written by a newer etcd-walker (format %d)", filepath.Base(file), snap.Version)
	}
	return &snap, nil
}

// listSnapshots returns every readable snapshot, newest first. Unreadable
// files are skipped rather than failing the whole picker: one corrupt file
// must not hide the others.
func listSnapshots() ([]snapshotMeta, error) {
	dir, err := snapshotsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []snapshotMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, e.Name())
		snap, rerr := readSnapshot(file)
		if rerr != nil {
			continue
		}
		size := int64(0)
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		out = append(out, snapshotMeta{
			file:    file,
			path:    snap.Path,
			created: snap.Created,
			keys:    snap.count(),
			bytes:   size,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].created.After(out[j].created) })
	return out, nil
}

// humanBytes renders a file size the way a picker should: short.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// snapshotForDelete saves the subtree about to be deleted and returns the line
// the success modal shows. The bool reports whether the delete may proceed.
//
// A recursive delete is the one irreversible thing this tool does: a single
// DeleteRange over a prefix, with no server-side undo (revision history is
// per key, and the keys are gone). Taking the export first costs one read that
// export already knows how to do, and turns the worst mistake available into
// an inconvenience. When the snapshot cannot be written the delete is refused
// rather than quietly proceeding unprotected — anyone who wants it anyway can
// turn snapshots off with -no-snapshot.
func (c *Controller) snapshotForDelete(dir string) (string, bool) {
	if !c.policy.SnapshotBeforeDelete {
		return "", true
	}
	if c.policy.DryRun {
		// Nothing is being deleted, so there is nothing to undo.
		return "", true
	}
	data, err := c.model.ExportAt(dir, 0)
	if err != nil {
		c.error("Delete cancelled", fmt.Errorf(
			"could not read %s to save an undo snapshot, so nothing was deleted: %w", dir, err), false)
		return "", false
	}
	if len(data) == 0 {
		return "Nothing to snapshot: the subtree held no keys.", true
	}
	rev, _ := c.model.Revision() // absent on v2; recorded as 0
	file, err := writeSnapshot(newSnapshot(dir, c.endpoint, rev, time.Now(), data))
	if err != nil {
		c.error("Delete cancelled", fmt.Errorf(
			"could not save the undo snapshot, so nothing was deleted: %w\n\nRun with -no-snapshot to delete without one", err), false)
		return "", false
	}
	return fmt.Sprintf("Undo snapshot: %d keys saved to %s (Ctrl+U to restore).", len(data), file), true
}

// showSnapshots lists the undo snapshots taken by recursive deletes, newest
// first, and offers to put one back or throw it away.
func (c *Controller) showSnapshots() *tcell.EventKey {
	snaps, err := listSnapshots()
	if err != nil {
		c.error("Snapshots", err, false)
		return nil
	}
	if len(snaps) == 0 {
		c.info("Snapshots", "No undo snapshots yet. One is saved automatically before every recursive directory delete.")
		return nil
	}

	title := fmt.Sprintf(" undo snapshots — %d ", len(snaps))
	title += tview.Escape("— [r/Enter] restore  [d] delete file  [Esc] close ")
	list := c.view.NewResultsList(title)
	for _, s := range snaps {
		list.AddItem(fmt.Sprintf("%s  %s  (%d keys, %s)",
			s.created.Local().Format("2006-01-02 15:04:05"),
			tview.Escape(s.path), s.keys, humanBytes(s.bytes)), "", 0, nil)
	}
	selected := func() snapshotMeta { return snaps[list.GetCurrentItem()] }
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEsc:
			c.view.Pages.RemovePage("modal")
			return nil
		case ev.Key() == tcell.KeyEnter,
			ev.Key() == tcell.KeyRune && (ev.Rune() == 'r' || ev.Rune() == 'R'):
			c.view.Pages.RemovePage("modal")
			c.restoreSnapshot(selected())
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'd' || ev.Rune() == 'D'):
			c.view.Pages.RemovePage("modal")
			c.deleteSnapshotFile(selected())
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(list, 84, 22), true, true)
	return nil
}

// restoreSnapshot writes a snapshot's keys back where they came from. It goes
// through Import — and therefore through the policy guard and the journal —
// so restoring is as reviewable, as gated and as rehearsable as any other
// bulk write. Keys that exist again are the interesting case, which is why the
// mode question (overwrite / skip) is asked rather than assumed.
func (c *Controller) restoreSnapshot(meta snapshotMeta) {
	snap, err := readSnapshot(meta.file)
	if err != nil {
		c.error("Cannot read snapshot", err, false)
		return
	}
	if snap.count() == 0 {
		c.info("Restore", "That snapshot holds no keys.")
		return
	}

	q := c.view.NewImportModeQ(fmt.Sprintf(
		"Restore %d keys under %s,\ndeleted %s?",
		snap.count(), snap.Path, snap.Created.Local().Format("2006-01-02 15:04:05")))
	q.SetDoneFunc(func(_ int, label string) {
		c.view.Pages.RemovePage("modal")
		if label == "" || label == "cancel" {
			return
		}
		c.applyRestore(snap, label == "overwrite")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(q, 60, 9), true, true)
}

// applyRestore writes a snapshot's keys back, gated by the policy and recorded
// in the journal like any other bulk write.
func (c *Controller) applyRestore(snap *snapshot, overwrite bool) {
	items, err := snap.values()
	if err != nil {
		c.error("Cannot read snapshot", err, false)
		return
	}
	targets := make([]string, 0, len(items))
	for k := range items {
		targets = append(targets, k)
	}
	sort.Strings(targets)
	c.guarded(guard{action: "restore snapshot", paths: targets, do: func() {
		written, skipped, ierr := c.model.Import(items, overwrite)
		if ierr != nil {
			c.error("Restore failed", ierr, false)
			return
		}
		c.updateList()
		if c.policy.DryRun {
			c.info("Restore (dry run)", fmt.Sprintf(
				"%d keys would be written — nothing was sent to the cluster.\nPress Ctrl+A to review the journal.", written))
			return
		}
		c.info("Restored", fmt.Sprintf("%d written, %d skipped, from %s", written, skipped, snap.Path))
	}})
}

// deleteSnapshotFile removes a snapshot from disk, after asking. It touches no
// cluster state, so it is not a policy-gated mutation — but it does destroy
// the only copy of a deleted subtree, so it still asks.
func (c *Controller) deleteSnapshotFile(meta snapshotMeta) {
	q := c.view.NewDeleteQ(fmt.Sprintf("the snapshot of %s (%d keys)", meta.path, meta.keys))
	q.SetDoneFunc(func(_ int, label string) {
		c.view.Pages.RemovePage("modal")
		if label != "ok" {
			return
		}
		if err := os.Remove(meta.file); err != nil {
			c.error("Cannot delete snapshot", err, false)
			return
		}
		c.info("Snapshot deleted", meta.file)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(q, 60, 9), true, true)
}

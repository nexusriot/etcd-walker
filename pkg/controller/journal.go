package controller

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/util/clip"
	"github.com/rivo/tview"
)

// JournalEntry is one mutation the session performed (or, under dry-run,
// would have performed).
type JournalEntry struct {
	// Summary is the human-readable line shown in the journal viewer.
	Summary string
	// Script renders the change as etcdctl. Operations etcdctl cannot express
	// faithfully from what we recorded are emitted as "#" comment lines rather
	// than as plausible-looking commands that would do the wrong thing.
	Script []string
}

// Journal is the ordered record of a session's mutations. It exists so a
// browse-and-poke session can be turned into something reviewable afterwards:
// a change ticket, a runbook, or an etcdctl script to replay elsewhere.
type Journal struct {
	entries []JournalEntry
	// dryRun marks the whole journal as a rehearsal, which changes the wording
	// of both the viewer and the exported script header.
	dryRun bool
}

func (j *Journal) Len() int                { return len(j.entries) }
func (j *Journal) Entries() []JournalEntry { return j.entries }

func (j *Journal) add(summary string, script ...string) {
	j.entries = append(j.entries, JournalEntry{Summary: summary, Script: script})
}

// Script renders the whole journal as a shell script. The header states what
// the file is and, crucially, which lines were not expressible as etcdctl, so
// nobody runs it believing it is complete.
func (j *Journal) Script() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if j.dryRun {
		b.WriteString("# etcd-walker session journal (DRY RUN — nothing was actually written)\n")
	} else {
		b.WriteString("# etcd-walker session journal — these changes were applied\n")
	}
	b.WriteString("#\n")
	b.WriteString("# Lines beginning with '#' are changes etcdctl cannot reproduce from\n")
	b.WriteString("# what was recorded (renames and copies need the value, TTLs need a\n")
	b.WriteString("# lease id). Review before running: this script is not idempotent.\n\n")

	if len(j.entries) == 0 {
		b.WriteString("# (no changes)\n")
		return b.String()
	}
	for _, e := range j.entries {
		for _, line := range e.Script {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// shellQuote renders s as a single-quoted shell word. Single quotes inside are
// closed, escaped and reopened — the only escape a POSIX shell honours there.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// scriptSafe reports whether a value survives a shell script intact. Invalid
// UTF-8 and control characters do not; newlines and tabs inside single quotes
// do. An unsafe value gets a comment instead of a command, because a mangled
// put is worse than an obvious gap.
func scriptSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// putScript renders a key/value write, or a comment when the value cannot be
// carried through a shell script.
func putScript(key, value string) string {
	if !scriptSafe(value) {
		return fmt.Sprintf("# put %s — %d bytes of binary/control data, supply the value separately",
			shellQuote(key), len(value))
	}
	return fmt.Sprintf("etcdctl put %s %s", shellQuote(key), shellQuote(value))
}

// journaling wraps a modelAPI so every successful mutation is recorded, and —
// under dry-run — so mutations are recorded *instead of* being performed.
//
// It decorates the model rather than hooking the controller's policy guard
// because only the model layer sees both the exact arguments and whether the
// call actually succeeded; a guard-level hook would log intent, including
// intent that later failed.
//
// Read methods are promoted from the embedded interface. Any new *mutating*
// method added to modelAPI must be overridden here or it silently escapes the
// journal — TestModelAPISurfaceIsJournalled guards that.
type journaling struct {
	modelAPI
	j      *Journal
	dryRun bool
}

func newJournaling(m modelAPI, j *Journal, dryRun bool) *journaling {
	return &journaling{modelAPI: m, j: j, dryRun: dryRun}
}

// apply runs the underlying mutation unless this is a rehearsal, and records
// it only when it actually took effect.
func (w *journaling) apply(do func() error, summary string, script ...string) error {
	if !w.dryRun {
		if err := do(); err != nil {
			return err
		}
	}
	w.j.add(summary, script...)
	return nil
}

func (w *journaling) Set(key, value string) error {
	return w.apply(func() error { return w.modelAPI.Set(key, value) },
		fmt.Sprintf("set %s (%d bytes)", key, len(value)),
		putScript(key, value))
}

func (w *journaling) SetTTL(key, value string, ttlSeconds int64) error {
	return w.apply(func() error { return w.modelAPI.SetTTL(key, value, ttlSeconds) },
		fmt.Sprintf("set TTL %s on %s", formatTTL(ttlSeconds), key),
		fmt.Sprintf("# TTL %ds on %s — etcdctl lease grant %d, then put with --lease=<id>",
			ttlSeconds, shellQuote(key), ttlSeconds))
}

func (w *journaling) SetKeepTTL(key, value string, leaseID, ttlSeconds int64) error {
	summary := fmt.Sprintf("edit %s (%d bytes)", key, len(value))
	script := putScript(key, value)
	if leaseID != 0 {
		// The value write is faithful, but re-attaching the same lease is not
		// reproducible without that lease's id at replay time.
		script = fmt.Sprintf("# %s (keeping lease %d — re-attach manually)", script, leaseID)
	}
	return w.apply(func() error { return w.modelAPI.SetKeepTTL(key, value, leaseID, ttlSeconds) },
		summary, script)
}

func (w *journaling) MkDir(directory string) error {
	return w.apply(func() error { return w.modelAPI.MkDir(directory) },
		fmt.Sprintf("mkdir %s", directory),
		fmt.Sprintf("etcdctl put %s ''", shellQuote(strings.TrimSuffix(directory, "/")+"/.dir")))
}

func (w *journaling) Del(key string) error {
	return w.apply(func() error { return w.modelAPI.Del(key) },
		fmt.Sprintf("delete %s", key),
		fmt.Sprintf("etcdctl del %s", shellQuote(key)))
}

func (w *journaling) DelDir(key string) error {
	prefix := strings.TrimSuffix(normAbs(key), "/") + "/"
	return w.apply(func() error { return w.modelAPI.DelDir(key) },
		fmt.Sprintf("delete %s recursively", key),
		fmt.Sprintf("etcdctl del --prefix %s", shellQuote(prefix)))
}

func (w *journaling) RenameDir(oldDir, newDir string) error {
	return w.apply(func() error { return w.modelAPI.RenameDir(oldDir, newDir) },
		fmt.Sprintf("rename dir %s -> %s", oldDir, newDir),
		fmt.Sprintf("# rename dir %s -> %s (copy the subtree, then del --prefix the source)",
			shellQuote(oldDir), shellQuote(newDir)))
}

func (w *journaling) RenameKey(oldKey, newKey string) error {
	return w.apply(func() error { return w.modelAPI.RenameKey(oldKey, newKey) },
		fmt.Sprintf("rename %s -> %s", oldKey, newKey),
		fmt.Sprintf("# rename %s -> %s (put the value at the new key, then del the old)",
			shellQuote(oldKey), shellQuote(newKey)))
}

func (w *journaling) CopyKey(src, dst string) error {
	return w.apply(func() error { return w.modelAPI.CopyKey(src, dst) },
		fmt.Sprintf("copy %s -> %s", src, dst),
		fmt.Sprintf("# copy %s -> %s (put the source's value at the target)",
			shellQuote(src), shellQuote(dst)))
}

func (w *journaling) CopyDir(srcDir, dstDir string) error {
	return w.apply(func() error { return w.modelAPI.CopyDir(srcDir, dstDir) },
		fmt.Sprintf("copy dir %s -> %s", srcDir, dstDir),
		fmt.Sprintf("# copy subtree %s -> %s", shellQuote(srcDir), shellQuote(dstDir)))
}

// Import is the one mutation whose full payload we hold, so it renders as real
// put commands rather than a comment.
func (w *journaling) Import(items map[string]string, overwrite bool) (int, int, error) {
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys) // map order is random; a journal must be reproducible

	script := make([]string, 0, len(keys)+1)
	mode := "skipping existing keys"
	if overwrite {
		mode = "overwriting existing keys"
	}
	script = append(script, fmt.Sprintf("# import of %d keys, %s", len(keys), mode))
	for _, k := range keys {
		script = append(script, putScript(k, items[k]))
	}

	if w.dryRun {
		w.j.add(fmt.Sprintf("import %d keys (%s)", len(keys), mode), script...)
		return len(keys), 0, nil
	}
	written, skipped, err := w.modelAPI.Import(items, overwrite)
	if err != nil {
		return written, skipped, err
	}
	w.j.add(fmt.Sprintf("import %d keys, %d skipped (%s)", written, skipped, mode), script...)
	return written, skipped, nil
}

// Compile-time assurance that the decorator still satisfies the interface it
// decorates.
var _ modelAPI = (*journaling)(nil)

// showJournal lists what this session changed, offering the whole thing as an
// etcdctl script — to a file or the clipboard.
func (c *Controller) showJournal() *tcell.EventKey {
	if c.journal.Len() == 0 {
		what := "No changes have been made in this session"
		if c.policy.DryRun {
			what = "No changes have been rehearsed in this session"
		}
		c.info("Session journal", what)
		return nil
	}

	title := fmt.Sprintf(" session journal — %d changes ", c.journal.Len())
	if c.policy.DryRun {
		title = fmt.Sprintf(" session journal — %d changes (DRY RUN, nothing written) ", c.journal.Len())
	}
	title += tview.Escape("— [e] export  [c] copy  [Esc] close ")

	list := c.view.NewResultsList(title)
	for i, e := range c.journal.Entries() {
		list.AddItem(fmt.Sprintf("%3d. %s", i+1, tview.Escape(e.Summary)), "", 0, nil)
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEsc:
			c.view.Pages.RemovePage("modal")
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'e' || ev.Rune() == 'E'):
			c.view.Pages.RemovePage("modal")
			c.exportJournal()
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'c' || ev.Rune() == 'C'):
			c.view.Pages.RemovePage("modal")
			if err := clip.Copy(c.journal.Script()); err != nil {
				c.error("Clipboard error", err, false)
				return nil
			}
			c.copied(fmt.Sprintf("etcdctl script for %d changes", c.journal.Len()))
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(list, 84, 22), true, true)
	return nil
}

// exportJournal prompts for a path and writes the etcdctl script, reusing the
// same overwrite guard as the key export.
func (c *Controller) exportJournal() {
	defaultPath := "etcd-walker-session.sh"
	if home, err := os.UserHomeDir(); err == nil {
		defaultPath = home + "/etcd-walker-session.sh"
	}
	inp := c.view.NewExportInput("session journal", defaultPath)
	inp.SetDoneFunc(func(key tcell.Key) {
		c.view.Pages.RemovePage("modal")
		if key != tcell.KeyEnter {
			return
		}
		filename := strings.TrimSpace(inp.GetText())
		if filename == "" {
			return
		}
		c.confirmOverwrite(filename, "journal", c.writeJournal)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 60, 5), true, true)
}

// writeJournal saves the etcdctl script. It is written executable: the point
// of the file is to be reviewed and then run.
func (c *Controller) writeJournal(filename string) {
	if err := os.WriteFile(filename, []byte(c.journal.Script()), 0o700); err != nil {
		c.error("Cannot write file", fmt.Errorf("%s: %w", filename, err), false)
		return
	}
	c.info("Journal saved", fmt.Sprintf("%d changes written to %s", c.journal.Len(), filename))
}

// mutatingModelMethods names every modelAPI method that changes cluster state.
// It is the contract TestModelAPISurfaceIsJournalled checks against, so adding
// a method to modelAPI forces a deliberate decision about journalling it.
var mutatingModelMethods = []string{
	"Set", "SetTTL", "SetKeepTTL", "MkDir", "Del", "DelDir",
	"RenameDir", "RenameKey", "CopyKey", "CopyDir", "Import",
}

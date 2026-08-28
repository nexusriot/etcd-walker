package controller

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// parseRevisionInput turns what the user typed in the revision picker into an
// absolute revision. An empty string means "back to live" (0). A bare number
// is that revision. A leading '-' is relative to current — "-100" is a hundred
// revisions ago — which is how anyone actually thinks about "just before the
// incident", since nobody knows the absolute number off-hand.
func parseRevisionInput(raw string, current int64) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a revision (enter a number, -N for N revisions back, or nothing to return to live)", raw)
	}
	if n < 0 {
		if current <= 0 {
			return 0, fmt.Errorf("relative revisions need the cluster's current revision, which is unavailable")
		}
		target := current + n // n is negative
		if target < 1 {
			return 0, fmt.Errorf("%d revisions back from %d is before the store began", -n, current)
		}
		return target, nil
	}
	if n == 0 {
		return 0, nil
	}
	if current > 0 && n > current {
		return 0, fmt.Errorf("revision %d is in the future — the cluster is at %d", n, current)
	}
	return n, nil
}

// goToRevision pins this pane to a past etcd revision, or releases it back to
// the live cluster. Every read the pane makes then carries that revision, so
// the whole browser — listings, values, find, export — shows the keyspace as
// it was at that moment. Writes are refused while it is set.
//
// etcd's revision is global and monotonic, which is what makes this cheap: the
// same range reads with one extra option. The limit is compaction, and a read
// below the compaction point fails loudly rather than showing an empty tree.
func (c *Controller) goToRevision() *tcell.EventKey {
	// The current revision bounds the input and resolves relative ones. It is
	// not fatal if the cluster will not say (v2 has no revisions at all): the
	// picker still opens and explains itself.
	current, cerr := c.model.Revision()

	prompt := " Go to revision — number, -N for N back, empty = live "
	if cerr == nil {
		prompt = fmt.Sprintf(" Go to revision (current: %d) — number, -N for N back, empty = live ", current)
	}
	inp := c.view.NewTypedConfirm(prompt)
	if c.atRevision() {
		inp.SetText(strconv.FormatInt(c.rev, 10))
	}
	inp.SetDoneFunc(func(key tcell.Key) {
		c.view.Pages.RemovePage("modal")
		if key != tcell.KeyEnter {
			return
		}
		raw := strings.TrimSpace(inp.GetText())
		if raw != "" && cerr != nil {
			c.error("Revision browsing unavailable", cerr, false)
			return
		}
		rev, err := parseRevisionInput(raw, current)
		if err != nil {
			c.error("Invalid revision", err, false)
			return
		}
		c.applyRevision(rev)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 76, 5), true, true)
	return nil
}

// applyRevision switches the pane to rev, verifying the read before committing
// to it: a revision below the compaction point must leave the pane where it
// was, showing an error, rather than switching to a view that cannot load.
func (c *Controller) applyRevision(rev int64) {
	previous := c.rev
	c.rev = rev
	if rev > 0 {
		if _, err := c.paneLs(c.currentDir); err != nil {
			c.rev = previous
			c.error("Cannot read revision "+strconv.FormatInt(rev, 10), reviseCompactionError(err, rev), false)
			return
		}
	}
	c.updateList()
	c.fillDetails(currentMapKeyOf(c.view.List))
	if rev == 0 {
		if previous > 0 {
			c.info("Live", "Back to the current cluster state.")
		}
		return
	}
	c.info(fmt.Sprintf("Revision %d", rev),
		"This pane now shows the keyspace as it was at that revision. It is read-only; press Ctrl+G and clear it to return to live.")
}

// reviseCompactionError turns etcd's terse compaction error into the one
// sentence that explains it: the data is not hidden, it is gone.
func reviseCompactionError(err error, rev int64) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "compacted") {
		return fmt.Errorf("revision %d has been compacted away — the cluster no longer stores it: %w", rev, err)
	}
	return err
}

package controller

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// newPaneState returns a pane parked at the root, ready to list.
func newPaneState() paneState {
	return paneState{
		currentDir:  "/",
		lastGoodDir: "/",
		position:    make(map[string]int),
	}
}

// swapPanes exchanges the active pane's state with the parked one and repoints
// the view at the newly active list. It is one struct assignment on purpose:
// every per-pane field travels together, so a field added to paneState cannot
// be forgotten here.
func (c *Controller) swapPanes() {
	c.paneState, c.inactive = c.inactive, c.paneState
	c.active = 1 - c.active
	if c.view.Lists[c.active] != nil {
		c.view.List = c.view.Lists[c.active]
	}
}

// withOtherPane runs do with the other pane active, then restores the current
// one. Refreshing the pane that was just written to goes through here, because
// updateList and every helper it calls address the active pane by definition.
func (c *Controller) withOtherPane(do func()) {
	c.swapPanes()
	do()
	c.swapPanes()
}

// otherDir is the directory the inactive pane is showing — the target of the
// cross-pane copy and move bindings.
func (c *Controller) otherDir() string { return normAbs(c.inactive.currentDir) }

// switchPane moves the focus to the other pane (Tab). The panes keep their own
// directory, cursor and revision, so switching is not a refresh: what the pane
// showed when it lost focus is what it shows on return, until something is
// written to it.
func (c *Controller) switchPane() {
	if !c.dual {
		return
	}
	c.swapPanes()
	c.view.App.SetFocus(c.view.List)
	c.applyPaneStyles()
	c.fillDetails(currentMapKeyOf(c.view.List))
}

// currentMapKeyOf reads the focused row's mapKey (kept in the row's hidden
// secondary text) from a list.
func currentMapKeyOf(l *tview.List) string {
	if l == nil || l.GetItemCount() == 0 {
		return ""
	}
	_, sec := l.GetItemText(l.GetCurrentItem())
	return strings.TrimSpace(sec)
}

// applyPaneStyles marks which pane has the focus. Without it the two panes are
// indistinguishable and a keystroke lands in whichever one the user is not
// looking at — the single worst failure mode of a two-pane browser.
func (c *Controller) applyPaneStyles() {
	for i, l := range c.view.Lists {
		if l == nil {
			continue
		}
		if c.dual && i == c.active {
			l.SetBorderColor(tcell.ColorYellow)
			l.SetTitleColor(tcell.ColorYellow)
			continue
		}
		l.SetBorderColor(tcell.ColorWhite)
		l.SetTitleColor(tcell.ColorWhite)
	}
}

// SetDualPane turns the two-pane layout on or off. The pane being switched on
// is listed on first use rather than at startup, so a second connection's
// worth of round trips is only spent if the user asks for it.
func (c *Controller) SetDualPane(on bool) {
	if c.dual == on {
		return
	}
	c.dual = on
	c.view.SetDual(on)
	if !on {
		// Only the first list stays in the layout, so a session focused on the
		// right pane has to be re-rendered into the left widget. Move the
		// widget, never the state: swapping here would collapse the user into
		// the *other* pane's directory, which is a silent jump.
		if c.active != 0 {
			c.active = 0
			c.view.List = c.view.Lists[0]
		}
		c.applyPaneStyles()
		c.updateList()
		c.view.App.SetFocus(c.view.List)
		return
	}
	// The pane being revealed has never been listed; do it now, without
	// disturbing the focused one.
	c.withOtherPane(func() { c.updateList() })
	c.applyPaneStyles()
	c.view.App.SetFocus(c.view.List)
}

// crossTransfer implements the two cross-pane bindings: copy (F5) and move
// (F6) of the focused entry into the other pane's directory, under the same
// basename. They are the reason for the second pane — the whole point of a
// two-pane browser is that "the other side" is the implied destination, so
// neither binding asks for a path.
func (c *Controller) crossTransfer(move bool) *tcell.EventKey {
	action, title, verb := "copy", "Copy", "Copied"
	if move {
		action, title, verb = "move", "Move", "Moved"
	}
	if !c.dual {
		c.info(title, fmt.Sprintf(
			"%s to the other pane needs the two-pane layout — press Ctrl+B to turn it on.", action))
		return nil
	}
	val, ok := c.focusedNode()
	if !ok {
		return nil
	}
	// The destination pane must be showing the live cluster: writing into a
	// pane that is displaying a past revision would land the data somewhere
	// the pane is not showing, and its listing would not reflect it.
	if c.inactive.rev > 0 {
		c.info("Target pane is a snapshot", fmt.Sprintf(
			"The other pane is browsing revision %d. Clear it there (Ctrl+G) before %sing into it.",
			c.inactive.rev, action))
		return nil
	}

	src := normAbs(val.node.Name)
	dstDir := c.otherDir()
	dst := normAbs(dstDir + "/" + baseOf(src))
	if dst == src {
		c.info(title, "Both panes are in the same directory — nothing to do.")
		return nil
	}

	// A move both removes the source and writes the target, so both sides gate
	// it; a copy only writes the target.
	paths := []string{dst}
	if move {
		paths = []string{src, dst}
	}
	c.guarded(guard{action: action, paths: paths, do: func() {
		if nd, gerr := c.model.GetAt(dst, 0); gerr == nil && nd != nil {
			c.error("Target exists", fmt.Errorf("%q already exists in the other pane", dst), false)
			return
		}
		var err error
		switch {
		case move && val.node.IsDir:
			err = c.model.RenameDir(src, dst)
		case move:
			err = c.model.RenameKey(src, dst)
		case val.node.IsDir:
			err = c.model.CopyDir(src, dst)
		default:
			err = c.model.CopyKey(src, dst)
		}
		if err != nil {
			c.error("Failed to "+action, err, false)
			return
		}
		if strings.HasPrefix(baseOf(dst), "_") {
			c.injectWritten(&model.Node{Name: dst, IsDir: val.node.IsDir, ClusterId: val.node.ClusterId})
		}
		if move {
			c.removeWritten(val.node)
		}
		c.updateList()
		c.withOtherPane(func() { c.updateList() })
		c.info(c.writeHeader(verb), fmt.Sprintf("%s → %s", src, dst))
	}})
	return nil
}

// focusedNode returns the node under the cursor, or false when the cursor is
// on "[..]" or the list is empty. Every entry-scoped binding starts here.
func (c *Controller) focusedNode() (*Node, bool) {
	mapKey := currentMapKeyOf(c.view.List)
	if mapKey == "" || mapKey == ".." {
		return nil, false
	}
	val, ok := c.currentNodes[mapKey]
	if !ok || val == nil || val.node == nil {
		return nil, false
	}
	return val, true
}

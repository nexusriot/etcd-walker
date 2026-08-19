package controller

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
)

// historyLimit caps how many revisions the history walk retrieves; the walk
// costs one round-trip per revision, so the cap keeps Ctrl+V snappy even on
// heavily rewritten keys.
const historyLimit = 100

// historyHexLimit bounds the hex dump shown for binary revision values.
const historyHexLimit = 1024

// historyValueLimit bounds the printable value shown on the revision detail
// screen. It is far larger than the details pane's 512-byte preview — this is
// a dedicated screen — but still bounded so a multi-megabyte value cannot
// stall the draw. 'f' re-renders without the cap.
const historyValueLimit = 8192

// history opens the revision-history flow for the focused key (v3 only —
// the model returns a descriptive error on v2). Directories are refused:
// they are synthetic and have no revisions of their own.
func (c *Controller) history() *tcell.EventKey {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	i := c.view.List.GetCurrentItem()
	_, mapKey := c.view.List.GetItemText(i)
	mapKey = strings.TrimSpace(mapKey)
	if mapKey == ".." {
		return nil
	}
	val, ok := c.currentNodes[mapKey]
	if !ok || val.node == nil {
		return nil
	}
	if val.node.IsDir {
		c.error("History", fmt.Errorf("revision history is only available for keys, not directories"), false)
		return nil
	}

	revs, truncated, err := c.model.History(val.node.Name, historyLimit)
	if err != nil {
		c.error("History failed", err, false)
		return nil
	}
	if len(revs) <= 1 {
		msg := "Only the current version is stored"
		if len(revs) == 1 && revs[0].Version > 1 {
			msg = "Older revisions have been compacted away; only the current version is available"
		}
		c.info("History", msg)
		return nil
	}
	c.showHistoryPicker(val.node, revs, truncated)
	return nil
}

// showHistoryPicker lists a key's revisions newest-first; choosing one opens
// the detail screen.
func (c *Controller) showHistoryPicker(nd *model.Node, revs []*model.Revision, truncated bool) {
	name := tview.Escape(nd.Name) // titles are colour-tag parsed like any text
	title := fmt.Sprintf(" %d revisions of %s ", len(revs), name)
	switch {
	case truncated:
		title = fmt.Sprintf(" newest %d revisions of %s (limit reached) ", len(revs), name)
	case revs[len(revs)-1].Version > 1:
		title = fmt.Sprintf(" %d revisions of %s (older compacted) ", len(revs), name)
	}

	list := c.view.NewResultsList(title)
	for idx, r := range revs {
		idx, r := idx, r
		label := fmt.Sprintf("rev %-9d v%-5d %8s  %s",
			r.Rev, r.Version, fmt.Sprintf("%d B", len(r.Value)), revPreview(r.Value))
		if idx == 0 {
			label = "[::b]" + label + "  (current)[::-]"
		}
		list.AddItem(label, "", 0, func() {
			c.view.Pages.RemovePage("modal")
			c.showRevisionDetail(nd, revs, idx, truncated, false)
		})
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("modal")
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(list, 78, 20), true, true)
}

// revPreview renders the first line of a value for a picker row, trimmed to
// a sane width; binary values get a placeholder instead of mojibake.
func revPreview(v string) string {
	if v == "" {
		return "(empty)"
	}
	if !utf8.ValidString(v) {
		return "(binary)"
	}
	if nl := strings.IndexByte(v, '\n'); nl >= 0 {
		v = v[:nl] + "…"
	}
	const max = 38
	if r := []rune(v); len(r) > max {
		v = string(r[:max]) + "…"
	}
	return tview.Escape(v)
}

// showRevisionDetail renders one revision: metadata plus its value (hex for
// binary). Large values are capped — a multi-megabyte value rendered in full
// stalls the draw — and 'f' re-opens the screen uncapped. 'r' restores the
// revision, 'd' diffs it against the current value, Esc returns to the picker.
func (c *Controller) showRevisionDetail(nd *model.Node, revs []*model.Revision, idx int, truncated, full bool) {
	r := revs[idx]

	// Work out what the value pane will show first: the hint line advertises
	// [f] only when something is actually being held back.
	size, lines, printable := valueStats(r.Value)
	shown, title := r.Value, "Value"
	if printable {
		if pj, ok := prettyJSON(r.Value); ok {
			shown, title = pj, "Value (JSON)"
		}
	}
	hexLimit := historyHexLimit
	if full {
		hexLimit = len(r.Value)
	}
	cut, clipped := shown, false
	if !full && printable {
		cut, clipped = truncateBytes(shown, historyValueLimit)
	}
	if !printable {
		clipped = !full && len(r.Value) > historyHexLimit
	}

	// tview reads "[d]" as a colour tag and swallows it — in titles as much as
	// in text — so every bracketed hotkey hint has to be escaped or it renders
	// as a blank. tview.Escape turns "[d]" into the literal-bracket form.
	hint := tview.Escape("[d] diff vs current · [Esc] back")
	// The 'r' binding still runs the policy guard, which refuses in read-only
	// mode; advertising it there would just invite a refusal modal.
	if idx > 0 && !c.policy.ReadOnly {
		hint = tview.Escape("[r] restore · ") + hint
	}
	if clipped {
		hint = tview.Escape("[f] full value · ") + hint
	}
	tv := c.view.NewHistoryDetail(fmt.Sprintf(" %s @ rev %d — %s ", tview.Escape(nd.Name), r.Rev, hint))

	fmt.Fprintf(tv, "[::b]Revision info[::-]\n")
	fmt.Fprintf(tv, "  [green]Key:[-] %s\n", nd.Name)
	if idx == 0 {
		fmt.Fprintf(tv, "  [green]Revision:[-] %d (newest known)\n", r.Rev)
	} else {
		fmt.Fprintf(tv, "  [green]Revision:[-] %d\n", r.Rev)
	}
	fmt.Fprintf(tv, "  [green]Version:[-] %d\n", r.Version)
	fmt.Fprintf(tv, "  [green]Size:[-] %d bytes · [green]Lines:[-] %d\n", size, lines)
	fmt.Fprintf(tv, "  [green]SHA-256:[-] %s\n", shortHash(r.Value))

	more := fmt.Sprintf("[gray]press %s to show the whole value[-]", tview.Escape("[f]"))
	switch {
	case printable && clipped:
		fmt.Fprintf(tv, "\n[::b]%s (first %d of %d bytes)[::-]\n%s…\n\n%s\n",
			title, len(cut), len(shown), tview.Escape(cut), more)
	case printable:
		fmt.Fprintf(tv, "\n[::b]%s[::-]\n%s\n", title, tview.Escape(cut))
	case clipped:
		fmt.Fprintf(tv, "\n[::b]Value (hex, first %d of %d bytes)[::-]\n%s\n%s\n",
			hexLimit, len(r.Value), tview.Escape(hexDump(r.Value, hexLimit)), more)
	default:
		fmt.Fprintf(tv, "\n[::b]Value (hex, %d bytes)[::-]\n%s", len(r.Value),
			tview.Escape(hexDump(r.Value, hexLimit)))
	}

	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEsc:
			c.view.Pages.RemovePage("modal")
			c.showHistoryPicker(nd, revs, truncated)
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'f' || ev.Rune() == 'F'):
			if !clipped {
				return nil // nothing is being held back
			}
			c.view.Pages.RemovePage("modal")
			c.showRevisionDetail(nd, revs, idx, truncated, true)
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'd' || ev.Rune() == 'D'):
			c.view.Pages.RemovePage("modal")
			c.showRevisionDiff(nd, revs, idx, truncated)
			return nil
		case ev.Key() == tcell.KeyRune && (ev.Rune() == 'r' || ev.Rune() == 'R'):
			if idx == 0 {
				return nil // the current revision needs no restoring
			}
			c.view.Pages.RemovePage("modal")
			c.confirmRestore(nd, revs, idx, truncated)
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(tv, 78, 24), true, true)
}

// confirmRestore asks before overwriting the current value; cancel returns
// to the detail screen so the user keeps their place in the flow.
func (c *Controller) confirmRestore(nd *model.Node, revs []*model.Revision, idx int, truncated bool) {
	r := revs[idx]
	q := c.view.NewRestoreQ(fmt.Sprintf(
		"Restore %s to its rev %d value? The current value will be overwritten (TTL/lease is kept).",
		nd.Name, r.Rev))
	q.SetDoneFunc(func(_ int, label string) {
		c.view.Pages.RemovePage("modal")
		if label != "restore" {
			c.showRevisionDetail(nd, revs, idx, truncated, false)
			return
		}
		c.restoreRevision(nd, r)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(q, 60, 9), true, true)
}

// restoreRevision writes an old revision's value back as the key's current
// value. The key is re-read first so the write goes through SetKeepTTL with
// the *live* lease/TTL — restoring an old value must not clobber the key's
// expiry, the same contract the value editor honours.
func (c *Controller) restoreRevision(nd *model.Node, r *model.Revision) {
	cur, err := c.model.Get(nd.Name)
	if err != nil {
		c.error("Restore failed", fmt.Errorf("re-reading %s: %w", nd.Name, err), false)
		return
	}
	// A restore is a write like any other, and it reaches here without passing
	// the main list's key bindings, so it needs the policy guard of its own.
	c.guarded(guard{action: "restore", paths: []string{nd.Name}, do: func() {
		if err := c.model.SetKeepTTL(nd.Name, r.Value, cur.LeaseID, cur.TTL); err != nil {
			c.error("Restore failed", err, false)
			return
		}
		ordered := c.updateList()
		target := displayName(baseOf(nd.Name), false)
		c.selectRow(target, ordered)
		c.fillDetails(makeMapKey(baseOf(nd.Name), false))
		c.info(c.writeHeader("Restored"), fmt.Sprintf("%s reverted to rev %d value", nd.Name, r.Rev))
	}})
}

// showRevisionDiff renders a line diff of the selected revision against the
// current value (minus = selected revision, plus = current).
func (c *Controller) showRevisionDiff(nd *model.Node, revs []*model.Revision, idx int, truncated bool) {
	r := revs[idx]

	// Diff against the key as it is *now*, re-read here, rather than against
	// revs[0]. That entry is a snapshot from when History ran, so calling it
	// "current" stops being true the moment anyone else writes the key — and
	// this screen is exactly where someone decides whether to restore.
	cur, err := c.model.Get(nd.Name)
	if err != nil {
		c.error("Diff failed", fmt.Errorf("re-reading %s: %w", nd.Name, err), false)
		return
	}
	if cur == nil || cur.IsDir {
		c.error("Diff failed", fmt.Errorf("%s is no longer a key", nd.Name), false)
		return
	}

	note := ""
	if cur.ModRev != revs[0].Rev {
		note = fmt.Sprintf("[yellow]note: the key changed since this history was read (rev %d → %d)[-]\n\n",
			revs[0].Rev, cur.ModRev)
	}

	tv := c.view.NewHistoryDetail(fmt.Sprintf(
		" diff %s: rev %d → current (rev %d) — %s ",
		tview.Escape(nd.Name), r.Rev, cur.ModRev, tview.Escape("[Esc] back")))
	fmt.Fprint(tv, note)

	switch {
	case !utf8.ValidString(r.Value) || !utf8.ValidString(cur.Value):
		fmt.Fprintf(tv, "binary value — cannot diff (rev %d: %d bytes, current: %d bytes)\n",
			r.Rev, len(r.Value), len(cur.Value))
	default:
		ops := diffLines(r.Value, cur.Value)
		minus, plus := diffCounts(ops)
		switch {
		case ops == nil:
			fmt.Fprintf(tv, "values too large to diff (rev %d: %d bytes, current: %d bytes)\n",
				r.Rev, len(r.Value), len(cur.Value))
		case minus == 0 && plus == 0:
			fmt.Fprintf(tv, "no differences — the value at rev %d is identical to the current value\n", r.Rev)
		default:
			fmt.Fprintf(tv, "[red]--- rev %d[-]  [green]+++ current (rev %d)[-]  ([red]-%d[-] / [green]+%d[-] lines)\n\n",
				r.Rev, cur.ModRev, minus, plus)
			fmt.Fprint(tv, renderDiff(ops))
		}
	}

	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("modal")
			c.showRevisionDetail(nd, revs, idx, truncated, false)
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(tv, 80, 24), true, true)
}

type diffOp struct {
	tag  byte // ' ' unchanged, '-' only in old, '+' only in new
	text string
}

// maxDiffCells bounds the LCS table so a diff of two huge values cannot
// stall the UI; past it diffLines returns nil and the caller shows a notice.
const maxDiffCells = 1 << 20

// diffLines computes a line-based LCS diff between old and new. Deletions
// come out before insertions at each divergence, giving the conventional
// unified-diff reading order.
func diffLines(oldText, newText string) []diffOp {
	a, b := splitDiffLines(oldText), splitDiffLines(newText)
	n, m := len(a), len(b)
	if n*m > maxDiffCells {
		return nil
	}

	// dp[i][j] = LCS length of a[i:] vs b[j:]
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// splitDiffLines treats the empty value as zero lines, so "" vs "x" diffs as
// a pure insertion rather than a change against a phantom empty line.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func diffCounts(ops []diffOp) (minus, plus int) {
	for _, op := range ops {
		switch op.tag {
		case '-':
			minus++
		case '+':
			plus++
		}
	}
	return minus, plus
}

// renderDiff turns diff ops into tview-colored text, collapsing long runs of
// unchanged lines around the changes the way unified diffs show context.
func renderDiff(ops []diffOp) string {
	const keep = 3
	var b strings.Builder
	writeCtx := func(op diffOp) { fmt.Fprintf(&b, "  %s\n", tview.Escape(op.text)) }

	i := 0
	for i < len(ops) {
		if ops[i].tag != ' ' {
			switch ops[i].tag {
			case '-':
				fmt.Fprintf(&b, "[red]- %s[-]\n", tview.Escape(ops[i].text))
			case '+':
				fmt.Fprintf(&b, "[green]+ %s[-]\n", tview.Escape(ops[i].text))
			}
			i++
			continue
		}
		j := i
		for j < len(ops) && ops[j].tag == ' ' {
			j++
		}
		run := j - i
		// Trailing/leading context at the buffer edges still collapses; only
		// keep lines adjacent to actual changes.
		if run > 2*keep+1 {
			for k := i; k < i+keep; k++ {
				writeCtx(ops[k])
			}
			fmt.Fprintf(&b, "[gray]··· %d unchanged lines ···[-]\n", run-2*keep)
			for k := j - keep; k < j; k++ {
				writeCtx(ops[k])
			}
		} else {
			for k := i; k < j; k++ {
				writeCtx(ops[k])
			}
		}
		i = j
	}
	return b.String()
}

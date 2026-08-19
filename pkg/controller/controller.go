package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/util/clip"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"
)

// modelAPI is the model surface the controller drives. *model.Model
// satisfies it; tests substitute an in-memory fake.
type modelAPI interface {
	ProtocolVersion() string
	AuthLabel() string
	Ls(directory string) ([]*model.Node, error)
	Get(key string) (*model.Node, error)
	Set(key, value string) error
	SetTTL(key, value string, ttlSeconds int64) error
	SetKeepTTL(key, value string, leaseID, ttlSeconds int64) error
	MkDir(directory string) error
	Del(key string) error
	DelDir(key string) error
	RenameDir(oldDir, newDir string) error
	RenameKey(oldKey, newKey string) error
	CopyKey(src, dst string) error
	CopyDir(srcDir, dstDir string) error
	Search(dir, query string, inValues bool, limit int) ([]*model.Node, bool, error)
	History(key string, limit int) ([]*model.Revision, bool, error)
	Export(dir string) (map[string]string, error)
	Import(items map[string]string, overwrite bool) (written, skipped int, err error)
}

// Policy is the session's safety configuration: what may be changed at all,
// and which paths demand extra deliberation before they are. It is deliberately
// enforced in the controller rather than the model — it describes what this
// session is allowed to do, not what the cluster supports.
type Policy struct {
	// ReadOnly refuses every mutating action outright.
	ReadOnly bool
	// ProtectedPrefixes are paths whose subtrees need a typed confirmation
	// before any write. A bare "/" protects the whole keyspace.
	ProtectedPrefixes []string
	// DryRun records what each mutation would do without performing it, so a
	// change can be rehearsed against the real cluster and reviewed from the
	// session journal before being run for real.
	DryRun bool
}

// protects reports the protected prefix covering path, if any. A prefix covers
// itself and everything beneath it, but not siblings that merely share a
// string prefix: "/reg" does not protect "/registry".
func (p Policy) protects(path string) (string, bool) {
	target := normAbs(path)
	for _, raw := range p.ProtectedPrefixes {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		pfx := normAbs(raw)
		if pfx == "/" || target == pfx || strings.HasPrefix(target, pfx+"/") {
			return pfx, true
		}
	}
	return "", false
}

// guard describes one policy-gated mutation.
type guard struct {
	// action names the operation for the refusal/confirmation text ("delete").
	action string
	// paths are everything the operation writes to; the whole operation is
	// gated if any of them is protected.
	paths []string
	// confirmWord, when set, demands a typed confirmation even for unprotected
	// paths — recursive deletes use it. A protected path supersedes it so the
	// user is never asked to type two different words for one action.
	confirmWord string
	// do performs the mutation once the policy allows it.
	do func()
	// onCancel runs when a protected confirmation is declined. Optional; the
	// value editor uses it to hand the user back their unsaved text.
	onCancel func()
}

type Controller struct {
	view   *view.View
	model  modelAPI
	policy Policy
	// journal records every mutation this session made, for review or export
	// as an etcdctl script. Written by the journalling model decorator.
	journal      *Journal
	currentDir   string
	currentNodes map[string]*Node // mapKey => Node (mapKey is "<basename>|dir" or "<basename>|file")
	position     map[string]int
	injected     map[string]map[string]*model.Node
	// ordered mirrors the display names of the list rows (below "[..]") in the
	// exact order updateList built them; search() selects rows through it.
	ordered []string
	// lastGoodDir is the most recent directory that listed successfully; a
	// failed listing falls back here instead of aborting the session.
	lastGoodDir string

	startupErr error
}

type Node struct {
	node *model.Node
	// stale records that the last on-focus refresh of this node failed, so
	// what is displayed came from the listing cache rather than the server.
	// v3 listings are keys-only, which makes a silently cached value actively
	// dangerous — see DESIGN §12 B-7.
	stale bool
}

func splitFunc(r rune) bool { return r == '/' }

// appVersion is the user-facing version. DESIGN §9.3: it is duplicated in the
// Makefile and build-deb.sh, and all three must move together until F-3 lands.
const appVersion = "0.9.0"

// headerText renders the frame's status line and the colour that goes with it.
// It is separated from NewController so it can be tested without dialling a
// cluster — and it needs testing, because every bracketed tag here has to be
// escaped or tview's colour parser eats it (DESIGN §12 B-13; the unescaped
// "[TLS]" was invisible for exactly that reason).
func headerText(opts model.Options, proto, auth string, policy Policy) (string, tcell.Color) {
	tlsTag := ""
	if opts.TLSEnabled {
		tlsTag = " " + tview.Escape("[TLS]")
	}

	// The safety mode belongs in the header: it is the one thing the user must
	// not have to remember about the session they are in.
	color := tcell.ColorGreen
	mode := ""
	if n := len(policy.ProtectedPrefixes); n > 0 {
		mode = fmt.Sprintf("  |  protected: %d", n)
	}
	if policy.DryRun {
		mode = "  |  " + tview.Escape("[DRY-RUN]") + mode
		color = tcell.ColorAqua
	}
	if policy.ReadOnly {
		mode = "  |  " + tview.Escape("[READ-ONLY]") + mode
		color = tcell.ColorYellow
	}

	return fmt.Sprintf("Etcd-walker v.%s (on %s:%s%s)  –  protocol: %s  |  Auth: %s%s",
		appVersion, opts.Host, opts.Port, tlsTag, proto, auth, mode), color
}

func NewController(opts model.Options, policy Policy) *Controller {
	m, err := model.NewModel(opts)

	v := view.NewView()

	headerProto := opts.Protocol
	auth := "?"
	if err == nil && m != nil {
		headerProto = m.ProtocolVersion()
		auth = m.AuthLabel()
	}

	text, headerColor := headerText(opts, headerProto, auth, policy)
	v.Frame.AddText(text, true, tview.AlignCenter, headerColor)

	controller := &Controller{
		view:        v,
		policy:      policy,
		journal:     &Journal{dryRun: policy.DryRun},
		currentDir:  "/",
		lastGoodDir: "/",
		position:    make(map[string]int),
		injected:    make(map[string]map[string]*model.Node),
		startupErr:  err,
	}
	// Assign only a real model: a nil *model.Model stored in the interface
	// would read as non-nil. On startup error Run() never touches the model.
	// Every mutation goes through the journalling decorator, which is also
	// what makes dry-run work — it records instead of writing.
	if m != nil {
		controller.model = newJournaling(m, controller.journal, policy.DryRun)
	}
	return controller
}

// makeMapKey ensures uniqueness when file and dir share the same basename.
func makeMapKey(base string, isDir bool) string {
	if isDir {
		return base + "|dir"
	}
	return base + "|file"
}

// displayName returns what user sees in the list/search.
func displayName(base string, isDir bool) string {
	if isDir {
		return base + "/"
	}
	return base
}

func normAbs(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return p
}

func parentOf(p string) string {
	p = normAbs(p)
	if p == "/" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func baseOf(p string) string {
	p = normAbs(p)
	if p == "/" {
		return "/"
	}
	i := strings.LastIndex(p, "/")
	if i < 0 || i == len(p)-1 {
		return p
	}
	return p[i+1:]
}

func (c *Controller) ensureInjectedBucket(parent string) map[string]*model.Node {
	parent = normAbs(parent)
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	if c.injected[parent] == nil {
		c.injected[parent] = make(map[string]*model.Node)
	}
	return c.injected[parent]
}

// inject node so it appears in the list for its parent directory.
func (c *Controller) injectNode(nd *model.Node) {
	if nd == nil {
		return
	}
	name := normAbs(nd.Name)
	if name == "/" {
		return // root is nobody's child; there is no bucket to put it in
	}
	par := parentOf(name)
	if par != "/" {
		par += "/"
	}
	bucket := c.ensureInjectedBucket(par)
	mk := makeMapKey(baseOf(name), nd.IsDir)
	bucket[mk] = nd
	log.Debugf("injected node %s under %s as %s (dir=%t)", nd.Name, par, mk, nd.IsDir)
}

// remove injected node (e.g., after delete/rename)
func (c *Controller) removeInjected(nd *model.Node) {
	if nd == nil {
		return
	}
	name := normAbs(nd.Name)
	if name == "/" {
		return
	}
	par := parentOf(name)
	if par != "/" {
		par += "/"
	}
	mk := makeMapKey(baseOf(name), nd.IsDir)
	if bucket, ok := c.injected[par]; ok {
		delete(bucket, mk)
		if len(bucket) == 0 {
			delete(c.injected, par)
		}
	}
}

// re-inject after rename (old -> new)
func (c *Controller) reinjectRename(oldName, newName string, isDir bool, clusterID, value string) {
	old := &model.Node{Name: normAbs(oldName), IsDir: isDir, ClusterId: clusterID, Value: value}
	c.removeInjected(old)
	newN := &model.Node{Name: normAbs(newName), IsDir: isDir, ClusterId: clusterID, Value: value}
	c.injectNode(newN)
}

// The three helpers below record a mutation this session just performed in the
// local injected cache. They are no-ops under dry-run: nothing reached the
// cluster, so faking the row would show a key that does not exist (or hide one
// that still does) — exactly the ghost-row class of bug B-8. Navigation keeps
// calling injectNode directly, because a jumped-to key really is on the server.

func (c *Controller) injectWritten(nd *model.Node) {
	if c.policy.DryRun {
		return
	}
	c.injectNode(nd)
}

func (c *Controller) removeWritten(nd *model.Node) {
	if c.policy.DryRun {
		return
	}
	c.removeInjected(nd)
}

func (c *Controller) reinjectWritten(oldName, newName string, isDir bool, clusterID, value string) {
	if c.policy.DryRun {
		return
	}
	c.reinjectRename(oldName, newName, isDir, clusterID, value)
}

// writeHeader labels a success modal honestly: under dry-run the reported
// change was recorded in the journal, not made.
func (c *Controller) writeHeader(header string) string {
	if c.policy.DryRun {
		return header + " (dry run — not written)"
	}
	return header
}

func (c *Controller) makeNodeMap() error {
	log.Debugf("updating node map started")
	m := make(map[string]*Node)

	// Model-provided listing
	list, err := c.model.Ls(c.currentDir)
	if err != nil {
		return err
	}
	for _, n := range list {
		// baseOf, not FieldsFunc(...)[len-1]: the latter panics on a name that
		// splits into nothing (""), and baseOf is what every other call site
		// uses, so the map key matches the one actions look up.
		base := baseOf(n.Name)
		mapKey := makeMapKey(base, n.IsDir)
		cNode := Node{node: n}
		m[mapKey] = &cNode
		log.Debugf("added node %s -> base=%s key=%s isDir=%t", n.Name, base, mapKey, n.IsDir)
	}

	// Merge injected entries for current directory
	cur := normAbs(c.currentDir)
	if !strings.HasSuffix(cur, "/") {
		cur += "/"
	}
	if bucket, ok := c.injected[cur]; ok {
		for mk, nd := range bucket {
			// If server listing already returned an entry with same mk, keep server version (prefer real listing)
			if _, exists := m[mk]; !exists {
				m[mk] = &Node{node: nd}
				log.Debugf("merged injected node %s into current map as %s", nd.Name, mk)
			}
		}
	}

	c.currentNodes = m
	log.Debugf("updating node map completed")
	return nil
}

func (c *Controller) colorize(base, label string) string {
	// Highlight entries that start with '_' in yellow
	if strings.HasPrefix(base, "_") {
		return "[yellow]" + label + "[-]"
	}
	return label
}

// setStale records whether a node's last refresh failed and, when that changed,
// re-renders its row in place so the list reflects it immediately rather than
// waiting for the next full rebuild.
func (c *Controller) setStale(mapKey string, entry *Node, stale bool) {
	if entry.stale == stale {
		return
	}
	entry.stale = stale
	for i := 0; i < c.view.List.GetItemCount(); i++ {
		if _, sec := c.view.List.GetItemText(i); strings.TrimSpace(sec) == mapKey {
			c.view.List.SetItemText(i, c.rowLabel(entry, displayName(baseOf(entry.node.Name), entry.node.IsDir)), mapKey)
			return
		}
	}
}

// rowLabel renders one list row. A node whose last refresh failed is greyed
// out and tagged, so the list itself shows that what is on screen is cached
// rather than live — the details pane says the same thing at length.
func (c *Controller) rowLabel(entry *Node, display string) string {
	n := entry.node
	prefix := "   "
	if n.IsDir {
		prefix = "📁 "
	}
	label := prefix + display
	if entry.stale {
		return "[gray]" + label + "  (cached)[-]"
	}
	return c.colorize(baseOf(n.Name), label)
}

// orderedEntries returns the map keys and matching display names sorted for
// the list: directories first, then files, each alphabetical by basename.
// Sorting must use the basename, not the "<base>|dir" map key — '|' sorts
// after alphanumerics, so the raw map-key order would put "app2/" before
// "app/" whenever one name is a prefix of another.
func orderedEntries(nodes map[string]*Node) (mks, display []string) {
	type entry struct {
		mk, base string
		isDir    bool
	}
	var dirs, files []entry
	for mk, v := range nodes {
		e := entry{mk: mk, base: baseOf(v.node.Name), isDir: v.node.IsDir}
		if e.isDir {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	byBase := func(s []entry) func(i, j int) bool {
		return func(i, j int) bool { return s[i].base < s[j].base }
	}
	sort.Slice(dirs, byBase(dirs))
	sort.Slice(files, byBase(files))

	all := append(dirs, files...)
	mks = make([]string, 0, len(all))
	display = make([]string, 0, len(all))
	for _, e := range all {
		mks = append(mks, e.mk)
		display = append(display, displayName(e.base, e.isDir))
	}
	return mks, display
}

func (c *Controller) updateList() []string {
	log.Debugf("updating list")
	if err := c.makeNodeMap(); err != nil {
		// A transient listing failure must not kill the session: report it,
		// then fall back to the last directory that listed cleanly.
		c.error("Failed to load "+c.currentDir, err, false)
		if normAbs(c.currentDir) != normAbs(c.lastGoodDir) {
			c.currentDir = c.lastGoodDir
			if err2 := c.makeNodeMap(); err2 != nil {
				c.currentNodes = make(map[string]*Node)
			}
		} else {
			// Nothing to fall back to; show an empty listing rather than
			// stale entries from another directory.
			c.currentNodes = make(map[string]*Node)
		}
	} else {
		c.lastGoodDir = c.currentDir
	}

	c.view.List.Clear()
	c.view.List.SetTitle("[ [::b]" + c.currentDir + "[::-] ]")

	// [..] always on top
	c.view.List.AddItem("[..]", "..", 0, func() {
		c.Up()
	})

	mks, display := orderedEntries(c.currentNodes)
	for idx, mk := range mks {
		entry := c.currentNodes[mk]
		label := c.rowLabel(entry, display[idx])
		if entry.node.IsDir {
			// Use mapKey as secondary text (stable key for actions)
			c.view.List.AddItem(label, mk, 0, func() {
				i := c.view.List.GetCurrentItem()
				_, curMK := c.view.List.GetItemText(i) // secondary text is mapKey
				curMK = strings.TrimSpace(curMK)
				if val, ok := c.currentNodes[curMK]; ok && val.node.IsDir {
					// Save cursor before moving
					c.position[c.currentDir] = c.view.List.GetCurrentItem()
					c.Down(baseOf(val.node.Name))
				}
			})
		} else {
			c.view.List.AddItem(label, mk, 0, func() {
				// no-op; details pane updates via SetChangedFunc
			})
		}
	}

	// Restore cursor position if we saved it before
	if val, ok := c.position[c.currentDir]; ok {
		c.view.List.SetCurrentItem(val)
		delete(c.position, c.currentDir)
	}

	c.ordered = display
	return display
}

func (c *Controller) fillDetails(mapKey string) {
	c.view.Details.Clear()

	val, ok := c.currentNodes[mapKey]
	if !ok {
		return
	}

	n := val.node
	// Re-fetch keys from the server so volatile fields (TTL countdown, value)
	// reflect current state each time the key gets focus instead of the value
	// cached at list time. Fall back to the cached node on any error (e.g.
	// injected entries not yet readable) — but record that we did, because a
	// cached v3 node has no value at all and silently showing one as if it were
	// live is what made B-7 destructive.
	if !n.IsDir {
		refreshErr := error(nil)
		fresh, err := c.model.Get(n.Name)
		switch {
		case err != nil:
			refreshErr = err
		case fresh == nil || fresh.IsDir:
			refreshErr = fmt.Errorf("%s is no longer a key", n.Name)
		default:
			n = fresh
			val.node = fresh
		}
		c.setStale(mapKey, val, refreshErr != nil)
		if refreshErr != nil {
			fmt.Fprintf(c.view.Details, "[red]⚠ cached — could not refresh from the server[-]\n")
			fmt.Fprintf(c.view.Details, "[red]  %s[-]\n", tview.Escape(refreshErr.Error()))
			fmt.Fprintf(c.view.Details, "[red]  values below may be out of date or incomplete[-]\n\n")
		}
	}
	base := baseOf(n.Name)
	parent := parentOf(n.Name)

	fmt.Fprintf(c.view.Details, "[::b]Path info[::-]\n")
	fmt.Fprintf(c.view.Details, "  [green]Type:[-] %s\n", map[bool]string{true: "Directory", false: "Key"}[n.IsDir])
	fmt.Fprintf(c.view.Details, "  [green]Basename:[-] %s\n", base)
	fmt.Fprintf(c.view.Details, "  [green]Parent:[-] %s\n", parent)
	fmt.Fprintf(c.view.Details, "  [green]Full path:[-] %s\n", n.Name)
	fmt.Fprintf(c.view.Details, "  [green]Depth:[-] %d\n", depthOf(n.Name))

	fmt.Fprintf(c.view.Details, "\n[::b]Cluster info[::-]\n")
	fmt.Fprintf(c.view.Details, "  [green]Protocol:[-] %s\n", c.model.ProtocolVersion())
	fmt.Fprintf(c.view.Details, "  [green]Cluster ID:[-] %s\n", n.ClusterId)

	if !n.IsDir {
		size, lines, printable := valueStats(n.Value)

		fmt.Fprintf(c.view.Details, "\n[::b]Value info[::-]\n")
		fmt.Fprintf(c.view.Details, "  [green]Size:[-] %d bytes\n", size)
		fmt.Fprintf(c.view.Details, "  [green]Lines:[-] %d\n", lines)
		fmt.Fprintf(c.view.Details, "  [green]SHA-256:[-] %s\n", shortHash(n.Value))
		if n.TTL > 0 {
			fmt.Fprintf(c.view.Details, "  [green]TTL:[-] %s\n", formatTTL(n.TTL))
		} else {
			fmt.Fprintf(c.view.Details, "  [green]TTL:[-] none\n")
		}

		if n.CreateRev > 0 || n.ModRev > 0 {
			fmt.Fprintf(c.view.Details, "\n[::b]Revision info[::-]\n")
			fmt.Fprintf(c.view.Details, "  [green]Create rev:[-] %d\n", n.CreateRev)
			fmt.Fprintf(c.view.Details, "  [green]Mod rev:[-] %d\n", n.ModRev)
			if n.Version > 0 {
				fmt.Fprintf(c.view.Details, "  [green]Version:[-] %d\n", n.Version)
			}
		}

		const previewLimit = 512
		if printable {
			shown := n.Value
			title := fmt.Sprintf("Preview (%d chars)", previewLimit)
			if pj, ok := prettyJSON(n.Value); ok {
				shown = pj
				title = fmt.Sprintf("Preview (JSON, %d chars)", previewLimit)
			}
			fmt.Fprintf(c.view.Details, "\n[::b]%s[::-]\n", title)
			if cut, truncated := truncateBytes(shown, previewLimit); truncated {
				fmt.Fprintf(c.view.Details, "%s…\n", tview.Escape(cut))
			} else {
				fmt.Fprintf(c.view.Details, "%s\n", tview.Escape(cut))
			}
		} else {
			const hexLimit = 256
			fmt.Fprintf(c.view.Details, "\n[::b]Preview (hex, first %d bytes)[::-]\n", hexLimit)
			fmt.Fprintf(c.view.Details, "%s", tview.Escape(hexDump(n.Value, hexLimit)))
		}
	} else {
		dirPath := normAbs(n.Name)
		if dirPath != "/" && !strings.HasSuffix(dirPath, "/") {
			dirPath = dirPath + "/"
		}

		list, err := c.model.Ls(dirPath)
		if err != nil {
			fmt.Fprintf(c.view.Details, "\n[::b]Directory info[::-]\n")
			fmt.Fprintf(c.view.Details, "  [red]Failed to list children:[-] %s\n", err.Error())
			return
		}

		subdirs := 0
		keys := 0
		seen := make(map[string]struct{}, len(list))
		for _, ch := range list {
			if ch == nil {
				continue
			}
			b := baseOf(ch.Name)
			mk := makeMapKey(b, ch.IsDir)
			if _, ok := seen[mk]; ok {
				continue
			}
			seen[mk] = struct{}{}
			if ch.IsDir {
				subdirs++
			} else {
				keys++
			}
		}

		fmt.Fprintf(c.view.Details, "\n[::b]Directory info[::-]\n")
		fmt.Fprintf(c.view.Details, "  [green]Children:[-] %d\n", subdirs+keys)
		fmt.Fprintf(c.view.Details, "  [green]Subdirs:[-] %d\n", subdirs)
		fmt.Fprintf(c.view.Details, "  [green]Keys:[-] %d\n", keys)
	}

}

// getPosition returns the index of element in slice, or -1 when it is absent.
// Callers must check: treating "missing" as index 0 used to silently move the
// cursor to the first row — a search that matched nothing looked like a hit.
func (c *Controller) getPosition(element string, slice []string) int {
	for k, v := range slice {
		if element == v {
			return k
		}
	}
	return -1
}

// selectRow moves the cursor to the list row showing target (accounting for
// the "[..]" entry at the top). A target that is not in the list leaves the
// cursor where it is.
func (c *Controller) selectRow(target string, ordered []string) {
	if pos := c.getPosition(target, ordered); pos >= 0 {
		c.view.List.SetCurrentItem(pos + 1) // +1 for [..]
	}
}

// guarded runs g.do subject to the session policy: refused outright in
// read-only mode, gated behind a typed confirmation when any target path lies
// under a protected prefix, and run immediately otherwise.
//
// Every mutation funnels through here rather than through a check at the key
// bindings alone, so a path that reaches a write by another route (the
// revision-restore flow, say) is still covered.
func (c *Controller) guarded(g guard) {
	if c.policy.ReadOnly {
		c.refuseReadOnly(g.action)
		return
	}
	for _, p := range g.paths {
		if prefix, ok := c.policy.protects(p); ok {
			c.confirmTyped(
				fmt.Sprintf("%s is protected — type %s to %s (Esc cancels)", prefix, baseOf(prefix), g.action),
				baseOf(prefix), g.do, g.onCancel)
			return
		}
	}
	if g.confirmWord != "" {
		c.confirmTyped(
			fmt.Sprintf("%s — type %s to confirm (Esc cancels)", g.action, g.confirmWord),
			g.confirmWord, g.do, g.onCancel)
		return
	}
	g.do()
}

// refuseReadOnly explains why nothing happened.
func (c *Controller) refuseReadOnly(action string) {
	c.info("Read-only session", fmt.Sprintf(
		"%s is disabled. Restart without -read-only (or set read_only:false) to make changes.", action))
}

// confirmTyped runs do only once want has been typed out verbatim. Used both
// for protected prefixes (where want is the prefix's basename, which keeps the
// prompt honest for bulk writes and names the boundary crossed) and for
// recursive deletes (where it is the directory's own name).
func (c *Controller) confirmTyped(prompt, want string, do, onCancel func()) {
	inp := c.view.NewTypedConfirm(prompt)
	inp.SetDoneFunc(func(key tcell.Key) {
		// Drop this dialog before anything below adds its own: modals share
		// the "modal" page name.
		c.view.Pages.RemovePage("modal")
		if key != tcell.KeyEnter || strings.TrimSpace(inp.GetText()) != want {
			// When a caller takes the screen back — the value editor re-opens
			// with the user's unsaved text — do NOT also queue a notice. The
			// editor swaps the application root, so a modal added to Pages here
			// is hidden behind it and then resurfaces when the editor closes,
			// drawn over a list that already has the focus. The re-opened
			// editor is feedback enough.
			if onCancel != nil {
				onCancel()
				return
			}
			c.info("Cancelled", "not confirmed — nothing was changed")
			return
		}
		do()
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 76, 5), true, true)
}

func (c *Controller) info(header, details string) {
	m := c.view.NewInfoMessageQ(header, details)
	m.SetDoneFunc(func(int, string) {
		c.view.Pages.RemovePage("modal-info")
	})
	c.view.Pages.AddPage("modal-info", c.view.ModalEdit(m, 60, 7), true, true)
}

func (c *Controller) copied(details string) {
	m := c.view.NewCopiedMessageQ(details)
	m.SetDoneFunc(func(int, string) {
		c.view.Pages.RemovePage("modal-info")
	})
	c.view.Pages.AddPage("modal-info", c.view.ModalEdit(m, 60, 7), true, true)
}

func (c *Controller) copyPath() *tcell.EventKey {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	i := c.view.List.GetCurrentItem()
	_, mapKey := c.view.List.GetItemText(i)
	mapKey = strings.TrimSpace(mapKey)

	// Copy current directory when cursor is on "[..]"
	if mapKey == ".." {
		if err := clip.Copy(normAbs(c.currentDir)); err != nil {
			c.error("Clipboard error", err, false)
			return nil
		}
		c.copied("Directory path copied")
		return nil
	}

	val, ok := c.currentNodes[mapKey]
	if !ok || val.node == nil {
		return nil
	}

	text := normAbs(val.node.Name)
	if val.node.IsDir {
		text = text + "/"
	}
	if err := clip.Copy(text); err != nil {
		if errors.Is(err, clip.ErrNoClipboard) {
			c.error("Clipboard error", fmt.Errorf("no clipboard available — use a terminal that supports OSC52 (iTerm2 and most modern terminals), or run inside tmux with allow-passthrough"), false)
			return nil
		}
		c.error("Clipboard error", err, false)
		return nil
	}

	if val.node.IsDir {
		c.copied("Directory path copied")
	} else {
		c.copied("Key path copied")
	}
	return nil
}

func (c *Controller) copyValue() *tcell.EventKey {
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
		c.error("Copy value", fmt.Errorf("selected item is a directory"), false)
		return nil
	}

	if err := clip.Copy(val.node.Value); err != nil {
		c.error("Clipboard error", err, false)
		return nil
	}
	c.copied("Key value copied")
	return nil
}

// mutatingKeys are the list bindings that change cluster state, mapped to the
// verb used in a read-only refusal. Ctrl+E is absent on purpose: it opens the
// value editor, which doubles as the only way to read a value in full, so it
// opens disabled instead of being blocked.
var mutatingKeys = map[tcell.Key]string{
	tcell.KeyCtrlN:  "create",
	tcell.KeyDelete: "delete",
	tcell.KeyCtrlR:  "rename",
	tcell.KeyCtrlT:  "setting a TTL",
	tcell.KeyCtrlD:  "duplicate",
	tcell.KeyCtrlO:  "import",
}

func (c *Controller) setInput() {
	c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlQ:
			c.Stop()
			return nil
		}
		return event
	})
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		// Refuse mutating bindings up front in a read-only session, so the
		// dialog never opens rather than opening and then refusing. The guard
		// inside each handler is still the authority — this only saves the
		// user a pointless form.
		if action, ok := mutatingKeys[event.Key()]; ok && c.policy.ReadOnly {
			c.refuseReadOnly(action)
			return nil
		}
		switch event.Key() {

		case tcell.KeyCtrlN:
			return c.create()
		case tcell.KeyDelete:
			return c.delete()
		case tcell.KeyCtrlE:
			return c.editMultiline()
		case tcell.KeyCtrlR:
			return c.rename()
		case tcell.KeyCtrlT:
			return c.setTTL()
		case tcell.KeyCtrlV:
			return c.history()
		case tcell.KeyCtrlP:
			return c.copyPath()
		case tcell.KeyCtrlY:
			return c.copyValue()
		case tcell.KeyCtrlS:
			return c.search()
		case tcell.KeyCtrlF:
			return c.findRecursive()
		case tcell.KeyCtrlD:
			return c.duplicate()
		case tcell.KeyCtrlJ:
			return c.jump()
		case tcell.KeyCtrlW:
			return c.export()
		case tcell.KeyCtrlO:
			return c.importJSON()
		case tcell.KeyCtrlA:
			return c.showJournal()
		case tcell.KeyCtrlH:
			help := c.view.NewHotkeysModal()

			help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-help")
				return nil
			})

			c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 70, 30), true, true)
			return nil

		case tcell.KeyBackspace2:
			c.Up()
			return nil

		case tcell.KeyRune:
			switch event.Rune() {
			case '/':
				return c.search()
			}
		}
		return event
	})
}

func (c *Controller) Down(cur string) {
	var newDir string
	if c.currentDir == "/" {
		newDir = "/" + strings.TrimPrefix(cur, "/") + "/"
	} else {
		newDir = strings.TrimSuffix(c.currentDir, "/") + "/" + strings.TrimPrefix(cur, "/") + "/"
	}
	log.Debugf("command: down - current dir: %s, new dir: %s", c.currentDir, newDir)
	c.currentDir = newDir
	c.updateList()
}

func (c *Controller) Up() {
	fields := strings.FieldsFunc(strings.TrimSpace(c.currentDir), splitFunc)
	if len(fields) == 0 {
		return
	}
	newDir := "/" + strings.Join(fields[:len(fields)-1], "/")
	if len(fields) > 1 {
		newDir = newDir + "/"
	}

	log.Debugf("command: up - current dir: %s, new dir: %s", c.currentDir, newDir)
	c.currentDir = newDir
	c.updateList()
}

func (c *Controller) Stop() {
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) Run() error {
	if c.startupErr != nil {
		c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyCtrlQ {
				c.Stop()
				return nil
			}
			return event
		})

		msg := c.startupErr.Error()
		header := "Connection error"

		// Friendly hint for the exact issue you reported
		if strings.Contains(msg, "user name is empty") ||
			strings.Contains(msg, "password is set but username is empty") {
			header = "Auth configuration error"
			msg = msg + "\n\nFix:\n- set --username (and --password)\n- or put username/password in config.json\n- or remove password if auth is disabled"
		}

		c.error(header, fmt.Errorf("%s", msg), true)
		return c.view.App.Run()
	}

	// Normal flow
	c.view.List.SetChangedFunc(func(i int, main string, secondary string, _ rune) {
		curMK := strings.TrimSpace(secondary) // mapKey
		c.fillDetails(curMK)
	})
	c.updateList()
	c.setInput()
	return c.view.App.Run()
}

func (c *Controller) search() *tcell.EventKey {
	search := c.view.NewSearch()

	// Use the exact ordering the visible list was built with. Recomputing it
	// here used to sort differently from updateList, landing the cursor on
	// the wrong row whenever one name was a prefix of another.
	ordered := c.ordered

	search.SetDoneFunc(func(key tcell.Key) {
		oldPos := c.view.List.GetCurrentItem()
		value := strings.TrimSpace(search.GetText())
		pos := c.getPosition(value, ordered)
		// +1 because of the top "[..]" entry. A miss (pos < 0) leaves the
		// cursor alone rather than yanking it to the first row.
		if pos >= 0 && pos+1 != oldPos && key == tcell.KeyEnter {
			c.view.List.SetCurrentItem(pos + 1)
		}
		c.view.Pages.RemovePage("modal")
	})

	search.SetAutocompleteFunc(func(currentText string) []string {
		prefix := strings.TrimSpace(strings.ToLower(currentText))
		if prefix == "" {
			return nil
		}
		result := make([]string, 0, len(ordered))
		for _, word := range ordered {
			if strings.HasPrefix(strings.ToLower(word), prefix) {
				result = append(result, word)
			}
		}
		return result
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(search, 60, 5), true, true)
	return nil
}

func (c *Controller) delete() *tcell.EventKey {
	if c.view.List.GetItemCount() == 0 {
		return nil
	}
	var err error
	i := c.view.List.GetCurrentItem()
	_, mapKey := c.view.List.GetItemText(i) // secondary text is mapKey
	mapKey = strings.TrimSpace(mapKey)

	if mapKey == ".." {
		return nil
	}

	val, ok := c.currentNodes[mapKey]
	if !ok {
		return nil
	}

	doDelete := func() {
		if !val.node.IsDir {
			err = c.model.Del(val.node.Name)
		} else {
			err = c.model.DelDir(val.node.Name)
		}
		if err != nil {
			c.error("Error deleting node", err, false)
			return
		}
		// Remove from injected cache if present
		c.removeWritten(val.node)
		c.view.Details.Clear()
		c.updateList()
	}

	// A recursive delete is the most destructive thing this tool can do — a
	// single DeleteRange over a fat prefix, with no undo. It gets a
	// type-the-name confirmation rather than a bare ok/cancel that a stray
	// Enter can dismiss. A protected path supersedes the word (see guarded),
	// so the user is never asked to type two different things for one delete.
	if val.node.IsDir {
		c.guarded(guard{
			action:      fmt.Sprintf("delete %s and everything under it", val.node.Name),
			paths:       []string{val.node.Name},
			confirmWord: baseOf(val.node.Name),
			do:          doDelete,
		})
		return nil
	}

	delQ := c.view.NewDeleteQ(displayName(baseOf(val.node.Name), false))
	delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		// Drop the confirm dialog first: any modal added below (error or
		// via updateList) shares the "modal" page name and would be
		// clobbered by a later remove.
		c.view.Pages.RemovePage("modal")
		if buttonLabel != "ok" {
			return
		}
		c.guarded(guard{action: "delete", paths: []string{val.node.Name}, do: doDelete})
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(delQ, 20, 7), true, true)
	return nil
}

func (c *Controller) create() *tcell.EventKey {
	createForm := c.view.NewCreateForm(fmt.Sprintf("Create Node: %s", c.currentDir))
	createForm.AddButton("Save", func() {
		node := strings.TrimSpace(createForm.GetFormItem(0).(*tview.InputField).GetText())
		value := createForm.GetFormItem(1).(*tview.InputField).GetText()
		isDir := createForm.GetFormItem(2).(*tview.Checkbox).IsChecked()
		c.view.Pages.RemovePage("modal")
		if node == "" || strings.Contains(node, "/") {
			c.error("Invalid name", fmt.Errorf("name must be non-empty and must not contain '/'"), false)
			return
		}
		if model.IsReservedName(node) {
			c.error("Invalid name", fmt.Errorf("%q is reserved for internal use", node), false)
			return
		}
		log.Debugf("Creating Node: name: %s, isDir: %t, value: %s", node, isDir, value)
		full := normAbs(c.currentDir + node)
		c.guarded(guard{action: "create", paths: []string{full}, do: func() {
			// Refuse to overwrite an existing key or directory silently — the
			// same guard rename applies.
			if nd, gerr := c.model.Get(full); gerr == nil && nd != nil {
				c.error("Target exists", fmt.Errorf("%q already exists; choose a different name", full), false)
				return
			}
			var err error
			if !isDir {
				err = c.model.Set(full, value)
			} else {
				err = c.model.MkDir(full)
			}
			if err != nil {
				c.error("Error creating node", err, false)
				return
			}
			// If underscore-prefixed, inject so it shows even in v2
			if strings.HasPrefix(node, "_") {
				nd := &model.Node{Name: full, IsDir: isDir, Value: value}
				c.injectWritten(nd)
			}
			ordered := c.updateList()
			target := node
			if isDir {
				target = node + "/"
			}
			c.selectRow(target, ordered)
		}})
	})
	createForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(createForm, 60, 11), true, true)
	return nil
}

func (c *Controller) rename() *tcell.EventKey {
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
	if !ok {
		return nil
	}

	curBase := baseOf(val.node.Name)

	title := fmt.Sprintf("Rename key: %s", val.node.Name)
	if val.node.IsDir {
		title = fmt.Sprintf("Rename folder: %s", val.node.Name)
	}

	renameForm := c.view.NewEditValueForm(title, curBase)
	renameForm.AddButton("Save", func() {
		newName := strings.TrimSpace(renameForm.GetFormItem(0).(*tview.InputField).GetText())
		c.view.Pages.RemovePage("modal")
		if newName == "" || strings.Contains(newName, "/") {
			c.error("Invalid name", fmt.Errorf("name must be non-empty and must not contain '/'"), false)
			return
		}
		oldPath := val.node.Name
		newPath := normAbs(c.currentDir + newName)
		if newPath == oldPath {
			return
		}
		// A rename both removes the old path and writes the new one, so either
		// side being protected gates the whole operation.
		c.guarded(guard{action: "rename", paths: []string{oldPath, newPath}, do: func() {
			// Refuse to overwrite an existing key or directory silently.
			if nd, err := c.model.Get(newPath); err == nil && nd != nil {
				c.error("Target exists", fmt.Errorf("%q already exists; choose a different name", newPath), false)
				return
			}
			var err error
			if val.node.IsDir {
				log.Debugf("Renaming directory: %s -> %s", oldPath, newPath)
				err = c.model.RenameDir(oldPath, newPath)
			} else {
				log.Debugf("Renaming key: %s -> %s", oldPath, newPath)
				err = c.model.RenameKey(oldPath, newPath)
			}
			if err != nil {
				c.error("Failed to rename", err, false)
				return
			}
			// Update injected cache if underscore-prefixed names are involved
			if strings.HasPrefix(curBase, "_") || strings.HasPrefix(newName, "_") {
				c.reinjectWritten(oldPath, newPath, val.node.IsDir, val.node.ClusterId, val.node.Value)
			}
			ordered := c.updateList()
			target := newName
			if val.node.IsDir {
				target = newName + "/"
			}
			c.selectRow(target, ordered)
		}})
	})
	renameForm.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(renameForm, 60, 7), true, true)
	return nil
}

// setTTL prompts for a time-to-live (in seconds) for the selected key and
// applies it, re-writing the key with the new lease. Entering 0 clears any
// existing expiry. Directories are not supported.
func (c *Controller) setTTL() *tcell.EventKey {
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
		c.error("Set TTL", fmt.Errorf("TTL can only be set on keys, not directories"), false)
		return nil
	}

	cur := ""
	if val.node.TTL > 0 {
		cur = strconv.FormatInt(val.node.TTL, 10)
	}

	form := c.view.NewTTLForm(fmt.Sprintf("Set TTL: %s", val.node.Name), cur)
	form.AddButton("Save", func() {
		raw := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		c.view.Pages.RemovePage("modal")
		if raw == "" {
			raw = "0"
		}
		secs, perr := parseTTLInput(raw)
		if perr != nil {
			c.error("Invalid TTL", perr, false)
			return
		}
		// SetTTL rewrites the key, so it needs the *current* value: the cached
		// one is empty on v3 until fillDetails fetches it, and stale after any
		// concurrent write. Re-read immediately before the write — this also
		// stops a TTL change from resurrecting a key deleted in the meantime.
		fresh, gerr := c.model.Get(val.node.Name)
		if gerr != nil {
			c.error("Failed to set TTL", fmt.Errorf("re-reading %s: %w", val.node.Name, gerr), false)
			return
		}
		if fresh == nil || fresh.IsDir {
			c.error("Failed to set TTL", fmt.Errorf("%s is no longer a key", val.node.Name), false)
			return
		}
		val.node = fresh
		c.guarded(guard{action: "set TTL", paths: []string{val.node.Name}, do: func() {
			if err := c.model.SetTTL(val.node.Name, val.node.Value, secs); err != nil {
				c.error("Failed to set TTL", err, false)
				return
			}
			if strings.HasPrefix(baseOf(val.node.Name), "_") {
				nd := &model.Node{Name: val.node.Name, IsDir: false, Value: val.node.Value, ClusterId: val.node.ClusterId, TTL: secs}
				c.injectWritten(nd)
			}
			ordered := c.updateList()
			target := displayName(baseOf(val.node.Name), false)
			c.selectRow(target, ordered)
			c.fillDetails(mapKey)
		}})
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 65, 7), true, true)
	return nil
}

func (c *Controller) editMultiline() *tcell.EventKey {
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
	if !ok {
		return nil
	}
	if val.node.IsDir {
		// Directories have no value to edit; Ctrl+E falls through to the
		// same rename dialog Ctrl+R opens.
		return c.rename()
	}

	// Never edit the cached value: v3 listings are keys-only, so a node starts
	// out with an empty Value, and fillDetails' on-focus refresh swallows its
	// own Get error. Opening the editor on that empty string and saving would
	// silently wipe the key, so re-read it here and refuse to edit what we
	// cannot read.
	fresh, gerr := c.model.Get(val.node.Name)
	if gerr != nil {
		c.error("Cannot edit", fmt.Errorf("re-reading %s: %w", val.node.Name, gerr), false)
		return nil
	}
	if fresh == nil || fresh.IsDir {
		c.error("Cannot edit", fmt.Errorf("%s is no longer a key", val.node.Name), false)
		return nil
	}
	val.node = fresh

	c.openValueEditor(val, val.node.Value)
	return nil
}

// openValueEditor puts the full-screen value editor on screen, seeded with
// initial. It is re-entrant so a declined protected-path confirmation can hand
// the user back exactly what they had typed instead of discarding it.
//
// In a read-only session the editor still opens — it is the only way to read a
// value past the details pane's preview cap — but disabled, and saving is
// refused by the policy guard regardless.
func (c *Controller) openValueEditor(val *Node, initial string) {
	title := fmt.Sprintf(" Edit (multiline): %s ", val.node.Name)
	if c.policy.ReadOnly {
		title = fmt.Sprintf(" View (read-only): %s ", val.node.Name)
	}
	ta := c.view.NewMultilineEditor(title, initial)
	if c.policy.ReadOnly {
		ta.SetDisabled(true)
	}

	// Inside editor:
	//   Ctrl+S = save
	//   Esc / Ctrl+Q = cancel
	ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyCtrlS:
			value := ta.GetText()
			log.Debugf("Multiline save: %s (%d bytes)", val.node.Name, len(value))
			// Leave the editor before the guard: a confirmation modal cannot
			// show over the full-screen editor root.
			c.view.CloseEditor()
			c.guarded(guard{
				action: "save",
				paths:  []string{val.node.Name},
				do: func() {
					// Preserve any expiry: SetKeepTTL re-attaches the v3 lease / re-applies
					// the v2 TTL so editing a key's value no longer silently drops it.
					if err := c.model.SetKeepTTL(val.node.Name, value, val.node.LeaseID, val.node.TTL); err != nil {
						c.error("Failed to save value", err, false)
						return
					}
					if strings.HasPrefix(baseOf(val.node.Name), "_") {
						nd := &model.Node{Name: val.node.Name, IsDir: false, Value: value, ClusterId: val.node.ClusterId, TTL: val.node.TTL, LeaseID: val.node.LeaseID}
						c.injectWritten(nd)
					}
					ordered := c.updateList()
					c.selectRow(displayName(baseOf(val.node.Name), false), ordered)
					// Refresh details panel with mapKey
					i := c.view.List.GetCurrentItem()
					_, mk := c.view.List.GetItemText(i)
					c.fillDetails(strings.TrimSpace(mk))
				},
				// Declining the confirmation must not cost the user their edit.
				onCancel: func() { c.openValueEditor(val, value) },
			})
			return nil

		case tcell.KeyEsc, tcell.KeyCtrlQ:
			c.view.CloseEditor()
			return nil
		}
		return ev
	})

	c.view.OpenEditor(ta)
}

// export prompts for a filename and writes every non-directory key under the
// current directory — the whole subtree, not just this level — to a JSON file
// as {"key": "value", ...}. Directory markers are omitted, so the result
// round-trips through importJSON.
func (c *Controller) export() *tcell.EventKey {
	defaultPath := "export.json"
	if home, err := os.UserHomeDir(); err == nil {
		defaultPath = home + "/export.json"
	}
	inp := c.view.NewExportInput(c.currentDir, defaultPath)
	inp.SetDoneFunc(func(key tcell.Key) {
		// Remove the input first: error modals below reuse the "modal" page
		// name, and a deferred remove would dismiss them instantly.
		c.view.Pages.RemovePage("modal")
		if key != tcell.KeyEnter {
			return
		}
		filename := strings.TrimSpace(inp.GetText())
		if filename == "" {
			return
		}
		// The default path is a fixed ~/export.json, so a second export is far
		// more likely to land on a real file than not. Ask before replacing it.
		c.confirmOverwrite(filename, "export", c.writeExport)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 60, 5), true, true)
	return nil
}

// confirmOverwrite guards any local file write that would replace something
// already on disk, running write only once the user agrees. Both the key
// export and the journal export go through it.
func (c *Controller) confirmOverwrite(filename, what string, write func(string)) {
	if _, err := os.Stat(filename); err != nil {
		write(filename) // nothing there (or unreadable — let the write report it)
		return
	}
	q := c.view.NewOverwriteQ(fmt.Sprintf("%s already exists.\nReplace it with this %s?", filename, what))
	q.SetDoneFunc(func(_ int, label string) {
		c.view.Pages.RemovePage("modal")
		if label != "overwrite" {
			return
		}
		write(filename)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(q, 60, 9), true, true)
}

// writeExport dumps the current subtree to filename as JSON.
func (c *Controller) writeExport(filename string) {
	data, err := c.model.Export(c.currentDir)
	if err != nil {
		c.error("Export failed", err, false)
		return
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		c.error("Export failed", err, false)
		return
	}
	if err := os.WriteFile(filename, raw, 0o600); err != nil {
		c.error("Cannot write file", fmt.Errorf("%s: %w", filename, err), false)
		return
	}
	c.info("Exported", fmt.Sprintf("Saved %d keys to %s", len(data), filename))
}

// resolveImportKey turns a key from an imported file into an absolute etcd
// path. Absolute keys (leading '/') are used as-is; relative keys are joined
// onto the current directory.
func (c *Controller) resolveImportKey(k string) string {
	k = strings.TrimSpace(k)
	if strings.HasPrefix(k, "/") {
		return normAbs(k)
	}
	cur := normAbs(c.currentDir)
	if cur == "/" {
		return normAbs("/" + k)
	}
	return normAbs(cur + "/" + k)
}

// importJSON opens a small filesystem browser to pick a JSON file.
func (c *Controller) importJSON() *tcell.EventKey {
	start, err := os.UserHomeDir()
	if err != nil || start == "" {
		start = "/"
	}

	browser := c.view.NewFileBrowser(" Select JSON file ")

	var render func(dir string)
	render = func(dir string) {
		dir = filepath.Clean(dir)
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			c.view.Pages.RemovePage("modal")
			c.error("Cannot open directory", fmt.Errorf("%s: %w", dir, rerr), false)
			return
		}
		browser.Clear()
		browser.SetTitle(fmt.Sprintf(" Select JSON file — %s ", dir))

		parent := filepath.Dir(dir)
		browser.AddItem("[..]", parent, 0, func() { render(parent) })

		type ent struct {
			name string
			path string
		}
		var dirs, files []ent
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			if e.IsDir() {
				dirs = append(dirs, ent{name, full})
			} else if strings.HasSuffix(strings.ToLower(name), ".json") {
				files = append(files, ent{name, full})
			}
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].name < dirs[j].name })
		sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

		for _, d := range dirs {
			p := d.path
			browser.AddItem("📁 "+d.name+"/", p, 0, func() { render(p) })
		}
		for _, f := range files {
			p := f.path
			browser.AddItem("   "+f.name, p, 0, func() { c.promptImportMode(p) })
		}
	}

	browser.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("modal")
			return nil
		}
		return ev
	})

	render(start)
	c.view.Pages.AddPage("modal", c.view.ModalEdit(browser, 70, 20), true, true)
	return nil
}

// promptImportMode parses the chosen file and asks how to treat existing keys.
func (c *Controller) promptImportMode(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		c.view.Pages.RemovePage("modal")
		c.error("Cannot read file", fmt.Errorf("%s: %w", path, err), false)
		return
	}
	var items map[string]string
	if err := json.Unmarshal(data, &items); err != nil {
		c.view.Pages.RemovePage("modal")
		c.error("Invalid JSON", fmt.Errorf("%s: %w", filepath.Base(path), err), false)
		return
	}
	if len(items) == 0 {
		c.view.Pages.RemovePage("modal")
		c.info("Import", "No keys found in file")
		return
	}

	q := c.view.NewImportModeQ(fmt.Sprintf("Import %d keys from %s into %s ?",
		len(items), filepath.Base(path), c.currentDir))
	q.SetDoneFunc(func(_ int, label string) {
		if label == "" || label == "cancel" {
			c.view.Pages.RemovePage("modal")
			return
		}
		overwrite := label == "overwrite"
		resolved := make(map[string]string, len(items))
		targets := make([]string, 0, len(items))
		for k, val := range items {
			key := c.resolveImportKey(k)
			resolved[key] = val
			targets = append(targets, key)
		}
		c.view.Pages.RemovePage("modal")
		// One import can touch many keys; a single protected target gates the
		// whole batch, which is why the confirmation names the prefix rather
		// than any one key.
		c.guarded(guard{action: "import", paths: targets, do: func() {
			written, skipped, ierr := c.model.Import(resolved, overwrite)
			if ierr != nil {
				c.error("Import failed", ierr, false)
				return
			}
			c.updateList()
			if c.policy.DryRun {
				c.info("Import (dry run)", fmt.Sprintf(
					"%d keys would be written — nothing was sent to the cluster.\nPress Ctrl+A to review the journal.", written))
				return
			}
			c.info("Imported", fmt.Sprintf("%d written, %d skipped", written, skipped))
		}})
	})
	c.view.Pages.RemovePage("modal")
	c.view.Pages.AddPage("modal", c.view.ModalEdit(q, 60, 9), true, true)
}

func (c *Controller) error(header string, err error, fatal bool) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
		if fatal {
			c.view.App.Stop()
		}
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 60, 7), true, true)
}

func valueStats(v string) (bytes int, lines int, printable bool) {
	bytes = len(v)
	printable = utf8.ValidString(v)
	if v == "" {
		return bytes, 0, printable // an empty value has no lines, not one
	}
	lines = strings.Count(v, "\n") + 1
	return
}

// parseTTLInput accepts either a bare non-negative number of seconds ("3600")
// or a Go duration string ("1h30m", "90m", "45s") and returns the TTL in whole
// seconds. The caller treats 0 (and empty input) as "no expiry". A duration
// that rounds below one second is rejected so a typo like "500ms" does not
// silently clear the TTL.
func parseTTLInput(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	invalid := fmt.Errorf("enter a non-negative number of seconds, or a duration like 1h30m (0 = no expiry)")
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs < 0 {
			return 0, invalid
		}
		return secs, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || (d > 0 && d < time.Second) {
		return 0, invalid
	}
	return int64(d / time.Second), nil
}

// formatTTL renders a remaining lifetime in seconds as "1h2m3s (3723s)".
func formatTTL(secs int64) string {
	if secs <= 0 {
		return "none"
	}
	d := time.Duration(secs) * time.Second
	var b strings.Builder
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	fmt.Fprintf(&b, "%ds", d/time.Second)
	return fmt.Sprintf("%s (%ds)", b.String(), secs)
}

// truncateBytes cuts s to at most max bytes without splitting a UTF-8 rune —
// a naive s[:max] can leave a partial sequence that renders as a replacement
// glyph. Returns the prefix and whether anything was dropped.
func truncateBytes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

func shortHash(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:8])
}

// prettyJSON re-indents v when it is a JSON object or array. Scalars are
// reported as non-JSON so plain numbers/strings keep their raw preview.
func prettyJSON(v string) (string, bool) {
	t := strings.TrimSpace(v)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(t), "", "  "); err != nil {
		return "", false
	}
	return buf.String(), true
}

// hexDump renders up to max bytes of v xxd-style: offset, 16 hex bytes and a
// printable-ASCII gutter per row.
func hexDump(v string, max int) string {
	b := []byte(v)
	if len(b) > max {
		b = b[:max]
	}
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := off + 16
		if end > len(b) {
			end = len(b)
		}
		row := b[off:end]
		fmt.Fprintf(&sb, "%08x  ", off)
		for i := 0; i < 16; i++ {
			if i < len(row) {
				fmt.Fprintf(&sb, "%02x ", row[i])
			} else {
				sb.WriteString("   ")
			}
			if i == 7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for _, ch := range row {
			if ch >= 32 && ch < 127 {
				sb.WriteByte(ch)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

func depthOf(path string) int {
	if path == "/" {
		return 0
	}
	return strings.Count(strings.Trim(path, "/"), "/") + 1
}

func (c *Controller) jump() *tcell.EventKey {
	inp := c.view.NewJump()
	inp.SetDoneFunc(func(key tcell.Key) {
		// Remove the input first: error modals below reuse the "modal" page
		// name, and a deferred remove would dismiss them instantly.
		c.view.Pages.RemovePage("modal")
		if key != tcell.KeyEnter {
			return
		}

		raw := strings.TrimSpace(inp.GetText())
		if raw == "" {
			return
		}

		isDirHint := strings.HasSuffix(raw, "/")
		var target string
		if strings.HasPrefix(raw, "/") {
			target = normAbs(raw)
		} else {
			cur := normAbs(c.currentDir)
			if cur != "/" {
				target = normAbs(cur + "/" + raw)
			} else {
				target = normAbs("/" + raw)
			}
		}

		nd, err := c.model.Get(target)
		if err != nil {
			c.error("Not found", fmt.Errorf("%s", target), false)
			return
		}

		if isDirHint && !nd.IsDir {
			c.error("Not a folder", fmt.Errorf("%s", target), false)
			return
		}

		c.navigateTo(nd)
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 60, 5), true, true)
	return nil
}

// navigateTo moves the browser to a node: into it when it is a directory,
// otherwise to its parent with the cursor on the key itself.
func (c *Controller) navigateTo(nd *model.Node) {
	// Only inject what a listing genuinely hides — etcd v2 omits
	// underscore-prefixed keys, which is the sole reason the injected cache
	// exists. Injecting every jump/find target left the entry in the cache for
	// the rest of the session, so a key deleted server-side afterwards kept
	// showing up as a ghost row that no longer resolved.
	if strings.HasPrefix(baseOf(nd.Name), "_") {
		c.injectNode(nd)
	}

	if nd.IsDir {
		dir := normAbs(nd.Name)
		if dir != "/" {
			dir += "/"
		}
		c.currentDir = dir
		c.updateList()
		return
	}

	parent := parentOf(nd.Name)
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	c.currentDir = parent
	ordered := c.updateList()

	base := baseOf(nd.Name)
	for i, v := range ordered {
		if v == base || v == base+"/" {
			c.view.List.SetCurrentItem(i + 1) // +1 for [..]
			idx := c.view.List.GetCurrentItem()
			_, mk := c.view.List.GetItemText(idx)
			c.fillDetails(strings.TrimSpace(mk))
			return
		}
	}
	c.error("Not found", fmt.Errorf("%s", nd.Name), false)
}

// searchLimit caps recursive-search results so a broad query on a huge tree
// stays responsive; the picker title says when the cap was hit.
const searchLimit = 500

// findRecursive prompts for a substring and searches every key under the
// current directory (paths, optionally values too), then shows the matches
// in a picker; choosing one navigates to it.
func (c *Controller) findRecursive() *tcell.EventKey {
	form := c.view.NewFindForm(fmt.Sprintf("Find under %s", c.currentDir))
	form.AddButton("Search", func() {
		query := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		inValues := form.GetFormItem(1).(*tview.Checkbox).IsChecked()
		c.view.Pages.RemovePage("modal")
		if query == "" {
			return
		}
		nodes, truncated, err := c.model.Search(c.currentDir, query, inValues, searchLimit)
		if err != nil {
			c.error("Search failed", err, false)
			return
		}
		if len(nodes) == 0 {
			c.info("Find", fmt.Sprintf("No matches for %q under %s", query, c.currentDir))
			return
		}
		c.showFindResults(query, nodes, truncated)
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 60, 9), true, true)
	return nil
}

func (c *Controller) showFindResults(query string, nodes []*model.Node, truncated bool) {
	title := fmt.Sprintf(" %d matches for %q ", len(nodes), query)
	if truncated {
		title = fmt.Sprintf(" first %d matches for %q (limit reached) ", len(nodes), query)
	}
	results := c.view.NewResultsList(title)
	for _, nd := range nodes {
		nd := nd
		results.AddItem(tview.Escape(nd.Name), "", 0, func() {
			c.view.Pages.RemovePage("modal")
			c.navigateTo(nd)
		})
	}
	results.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEsc {
			c.view.Pages.RemovePage("modal")
			return nil
		}
		return ev
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(results, 76, 20), true, true)
}

// duplicate copies the selected key or directory to a new path entered by
// the user (absolute, or relative to the current directory). The source is
// left untouched; TTLs travel with the copy.
func (c *Controller) duplicate() *tcell.EventKey {
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

	src := normAbs(val.node.Name)
	title := fmt.Sprintf("Copy key: %s", src)
	if val.node.IsDir {
		title = fmt.Sprintf("Copy folder: %s", src)
	}
	form := c.view.NewEditValueForm(title, src+"-copy")
	form.AddButton("Save", func() {
		raw := strings.TrimSpace(form.GetFormItem(0).(*tview.InputField).GetText())
		c.view.Pages.RemovePage("modal")
		if raw == "" {
			c.error("Invalid target", fmt.Errorf("enter a target path (absolute, or relative to %s)", c.currentDir), false)
			return
		}
		var dst string
		if strings.HasPrefix(raw, "/") {
			dst = normAbs(raw)
		} else {
			dst = normAbs(c.currentDir + raw)
		}
		if dst == "/" || dst == src {
			c.error("Invalid target", fmt.Errorf("cannot copy %s onto itself", src), false)
			return
		}
		// Only the destination is written; reading from a protected prefix is
		// harmless, so the source does not gate the copy.
		c.guarded(guard{action: "copy", paths: []string{dst}, do: func() {
			if nd, gerr := c.model.Get(dst); gerr == nil && nd != nil {
				c.error("Target exists", fmt.Errorf("%q already exists; choose a different name", dst), false)
				return
			}
			var err error
			if val.node.IsDir {
				err = c.model.CopyDir(src, dst)
			} else {
				err = c.model.CopyKey(src, dst)
			}
			if err != nil {
				c.error("Failed to copy", err, false)
				return
			}
			if strings.HasPrefix(baseOf(dst), "_") {
				c.injectWritten(&model.Node{Name: dst, IsDir: val.node.IsDir, Value: val.node.Value, ClusterId: val.node.ClusterId})
			}
			ordered := c.updateList()
			if parentOf(dst) == normAbs(c.currentDir) {
				target := displayName(baseOf(dst), val.node.IsDir)
				c.selectRow(target, ordered)
			} else {
				c.info(c.writeHeader("Copied"), fmt.Sprintf("%s → %s", src, dst))
			}
		}})
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 60, 7), true, true)
	return nil
}

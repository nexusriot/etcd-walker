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
	Export(dir string) (map[string]string, error)
	Import(items map[string]string, overwrite bool) (written, skipped int, err error)
}

type Controller struct {
	debug        bool
	view         *view.View
	model        modelAPI
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
}

func splitFunc(r rune) bool { return r == '/' }

func NewController(opts model.Options, debug bool) *Controller {
	m, err := model.NewModel(opts)

	v := view.NewView()

	headerProto := opts.Protocol
	auth := "?"
	if err == nil && m != nil {
		headerProto = m.ProtocolVersion()
		auth = m.AuthLabel()
	}

	tlsTag := ""
	if opts.TLSEnabled {
		tlsTag = " [TLS]"
	}

	v.Frame.AddText(
		fmt.Sprintf("Etcd-walker v.0.7.0 (on %s:%s%s)  –  protocol: %s  |  Auth: %s",
			opts.Host, opts.Port, tlsTag, headerProto, auth),
		true, tview.AlignCenter, tcell.ColorGreen,
	)

	controller := &Controller{
		debug:       debug,
		view:        v,
		currentDir:  "/",
		lastGoodDir: "/",
		position:    make(map[string]int),
		injected:    make(map[string]map[string]*model.Node),
		startupErr:  err,
	}
	// Assign only a real model: a nil *model.Model stored in the interface
	// would read as non-nil. On startup error Run() never touches the model.
	if m != nil {
		controller.model = m
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

func (c *Controller) makeNodeMap() error {
	log.Debugf("updating node map started")
	m := make(map[string]*Node)

	// Model-provided listing
	list, err := c.model.Ls(c.currentDir)
	if err != nil {
		return err
	}
	for _, n := range list {
		rawName := n.Name
		fields := strings.FieldsFunc(strings.TrimSpace(rawName), splitFunc)
		base := fields[len(fields)-1]
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

func (c *Controller) colorize(base string, isDir bool, label string) string {
	// Highlight entries that start with '_' in yellow
	if strings.HasPrefix(base, "_") {
		return "[yellow]" + label + "[-]"
	}
	return label
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
		n := c.currentNodes[mk].node
		base := baseOf(n.Name)
		if n.IsDir {
			label := c.colorize(base, true, "📁 "+display[idx])
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
			label := c.colorize(base, false, "   "+display[idx])
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
	// injected entries not yet readable).
	if !n.IsDir {
		if fresh, err := c.model.Get(n.Name); err == nil && fresh != nil && !fresh.IsDir {
			n = fresh
			val.node = fresh
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
			if len(shown) > previewLimit {
				fmt.Fprintf(c.view.Details, "%s…\n", tview.Escape(shown[:previewLimit]))
			} else {
				fmt.Fprintf(c.view.Details, "%s\n", tview.Escape(shown))
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

func (c *Controller) getPosition(element string, slice []string) int {
	for k, v := range slice {
		if element == v {
			return k
		}
	}
	return 0
}

func (c *Controller) info(header, details string) {
	m := c.view.NewInfoMessageQ(header, details)
	m.SetDoneFunc(func(int, string) {
		c.view.Pages.RemovePage("modal-info")
	})
	c.view.Pages.AddPage("modal-info", c.view.ModalEdit(m, 60, 7), true, true)
}

func (c *Controller) copied(header, details string) {
	m := c.view.NewCopiedMessageQ(header, details)
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
		c.copied("Copied", "Directory path copied")
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
			c.error("Clipboard error", fmt.Errorf("No clipboard available. Tip: use a terminal that supports OSC52 (iTerm2, many modern terminals), or run inside tmux with allow-passthrough"), false)
			return nil
		}
		c.error("Clipboard error", err, false)
		return nil
	}

	if val.node.IsDir {
		c.copied("Copied", "Directory path copied")
	} else {
		c.copied("Copied", "Key path copied")
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
	c.copied("Copied", "Key value copied")
	return nil
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
	c.Cd(c.currentDir)
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
	c.Cd(c.currentDir)
}

func (c *Controller) Cd(path string) { c.updateList() }

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
		// +1 because of the top "[..]" entry
		if pos+1 != oldPos && key == tcell.KeyEnter {
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

	if val, ok := c.currentNodes[mapKey]; ok {
		elem := displayName(baseOf(val.node.Name), val.node.IsDir)
		if val.node.IsDir {
			elem = elem + " (recursive)"
		}
		delQ := c.view.NewDeleteQ(elem)
		delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			// Drop the confirm dialog first: any modal added below (error or
			// via updateList) shares the "modal" page name and would be
			// clobbered by a later remove.
			c.view.Pages.RemovePage("modal")
			if buttonLabel != "ok" {
				return
			}
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
			c.removeInjected(val.node)
			c.view.Details.Clear()
			c.updateList()
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(delQ, 20, 7), true, true)
	}
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
			c.injectNode(nd)
		}
		ordered := c.updateList()
		target := node
		if isDir {
			target = node + "/"
		}
		c.view.List.SetCurrentItem(c.getPosition(target, ordered) + 1) // +1 for [..]
	})
	createForm.AddButton("Quit", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(createForm, 60, 11), true, true)
	return nil
}

func (c *Controller) edit() *tcell.EventKey {
	var err error
	pos := 0
	i := c.view.List.GetCurrentItem()
	_, mapKey := c.view.List.GetItemText(i) // secondary is mapKey
	mapKey = strings.TrimSpace(mapKey)

	if mapKey == ".." {
		return nil
	}

	if val, ok := c.currentNodes[mapKey]; ok {
		// Edit file (value)
		if !val.node.IsDir {
			editValueForm := c.view.NewEditValueForm(fmt.Sprintf("Edit: %s", val.node.Name), val.node.Value)
			editValueForm.AddButton("Save", func() {
				value := editValueForm.GetFormItem(0).(*tview.InputField).GetText()
				c.view.Pages.RemovePage("modal")
				log.Debugf("Editing Node Value: name: %s, value: %s", val.node.Name, value)
				err = c.model.Set(val.node.Name, value)
				if err != nil {
					c.error(fmt.Errorf("Failed to edit %s: %w", val.node.Name, err).Error(), err, false)
					return
				}
				// If underscore, refresh injected value (path unchanged)
				if strings.HasPrefix(baseOf(val.node.Name), "_") {
					nd := &model.Node{Name: val.node.Name, IsDir: false, Value: value, ClusterId: val.node.ClusterId}
					c.injectNode(nd)
				}
				ordered := c.updateList()
				target := displayName(baseOf(val.node.Name), false)
				pos = c.getPosition(target, ordered) + 1
				c.view.List.SetCurrentItem(pos)
			})
			editValueForm.AddButton("Quit", func() {
				c.view.Pages.RemovePage("modal")
			})
			c.view.Pages.AddPage("modal", c.view.ModalEdit(editValueForm, 60, 7), true, true)
			return nil
		}

		// Edit directory (rename)
		fs := strings.FieldsFunc(val.node.Name, splitFunc)
		curBase := fs[len(fs)-1]
		editDirForm := c.view.NewEditValueForm(fmt.Sprintf("Rename folder: %s", val.node.Name), curBase)
		editDirForm.AddButton("Save", func() {
			newName := strings.TrimSpace(editDirForm.GetFormItem(0).(*tview.InputField).GetText())
			c.view.Pages.RemovePage("modal")
			if newName == "" || strings.Contains(newName, "/") {
				c.error("Invalid folder name", fmt.Errorf("name must be non-empty and must not contain '/'"), false)
				return
			}
			oldPath := val.node.Name
			newPath := normAbs(c.currentDir + newName)
			if newPath == oldPath {
				return
			}
			if nd, err2 := c.model.Get(newPath); err2 == nil && nd != nil {
				c.error("Target exists", fmt.Errorf("%q already exists; choose a different name", newPath), false)
				return
			}
			log.Debugf("Renaming directory: %s -> %s", oldPath, newPath)
			err = c.model.RenameDir(oldPath, newPath)
			if err != nil {
				c.error("Failed to rename folder", err, false)
				return
			}
			// Update injected cache if underscore involved
			if strings.HasPrefix(curBase, "_") || strings.HasPrefix(newName, "_") {
				c.reinjectRename(oldPath, newPath, true, val.node.ClusterId, "")
			}
			ordered := c.updateList()
			pos = c.getPosition(newName+"/", ordered) + 1
			c.view.List.SetCurrentItem(pos)
		})
		editDirForm.AddButton("Quit", func() {
			c.view.Pages.RemovePage("modal")
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(editDirForm, 60, 7), true, true)
	}
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

	fs := strings.FieldsFunc(val.node.Name, splitFunc)
	curBase := fs[len(fs)-1]

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
			c.reinjectRename(oldPath, newPath, val.node.IsDir, val.node.ClusterId, val.node.Value)
		}
		ordered := c.updateList()
		target := newName
		if val.node.IsDir {
			target = newName + "/"
		}
		c.view.List.SetCurrentItem(c.getPosition(target, ordered) + 1)
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
		if err := c.model.SetTTL(val.node.Name, val.node.Value, secs); err != nil {
			c.error("Failed to set TTL", err, false)
			return
		}
		if strings.HasPrefix(baseOf(val.node.Name), "_") {
			nd := &model.Node{Name: val.node.Name, IsDir: false, Value: val.node.Value, ClusterId: val.node.ClusterId, TTL: secs}
			c.injectNode(nd)
		}
		ordered := c.updateList()
		target := displayName(baseOf(val.node.Name), false)
		c.view.List.SetCurrentItem(c.getPosition(target, ordered) + 1)
		c.fillDetails(mapKey)
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
		// edit folder
		return c.edit()
	}

	title := fmt.Sprintf(" Edit (multiline): %s ", val.node.Name)
	ta := c.view.NewMultilineEditor(title, val.node.Value)

	// Inside editor:
	//   Ctrl+S = save
	//   Esc / Ctrl+Q = cancel
	ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyCtrlS:
			value := ta.GetText()
			log.Debugf("Multiline save: %s (%d bytes)", val.node.Name, len(value))
			// Preserve any expiry: SetKeepTTL re-attaches the v3 lease / re-applies
			// the v2 TTL so editing a key's value no longer silently drops it.
			if err := c.model.SetKeepTTL(val.node.Name, value, val.node.LeaseID, val.node.TTL); err != nil {
				c.view.CloseEditor()
				c.error("Failed to save value", err, false)
				return nil
			}
			if strings.HasPrefix(baseOf(val.node.Name), "_") {
				nd := &model.Node{Name: val.node.Name, IsDir: false, Value: value, ClusterId: val.node.ClusterId, TTL: val.node.TTL, LeaseID: val.node.LeaseID}
				c.injectNode(nd)
			}
			c.view.CloseEditor()
			ordered := c.updateList()
			base := displayName(baseOf(val.node.Name), false)
			pos := c.getPosition(base, ordered) + 1
			c.view.List.SetCurrentItem(pos)
			// Refresh details panel with mapKey
			i := c.view.List.GetCurrentItem()
			_, mk := c.view.List.GetItemText(i)
			c.fillDetails(strings.TrimSpace(mk))
			return nil

		case tcell.KeyEsc, tcell.KeyCtrlQ:
			c.view.CloseEditor()
			return nil
		}
		return ev
	})

	c.view.OpenEditor(ta)
	return nil
}

// export prompts for a filename and writes all non-directory keys in the
// current directory to a JSON file as {"key": "value", ...}.
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
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 60, 5), true, true)
	return nil
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
		for k, val := range items {
			resolved[c.resolveImportKey(k)] = val
		}
		written, skipped, ierr := c.model.Import(resolved, overwrite)
		c.view.Pages.RemovePage("modal")
		if ierr != nil {
			c.error("Import failed", ierr, false)
			return
		}
		c.updateList()
		c.info("Imported", fmt.Sprintf("%d written, %d skipped", written, skipped))
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
	lines = strings.Count(v, "\n") + 1
	printable = utf8.ValidString(v)
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
	c.injectNode(nd)

	if nd.IsDir {
		dir := normAbs(nd.Name)
		if dir != "/" {
			dir += "/"
		}
		c.currentDir = dir
		c.Cd(c.currentDir)
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
			c.injectNode(&model.Node{Name: dst, IsDir: val.node.IsDir, Value: val.node.Value, ClusterId: val.node.ClusterId})
		}
		ordered := c.updateList()
		if parentOf(dst) == normAbs(c.currentDir) {
			target := displayName(baseOf(dst), val.node.IsDir)
			c.view.List.SetCurrentItem(c.getPosition(target, ordered) + 1)
		} else {
			c.info("Copied", fmt.Sprintf("%s → %s", src, dst))
		}
	})
	form.AddButton("Cancel", func() {
		c.view.Pages.RemovePage("modal")
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(form, 60, 7), true, true)
	return nil
}

package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"
)

type Controller struct {
	debug        bool
	view         *view.View
	model        *model.Model
	currentDir   string
	currentNodes map[string]*Node // mapKey => Node (mapKey is "<basename>|dir" or "<basename>|file")
	position     map[string]int
	injected     map[string]map[string]*model.Node

	startupErr error
}

type Node struct {
	node *model.Node
}

func splitFunc(r rune) bool { return r == '/' }

func NewController(host, port string, debug bool, protocol string) *Controller {
	m, err := model.NewModel(host, port, protocol)

	v := view.NewView()
	headerProto := protocol
	if err == nil && m != nil {
		headerProto = m.ProtocolVersion()
	}
	v.Frame.AddText(
		fmt.Sprintf("Etcd-walker v.0.2.4 (on %s:%s)  –  protocol: %s", host, port, headerProto),
		true, tview.AlignCenter, tcell.ColorGreen,
	)

	controller := &Controller{
		debug:      debug,
		view:       v,
		model:      m,
		currentDir: "/",
		position:   make(map[string]int),
		injected:   make(map[string]map[string]*model.Node),
		startupErr: err,
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
	par := parentOf(nd.Name)
	if par != "/" {
		par += "/"
	}
	bucket := c.ensureInjectedBucket(par)
	fields := strings.FieldsFunc(nd.Name, splitFunc)
	base := fields[len(fields)-1]
	mk := makeMapKey(base, nd.IsDir)
	bucket[mk] = nd
	log.Debugf("injected node %s under %s as %s (dir=%t)", nd.Name, par, mk, nd.IsDir)
}

// remove injected node (e.g., after delete/rename)
func (c *Controller) removeInjected(nd *model.Node) {
	if nd == nil {
		return
	}
	par := parentOf(nd.Name)
	if par != "/" {
		par += "/"
	}
	fields := strings.FieldsFunc(nd.Name, splitFunc)
	base := fields[len(fields)-1]
	mk := makeMapKey(base, nd.IsDir)
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

func (c *Controller) updateList() []string {
	log.Debugf("updating list")
	c.view.List.Clear()
	c.view.List.SetTitle("[ [::b]" + c.currentDir + "[::-] ]")
	if err := c.makeNodeMap(); err != nil {
		c.error("failed to load nodes", err, true)
	}

	// [..] always on top
	c.view.List.AddItem("[..]", "..", 0, func() {
		c.Up()
	})

	// Collect and split into dirs and files using mapKey suffix.
	dirKeys := make([]string, 0, len(c.currentNodes))  // mapKey
	fileKeys := make([]string, 0, len(c.currentNodes)) // mapKey
	for mk := range c.currentNodes {
		if strings.HasSuffix(mk, "|dir") {
			dirKeys = append(dirKeys, mk)
		} else {
			fileKeys = append(fileKeys, mk)
		}
	}
	sort.Strings(dirKeys)
	sort.Strings(fileKeys)

	// Directories
	for _, mk := range dirKeys {
		n := c.currentNodes[mk].node
		fields := strings.FieldsFunc(n.Name, splitFunc)
		base := fields[len(fields)-1]
		rawLabel := "📁 " + displayName(base, true)
		label := c.colorize(base, true, rawLabel)
		// Use mapKey as secondary text (stable key for actions)
		c.view.List.AddItem(label, mk, 0, func() {
			i := c.view.List.GetCurrentItem()
			_, curMK := c.view.List.GetItemText(i) // secondary text is mapKey
			curMK = strings.TrimSpace(curMK)
			if val, ok := c.currentNodes[curMK]; ok && val.node.IsDir {
				// Save cursor before moving
				c.position[c.currentDir] = c.view.List.GetCurrentItem()
				fields := strings.FieldsFunc(val.node.Name, splitFunc)
				base := fields[len(fields)-1]
				c.Down(base)
			}
		})
	}

	// Files
	for _, mk := range fileKeys {
		n := c.currentNodes[mk].node
		fields := strings.FieldsFunc(n.Name, splitFunc)
		base := fields[len(fields)-1]
		rawLabel := "   " + displayName(base, false)
		label := c.colorize(base, false, rawLabel)
		c.view.List.AddItem(label, mk, 0, func() {
			// no-op; details pane updates via SetChangedFunc
		})
	}

	// Restore cursor position if we saved it before
	if val, ok := c.position[c.currentDir]; ok {
		c.view.List.SetCurrentItem(val)
		delete(c.position, c.currentDir)
	}

	ordered := make([]string, 0, len(dirKeys)+len(fileKeys))
	for _, mk := range dirKeys {
		n := c.currentNodes[mk].node
		fs := strings.FieldsFunc(n.Name, splitFunc)
		ordered = append(ordered, displayName(fs[len(fs)-1], true))
	}
	for _, mk := range fileKeys {
		n := c.currentNodes[mk].node
		fs := strings.FieldsFunc(n.Name, splitFunc)
		ordered = append(ordered, displayName(fs[len(fs)-1], false))
	}
	return ordered
}

func (c *Controller) fillDetails(mapKey string) {
	c.view.Details.Clear()
	if val, ok := c.currentNodes[mapKey]; ok {
		log.Debugf("Node details name: %s, isDir: %t, clusterId: %s", val.node.Name, val.node.IsDir, val.node.ClusterId)
		fmt.Fprintf(c.view.Details, "[blue] Cluster Id: [gray] %s\n", val.node.ClusterId)
		fmt.Fprintf(c.view.Details, "[green] Full name: [white] %s\n", val.node.Name)
		fmt.Fprintf(c.view.Details, "[green] Is directory: [white] %t\n\n", val.node.IsDir)
		if !val.node.IsDir {
			fmt.Fprintf(c.view.Details, "[green] Value: [white] \n%s\n", val.node.Value)
		}
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
		case tcell.KeyCtrlS:
			return c.search()
		case tcell.KeyCtrlJ:
			return c.jump()
		case tcell.KeyCtrlH:
			help := c.view.NewHotkeysModal()

			help.SetInputCapture(func(_ *tcell.EventKey) *tcell.EventKey {
				c.view.Pages.RemovePage("modal-help")
				return nil
			})

			c.view.Pages.AddPage("modal-help", c.view.ModalEdit(help, 70, 18), true, true)
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
		// Allow Ctrl+Q to exit as well
		c.view.App.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			if event.Key() == tcell.KeyCtrlQ {
				c.Stop()
				return nil
			}
			return event
		})
		c.error("Connection error", c.startupErr, true)
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

	// Recompute visual ordering to match the list
	dirNames := []string{}
	fileNames := []string{}
	for mk, v := range c.currentNodes {
		fs := strings.FieldsFunc(v.node.Name, splitFunc)
		base := fs[len(fs)-1]
		if strings.HasSuffix(mk, "|dir") {
			dirNames = append(dirNames, displayName(base, true))
		} else {
			fileNames = append(fileNames, displayName(base, false))
		}
	}
	sort.Strings(dirNames)
	sort.Strings(fileNames)
	ordered := append(dirNames, fileNames...)

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
		base := displayName(strings.FieldsFunc(val.node.Name, splitFunc)[len(strings.FieldsFunc(val.node.Name, splitFunc))-1], val.node.IsDir)
		elem := base
		if val.node.IsDir {
			elem = elem + " (recursive)"
		}
		delQ := c.view.NewDeleteQ(elem)
		delQ.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == "ok" {
				if !val.node.IsDir {
					err = c.model.Del(val.node.Name)
				} else {
					err = c.model.DelDir(val.node.Name)
				}
				if err != nil {
					c.view.Pages.RemovePage("modal")
					c.error("Error deleting node", err, false)
					return
				}
				// Remove from injected cache if present
				c.removeInjected(val.node)
				c.view.Details.Clear()
				c.updateList()
			}
			c.view.Pages.RemovePage("modal")
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(delQ, 20, 7), true, true)
	}
	return nil
}

func (c *Controller) create() *tcell.EventKey {
	pos := 0
	var err error
	createForm := c.view.NewCreateForm(fmt.Sprintf("Create Node: %s", c.currentDir))
	createForm.AddButton("Save", func() {
		node := createForm.GetFormItem(0).(*tview.InputField).GetText()
		value := createForm.GetFormItem(1).(*tview.InputField).GetText()
		isDir := createForm.GetFormItem(2).(*tview.Checkbox).IsChecked()
		if node != "" {
			log.Debugf("Creating Node: name: %s, isDir: %t, value: %s", node, isDir, value)
			full := normAbs(c.currentDir + node)
			if !isDir {
				err = c.model.Set(full, value)
			} else {
				err = c.model.MkDir(full)
			}
			if err != nil {
				c.view.Pages.RemovePage("modal")
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
			pos = c.getPosition(target, ordered) + 1 // +1 for [..]
			c.view.Pages.RemovePage("modal")
			c.view.List.SetCurrentItem(pos)
		}
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
				log.Debugf("Editing Node Value: name: %s, value: %s", val.node.Name, value)
				err = c.model.Set(val.node.Name, value)
				if err != nil {
					c.view.Pages.RemovePage("modal")
					c.error(fmt.Errorf("Failed to edit %s: %w", val.node.Name, err).Error(), err, false)
					return
				}
				// If underscore, refresh injected value (path unchanged)
				if strings.HasPrefix(baseOf(val.node.Name), "_") {
					nd := &model.Node{Name: val.node.Name, IsDir: false, Value: value, ClusterId: val.node.ClusterId}
					c.injectNode(nd)
				}
				ordered := c.updateList()
				fs := strings.FieldsFunc(val.node.Name, splitFunc)
				target := displayName(fs[len(fs)-1], false)
				pos = c.getPosition(target, ordered) + 1
				c.view.Pages.RemovePage("modal")
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
			if newName == "" || strings.Contains(newName, "/") {
				c.view.Pages.RemovePage("modal")
				c.error("Invalid folder name", fmt.Errorf("name must be non-empty and must not contain '/'"), false)
				return
			}
			oldPath := val.node.Name
			newPath := normAbs(c.currentDir + newName)
			if newPath == oldPath {
				c.view.Pages.RemovePage("modal")
				return
			}
			log.Debugf("Renaming directory: %s -> %s", oldPath, newPath)
			err = c.model.RenameDir(oldPath, newPath)
			if err != nil {
				c.view.Pages.RemovePage("modal")
				c.error("Failed to rename folder", err, false)
				return
			}
			// Update injected cache if underscore involved
			if strings.HasPrefix(curBase, "_") || strings.HasPrefix(newName, "_") {
				c.reinjectRename(oldPath, newPath, true, val.node.ClusterId, "")
			}
			ordered := c.updateList()
			pos = c.getPosition(newName+"/", ordered) + 1
			c.view.Pages.RemovePage("modal")
			c.view.List.SetCurrentItem(pos)
		})
		editDirForm.AddButton("Quit", func() {
			c.view.Pages.RemovePage("modal")
		})
		c.view.Pages.AddPage("modal", c.view.ModalEdit(editDirForm, 60, 7), true, true)
	}
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
			if err := c.model.Set(val.node.Name, value); err != nil {
				c.view.CloseEditor()
				c.error("Failed to save value", err, false)
				return nil
			}
			if strings.HasPrefix(baseOf(val.node.Name), "_") {
				nd := &model.Node{Name: val.node.Name, IsDir: false, Value: value, ClusterId: val.node.ClusterId}
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

func (c *Controller) error(header string, err error, fatal bool) {
	errMsg := c.view.NewErrorMessageQ(header, err.Error())
	errMsg.SetDoneFunc(func(buttonIndex int, buttonLabel string) {
		c.view.Pages.RemovePage("modal")
		if fatal {
			c.view.App.Stop()
		}
	})
	c.view.Pages.AddPage("modal", c.view.ModalEdit(errMsg, 8, 3), true, true)
}

func (c *Controller) jump() *tcell.EventKey {
	inp := c.view.NewJump()
	inp.SetDoneFunc(func(key tcell.Key) {
		defer c.view.Pages.RemovePage("modal")
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

		c.injectNode(nd)

		if nd.IsDir {
			c.currentDir = normAbs(nd.Name) + "/"
			c.Cd(c.currentDir)
			return
		}

		parent := parentOf(nd.Name)
		base := baseOf(nd.Name)
		if !strings.HasSuffix(parent, "/") {
			parent += "/"
		}
		c.currentDir = parent
		ordered := c.updateList()

		findIndex := func(name string, list []string) int {
			for i, v := range list {
				if v == name {
					return i
				}
			}
			return -1
		}

		if pos := findIndex(base, ordered); pos >= 0 {
			c.view.List.SetCurrentItem(pos + 1) // +1 for [..]
			i := c.view.List.GetCurrentItem()
			_, mk := c.view.List.GetItemText(i)
			c.fillDetails(strings.TrimSpace(mk))
			return
		}
		if pos := findIndex(base+"/", ordered); pos >= 0 {
			c.view.List.SetCurrentItem(pos + 1)
			i := c.view.List.GetCurrentItem()
			_, mk := c.view.List.GetItemText(i)
			c.fillDetails(strings.TrimSpace(mk))
			return
		}

		c.error("Not found", fmt.Errorf("%s", target), false)
	})

	c.view.Pages.AddPage("modal", c.view.ModalEdit(inp, 60, 5), true, true)
	return nil
}

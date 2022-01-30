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
	currentNodes map[string]*Node
	position     map[string]int
}

type Node struct {
	node *model.Node
}

func splitFunc(r rune) bool {
	return r == '/'
}

func NewController(
	host string,
	port string,
	debug bool,
) *Controller {
	m := model.NewModel(host, port)
	v := view.NewView()
	v.Frame.AddText(fmt.Sprintf("Etcd-walker v.0.0.4 (on %s:%s)", host, port), true, tview.AlignCenter, tcell.ColorGreen)

	controller := Controller{
		debug:      debug,
		view:       v,
		model:      m,
		currentDir: "/",
		position:   make(map[string]int),
	}
	return &controller
}

func (c *Controller) makeNodeMap() error {
	log.Debugf("updating node map started")
	m := make(map[string]*Node)
	list, err := c.model.Ls(c.currentDir)
	if err != nil {
		return err
	}
	for _, node := range list {
		rawName := node.Name
		fields := strings.FieldsFunc(strings.TrimSpace(rawName), splitFunc)
		cNode := Node{
			node: node,
		}
		m[fields[len(fields)-1]] = &cNode
		log.Debugf("added node %s, %v", node.Name, cNode)
	}
	c.currentNodes = m
	log.Debugf("updating node map completed")
	return nil
}

func (c *Controller) updateList() []string {
	log.Debugf("updating list")
	c.view.List.Clear()
	c.view.List.SetTitle("[ [::b]" + c.currentDir + "[::-] ]")
	c.makeNodeMap()
	keys := make([]string, 0, len(c.currentNodes))
	for k := range c.currentNodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c.view.List.SetMainTextColor(tcell.Color31)
		c.view.List.AddItem(key, key, 0, func() {
			i := c.view.List.GetCurrentItem()
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
			if val, ok := c.currentNodes[cur]; ok {
				if val.node.IsDir {
					c.position[c.currentDir] = c.view.List.GetCurrentItem()
					c.Down(cur)
				}
			}
		})
	}
	if val, ok := c.position[c.currentDir]; ok {
		c.view.List.SetCurrentItem(val)
		delete(c.position, c.currentDir)
	}
	return keys
}

func (c *Controller) fillDetails(key string) {
	c.view.Details.Clear()
	if val, ok := c.currentNodes[key]; ok {
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
		case tcell.KeyRune:
			switch event.Rune() {
			case 'q':
				c.Stop()
				return nil
			}

		}
		return event
	})
	c.view.List.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyRune:
			switch event.Rune() {
			case 'c':
				pos := 0
				createForm := c.view.NewCreateForm(fmt.Sprintf("Create Node: %s", c.currentDir))
				createForm.AddButton("Save", func() {
					node := createForm.GetFormItem(0).(*tview.InputField).GetText()
					value := createForm.GetFormItem(1).(*tview.InputField).GetText()
					isDir := createForm.GetFormItem(2).(*tview.Checkbox).IsChecked()
					if node != "" {
						log.Debugf("Creating Node: name: %s, isDir: %t, value: %s", node, isDir, value)
						// TODO: error handling & reporting
						if !isDir {
							c.model.Set(c.currentDir+"/"+node, value)
						} else {
							c.model.MkDir(c.currentDir + node)
						}
						pos = c.getPosition(node, c.updateList())
						c.view.Pages.RemovePage("modal")
						c.view.List.SetCurrentItem(pos)
					}

				})
				createForm.AddButton("Quit", func() {
					c.view.Pages.RemovePage("modal")
				})
				c.view.Pages.AddPage("modal", c.view.ModalEdit(createForm, 60, 11), true, true)
				return nil
			case 'e':
				pos := 0
				i := c.view.List.GetCurrentItem()
				_, cur := c.view.List.GetItemText(i)
				cur = strings.TrimSpace(cur)
				if val, ok := c.currentNodes[cur]; ok {
					if !val.node.IsDir {
						editValueForm := c.view.NewEditValueForm(fmt.Sprintf("Edit: %s", val.node.Name), val.node.Value)
						editValueForm.AddButton("Save", func() {
							value := editValueForm.GetFormItem(0).(*tview.InputField).GetText()
							log.Debugf("Editing Node Value: name: %s, value: %s", val.node.Name, value)
							// TODO: error handling & reporting
							c.model.Set(val.node.Name, value)
							pos = c.getPosition(cur, c.updateList())
							c.view.Pages.RemovePage("modal")
							c.view.List.SetCurrentItem(pos)
						})
						editValueForm.AddButton("Quit", func() {
							c.view.Pages.RemovePage("modal")
						})
						c.view.Pages.AddPage("modal", c.view.ModalEdit(editValueForm, 60, 7), true, true)
					}
				}
				return nil
			case '/':
				search := c.view.NewSearch()
				keys := make([]string, 0, len(c.currentNodes))
				for k := range c.currentNodes {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				search.SetDoneFunc(func(key tcell.Key) {
					oldPos := c.view.List.GetCurrentItem()
					value := search.GetText()
					pos := c.getPosition(value, keys)
					if pos != oldPos && key == tcell.KeyEnter {
						c.view.List.SetCurrentItem(pos)
					}
					c.view.Pages.RemovePage("modal")
				})
				search.SetAutocompleteFunc(func(currentText string) []string {
					prefix := strings.TrimSpace(strings.ToLower(currentText))
					if prefix == "" {
						return nil
					}
					result := make([]string, 0, len(c.currentNodes))
					for _, word := range keys {
						if strings.HasPrefix(strings.ToLower(word), strings.ToLower(currentText)) {
							result = append(result, word)
						}
					}
					return result
				})
				c.view.Pages.AddPage("modal", c.view.ModalEdit(search, 60, 5), true, true)
				return nil
			case 'u', 'h':
				c.Up()
				return nil
			case 'l':
				return tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)
			}
		}
		return event
	})
}

func (c *Controller) Down(cur string) {
	newDir := c.currentDir + cur + "/"
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

func (c *Controller) Cd(path string) {
	c.updateList()
}

func (c *Controller) Stop() {
	log.Debugf("exit...")
	c.view.App.Stop()
}

func (c *Controller) Run() error {
	c.view.List.SetChangedFunc(func(i int, s string, s2 string, r rune) {
		_, cur := c.view.List.GetItemText(i)
		cur = strings.TrimSpace(cur)
		c.fillDetails(cur)
	})
	c.updateList()
	c.setInput()
	return c.view.App.Run()
}

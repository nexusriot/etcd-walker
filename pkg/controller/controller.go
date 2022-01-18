package controller

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
)

type Controller struct {
	debug        bool
	view         *view.View
	model        *model.Model
	currentDir   string
	currentNodes map[string]*Node
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
	v.Frame.AddText(fmt.Sprintf("Etcd-walker v.0.0.2 (on %s:%s)", host, port), true, tview.AlignCenter, tcell.ColorGreen)

	controller := Controller{
		debug:      debug,
		view:       v,
		model:      m,
		currentDir: "/",
	}
	return &controller
}

func (c *Controller) makeNodeMap() error {
	m := make(map[string]*Node)
	list, err := c.model.Ls(c.currentDir)
	if err != nil {
		return err
	}
	for _, node := range list {
		rawName := node.Name
		fields := strings.FieldsFunc(strings.TrimSpace(rawName), splitFunc)
		m[fields[len(fields)-1]] = &Node{
			node: node,
		}
	}
	c.currentNodes = m
	return nil
}

func (c *Controller) updateList() {
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
					c.Down(cur)
				}
			}
		})
	}
}

func (c *Controller) fillDetails(key string) {
	go func() {
		c.view.Details.Clear()
		if val, ok := c.currentNodes[key]; ok {
			fmt.Fprintf(c.view.Details, "[blue] Cluster Id: [gray] %s\n", val.node.ClusterId)
			fmt.Fprintf(c.view.Details, "[green] Full name: [white] %s\n", val.node.Name)
			fmt.Fprintf(c.view.Details, "[green] Is directory: [white] %t\n\n", val.node.IsDir)
			if !val.node.IsDir {
				fmt.Fprintf(c.view.Details, "[green] Value: [white] \n%s\n", val.node.Value)
			}
		}
	}()
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
	c.currentDir = c.currentDir + cur + "/"
	c.Cd(c.currentDir)
}

func (c *Controller) Up() {
	fields := strings.FieldsFunc(strings.TrimSpace(c.currentDir), splitFunc)
	if len(fields) == 0 {
		return
	}
	c.currentDir = "/" + strings.Join(fields[:len(fields)-1], "/")
	c.Cd(c.currentDir + fields[0] + "/")
}

func (c *Controller) Cd(path string) {
	c.updateList()
}

func (c *Controller) Stop() {
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

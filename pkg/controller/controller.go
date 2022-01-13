package controller

import (
	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
	"strings"
)

type Controller struct {
	view       *view.View
	model      *model.Model
	currentDir string
}

func NewController(
	host string,
	port string,
	debug string,
) *Controller {
	m := model.NewModel()
	v := view.NewView()

	v.Frame.AddText("Etcd-walker v.0.0.1-poc", true, tview.AlignCenter, tcell.ColorGreen)
	controller := Controller{
		view:       v,
		model:      m,
		currentDir: "/",
	}
	return &controller
}

func (c *Controller) updateList() {
	c.view.List.Clear()
	c.view.List.SetTitle("[ [::b]" + c.currentDir + "[::-] ]")
	list, err := c.model.List(c.currentDir)
	if err != nil {
		panic(err)
	}
	for _, node := range list {
		c.view.List.SetMainTextColor(tcell.Color31)
		c.view.List.AddItem(node.Name, node.Name, 0, func() {
			i := c.view.List.GetCurrentItem()
			_, cur := c.view.List.GetItemText(i)
			cur = strings.TrimSpace(cur)
			c.Cd(c.currentDir + cur)
		})
	}
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
}

func (c *Controller) Cd(path string) {
	c.currentDir = path
	c.updateList()
}

func (c *Controller) Stop() {
	c.view.App.Stop()
}

func (c *Controller) Run() error {
	c.updateList()
	c.setInput()
	return c.view.App.Run()
}

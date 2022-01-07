package controller

import (
	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/nexusriot/etcd-walker/pkg/view"
	"github.com/rivo/tview"
)

type Controller struct {
	view  *view.View
	model *model.Model
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
		view:  v,
		model: m,
	}
	return &controller
}

func (c *Controller) updateList() {
	c.view.List.Clear()
	list, err := c.model.ListRoot()
	if err != nil {
		panic(err)
	}
	for _, node := range list {
		var nodeV string
		if !node.IsDir {
			nodeV = "*" + node.Name
		} else {
			nodeV = node.Name
		}
		c.view.List.AddItem(nodeV, nodeV, 0, func() {})
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

func (c *Controller) Stop() {
	c.view.App.Stop()
}

func (c *Controller) Run() error {
	c.updateList()
	c.setInput()
	return c.view.App.Run()
}

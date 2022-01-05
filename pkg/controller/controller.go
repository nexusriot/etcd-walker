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
	for _, key := range list {
		c.view.List.AddItem(key, key, 0, func() {})
	}
}

func (c *Controller) Run() error {
	c.updateList()
	return c.view.App.Run()
}

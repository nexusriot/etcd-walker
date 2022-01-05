package main

import (
	"context"
	"fmt"
	"github.com/coreos/etcd/client"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App   *tview.Application
	Frame *tview.Frame
	Pages *tview.Pages
	List  *tview.List
}

// NewView ...
func NewView() View {
	app := tview.NewApplication()

	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitleAlign(tview.AlignLeft)

	main := tview.NewFlex().
		AddItem(list, 0, 1, true)

	pages := tview.NewPages().
		AddPage("main", main, true, true)

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][↓,j/↑,k][::-] Down/Up [::b][Enter,l/u,h][::-] Lower/Upper [::b][d[][::-] Download [::b][q[][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
	}

	return v
}

func main() {

	etcd, err := client.New(client.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
	})
	fmt.Println(etcd)
	fmt.Println(err)

	api := client.NewKeysAPI(etcd)

	options := &client.GetOptions{Sort: true, Recursive: true}
	//_, err = api.Set(context.Background(), "ololo", "", &client.SetOptions{Dir: true, PrevExist: client.PrevIgnore})
	response, err := api.Get(context.Background(), "/", options)
	//_, err = api.Set(context.Background(), "/ololo/pp", "olol\nlo\nlo", nil)
	//
	//response, err = api.Get(context.Background(), "/ololo/pp", nil)
	fmt.Println(response)

	v := NewView()
	v.Frame.AddText("Ololo there", true, tview.AlignCenter, tcell.ColorWhite)
	app := tview.NewApplication()

	form := tview.NewForm().
		AddInputField("Last name", "", 20, nil, nil).
		AddButton("Save", nil).
		AddButton("Quit", func() {
			app.Stop()
		})

	form.SetBorder(true).SetTitle("Enter some data").SetTitleAlign(tview.AlignLeft)
	if err := app.SetRoot(form, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}

	v.App.Run()

}

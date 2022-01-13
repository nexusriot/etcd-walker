package view

import (
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
func NewView() *View {
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
	frame.AddText("[::b][↓,↑][::-] Down/Up [::b][Enter,l/u][::-] Lower/Upper [::b][q[][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
	}

	return &v
}

func CreateInputForm() {
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
}
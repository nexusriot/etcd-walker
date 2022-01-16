package view

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App     *tview.Application
	Frame   *tview.Frame
	Pages   *tview.Pages
	List    *tview.List
	Details *tview.TextView
}

// NewView ...
func NewView() *View {
	app := tview.NewApplication()
	list := tview.NewList().
		ShowSecondaryText(false)
	list.SetBorder(true).
		SetTitleAlign(tview.AlignLeft)

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true).SetTitle("Details")

	main := tview.NewFlex()
	main.AddItem(list, 0, 2, true)
	main.AddItem(tv, 0, 3, false)

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
		tv,
	}

	return &v
}

func CreateInputForm() {
	app := tview.NewApplication()

	form := tview.NewForm().
		AddInputField("Node name", "", 20, nil, nil).
		AddButton("Save", nil).
		AddButton("Quit", func() {
			app.Stop()
		})
	form.SetBorder(true).SetTitle("Node editing").SetTitleAlign(tview.AlignLeft)
	if err := app.SetRoot(form, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}
}

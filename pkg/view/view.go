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
	Box     *tview.Box
	Modal   func(p tview.Primitive, width, height int) tview.Primitive
	Form    *tview.Form
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

	box := tview.NewFlex().
		SetBorder(true).
		SetTitle("Box")

	pages := tview.NewPages().
		AddPage("main", main, true, true)

	form := tview.NewForm().
		AddInputField("Node name", "", 20, nil, nil).
		AddCheckbox("Is a Directory", false, func(checked bool) {
			if checked {

			}

		}).
		AddInputField("Value", "", 30, nil, nil).
		AddButton("Save", func() {
			pages.RemovePage("modal")
		}).
		AddButton("Quit", func() {
			app.Stop()
		})
	form.SetBorder(true)

	modal := func(p tview.Primitive, width, height int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(p, 0, 1, true).
				AddItem(nil, 0, 1, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][↓,↑][::-] Down/Up [::b][Enter,l/u][::-] Lower/Upper [::b][q[][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		tv,
		box,
		modal,
		form,
	}

	return &v
}

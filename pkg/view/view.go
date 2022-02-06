package view

import (
	"fmt"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View ...
type View struct {
	App       *tview.Application
	Frame     *tview.Frame
	Pages     *tview.Pages
	List      *tview.List
	Details   *tview.TextView
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive
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

	modal := func(p tview.Primitive, width, height int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(p, height, 1, true).
				AddItem(nil, 0, 1, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	frame := tview.NewFrame(pages)
	frame.AddText("[::b][↓,↑][::-] Down/Up [::b][Enter,l/u][::-] Lower/Upper [::b][c[][::-] Create [::b][d[][::-] Delete [::b][e[][::-] Edit value [::b][/][::-] Search [::b][q[][::-] Quit", false, tview.AlignCenter, tcell.ColorWhite)

	app.SetRoot(frame, true)

	v := View{
		app,
		frame,
		pages,
		list,
		tv,
		modal,
	}

	return &v
}

func (v *View) NewCreateForm(header string) *tview.Form {
	form := tview.NewForm().
		AddInputField("Node name", "", 30, nil, nil).
		AddInputField("Value", "", 30, nil, nil)

	form.AddCheckbox("Is a Directory", false, func(checked bool) {
	})
	form.SetBorder(true)
	form.SetTitle(header)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

func (v *View) NewEditValueForm(header string, value string) *tview.Form {
	form := tview.NewForm().
		AddInputField("Value", "", 30, nil, nil)
	form.GetFormItem(0).(*tview.InputField).SetText(value)
	form.SetBorder(true)
	form.SetTitle(header)
	form.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			v.Pages.RemovePage("modal")
		}
		return event
	})
	return form
}

func (v *View) NewSearch() *tview.InputField {
	search := tview.NewInputField().
		SetPlaceholder("search").
		SetFieldBackgroundColor(tcell.ColorGrey).
		SetFieldTextColor(tcell.ColorWhite)
	return search
}

func (v *View) NewDeleteQ(header string) *tview.Modal {
	deleteQ := tview.NewModal()
	deleteQ.SetText(fmt.Sprintf("Delete %s ?", header)).AddButtons([]string{"ok", "cancel"})
	return deleteQ
}

func (v *View) NewErrorMessageQ(header string, details string) *tview.Modal {
	errorQ := tview.NewModal()
	errorQ.SetText(header + ": " + details).SetBackgroundColor(tcell.ColorRed).AddButtons([]string{"ok"})
	return errorQ
}

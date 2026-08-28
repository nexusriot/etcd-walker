package view

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// View owns the tview widgets and the dialog constructors. It is passive: it
// never calls the model, and the controller installs every input capture and
// done handler.
type View struct {
	App   *tview.Application
	Frame *tview.Frame
	Pages *tview.Pages
	// List is the pane that currently has focus — always one of Lists. The
	// controller repoints it when the user switches panes, so every handler
	// can keep addressing "the list" without knowing how many there are.
	List *tview.List
	// Lists are the two browser panes. The second one exists even in
	// single-pane mode; only the layout decides whether it is on screen.
	Lists   [2]*tview.List
	Details *tview.TextView
	// Main is the layout SetDual rebuilds. Tests construct a View without it.
	Main      *tview.Flex
	ModalEdit func(p tview.Primitive, width, height int) tview.Primitive
	// ModalScroll centers a panel whose content can be longer than the
	// terminal. Its height is proportional instead of fixed, so the panel
	// shrinks with the screen rather than being clipped: a clipped panel hides
	// its bottom rows with no way to reach them, and scrolling *inside* it
	// cannot help, because those rows are never drawn in the first place.
	ModalScroll func(p tview.Primitive, width int) tview.Primitive
}

// SetDual switches between the single-pane layout (one list beside the details
// pane) and the two-pane one (both lists side by side above a full-width
// details pane). Details keeps the full width in dual mode because splitting
// three ways leaves nothing readable in an 80-column terminal.
func (v *View) SetDual(on bool) {
	if v.Main == nil {
		return // headless View built by a test: no layout to rearrange
	}
	v.Main.Clear()
	if !on {
		v.Main.SetDirection(tview.FlexColumn)
		v.Main.AddItem(v.Lists[0], 0, 2, true)
		v.Main.AddItem(v.Details, 0, 3, false)
		return
	}
	panes := tview.NewFlex().SetDirection(tview.FlexColumn)
	panes.AddItem(v.Lists[0], 0, 1, true)
	panes.AddItem(v.Lists[1], 0, 1, false)
	v.Main.SetDirection(tview.FlexRow)
	v.Main.AddItem(panes, 0, 3, true)
	v.Main.AddItem(v.Details, 0, 2, false)
}

// NewView builds the running UI — the two-pane main page, the page stack all
// modals share, and the frame carrying the header and the hotkey legend.
// Tests construct a View literal instead: this one's Details pane calls
// App.Draw on change, which needs a real screen.
func NewView() *View {
	app := tview.NewApplication()

	newPane := func() *tview.List {
		l := tview.NewList().
			ShowSecondaryText(false) // secondary text hidden but used to store raw keys
		l.SetBorder(true).
			SetTitleAlign(tview.AlignLeft)
		// Readable selection
		l.SetSelectedTextColor(tcell.ColorBlack).
			SetSelectedBackgroundColor(tcell.ColorYellow)
		return l
	}
	lists := [2]*tview.List{newPane(), newPane()}

	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	tv.SetBorder(true).SetTitle("Details")

	main := tview.NewFlex()

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

	// One row of padding above and below, and the panel takes everything left
	// over — so it is as tall as the terminal allows and never taller.
	modalScroll := func(p tview.Primitive, width int) tview.Primitive {
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 1, 0, false).
				AddItem(p, 0, 1, true).
				AddItem(nil, 1, 0, false), width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	frame := tview.NewFrame(pages)
	frame.AddText(
		"[::b][↓,↑][::-] Dwn/Up  [::b][Ent/Bs][::-]Open/Up [::b][Ctrl+N][::-]New [::b][Ctrl+D][::-]Dup [::b][Del[][::-]Delete [::b][Ctrl+E][::-]Edit [::b][Ctrl+R][::-]Rename [::b][Ctrl+V][::-]Hist [::b][Ctrl+G][::-]Rev [::b][Ctrl+B][::-]2-pane [::b][Tab[][::-]Pane [::b][F5/F6][::-]Copy/Move [::b][Ctrl+U][::-]Undo [::b][/,Ctrl+S][::-]Search [::b][Ctrl+F][::-]Find [::b][Ctrl+J][::-]Jump [::b][Ctrl+H][::-]Hotkeys [::b][Ctrl+Q][::-]Quit",
		false,
		tview.AlignCenter,
		tcell.ColorWhite,
	)

	v := View{
		App:         app,
		Frame:       frame,
		Pages:       pages,
		List:        lists[0],
		Lists:       lists,
		Details:     tv,
		Main:        main,
		ModalEdit:   modal,
		ModalScroll: modalScroll,
	}
	// Fill the layout BEFORE handing it to the application: SetRoot resolves
	// the focus by descending into the root's items, so rooting an empty Flex
	// leaves nothing focused — and every key binding on the list is installed
	// as an input capture, which only runs on the focused primitive. That is
	// how a build once started with the whole keyboard dead.
	v.SetDual(false)
	app.SetRoot(frame, true)
	app.SetFocus(lists[0])

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

func (v *View) NewInfoMessageQ(header string, details string) *tview.Modal {
	return v.NewInfoModal(header, details, "ok")
}

func (v *View) NewCopiedMessageQ(details string) *tview.Modal {
	return v.NewInfoModal("Copied", details, "copied")
}

func (v *View) NewInfoModal(header, details, button string) *tview.Modal {
	infoQ := tview.NewModal()
	infoQ.SetText(header + ": " + details).
		SetBackgroundColor(tcell.ColorDarkGreen).
		AddButtons([]string{button})
	return infoQ
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

// NewTTLForm builds a form to enter a key's time-to-live in seconds.
// current is pre-filled with the existing remaining TTL ("" when none).
func (v *View) NewTTLForm(header string, current string) *tview.Form {
	form := tview.NewForm().
		AddInputField("TTL seconds or duration e.g. 1h30m (0 = no expiry)", "", 10, nil, nil)
	form.GetFormItem(0).(*tview.InputField).SetText(current)
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

// NewFindForm builds the recursive-search form: substring input plus a
// checkbox to also match inside values.
func (v *View) NewFindForm(header string) *tview.Form {
	form := tview.NewForm().
		AddInputField("Find (substring, case-insensitive)", "", 20, nil, nil).
		AddCheckbox("Also search in values", false, func(checked bool) {})
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

// NewHistoryDetail returns a scrollable, color-enabled text pane used by the
// revision-history viewer for the revision detail and diff screens. The
// controller fills it and installs the key bindings.
func (v *View) NewHistoryDetail(title string) *tview.TextView {
	tv := tview.NewTextView()
	tv.SetDynamicColors(true)
	tv.SetWrap(true)
	tv.SetBorder(true)
	tv.SetTitle(title)
	tv.SetTitleAlign(tview.AlignLeft)
	return tv
}

// NewTypedConfirm asks the user to retype a word before a destructive or
// policy-gated action proceeds. The caller composes the prompt; it is escaped
// here because it carries key paths, and tview reads "[x]" in a title as a
// colour tag and would swallow a name containing brackets (DESIGN §12 B-13).
func (v *View) NewTypedConfirm(prompt string) *tview.InputField {
	inp := tview.NewInputField()
	inp.SetBorder(true).SetTitle(" " + tview.Escape(prompt) + " ")
	return inp
}

// NewOverwriteQ asks before clobbering a file that already exists on disk.
func (v *View) NewOverwriteQ(details string) *tview.Modal {
	m := tview.NewModal()
	m.SetText(details).AddButtons([]string{"overwrite", "cancel"})
	return m
}

// NewRestoreQ asks before overwriting the current value with an old revision.
func (v *View) NewRestoreQ(details string) *tview.Modal {
	m := tview.NewModal()
	m.SetText(details).AddButtons([]string{"restore", "cancel"})
	return m
}

// NewResultsList returns an empty list styled for pickers (e.g. recursive
// search results). The controller populates it.
func (v *View) NewResultsList(title string) *tview.List {
	l := tview.NewList().ShowSecondaryText(false)
	l.SetBorder(true).
		SetTitle(title).
		SetTitleAlign(tview.AlignLeft)
	l.SetSelectedTextColor(tcell.ColorBlack).
		SetSelectedBackgroundColor(tcell.ColorYellow)
	return l
}

func (v *View) NewSearch() *tview.InputField {
	search := tview.NewInputField().
		SetPlaceholder("search").
		SetFieldTextColor(tcell.ColorWhite)
	return search
}

func (v *View) NewJump() *tview.InputField {
	inp := tview.NewInputField().
		SetPlaceholder("Jump to key or dir/ (abs or relative).")
	inp.SetBorder(true).SetTitle(" Jump ")
	return inp
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

func (v *View) NewHotkeysModal() *tview.TextView {
	helpText := `
[::b]Navigation[::-]
  Enter         Open dir / select
  Backspace     Up ([..])
[::b]Actions[::-]
  Ctrl+N        Create node or directory
  Ctrl+D        Duplicate key/dir to a new path
  Ctrl+E        Edit value (multiline) / rename dir
  Ctrl+R        Rename key or directory
  Ctrl+T        Set/clear TTL on a key (seconds or 1h30m)
  Ctrl+V        Revision history of a key (v3): view/diff/restore
  Del           Delete (recursive for dirs)
  Ctrl+J        Jump to key/dir (dir ends with '/')
  Ctrl+P        Copy path (key/dir)
  Ctrl+Y        Copy key value
  Ctrl+W        Export all keys under current dir to JSON
  Ctrl+O        Import keys from a JSON file (file browser)
  Ctrl+A        Session journal: what this session changed,
                exportable as an etcdctl script
  Ctrl+U        Undo snapshots: restore a deleted directory
[::b]Time travel (v3)[::-]
  Ctrl+G        Browse a past revision: a number, -N for N
                revisions back, or empty to return to live.
                A pinned pane is read-only; each pane has
                its own revision.
[::b]Two panes[::-]
  Ctrl+B        Show/hide the second pane
  Tab           Switch panes
  F5            Copy the selected entry to the other pane
  F6            Move the selected entry to the other pane
[::b]Search[::-]
  /, Ctrl+S     Search by name (in current level)
  Ctrl+F        Find recursively (paths, optionally values)
[::b]Editor[::-]
  Ctrl+S        Save
  Esc/Ctrl+Q    Cancel/Cancel+Quit
[::b]Misc[::-]
  Ctrl+H        This help
  Ctrl+Q        Quit
[::b]Safety[::-]
  -read-only    Refuses every change; header shows READ-ONLY
  -dry-run      Records changes in the journal without making
                them; header shows DRY-RUN
  -protect P    Writes on/under prefix P need the prefix
                basename typed out first (repeat with commas)
  Deleting a directory always needs its name typed out,
  and its subtree is saved to an undo snapshot first
  (-no-snapshot turns that off).

[dim]↓,↑ · PgDn,PgUp · Home,End scroll   ·   Esc or q closes.[-]
`
	tv := tview.NewTextView()
	tv.SetDynamicColors(true)
	tv.SetTextAlign(tview.AlignLeft)
	tv.SetWordWrap(true)
	tv.SetText(helpText)
	// Scrollable is the default, but it is the whole point of this panel: the
	// binding list is longer than a short terminal, so it has to be reachable
	// by scrolling. The controller's input capture must let the scroll keys
	// through for this to mean anything.
	tv.SetScrollable(true)
	tv.SetBorder(true)
	// tview eats "[Esc]" as a colour tag, so the hint is escaped (DESIGN §12
	// B-13); the arrows survive because they are not plain letters.
	tv.SetTitle(" Hotkeys — [↓,↑] scroll · " + tview.Escape("[Esc]") + " closes ")

	return tv
}

func (v *View) NewExportInput(dir, defaultPath string) *tview.InputField {
	inp := tview.NewInputField().
		SetText(defaultPath)
	inp.SetBorder(true).SetTitle(fmt.Sprintf(" Export all keys under %q to JSON file ", dir))
	return inp
}

// NewFileBrowser returns an empty list styled for the JSON file picker.
// The controller populates and re-populates it as the user navigates.
func (v *View) NewFileBrowser(title string) *tview.List {
	l := tview.NewList().ShowSecondaryText(false)
	l.SetBorder(true).
		SetTitle(title).
		SetTitleAlign(tview.AlignLeft)
	l.SetSelectedTextColor(tcell.ColorBlack).
		SetSelectedBackgroundColor(tcell.ColorYellow)
	return l
}

// NewImportModeQ asks how to handle keys that already exist on import.
func (v *View) NewImportModeQ(details string) *tview.Modal {
	m := tview.NewModal()
	m.SetText(details).
		AddButtons([]string{"overwrite", "skip existing", "cancel"})
	return m
}

func (v *View) NewMultilineEditor(title, initial string) *tview.TextArea {
	ta := tview.NewTextArea().
		SetText(initial, false). // false -> caret at beginning (first line)
		SetPlaceholder("")
	ta.SetBorder(true).
		SetTitle(title + "  [Ctrl+S=Save | Esc=Cancel]")
	return ta
}

// OpenEditor replaces the Frame with a full-screen editor (hides bottom legend).
func (v *View) OpenEditor(p tview.Primitive) {
	editor := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(p, 0, 1, true)
	v.App.SetRoot(editor, true) // hide frame + legend while editing
	v.App.SetFocus(p)
}

// CloseEditor restores the normal UI.
func (v *View) CloseEditor() {
	v.App.SetRoot(v.Frame, true)
	v.App.SetFocus(v.List)
}

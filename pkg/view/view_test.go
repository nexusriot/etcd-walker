package view

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// The list carries every key binding as an input capture, and a capture only
// runs on the focused primitive. NewView must therefore hand the application a
// populated layout and leave the first pane focused: when the layout was
// filled after SetRoot, the running app started with no focus at all and not
// one list binding worked — while the UI looked perfectly normal.
func TestNewViewFocusesTheFirstPane(t *testing.T) {
	v := NewView()

	if v.Lists[0] == nil || v.Lists[1] == nil {
		t.Fatal("both panes must exist even in single-pane mode")
	}
	if v.List != v.Lists[0] {
		t.Error("View.List must start out pointing at the first pane")
	}
	if got := v.App.GetFocus(); got != v.Lists[0] {
		t.Errorf("focus = %T (%p), want the first list (%p)", got, got, v.Lists[0])
	}
	if !v.Lists[0].HasFocus() {
		t.Error("the first pane does not report focus, so its input capture never runs")
	}
}

// Switching layouts must keep both panes reachable and never drop the details
// pane, which is the only place a value is readable in full.
func TestSetDualRebuildsTheLayout(t *testing.T) {
	v := NewView()

	if got := v.Main.GetItemCount(); got != 2 {
		t.Errorf("single-pane layout has %d items, want list + details", got)
	}
	v.SetDual(true)
	if got := v.Main.GetItemCount(); got != 2 {
		t.Errorf("dual layout has %d items, want the pane row + details", got)
	}
	v.SetDual(false)
	if v.Main.GetItem(0) != v.Lists[0] || v.Main.GetItem(1) != v.Details {
		t.Error("returning to single-pane did not restore the list + details layout")
	}
}

// The hotkey panel is longer than a short terminal, so it has to be sized by
// the screen rather than by a constant. It used to be a fixed 30 rows: on an
// 80x24 terminal its last third — including the whole Safety section — was
// drawn past the bottom edge, where no amount of scrolling could reach it,
// because those rows are never drawn at all.
func TestHotkeysPanelFitsAShortTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 60}} {
		w, h := size[0], size[1]
		screen := tcell.NewSimulationScreen("UTF-8")
		if err := screen.Init(); err != nil {
			t.Fatal(err)
		}
		screen.SetSize(w, h)

		v := NewView()
		panel := v.ModalScroll(v.NewHotkeysModal(), 70)
		panel.SetRect(0, 0, w, h)
		panel.Draw(screen)
		screen.Show()

		// The panel's bottom border proves the whole box landed on screen.
		if !screenHasRune(screen, w, h, '┘') || !screenHasRune(screen, w, h, '┌') {
			t.Errorf("%dx%d: the panel's frame is not fully drawn — it is taller than the screen", w, h)
		}
		screen.Fini()
	}
}

func screenHasRune(screen tcell.SimulationScreen, w, h int, want rune) bool {
	cells, _, _ := screen.GetContents()
	for i := 0; i < w*h && i < len(cells); i++ {
		for _, r := range cells[i].Runes {
			if r == want {
				return true
			}
		}
	}
	return false
}

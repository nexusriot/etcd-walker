package controller

import (
	"strings"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/model"
)

func TestHistoryOpensPickerForKey(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		hist: map[string][]*model.Revision{"/k": {
			{Rev: 12, Version: 2, Value: "new"},
			{Rev: 5, Version: 1, Value: "old"},
		}},
	}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1) // row 0 is [..]

	c.history()

	if f.histCalls != 1 {
		t.Errorf("History called %d times, want 1", f.histCalls)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("revision picker page should be shown")
	}
}

func TestHistoryRefusedForDirectory(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/d", IsDir: true}}},
	}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.history()

	if f.histCalls != 0 {
		t.Errorf("History must not be called for a directory (calls=%d)", f.histCalls)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("an error modal should be shown for directories")
	}
}

// A key with only its current version stored gets an info modal, not an
// empty one-row picker.
func TestHistorySingleRevisionShowsInfo(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		hist: map[string][]*model.Revision{"/k": {
			{Rev: 7, Version: 3, Value: "v"}, // Version>1: older revs compacted
		}},
	}
	c := newTestController(f)
	c.updateList()
	c.view.List.SetCurrentItem(1)

	c.history()

	if c.view.Pages.HasPage("modal") {
		t.Error("no picker should open for a single-revision history")
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("an info modal should explain the single-revision case")
	}
}

// Restore must write the old value through SetKeepTTL with the key's *live*
// lease/TTL so the expiry survives the restore.
func TestRestoreRevisionPreservesLease(t *testing.T) {
	f := &fakeModel{
		nodes: map[string][]*model.Node{"/": {{Name: "/k"}}},
		gets: map[string]*model.Node{
			"/k": {Name: "/k", Value: "current", LeaseID: 7, TTL: 30},
		},
	}
	c := newTestController(f)
	c.updateList()

	c.restoreRevision(&model.Node{Name: "/k"}, &model.Revision{Rev: 5, Version: 1, Value: "old"})

	if len(f.setKeepCalls) != 1 {
		t.Fatalf("SetKeepTTL calls = %+v, want exactly one", f.setKeepCalls)
	}
	got := f.setKeepCalls[0]
	if got.key != "/k" || got.value != "old" || got.leaseID != 7 || got.ttl != 30 {
		t.Errorf("SetKeepTTL called with %+v, want /k/old/lease=7/ttl=30", got)
	}
	if !c.view.Pages.HasPage("modal-info") {
		t.Error("a success info modal should be shown after restore")
	}
}

// When the key vanished between opening history and restoring, the restore
// must fail with an error modal instead of writing blindly.
func TestRestoreRevisionMissingKeyErrors(t *testing.T) {
	f := &fakeModel{nodes: map[string][]*model.Node{"/": {}}}
	c := newTestController(f)
	c.updateList()

	c.restoreRevision(&model.Node{Name: "/gone"}, &model.Revision{Rev: 5, Value: "old"})

	if len(f.setKeepCalls) != 0 {
		t.Errorf("SetKeepTTL must not be called when the key is gone: %+v", f.setKeepCalls)
	}
	if !c.view.Pages.HasPage("modal") {
		t.Error("an error modal should be shown")
	}
}

func TestRevPreview(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "(empty)"},
		{"short", "short"},
		{"\xff\xfe", "(binary)"},
		{"line1\nline2", "line1…"},
	}
	for _, tc := range cases {
		if got := revPreview(tc.in); got != tc.want {
			t.Errorf("revPreview(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("x", 60)
	if got := revPreview(long); len([]rune(got)) != 39 { // 38 + ellipsis
		t.Errorf("long preview = %q (%d runes), want 39 runes", got, len([]rune(got)))
	}
}

func TestDiffLinesBasic(t *testing.T) {
	ops := diffLines("a\nb\nc", "a\nx\nc")
	want := []diffOp{{' ', "a"}, {'-', "b"}, {'+', "x"}, {' ', "c"}}
	if len(ops) != len(want) {
		t.Fatalf("ops = %+v, want %+v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("ops[%d] = %+v, want %+v", i, ops[i], want[i])
		}
	}
}

func TestDiffLinesInsertAndDeleteAtEnds(t *testing.T) {
	// Pure insertion into an empty value.
	ops := diffLines("", "a\nb")
	if minus, plus := diffCounts(ops); minus != 0 || plus != 2 {
		t.Errorf("empty->2 lines: -%d/+%d, want -0/+2", minus, plus)
	}
	// Deletion of a trailing line.
	ops = diffLines("a\nb", "a")
	if minus, plus := diffCounts(ops); minus != 1 || plus != 0 {
		t.Errorf("drop tail: -%d/+%d, want -1/+0", minus, plus)
	}
}

func TestDiffLinesIdentical(t *testing.T) {
	ops := diffLines("a\nb", "a\nb")
	if minus, plus := diffCounts(ops); minus != 0 || plus != 0 {
		t.Errorf("identical values: -%d/+%d, want -0/+0", minus, plus)
	}
}

func TestDiffLinesTooLargeReturnsNil(t *testing.T) {
	big := strings.Repeat("x\n", 1100) // 1101 lines squared > maxDiffCells
	if ops := diffLines(big, big+"y"); ops != nil {
		t.Errorf("oversized diff should return nil, got %d ops", len(ops))
	}
}

func TestRenderDiffCollapsesContext(t *testing.T) {
	base := strings.Repeat("same\n", 20) // 21 lines incl. trailing empty
	ops := diffLines(base+"old", base+"new")
	out := renderDiff(ops)

	if !strings.Contains(out, "unchanged lines") {
		t.Errorf("long context run should collapse:\n%s", out)
	}
	if !strings.Contains(out, "[red]- old[-]") || !strings.Contains(out, "[green]+ new[-]") {
		t.Errorf("changed lines missing or uncolored:\n%s", out)
	}
}

func TestRenderDiffKeepsShortContext(t *testing.T) {
	ops := diffLines("a\nb\nc\nd", "a\nb\nc\nx")
	out := renderDiff(ops)
	if strings.Contains(out, "unchanged lines") {
		t.Errorf("short context must not collapse:\n%s", out)
	}
	for _, want := range []string{"  a\n", "  b\n", "  c\n", "[red]- d[-]\n", "[green]+ x[-]\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered diff missing %q:\n%s", want, out)
		}
	}
}

package controller

import (
	"strings"
	"testing"

	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
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

// O-2: a huge printable value must not be rendered in full — it stalls the
// draw — but it has to stay reachable via [f].
func TestRevisionDetailCapsLargeValue(t *testing.T) {
	big := strings.Repeat("x", historyValueLimit*2) + "TAIL"
	nd := &model.Node{Name: "/k"}
	revs := []*model.Revision{{Rev: 9, Version: 2, Value: big}}

	c := newTestController(&fakeModel{})
	c.showRevisionDetail(nd, revs, 0, false, false)

	capped := frontPage(c).(*tview.TextView).GetText(true)
	if strings.Contains(capped, "TAIL") {
		t.Error("value rendered in full despite the cap")
	}
	if !strings.Contains(capped, "press [f] to show the whole value") {
		t.Errorf("no [f] affordance advertised:\n%s", capped[:200])
	}
	if !strings.Contains(capped, "first") {
		t.Error("header should say the value is truncated")
	}

	// [f] re-renders uncapped.
	c.showRevisionDetail(nd, revs, 0, false, true)
	full := frontPage(c).(*tview.TextView).GetText(true)
	if !strings.Contains(full, "TAIL") {
		t.Error("full view still truncated the value")
	}
}

// A value that fits is shown whole, with no [f] noise.
func TestRevisionDetailShowsSmallValueWhole(t *testing.T) {
	c := newTestController(&fakeModel{})
	c.showRevisionDetail(&model.Node{Name: "/k"}, []*model.Revision{{Rev: 9, Value: "short"}}, 0, false, false)

	got := frontPage(c).(*tview.TextView).GetText(true)
	if !strings.Contains(got, "short") {
		t.Error("small value missing")
	}
	if strings.Contains(got, "press [f]") {
		t.Error("[f] advertised for a value that is not truncated")
	}
}

// O-3: the diff must compare against the key as it is now, not against the
// picker's snapshot of "newest", which goes stale the moment anyone writes.
func TestRevisionDiffUsesLiveCurrentValue(t *testing.T) {
	f := &fakeModel{
		gets: map[string]*model.Node{
			"/k": {Name: "/k", Value: "live\n", ModRev: 42},
		},
	}
	revs := []*model.Revision{
		{Rev: 12, Version: 2, Value: "snapshot\n"}, // stale "newest"
		{Rev: 5, Version: 1, Value: "old\n"},
	}
	c := newTestController(f)

	c.showRevisionDiff(&model.Node{Name: "/k"}, revs, 1, false)

	got := frontPage(c).(*tview.TextView).GetText(true)
	if !strings.Contains(got, "live") {
		t.Errorf("diff did not use the live value:\n%s", got)
	}
	if strings.Contains(got, "snapshot") {
		t.Errorf("diff used the stale revs[0] snapshot:\n%s", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("diff should label the current side with the live revision:\n%s", got)
	}
	if !strings.Contains(got, "changed since this history was read") {
		t.Errorf("a drifted key should be called out:\n%s", got)
	}
}

// When nothing moved, the diff should not cry wolf.
func TestRevisionDiffQuietWhenUnchanged(t *testing.T) {
	f := &fakeModel{
		gets: map[string]*model.Node{"/k": {Name: "/k", Value: "new\n", ModRev: 12}},
	}
	revs := []*model.Revision{
		{Rev: 12, Version: 2, Value: "new\n"},
		{Rev: 5, Version: 1, Value: "old\n"},
	}
	c := newTestController(f)

	c.showRevisionDiff(&model.Node{Name: "/k"}, revs, 1, false)

	got := frontPage(c).(*tview.TextView).GetText(true)
	if strings.Contains(got, "changed since this history was read") {
		t.Errorf("spurious drift warning:\n%s", got)
	}
}

// A key that vanished while the history screen was open must not diff against
// a stale snapshot and pretend it is current.
func TestRevisionDiffErrorsWhenKeyGone(t *testing.T) {
	c := newTestController(&fakeModel{}) // Get always fails
	revs := []*model.Revision{
		{Rev: 12, Value: "new"},
		{Rev: 5, Value: "old"},
	}

	c.showRevisionDiff(&model.Node{Name: "/gone"}, revs, 1, false)

	if !c.view.Pages.HasPage("modal") {
		t.Fatal("expected an error modal")
	}
	if tv, ok := frontPage(c).(*tview.TextView); ok {
		t.Errorf("a diff was rendered anyway:\n%s", tv.GetText(true))
	}
}

// B-13: tview parses "[d]"/"[Esc]" in titles and text as colour tags and
// swallows them, so unescaped hotkey hints render as blanks. Every history
// screen advertises its keys in the title, so pin that they survive.
func TestHistoryHintsSurviveColorTagParsing(t *testing.T) {
	c := newTestController(&fakeModel{
		gets: map[string]*model.Node{"/k": {Name: "/k", Value: "live", ModRev: 12}},
	})
	revs := []*model.Revision{
		{Rev: 12, Version: 2, Value: "new"},
		{Rev: 5, Version: 1, Value: "old"},
	}
	nd := &model.Node{Name: "/k"}

	// rendered mimics what tview prints: titles go through the same colour-tag
	// parser as body text.
	rendered := func(title string) string {
		tv := tview.NewTextView().SetDynamicColors(true)
		tv.SetText(title)
		return tv.GetText(true)
	}

	c.showRevisionDetail(nd, revs, 1, false, false)
	got := rendered(frontPage(c).(*tview.TextView).GetTitle())
	for _, key := range []string{"[r]", "[d]", "[Esc]"} {
		if !strings.Contains(got, key) {
			t.Errorf("detail title lost %s: %q", key, got)
		}
	}

	c.showRevisionDiff(nd, revs, 1, false)
	got = rendered(frontPage(c).(*tview.TextView).GetTitle())
	if !strings.Contains(got, "[Esc]") {
		t.Errorf("diff title lost [Esc]: %q", got)
	}

	// The body's [f] affordance has to survive the same parser.
	big := []*model.Revision{{Rev: 9, Value: strings.Repeat("x", historyValueLimit*2)}}
	c.showRevisionDetail(nd, big, 0, false, false)
	body := frontPage(c).(*tview.TextView).GetText(true)
	if !strings.Contains(body, "[f]") {
		t.Errorf("body lost the [f] hint: %q", body)
	}
}

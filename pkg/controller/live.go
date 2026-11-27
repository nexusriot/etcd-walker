package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/nexusriot/etcd-walker/pkg/model"
	"github.com/rivo/tview"
	log "github.com/sirupsen/logrus"
)

// Live refresh (F-1).
//
// With live mode on, every pane showing the live cluster holds an etcd watch
// on its directory's prefix and re-lists itself when anything under it
// changes. The watch only says *that* something changed; the pane then re-reads
// its listing the ordinary way, so there is exactly one code path that knows
// how a directory is listed.
//
// Alongside the watches, a periodic Ping makes the connection state visible:
// the header reads LIVE while the cluster answers and turns red when it does
// not. A frozen tree that looks healthy is the failure this exists to prevent.
// When the cluster comes back every pane is re-read, because a v2 watch (and a
// v3 one past its compaction window) cannot replay what it missed.

const (
	// defaultLiveGap is the minimum spacing between two refreshes of one
	// pane. A busy prefix (Kubernetes /registry) changes many times a second;
	// re-listing on every event would make the UI unusable and hammer the
	// cluster. Events inside the gap are coalesced into one refresh.
	defaultLiveGap = 500 * time.Millisecond
	// defaultPingEvery is how often the connection is checked in live mode.
	defaultPingEvery = 5 * time.Second
)

// paneWatch is one running watch. gen tells a late event from a watch that
// has since been replaced apart from one belonging to the current watch.
type paneWatch struct {
	dir    string
	cancel context.CancelFunc
	gen    int
}

// liveState is embedded in Controller. Everything in it is owned by the UI
// goroutine; the watch and ping goroutines only ever reach it through c.queue.
type liveState struct {
	liveOn bool
	// watches is indexed by physical pane (0 = left, 1 = right), not by
	// active/inactive: the panes swap roles, the watches stay put.
	watches  [2]*paneWatch
	watchGen int
	// liveErr is the last watch/ping failure; nil means healthy.
	liveErr    error
	pingCancel context.CancelFunc
	// Overridable by tests; zero means the defaults above.
	liveGap   time.Duration
	pingEvery time.Duration
}

// SetLive turns live mode on or off.
func (c *Controller) SetLive(on bool) {
	if c.model == nil {
		return // startup failed: nothing to watch
	}
	was := c.liveOn
	c.liveOn = on
	if on {
		c.startPing()
	} else {
		c.stopPing()
		c.liveErr = nil
	}
	c.syncWatches()
	c.renderHeader()
	// A watch only reports changes from now on. Whatever changed while live
	// mode was off is already on the server, so re-read what is on screen
	// once, or the first thing live mode does is show a stale tree.
	if on && !was {
		for p := 0; p < 2; p++ {
			// currentNodes == nil: never listed yet (SetLive before Run);
			// the first listing will be fresh anyway.
			if c.watches[p] != nil && c.paneOf(p).currentNodes != nil {
				c.refreshPane(p)
			}
		}
	}
}

// toggleLive is the Ctrl+L binding.
func (c *Controller) toggleLive() *tcell.EventKey {
	c.SetLive(!c.liveOn)
	if c.liveOn && c.atRevision() {
		c.info("Live refresh on", fmt.Sprintf(
			"This pane is pinned to revision %d, which never changes, so it is not watched. "+
				"It goes live as soon as you return to the present (Ctrl+G, empty).", c.rev))
	}
	return nil
}

// stopLive tears everything down on quit, so no goroutine outlives the UI.
func (c *Controller) stopLive() {
	c.liveOn = false
	c.stopPing()
	c.syncWatches()
}

// paneOf returns the state of physical pane p, whichever role it has now.
func (c *Controller) paneOf(p int) *paneState {
	if p == c.active {
		return &c.paneState
	}
	return &c.inactive
}

// syncWatches makes the running watches match what is on screen: one per
// visible pane that shows the live cluster, on that pane's directory. It is
// called after every listing, so navigation, time travel and toggling the
// second pane all re-point the watches without each having to remember to.
func (c *Controller) syncWatches() {
	for p := 0; p < 2; p++ {
		st := c.paneOf(p)
		want := c.liveOn && c.model != nil && st.rev == 0 && (p == c.active || c.dual)
		dir := normAbs(st.currentDir)
		w := c.watches[p]
		if w != nil && want && w.dir == dir {
			continue
		}
		if w != nil {
			w.cancel()
			c.watches[p] = nil
		}
		if want {
			c.startWatch(p, dir)
		}
	}
}

func (c *Controller) startWatch(p int, dir string) {
	ctx, cancel := context.WithCancel(context.Background())
	c.watchGen++
	gen := c.watchGen
	c.watches[p] = &paneWatch{dir: dir, cancel: cancel, gen: gen}
	log.Debugf("live: watching %s for pane %d (gen %d)", dir, p, gen)
	ch := c.model.Watch(ctx, dir)
	gap := c.liveGap
	if gap <= 0 {
		gap = defaultLiveGap
	}
	queue := c.queue
	go pumpWatch(ctx, ch, gap, func(err error) {
		queue(func() { c.onWatch(p, gen, err) })
	})
}

// pumpWatch turns a raw event stream into deliveries: errors pass straight
// through, change notifications are coalesced so that at most one refresh is
// delivered per gap. The first change after a quiet spell is delivered at
// once — the gap only throttles bursts, it does not delay a lone change.
func pumpWatch(ctx context.Context, ch <-chan model.WatchEvent, gap time.Duration, deliver func(error)) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var last time.Time
	pending := false
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Err != nil {
				deliver(ev.Err)
				continue
			}
			if !pending {
				pending = true
				d := gap - time.Since(last)
				if d < 0 {
					d = 0
				}
				timer.Reset(d)
			}
		case <-timer.C:
			pending = false
			last = time.Now()
			deliver(nil)
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// onWatch runs on the UI goroutine for every delivery of watch gen on pane p.
func (c *Controller) onWatch(p, gen int, err error) {
	w := c.watches[p]
	if w == nil || w.gen != gen {
		return // that watch was replaced or stopped; its news is stale
	}
	if err != nil {
		log.Debugf("live: watch on %s failed: %v", w.dir, err)
		c.setLiveErr(err)
		return
	}
	c.setLiveErr(nil)
	c.refreshPane(p)
}

// refreshPane re-reads physical pane p in place.
func (c *Controller) refreshPane(p int) {
	if p == c.active {
		c.refreshActive(true)
		return
	}
	if !c.dual {
		return
	}
	c.withOtherPane(func() { c.refreshActive(false) })
}

// refreshActive re-lists the active pane, keeping the cursor on the same
// entry (or, if it vanished, at the same row). Unlike updateList it never
// pops a modal: a failed background refresh goes to the header, and the
// listing on screen stays as it was rather than jumping to another directory.
func (c *Controller) refreshActive(details bool) {
	mk := currentMapKeyOf(c.view.List)
	idx := c.view.List.GetCurrentItem()
	if err := c.makeNodeMap(); err != nil {
		c.setLiveErr(err)
		return
	}
	c.quietDetails = true
	c.renderList()
	row := -1
	for i := 0; i < c.view.List.GetItemCount(); i++ {
		if _, sec := c.view.List.GetItemText(i); strings.TrimSpace(sec) == mk {
			row = i
			break
		}
	}
	if row < 0 {
		row = idx
		if n := c.view.List.GetItemCount(); row >= n {
			row = n - 1
		}
	}
	if row >= 0 {
		c.view.List.SetCurrentItem(row)
	}
	c.quietDetails = false
	if details {
		c.fillDetails(currentMapKeyOf(c.view.List))
	}
}

func (c *Controller) startPing() {
	if c.pingCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.pingCancel = cancel
	every := c.pingEvery
	if every <= 0 {
		every = defaultPingEvery
	}
	m, queue := c.model, c.queue
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := m.Ping()
			queue(func() {
				if ctx.Err() == nil {
					c.onPing(err)
				}
			})
		}
	}()
}

func (c *Controller) stopPing() {
	if c.pingCancel != nil {
		c.pingCancel()
		c.pingCancel = nil
	}
}

// onPing records the connection state. Coming back from a failure restarts
// every watch from the present and re-reads every visible pane: whatever
// changed during the outage may not be replayable by the old watch, and the
// cluster that came back may not even be the one that went away (recreated or
// restored, with its revision counter reset).
func (c *Controller) onPing(err error) {
	wasDown := c.liveErr != nil
	c.setLiveErr(err)
	if err == nil && wasDown {
		for p := 0; p < 2; p++ {
			if w := c.watches[p]; w != nil {
				w.cancel()
				c.startWatch(p, w.dir) // new watch first, then the re-read: no gap
				c.refreshPane(p)
			}
		}
	}
}

func (c *Controller) setLiveErr(err error) {
	if (err == nil) == (c.liveErr == nil) && (err == nil || err.Error() == c.liveErr.Error()) {
		c.liveErr = err
		return // unchanged: skip the redraw
	}
	c.liveErr = err
	c.renderHeader()
}

// renderHeader draws the status line: the static part plus, in live mode, the
// connection indicator.
func (c *Controller) renderHeader() {
	text, color := liveHeader(c.headerBase, c.headerColor, c.liveOn, c.liveErr)
	c.view.SetHeader(text, color)
}

// liveHeader appends the live-mode indicator to the static header. The error
// text is escaped: etcd errors routinely contain brackets, and an unescaped
// one would be eaten by tview's colour-tag parser (DESIGN §12 B-13).
func liveHeader(base string, color tcell.Color, on bool, err error) (string, tcell.Color) {
	if !on {
		return base, color
	}
	if err == nil {
		return base + "  |  ● LIVE", color
	}
	msg := err.Error()
	if r := []rune(msg); len(r) > 60 {
		msg = string(r[:57]) + "..."
	}
	return base + "  |  " + tview.Escape("⚠ LIVE: "+msg), tcell.ColorRed
}

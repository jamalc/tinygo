// Package clock runs the poll-and-animate loop shared by the simulator
// and the on-device firmware.
//
// It periodically fetches arrivals from the service and pages the display
// through arrival groups, marqueeing each group's stop name and headsign.
//
// It also owns the on-display rendering (arrival groups, status messages, the
// color palette, and drawing primitives), in render.go and colors.go. These
// are kept in this package until something else needs to reuse them; only Info
// and Error are exported, for the device bootstrap.
package clock

import (
	"image/color"
	"log"
	"runtime"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/client"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/input"
)

// tickInterval paces the render loop; timers (poll, group cycle) are checked
// once per tick.
const tickInterval = 100 * time.Millisecond

// pollSlice is how often the buttons are sampled while waiting out a
// tick — frequent enough that quick taps and the releases between
// rapid taps aren't lost (see the latch loop at the bottom of Run).
const pollSlice = 5 * time.Millisecond

// navigate applies a button press to the group cursor, moving it by whole
// pages of slots groups and wrapping around n groups. input.Up selects the
// previous page, input.Down the next. The cursor stays page-aligned (a multiple
// of slots), so the same groups always appear together and a short final page
// shows on its own. This is the single spot that decides what the buttons mean;
// future behaviors (e.g. a direction filter) grow here without touching the
// input.Source port or adapters.
func navigate(cursor int, b input.Button, n, slots int) int {
	if n == 0 || slots <= 0 {
		return 0
	}
	switch b {
	case input.Down:
		next := cursor + slots
		if next >= n {
			return 0 // past the last page: wrap to the first
		}
		return next
	case input.Up:
		prev := cursor - slots
		if prev < 0 {
			return ((n - 1) / slots) * slots // first page: wrap to the last page's start
		}
		return prev
	default:
		return cursor
	}
}

// Display is the subset of a panel the loop drives: a drivers.Displayer that
// can also be cleared. Both hub75.Device and simulator.Display satisfy it.
type Display interface {
	// Size returns the current size of the display.
	Size() (x, y int16)

	// SetPixel modifies the internal buffer.
	SetPixel(x, y int16, c color.RGBA)

	// Display sends the buffer (if any) to the screen.
	Display() error
	Clear()
}

// fetchTimeout is how long a poll may run before the loop surfaces "fetch slow" to the display.
// The fetch itself is not abandoned (espradio can't cancel a stuck connect, and its own stack
// eventually times a dead connect out); this only bounds how long the UI stays silent about it.
const fetchTimeout = 12 * time.Second

// fetchResult carries a completed poll's outcome from the fetch goroutine to the render loop.
type fetchResult struct {
	groups []api.FlatArrivalGroup
	date   time.Time // the server's clock at the response; zero if it didn't say
	err    error
}

// fetcher runs arrivals polls on a single long-lived goroutine. This keeps the render loop from
// ever blocking on the network, and — decisively on the RAM-tight ESP32-S3 — lets each poll REUSE
// its buffers instead of allocating fresh ones: respBuf and a double-buffered pair of groups
// slices are allocated once, so only the JSON strings (small, hole-friendly) allocate per fetch.
// TinyGo's heap never compacts, so allocating big per-poll buffers fragments it until a fetch
// can't find a contiguous block (see the repo README, "Memory on TinyGo"). One goroutine also means a stalled
// dial can never pile up.
type fetcher struct {
	c       *client.Client
	reqCh   chan struct{}
	resCh   chan fetchResult
	bufs    [2]api.Board // double-buffered decode targets: render reads one while the goroutine fills the other
	cur     int          // index the NEXT fetch decodes into — touched only by the goroutine
	respBuf []byte       // reused HTTP body buffer — touched only by the goroutine
}

// newFetcher starts the fetch goroutine and returns the handle the render loop drives.
func newFetcher(c *client.Client) *fetcher {
	f := &fetcher{c: c, reqCh: make(chan struct{}, 1), resCh: make(chan fetchResult, 1)}
	go f.run()
	return f
}

// run is the fetch goroutine: wait for a request, fetch into the idle groups buffer (reusing
// respBuf and the buffer's backing), deliver the result, then flip to the other buffer so the
// render loop can keep reading the one just delivered while the next fetch fills its sibling.
func (f *fetcher) run() {
	for range f.reqCh {
		dst := &f.bufs[f.cur]
		date, err := f.c.LoadArrivals(dst, &f.respBuf)
		if err != nil {
			f.resCh <- fetchResult{err: err}
			continue
		}
		f.resCh <- fetchResult{groups: dst.Groups, date: date}
		f.cur ^= 1
	}
}

// request asks for a fetch. It never blocks: reqCh is buffered to 1 and the caller only requests
// when no fetch is in flight (fetchPending), so the send always succeeds and the goroutine picks
// it up. The render loop then blocks on resCh for the result (see Run).
func (f *fetcher) request() {
	select {
	case f.reqCh <- struct{}{}:
	default:
	}
}

// Run polls the arrivals service and animates the display. It loops forever
// and does not return under normal operation.
func Run(cfg config.Config, display Display, in input.Source) {
	if cfg.PollInterval == 0 || cfg.GroupInterval == 0 {
		log.Fatal("config must specify poll_interval and group_interval")
	}

	f := newFetcher(client.NewClient(cfg))

	// The layout mode and its page grid. The classic layout gives each group a
	// two-line slotHeight cell (route bullet, marqueed stop/headsign, ETAs); a
	// cell 128px or wider holds the stop name fixed and marquees only the
	// headsign. The compact layout gives each group a single 8px row (chip,
	// route, direction arrow, and the ETAs), so the same panel holds twice as
	// many groups. slots is how many
	// groups one page holds, one per cell.
	mode := layoutFromConfig(cfg.Layout)
	rowH := int16(slotHeight)
	if mode == layoutCompact {
		rowH = compactRowH
	}
	dispW, dispH := display.Size()
	g := gridForRows(dispW, dispH, cfg.Columns, rowH)
	slots := g.cells()

	// The board layout keeps its own state and ignores the page grid above: it
	// shows everything at once, so there's nothing to page through.
	var b *board
	if mode == layoutBoard {
		b = newBoard(dispW, dispH, cfg.Walk, cfg.PollInterval)
	}

	var groups []api.FlatArrivalGroup
	// Start one page before the first so the immediate auto-cycle on the first
	// tick lands on page 0, showing the first slots groups.
	currentGroup := -slots
	// scrollTick advances the line-0 marquee; it resets whenever the shown page
	// changes so each group starts sweeping from the left.
	scrollTick := 0
	// endHold counts ticks the marquee has rested at the start while a group
	// cycle is pending, so the text pauses there before the page swaps.
	endHold := 0
	lastPoll := time.Now().Add(-cfg.PollInterval)
	lastGroup := time.Now().Add(-cfg.GroupInterval)
	// pending is the button press latched while waiting out the last tick,
	// handled on the following pass.
	pending := input.None
	// Fetch state: fetchPending is true while a poll is in flight on the fetcher's
	// goroutine; warnedSlow makes the "fetch slow" notice fire once per in-flight
	// poll; fetchStart is when the in-flight poll was requested, which is what
	// slowness is measured against.
	fetchPending := false
	warnedSlow := false
	var fetchStart time.Time
	// page is the drawn page's text and marquee pace, rebuilt only when the page
	// or the arrivals data changes — NOT per tick. See pageText.
	var page pageText
	pageDirty := true

	for {
		now := time.Now()
		prevGroup := currentGroup

		// Poll when due: request a fetch on the fetcher's goroutine and keep
		// animating; the result is collected on a later tick. The loop never blocks
		// on the network — one fetch is in flight at a time, we never advance
		// lastPoll or start another while it runs, and the buffers stay owned by
		// the one goroutine, so a stalled dial can't pile anything up. (The loop
		// used to park here through every fetch for espradio's sake; the crash that
		// justified that was confounded with the churn fix — see the repo README,
		// "Memory on TinyGo" — and the parked arrangement lives in git history if a
		// cold connect ever destabilizes again.)
		if now.Sub(lastPoll) >= cfg.PollInterval && !fetchPending {
			// Reclaim the previous poll's JSON garbage before this fetch allocates. TinyGo's
			// GC otherwise runs only on allocation failure, by which point the tight ESP32-S3
			// heap may be too fragmented to satisfy a request. Panel refresh is DMA-driven,
			// so the GC pause here doesn't tear.
			runtime.GC()
			f.request()
			fetchPending = true
			warnedSlow = false
			fetchStart = now
		}
		if fetchPending {
			select {
			case r := <-f.resCh:
				fetchPending = false
				lastPoll = now
				if r.err != nil {
					// Only take over the screen when there's nothing on it yet. Once
					// arrivals are showing, a failed poll keeps them up — the next tick
					// would redraw over the message within milliseconds anyway, so all
					// it did was flash — and the board marks the data's age instead.
					if len(groups) == 0 {
						Warning(display, "Failed to fetch arrivals: "+r.err.Error())
					} else {
						println("fetch failed:", r.err.Error())
					}
				} else {
					groups = r.groups
					if b != nil {
						b.update(groups, now, r.date)
					}
					pageDirty = true // new data: the page's text is stale
					if len(groups) > 0 {
						// Keep the cursor in range and page-aligned after the group
						// count changes. Integer division preserves the negative
						// startup sentinel (-slots stays -slots).
						currentGroup = ((currentGroup % len(groups)) / slots) * slots
					} else {
						currentGroup = 0
					}
				}
			default:
				if !warnedSlow && now.Sub(fetchStart) >= fetchTimeout {
					if len(groups) == 0 {
						Warning(display, "Fetch slow…")
					} else {
						println("fetch slow")
					}
					warnedSlow = true
				}
			}
		}

		btn := pending
		pending = input.None
		freeze := false // hold the marquee in place this tick (end-of-page pause)
		if b != nil {
			// The board draws once a poll has landed, and after that only when
			// what it shows changes. The buttons have nothing to page.
			if !b.lastOK.IsZero() {
				b.render(display, now)
			}
		} else if len(groups) > 0 {
			// The navigation decision reads the pace of the page currently SHOWN,
			// so it uses the cached value from before any page change below.
			pc := page.pace

			switch {
			case btn != input.None:
				// Manual navigation: page the cursor and restart the auto-cycle
				// timer so the press isn't immediately undone.
				currentGroup = navigate(currentGroup, btn, len(groups), slots)
				lastGroup = now
				endHold = 0
			case now.Sub(lastGroup) >= cfg.GroupInterval:
				switch {
				case mode == layoutCompact:
					// The compact layout doesn't marquee, so there is no lap to
					// finish — swap the page as soon as it's due.
					currentGroup = navigate(currentGroup, input.Down, len(groups), slots)
					lastGroup = now
				case currentGroup >= 0 && endHold == 0 && scrollTick%pc.cycle >= marqueeHold:
					// A page swap is due, but hold off until the marquee has finished
					// its lap and returned to the start (resting there marqueeEndHold
					// ticks) so the page never swaps mid-scroll.
				case currentGroup >= 0 && endHold < marqueeEndHold:
					endHold++
					freeze = true
				default:
					currentGroup = navigate(currentGroup, input.Down, len(groups), slots)
					lastGroup = now
					endHold = 0
				}
			}

			if currentGroup != prevGroup {
				scrollTick = 0 // new page: restart the marquee
				pageDirty = true
			} else if !freeze {
				scrollTick++
			}

			// Compose the page's text at most once per page or per poll. Every
			// other tick reuses it, and only the classic marquee's offset moves.
			if pageDirty {
				if mode == layoutCompact {
					page.buildCompact(groups, currentGroup, g)
				} else {
					page.build(groups, currentGroup, g)
				}
				pageDirty = false
			}

			display.Clear()
			for i := range page.cells {
				if mode == layoutCompact {
					drawCompactGroup(display, page.cells[i])
				} else {
					drawGroup(display, page.cells[i], scrollTick, page.pace)
				}
			}
			display.Display()
		}

		// Wait out the tick while sampling the buttons every few
		// milliseconds. Sampling only once per 100 ms tick loses input:
		// taps shorter than the tick fall between samples, and rapid taps
		// merge into one apparent hold. The first press is latched and
		// handled on the next pass.
		deadline := time.Now().Add(tickInterval)
		for {
			if b := in.Poll(); pending == input.None && b != input.None {
				pending = b
			}
			if !time.Now().Before(deadline) {
				break
			}
			time.Sleep(pollSlice)
		}
	}
}

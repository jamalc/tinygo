// Package docks runs the Citi Bike dock board: how many open docks there are,
// right now, at a few nearby stations, so you know where to bring a bike back.
//
// It polls the serve backend's /bikes endpoint for the stations in the config's
// bikes: list (the full Citi Bike feed is a megabyte, far too much for the
// device, so the server keeps it and hands out only these) and draws one block
// per station, in config order. Like internal/clock it's portable: it drives any
// Display, so the same code runs on the device and in the simulator.
//
// The loop has the clock's shape, for the clock's reasons (see the repo README,
// "Memory on TinyGo"): one long-lived fetch goroutine with reused buffers, and a
// render loop that never blocks on the network and redraws only when what it
// shows changes.
package docks

import (
	"image/color"
	"log"
	"runtime"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/client"
	"github.com/jamalc/tinygo/internal/clock"
	"github.com/jamalc/tinygo/internal/config"
)

// Display is the subset of a panel the loop drives. Both hub75.Device and
// simulator.Display satisfy it.
type Display interface {
	Size() (x, y int16)
	SetPixel(x, y int16, c color.RGBA)
	Display() error
	Clear()
}

const (
	// tickInterval paces the loop. Nothing animates, so it only bounds how soon
	// a landed poll or a change in staleness shows.
	tickInterval = 250 * time.Millisecond

	// defaultPoll is the poll interval when the config doesn't set one. Citi
	// Bike's feed refreshes about once a minute, and the server polls it on its
	// own schedule, so polling the server much faster buys nothing.
	defaultPoll = 30 * time.Second

	// fetchTimeout is how long a poll may run before the loop says so, while
	// there's nothing on the screen yet. As in the clock, the fetch itself isn't
	// abandoned; this only bounds how long the panel stays silent about it.
	fetchTimeout = 12 * time.Second
)

// fetchResult carries a completed poll from the fetch goroutine to the loop.
type fetchResult struct {
	stations []api.BikeStation
	err      error
}

// fetcher runs polls on one long-lived goroutine, decoding into a
// double-buffered pair of station slices and one reused response buffer, so a
// steady loop makes no large per-poll allocation. The loop reads the buffer
// just delivered while the next poll fills its sibling.
type fetcher struct {
	c       *client.BikeClient
	reqCh   chan struct{}
	resCh   chan fetchResult
	bufs    [2][]api.BikeStation
	cur     int    // the buffer the next poll decodes into; the goroutine's alone
	respBuf []byte // the goroutine's alone
}

func newFetcher(c *client.BikeClient) *fetcher {
	f := &fetcher{c: c, reqCh: make(chan struct{}, 1), resCh: make(chan fetchResult, 1)}
	go f.run()
	return f
}

func (f *fetcher) run() {
	for range f.reqCh {
		dst := &f.bufs[f.cur]
		if _, err := f.c.LoadStations(dst, &f.respBuf); err != nil {
			f.resCh <- fetchResult{err: err}
			continue
		}
		f.resCh <- fetchResult{stations: *dst}
		f.cur ^= 1
	}
}

// request asks for a poll. The loop only asks when none is in flight, so the
// buffered send always lands.
func (f *fetcher) request() {
	select {
	case f.reqCh <- struct{}{}:
	default:
	}
}

// Run polls the server for the configured stations and draws them. It loops
// forever and does not return under normal operation; with no stations
// configured it says so on the panel and returns.
func Run(cfg config.Config, display Display) {
	if len(cfg.Bikes) == 0 {
		clock.Error(display, "No stations: list Citi Bike station numbers under bikes: in config.txt")
		log.Println("docks: no bikes: stations in config")
		return
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}

	f := newFetcher(client.NewBikeClient(cfg))
	w, h := display.Size()
	b := newBoard(w, h, poll)

	clock.Info(display, "Loading docks")

	lastPoll := time.Now().Add(-poll)
	pending := false
	warnedSlow := false
	var fetchStart time.Time

	for {
		now := time.Now()
		if !pending && now.Sub(lastPoll) >= poll {
			// Reclaim the last poll's JSON garbage before this one allocates; see
			// the same call in internal/clock.
			runtime.GC()
			f.request()
			pending, warnedSlow, fetchStart = true, false, now
		}
		if pending {
			select {
			case r := <-f.resCh:
				pending, lastPoll = false, now
				switch {
				case r.err == nil:
					b.update(r.stations, now)
				case b.lastOK.IsZero():
					// Nothing on screen yet, so the panel may as well say why.
					clock.Warning(display, "Failed to fetch docks: "+r.err.Error())
				default:
					// Keep the last good numbers up; the board marks their age.
					println("docks fetch failed:", r.err.Error())
				}
			default:
				if !warnedSlow && now.Sub(fetchStart) >= fetchTimeout {
					if b.lastOK.IsZero() {
						clock.Warning(display, "Fetch slow…")
					} else {
						println("docks fetch slow")
					}
					warnedSlow = true
				}
			}
		}

		if !b.lastOK.IsZero() {
			b.render(display, now)
		}
		time.Sleep(tickInterval)
	}
}

//go:build tinygo

// Command clock is the TinyGo firmware for the subway clock.
//
// It connects to WiFi, fetches arrival data from the server, and renders it on
// the LED matrix. It also reads the UP/DOWN buttons, and a rotary encoder on the
// STEMMA QT / Qw/ST port if one is plugged in, to page through arrival groups
// when there are more than fit on the display. Specify the server
// address and other parameters in the embedded config.txt file.
//
// It reports heap usage over serial at each startup stage (see reportHeap) and
// then every heapSampleInterval while running (sampleHeap), so the cost of a
// driver tuning change can be read off a real flash at the app's real geometry
// rather than estimated, and so steady-state growth is visible at all. cmd/bench
// measures the same startup step but at the canonical 64x32 DefaultConfig; this
// measures whatever config.txt actually says, alongside the WiFi stack that
// competes with it for the heap.
//
// The sampler lives here rather than in internal/hub75 on purpose. A driver has no
// business owning a goroutine or a serial console — and its buffers are allocated
// once at New and never grow, so periodically reprinting its footprint would just
// reprint a constant. What moves over time is the app.
//
// Usage:
//
//	make clock [BOARD=...]
package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	"github.com/jamalc/tinygo/internal/buttons"
	"github.com/jamalc/tinygo/internal/client"
	"github.com/jamalc/tinygo/internal/clock"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/encoder"
	"github.com/jamalc/tinygo/internal/hub75"
	"github.com/jamalc/tinygo/internal/input"
	"github.com/jamalc/tinygo/internal/wifi"
)

func main() {
	cfg := config.New()

	base := heapInuse()
	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		// A driver that won't fit fails here with ErrTooBig rather than OOM-ing,
		// so say what was being asked for — the geometry is the reason.
		fmt.Printf("hub75 New failed at %dx%d depth %d double=%v: %v\n",
			cfg.Width, cfg.Height, cfg.ColorDepth, cfg.DoubleBuffer, err)
		panic(err)
	}
	afterDriver := heapInuse()

	fmt.Printf("heap: %s driver %d B at %dx%d depth %d double=%v\n",
		hub75.DriverName(), afterDriver-base,
		cfg.Width, cfg.Height, cfg.ColorDepth, cfg.DoubleBuffer)

	clock.Info(panel, "Connecting to WiFi")
	if err := wifi.Connect(cfg.SSID, cfg.Passphrase); err != nil {
		clock.Error(panel, "Failed to connect to WiFi: "+err.Error())
		return
	}
	clock.Info(panel, "Connected to WiFi")

	// WiFi is the other big claimant on the same heap, and the pair of numbers is
	// the point: a driver that fits alone can still leave too little for it.
	afterWiFi := heapInuse()
	reportHeap(afterDriver-base, afterWiFi-afterDriver)

	// Print each fetch's allocation broken down by phase (see trace.go and
	// the repo README, "Memory on TinyGo"). Costs one heap walk per phase per poll — fine at
	// a 30s poll, not something to leave on in a tight loop.
	client.TraceAlloc = true

	// Everything above is a one-shot: the driver's buffers are allocated at New and
	// never grow, so its cost is a constant. What moves is the app — poll buffers,
	// the arrivals it decodes, whatever WiFi holds between fetches — which is why
	// the periodic sampler starts here, with the steady state as its baseline.
	go sampleHeap(heapSampleInterval)

	// The knob pages alongside the buttons: one page per detent, clockwise
	// forward. With no encoder plugged in, the probe finds nothing and its pager
	// polls None forever, leaving the buttons as they were.
	clock.Run(cfg, panel, input.Merge(buttons.New(), encoder.New().Pager()))
}

// heapSampleInterval is how often the running clock reports its heap. Set to 0 to
// turn the sampler off.
//
// 60s is slow enough that the console stays readable over hours and fast enough
// that a leak is a visible trend within a few minutes. Note each sample forces a
// collection (see sampleHeap), so this is not free — it is not meant to be tight.
const heapSampleInterval = 60 * time.Second

// heapInuse returns bytes of live heap, after a collection so the figure is the
// retained cost rather than whatever garbage happened to be pending. Same
// methodology as cmd/bench, deliberately — the two numbers are meant to be
// comparable.
//
// The collection is the point, not an accident: TinyGo's ReadMemStats counts
// allocated blocks from the heap metadata without marking, so an unforced reading
// includes every dead object not yet swept. That is fine for a rate but useless
// for "is this growing".
func heapInuse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return uint64(m.HeapInuse)
}

// sampleHeap reports the heap every d until the board is reset. It prints two
// independent signals, because they fail differently:
//
//   - live + its delta — RETENTION. A leak shows here as a delta that stays
//     positive sample after sample. A steady app holds this flat.
//   - allocated since last — CHURN. This climbs even when live is flat; it says
//     how hard the app is working the collector, which is what drives GC pauses.
//
// A leak looks like rising live with steady churn. Heavy-but-healthy work looks
// like flat live with high churn. Reporting only one of them cannot tell those
// apart.
func sampleHeap(d time.Duration) {
	if d <= 0 {
		return
	}

	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	prevLive, prevTotal := m.HeapInuse, m.TotalAlloc

	for {
		time.Sleep(d)

		runtime.GC()
		runtime.ReadMemStats(&m)

		fmt.Printf("heap: live %d B (%+d) | free %d B | churn %d B | %d objects\n",
			m.HeapInuse, int64(m.HeapInuse)-int64(prevLive),
			m.HeapIdle, m.TotalAlloc-prevTotal, m.HeapObjects)

		prevLive, prevTotal = m.HeapInuse, m.TotalAlloc
	}
}

// reportHeap prints the startup memory picture: what the driver took, what WiFi
// took on top of it, and what is left. Free heap is the number that decides
// whether a tuning change is safe — a driver saving is only real if it shows up
// here.
func reportHeap(driver, wifi uint64) {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("heap: driver %d B + wifi %d B = %d B live, %d B free (%d B total)\n",
		driver, wifi, uint64(m.HeapInuse), uint64(m.HeapIdle), uint64(m.HeapSys))
}

//go:build tinygo

// Command bench measures the lab's comparison metrics for whichever
// HUB75 driver the build selected, and prints them over serial as the
// raw material for a measurement row in internal/hub75/README.md. The
// driver is selected by the board
// (one driver per chip; see internal/hub75), so pick it with BOARD.
//
// Usage:
//
//	make bench BOARD=matrixportal-m4    # evsys; then read the serial monitor
//	make bench BOARD=interstate-75w     # pio
//	make bench BOARD=matrixportal-s3    # lcd
//
// Methodology:
//   - Driver heap: runtime.ReadMemStats before/after hub75.New
//     (static segments come from `make sizes` on the host).
//   - Scanout CPU: a calibrated spin benchmark — work batches per
//     500 ms window — before Configure (baseline) and after (with
//     refresh live). The deficit is everything the engine costs the
//     application: ISR time, DMA bus contention, all of it.
//   - Frame period: 100 back-to-back Displays. Each Display re-encodes the
//     grid, so this is encode+present, not the pure hardware refresh (which,
//     for the free-running engines, is higher).
//   - Per-frame cost: Clear + a full SetPixel sweep + Display, timed as one
//     unit through the Panel interface — the app's actual hot path.
package main

import (
	"image/color"
	"runtime"
	"time"

	"github.com/jamalc/tinygo/internal/hub75"
)

// benchSink defeats dead-code elimination of the spin workload.
var benchSink uint32

// spinRate counts 1000-iteration work batches completed in 500 ms.
func spinRate() uint32 {
	deadline := time.Now().Add(500 * time.Millisecond)
	var batches uint32
	for time.Now().Before(deadline) {
		for i := uint32(0); i < 1000; i++ {
			benchSink += i
		}
		batches++
	}
	return batches
}

func heapInuse() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return uint64(m.HeapInuse)
}

func main() {
	time.Sleep(3 * time.Second) // let the serial monitor attach
	// Bench the canonical DefaultConfig (64×32, 8-bit, single-buffered), NOT the app's
	// config.txt — so the numbers are reproducible and comparable across boards regardless of
	// any local config.
	cfg := hub75.DefaultConfig()
	width, height := cfg.Width, cfg.Height
	println("hub75 bench | driver:", hub75.DriverName(), "| panel:", width, "x", height,
		"| colordepth:", cfg.ColorDepth, "| doublebuffer:", cfg.DoubleBuffer)

	heap0 := heapInuse()
	spin0 := spinRate()

	panel, err := hub75.New(cfg)
	if err != nil {
		println("Configure failed:", err.Error())
		halt()
	}

	heap1 := heapInuse()
	spin1 := spinRate()

	println("driver heap:", uint32(heap1-heap0), "bytes")
	println("spin batches/500ms: baseline", spin0, "| with refresh", spin1)
	if spin1 < spin0 {
		permille := (spin0 - spin1) * 1000 / spin0
		println("scanout cpu overhead:", permille/10, ".", permille%10, "%")
	} else {
		println("scanout cpu overhead: ~0%")
	}

	// Frame period: warm up the swap pipeline, then time 100 frames.
	for i := 0; i < 5; i++ {
		panel.Display()
	}
	t0 := time.Now()
	const frames = 100
	for i := 0; i < frames; i++ {
		panel.Display()
	}
	per := time.Since(t0) / frames
	println("frame period:", uint32(per.Microseconds()), "µs | rate:",
		uint32(time.Second/per), "Hz (encode+present, not pure refresh)")

	// Per-frame cost: Clear + a full SetPixel sweep + Display, timed as one
	// unit — the app's actual hot path (the encode happens inside Display).
	// Averaged over 10 frames.
	t0 = time.Now()
	const draws = 10
	for f := 0; f < draws; f++ {
		panel.Clear()
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				panel.SetPixel(int16(x), int16(y), color.RGBA{
					R: uint8(x * 4), G: uint8(y * 8), B: uint8((x + y + f) * 3), A: 255,
				})
			}
		}
		panel.Display()
	}
	perFrame := time.Since(t0) / draws
	println("produce+present per frame (Clear + 2048 SetPixel + Display):",
		uint32(perFrame.Microseconds()), "µs")

	println("bench complete; leaving the gradient up")
	halt()
}

// halt parks the program forever while leaving the panel refreshing. It sleeps in a loop rather
// than `select {}`: a bare select has no event source, which the ESP32-S3 scheduler detects as a
// deadlock and aborts ("deadlocked: no event source"). A timer-backed sleep keeps an event
// source alive, so the engine's DMA/peripheral refresh keeps running on every target.
func halt() {
	for {
		time.Sleep(time.Hour)
	}
}

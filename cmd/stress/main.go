//go:build tinygo

// Command stress drives worst-case test patterns at the panel to verify
// HUB75 shift-register data timing (see internal/stress).
//
// Use it to check the panel's data-setup timing: flash this, let it run warm
// for a few minutes, and watch the per-channel stripe frames (green especially)
// for pixel corruption. Sparse apps like conway and clock won't surface the
// margin; this does.
//
// A pattern stays on screen until you navigate: the board's Up/Down buttons step
// forward/back, and so does a rotary encoder on the STEMMA QT / Qw/ST port when
// one answers at boot (clockwise is forward, a detent per pattern). There is no
// auto-advance, so a pattern can be studied warm for as long as needed.
//
// Usage:
//
//	make stress
package main

import (
	_ "embed"

	"github.com/jamalc/tinygo/internal/buttons"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/encoder"
	"github.com/jamalc/tinygo/internal/hub75"
	"github.com/jamalc/tinygo/internal/input"
	"github.com/jamalc/tinygo/internal/stress"
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	// The knob's pager reports a clockwise detent as Down, the clock's "next";
	// stress's next is Up, so it's reversed here. Without an encoder the pager
	// polls None and the buttons work alone.
	knob := input.Reverse(encoder.New().Pager())
	stress.Run(panel, cfg.Width, cfg.Height, input.Merge(buttons.New(), knob))
}

//go:build tinygo

// Command breakout is the Breakout game, as device firmware.
//
// The paddle takes its orders from an Adafruit seesaw rotary encoder if one
// answers on the STEMMA QT / Qw/ST port at boot — the knob the arcade original
// shipped with — and from leaning the panel otherwise. Which controller is in
// charge is discovered, not configured: plug the knob in and power up, and it
// wins; unplug it and the accelerometer is back. Either way only the input's X
// axis is used, because a paddle has one degree of freedom. Under tilt,
// whichever way the board is held at power-on becomes level.
//
// The serve waits for a press — the knob's click, or a board button under tilt —
// and serves itself after a few seconds on a board with neither.
//
// Usage:
//
//	make breakout [BOARD=matrixportal-m4]
package main

import (
	_ "embed"

	"github.com/jamalc/tinygo/internal/accel"
	"github.com/jamalc/tinygo/internal/breakout"
	"github.com/jamalc/tinygo/internal/buttons"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/encoder"
	"github.com/jamalc/tinygo/internal/hub75"
	"github.com/jamalc/tinygo/internal/input"
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	// Prefer the knob, which serves both ports (spin and click); fall back to
	// tilt with the board's buttons for the serve. Probing the encoder first
	// also means a knob-equipped board skips the accelerometer's calibration
	// entirely.
	enc := encoder.New()
	var in input.TiltSource = enc
	var src input.Source = enc
	if !enc.Enabled() {
		in = accel.New()
		src = buttons.New()
	}

	breakout.Run(cfg, panel, in, src)
}

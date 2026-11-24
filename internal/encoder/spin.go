// Package encoder reads an Adafruit seesaw rotary encoder (the I2C QT Rotary
// Encoder breakout, plugged into the STEMMA QT / Qw/ST port) as user input, so
// Breakout can be played with a knob — the control the arcade original shipped
// with.
//
// It reports through input.TiltSource, the same port the accelerometer serves:
// the knob's spin *rate* is presented as the lean. That is the honest mapping,
// not a shim. The game integrates a lean into paddle velocity (accelerate, then
// drag toward a speed the lean sets), so a rate-as-lean knob turns knob angle
// into paddle position in the limit — which is exactly what a paddle knob is.
// Presenting the knob's absolute position instead would fight that physics, and
// inventing a third input port for one game would buy nothing but a seam.
//
// The knob's push-switch is the other port: it reports through input.Source as
// a Click, which is what serves the ball. One adapter, both ports, exactly the
// arrangement the simulator keyboard already has.
//
// Whether an encoder is present is discovered at boot, not configured: New
// probes the I2C address and returns a disabled Source when nothing answers, so
// the app can fall back to the accelerometer. See cmd/breakout.
//
// This file is the whole of the feel — rate, smoothing, direction — as plain
// arithmetic over detent counts, with no hardware in it, so it is unit-tested
// on the host. The per-board files are left with nothing but I2C wiring to get
// wrong; see encoder.go for that side.
package encoder

import "github.com/jamalc/tinygo/internal/input"

// Feel. These are the numbers to reach for when the knob reads badly on real
// hardware; everything else in this package is mechanism.
const (
	// fullSpin is the rotation rate, in detents per poll, that counts as full
	// lean. The game polls at ~60 Hz and the knob has 24 detents per turn, so
	// 0.5 is about 30 detents a second — one brisk 180° flick. Lower it for a
	// lazier wrist, raise it to make the player crank.
	fullSpin = 0.5

	// smoothing is the weight of each new sample in the running average. At
	// hand speed the deltas arrive as a pulse train — 0, 0, 1, 0, 1 — and the
	// average is what turns that into the rate the pulses imply, instead of a
	// paddle that twitches on every detent.
	smoothing = 0.3

	// rest snaps the decaying rate to zero once it is imperceptible. Unlike the
	// accelerometer there is no sensor noise to hold off, so this is not a dead
	// zone — it just ends the asymptotic tail after the knob stops.
	rest = 0.01

	// direction maps the seesaw's count onto the screen's, +X being paddle-
	// right on a clockwise turn — which the breakout in hand delivers at +1,
	// despite Adafruit's examples negating the position. If another knob runs
	// backwards, this is the constant that's wrong.
	direction = 1
)

// tracker turns a stream of encoder deltas — detents turned since the previous
// poll — into a lean. It assumes it is polled at a steady cadence; the rate is
// per poll, and the game's fixed frame time is what makes that a speed.
type tracker struct {
	x float32 // the smoothed rate, as a fraction of fullSpin, clamped to [-1, 1]
}

// update folds one delta into the tracker and returns the lean it now reports.
func (t *tracker) update(delta int32) input.Tilt {
	target := float32(direction*delta) / fullSpin

	// Smooth, then clamp the stored value itself: a wild spin past fullSpin
	// must not bank up excess that keeps the paddle moving after the knob has
	// stopped.
	t.x = clamp32(t.x+(target-t.x)*smoothing, -1, 1)
	if abs32(t.x) < rest {
		t.x = 0
	}

	return input.Tilt{X: t.x}
}

func abs32(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

func clamp32(f, lo, hi float32) float32 {
	switch {
	case f < lo:
		return lo
	case f > hi:
		return hi
	}
	return f
}

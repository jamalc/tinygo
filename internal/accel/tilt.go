// Package accel reads a board's on-board accelerometer as user input, so the
// games can be played by tilting the panel.
//
// It reports the lean as an input.TiltSource, which is what the games built on
// analog tilt (marble, breakout) consume. It deliberately does not quantize that
// lean into discrete directions: nothing asks for it now, and a threshold nobody
// uses is a threshold nobody is tuning.
//
// This file is the whole of the reasoning — orientation, calibration, smoothing,
// and quantization — as plain arithmetic over raw sensor readings, with no
// hardware in it. That is deliberate: it means the logic is unit-tested on the
// host, and the per-board files are left with nothing but I2C wiring to get
// wrong. See accel.go for that side.
package accel

import "github.com/jamalc/tinygo/internal/input"

// Feel. These are the numbers to reach for when the tilt reads badly on real
// hardware; everything else in this package is mechanism.
const (
	// fullLean is the deviation from rest, in µg, that counts as leaning all the
	// way over. 450_000 µg is about 27° off however the board was resting at
	// startup — a comfortable wrist, not a shoulder. Lower it for a twitchier
	// board, raise it to make the player commit.
	fullLean = 450_000

	// deadZone is the lean below which the board reads as level. Hands shake and
	// the sensor is noisy; without this the marble drifts on a board the player
	// believes is still.
	deadZone = 0.09

	// smoothing is the weight of each new sample in the running average, so the
	// reported lean chases the sensor rather than jumping to it. Low enough to
	// swallow noise, high enough that the panel doesn't feel like it's steering
	// through syrup.
	smoothing = 0.28
)

// axis identifies one of the sensor's three raw axes and which way round it is.
// Sign is +1 or -1.
type axis struct {
	index int // 0 = sensor X, 1 = sensor Y, 2 = sensor Z
	sign  int32
}

// orientation says how the sensor is glued down relative to the panel: which raw
// axis increases as the board is leaned to the screen's right, and which as it is
// leaned toward the screen's bottom. Every board mounts its accelerometer at its
// own angle, so this is the one thing each board file must get right.
type orientation struct {
	right axis
	down  axis
}

// pick returns the reading along a, with its sign applied.
func (a axis) pick(v [3]int32) int32 { return a.sign * v[a.index] }

// tracker turns a stream of raw accelerometer readings into a lean.
//
// Rest is calibrated rather than assumed: the board is level when it is held
// however it was held at startup. A panel on a desk and a panel in two hands
// have completely different gravity vectors, and neither is more correct, so the
// tracker measures the *deviation* from the resting vector instead of trying to
// know which way is down.
type tracker struct {
	o   orientation
	ref [3]int32 // the resting reading: what "level" means for this session

	x, y float32 // the smoothed lean
}

// newTracker returns a tracker for a sensor mounted in the given orientation,
// with rest taken to be the reading at rest.
func newTracker(o orientation, rest [3]int32) *tracker {
	return &tracker{o: o, ref: rest}
}

// calibrate makes the current resting reading the new level.
func (t *tracker) calibrate(rest [3]int32) {
	t.ref = rest
	t.x, t.y = 0, 0
}

// update folds one raw reading (in µg) into the tracker and returns the lean it
// now reports.
func (t *tracker) update(raw [3]int32) input.Tilt {
	dev := [3]int32{raw[0] - t.ref[0], raw[1] - t.ref[1], raw[2] - t.ref[2]}

	// Raw lean, as a fraction of a full lean in each screen direction.
	rx := float32(t.o.right.pick(dev)) / fullLean
	ry := float32(t.o.down.pick(dev)) / fullLean

	t.x += (rx - t.x) * smoothing
	t.y += (ry - t.y) * smoothing

	return clean(input.Tilt{X: t.x, Y: t.y})
}

// clean applies the dead zone and enforces input.Tilt's magnitude contract, so
// every consumer gets a vector it can trust without re-checking it.
func clean(t input.Tilt) input.Tilt {
	mag := hypot(t.X, t.Y)
	if mag < deadZone {
		return input.Tilt{}
	}
	if mag > 1 {
		return input.Tilt{X: t.X / mag, Y: t.Y / mag}
	}
	return t
}

func hypot(x, y float32) float32 {
	// Not math.Hypot: this runs every frame on a microcontroller with no FPU to
	// speak of, and the overflow-safety math.Hypot buys is irrelevant for two
	// numbers that never leave [-2, 2].
	return sqrt32(x*x + y*y)
}

// sqrt32 is Newton's method, seeded generously. Kept local so this package
// pulls in no math import on the device.
func sqrt32(v float32) float32 {
	if v <= 0 {
		return 0
	}
	g := v
	if g > 1 {
		g = 1 + (v-1)/2 // a decent first guess for the range we see
	}
	for i := 0; i < 8; i++ {
		g = 0.5 * (g + v/g)
	}
	return g
}

func abs32(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

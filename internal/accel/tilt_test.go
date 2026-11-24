package accel

import (
	"testing"

	"github.com/jamalc/tinygo/internal/input"
)

// flat is a board lying screen-up on a desk: gravity entirely on the sensor's Z
// axis. Everything here measures deviation from a resting vector, so the actual
// values matter far less than the differences from them.
var flat = [3]int32{0, 0, 1_000_000}

// testOrientation is the mapping the board files start from: leaning right
// raises sensor X, leaning toward the screen's bottom lowers sensor Y.
var testOrientation = orientation{
	right: axis{index: 0, sign: 1},
	down:  axis{index: 1, sign: -1},
}

// settle feeds the same reading in until the smoothing filter has converged, so
// a test asserts on the lean the player would actually feel rather than on the
// first frame of the ramp toward it.
func settle(t *tracker, raw [3]int32) input.Tilt {
	var out input.Tilt
	for i := 0; i < 100; i++ {
		out = t.update(raw)
	}
	return out
}

func TestLevelBoardReadsLevel(t *testing.T) {
	tr := newTracker(testOrientation, flat)

	if got := settle(tr, flat); got != (input.Tilt{}) {
		t.Errorf("a board at rest reads %v, want dead level", got)
	}
}

func TestRestIsWhereverTheBoardStarted(t *testing.T) {
	// A panel held upright — gravity along -Y instead of Z — is just as valid a
	// resting position as flat on a desk, and must also read as level. This is
	// the whole reason rest is calibrated instead of assumed.
	upright := [3]int32{0, -1_000_000, 0}
	tr := newTracker(testOrientation, upright)

	if got := settle(tr, upright); got != (input.Tilt{}) {
		t.Errorf("a board resting upright reads %v, want dead level", got)
	}
}

func TestLeanMapsToScreenDirections(t *testing.T) {
	tests := []struct {
		name  string
		raw   [3]int32
		wantX string // "+", "-", or "0"
		wantY string
	}{
		{"leaning right", [3]int32{fullLean, 0, 1_000_000}, "+", "0"},
		{"leaning left", [3]int32{-fullLean, 0, 1_000_000}, "-", "0"},
		{"leaning down the screen", [3]int32{0, -fullLean, 1_000_000}, "0", "+"},
		{"leaning up the screen", [3]int32{0, fullLean, 1_000_000}, "0", "-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTracker(testOrientation, flat)
			got := settle(tr, tc.raw)

			if sign(got.X) != tc.wantX {
				t.Errorf("X = %v (%s), want %s", got.X, sign(got.X), tc.wantX)
			}
			if sign(got.Y) != tc.wantY {
				t.Errorf("Y = %v (%s), want %s", got.Y, sign(got.Y), tc.wantY)
			}
		})
	}
}

func TestFullLeanReadsFullScale(t *testing.T) {
	tr := newTracker(testOrientation, flat)

	got := settle(tr, [3]int32{fullLean, 0, 1_000_000})
	if abs32(got.X-1) > 0.02 {
		t.Errorf("X = %v at a full lean, want about 1", got.X)
	}
}

func TestTiltMagnitudeNeverExceedsOne(t *testing.T) {
	// input.Tilt promises |v| <= 1, and holding that is this package's job, not
	// the games'. A board leaned hard into a corner is the case that breaks it.
	tr := newTracker(testOrientation, flat)

	got := settle(tr, [3]int32{4 * fullLean, -4 * fullLean, 1_000_000})
	if mag := hypot(got.X, got.Y); mag > 1.0001 {
		t.Errorf("a board leaned hard diagonally reports magnitude %v, want it clamped to 1", mag)
	}
}

func TestDeadZoneSwallowsSmallLeans(t *testing.T) {
	tr := newTracker(testOrientation, flat)

	// A hand's tremor: well inside the dead zone.
	nudge := int32(float32(fullLean) * deadZone * 0.5)
	if got := settle(tr, [3]int32{nudge, nudge, 1_000_000}); got != (input.Tilt{}) {
		t.Errorf("a tiny nudge reads %v, want the dead zone to swallow it", got)
	}
}

func TestSmoothingRampsRatherThanJumps(t *testing.T) {
	tr := newTracker(testOrientation, flat)
	hard := [3]int32{fullLean, 0, 1_000_000}

	first := tr.update(hard)
	if first.X >= 1 {
		t.Errorf("the first frame of a hard lean already reads %v, want the lean to ramp in", first.X)
	}
	if settled := settle(tr, hard); settled.X <= first.X {
		t.Errorf("lean settled at %v but started at %v, want it to climb", settled.X, first.X)
	}
}

func TestCalibrateRezeroes(t *testing.T) {
	tr := newTracker(testOrientation, flat)
	leaned := [3]int32{fullLean, 0, 1_000_000}
	settle(tr, leaned)

	// The player got comfortable holding it at an angle and re-zeroed there.
	tr.calibrate(leaned)

	if got := settle(tr, leaned); got != (input.Tilt{}) {
		t.Errorf("after recalibrating at the held angle the board reads %v, want level", got)
	}
}

func TestAxisPickAppliesSign(t *testing.T) {
	v := [3]int32{10, 20, 30}

	if got := (axis{index: 1, sign: 1}).pick(v); got != 20 {
		t.Errorf("pick(Y) = %d, want 20", got)
	}
	if got := (axis{index: 1, sign: -1}).pick(v); got != -20 {
		t.Errorf("pick(-Y) = %d, want -20", got)
	}
}

func TestSqrt32(t *testing.T) {
	for _, v := range []float32{0, 0.0001, 0.25, 1, 2, 4} {
		got := sqrt32(v)
		if abs32(got*got-v) > 0.001 {
			t.Errorf("sqrt32(%v) = %v, which squares back to %v", v, got, got*got)
		}
	}
	if got := sqrt32(-1); got != 0 {
		t.Errorf("sqrt32(-1) = %v, want 0", got)
	}
}

func sign(f float32) string {
	switch {
	case f > 0.05:
		return "+"
	case f < -0.05:
		return "-"
	}
	return "0"
}

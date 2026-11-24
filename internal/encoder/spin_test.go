package encoder

import "testing"

// A clockwise turn must lean right. The literal +1 here is the hardware fact
// this pins: the knob in hand counts clockwise as positive (spin.go's direction
// constant is chosen to match, and the two must flip together — writing
// direction into the delta would test nothing).
func TestClockwiseLeansRight(t *testing.T) {
	var tr tracker
	tilt := tr.update(1)
	if tilt.X <= 0 {
		t.Fatalf("clockwise detent leaned X=%v, want > 0", tilt.X)
	}
	if tilt.Y != 0 {
		t.Fatalf("knob leaned Y=%v, want 0: a knob has one axis", tilt.Y)
	}
}

// A sustained hard spin saturates at full lean and never exceeds it, whatever
// the rate — Tilt's magnitude contract is the adapter's to keep.
func TestHardSpinSaturates(t *testing.T) {
	var tr tracker
	var last float32
	for i := 0; i < 60; i++ {
		last = tr.update(5).X
		if last > 1 {
			t.Fatalf("lean %v exceeds 1 on frame %d", last, i)
		}
	}
	if last != 1 {
		t.Fatalf("sustained spin settled at %v, want full lean 1", last)
	}
}

// When the knob stops, the lean must decay to exactly zero — not hover at a
// float residue that keeps the paddle creeping.
func TestStoppingComesToRest(t *testing.T) {
	var tr tracker
	for i := 0; i < 30; i++ {
		tr.update(2)
	}
	for i := 0; i < 60; i++ {
		if tr.update(0).X == 0 {
			return
		}
	}
	t.Fatalf("lean never reached zero after the knob stopped; stuck at %v", tr.x)
}

// A knob nobody touches reports exactly level from the first poll: there is no
// noise floor to warm up through.
func TestIdleIsExactlyLevel(t *testing.T) {
	var tr tracker
	for i := 0; i < 10; i++ {
		if tilt := tr.update(0); tilt.X != 0 || tilt.Y != 0 {
			t.Fatalf("idle knob reported %+v on poll %d, want level", tilt, i)
		}
	}
}

// A slow, steady turn reads as a steady partial lean, not a sequence of
// twitches: the smoothing must bridge the polls between detents.
func TestSlowTurnHoldsPartialLean(t *testing.T) {
	var tr tracker
	// One detent every third poll ≈ a third of fullSpin's rate... after the
	// average settles, every poll should report a moderate rightward lean.
	var min, max float32 = 2, -2
	for i := 0; i < 90; i++ {
		var delta int32
		if i%3 == 0 {
			delta = 1
		}
		x := tr.update(delta).X
		if i < 30 {
			continue // let the average settle
		}
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	if min <= 0.2 || max >= 1 {
		t.Fatalf("slow turn leaned between %v and %v, want a steady partial lean", min, max)
	}
}

// Package input defines the hardware-agnostic user-input ports shared by the
// display apps. A keyboard maps to them just as well as a GPIO button or an
// accelerometer: the platform adapter reports what the user did; the app
// decides what it means.
//
// There are two ports, because a lean is not a button press and flattening one
// into the other loses something either way:
//
//   - Source is discrete. It reports that a control fired — a button press, a
//     step. Right for paging through arrivals or test patterns (clock, stress),
//     where the input is a decision.
//   - TiltSource is analog. It reports how far the board is leaning, as a
//     vector. Right when the lean itself is the physics (marble, breakout),
//     where quantizing to a handful of directions would throw away exactly the
//     information the app is built on.
//
// One adapter may serve both — the simulator keyboard does, reporting arrow
// presses as a Source and arrows held as a lean. Neither is obliged to: a board
// with no accelerometer never reports a tilt, and an app is free to consume only
// one port. Both directions of that mismatch are fine by design.
//
// This package imports nothing, so depending on it adds no weight to a device
// firmware build.
package input

// Button is a logical navigation control reported by a Source. The set is
// deliberately small: stepping back and forth through a list, and Click — one
// affirmative "go", the press of a knob or the simulator's space bar, for the
// moments an app is waiting on the player (serving the ball). Anything wanting
// a direction in two dimensions wants TiltSource instead.
type Button int

const (
	None Button = iota
	Up
	Down
	Click
)

// Source is the port for discrete user navigation input. It is polled once per
// loop tick and reports any control actuated since the last poll. Adapters live
// in the platform layer (device GPIO, simulator keyboard).
type Source interface {
	Poll() Button
}

// Tilt is a lean, in board-relative screen coordinates: X grows to the right
// (east) and Y grows downward (south), matching the pixel coordinates the apps
// draw in, so an app can use a Tilt as a gravity vector without transforming it.
//
// The two components are each in [-1, 1] and the vector's magnitude is at most
// 1, so a hard diagonal lean is no stronger than a hard lean along one axis.
// Holding that contract is the adapter's job, not the app's.
type Tilt struct {
	X, Y float32
}

// TiltSource is the port for analog directional input: which way, and how hard,
// the board is being leaned. It is sampled once per frame and reports the
// current lean, not a change since the last sample — a TiltSource is a position,
// where a Source is an event.
type TiltSource interface {
	Tilt() Tilt
}

// Merge returns a Source that polls every source in order each time it is
// polled, and reports the first that fired. Every source is polled, not just up
// to the first hit, so an edge-triggered source never misses a transition
// because another one reported first. Use it to let a board's buttons and a knob
// drive the same app.
func Merge(sources ...Source) Source { return merged(sources) }

type merged []Source

// Reverse returns a Source that reports src's Up as Down and its Down as Up,
// passing everything else through. Use it when an input's natural direction
// runs against an app's: a knob turned clockwise means "next" in both the clock
// and stress, but the clock calls next Down and stress calls it Up.
func Reverse(src Source) Source { return reversed{src} }

type reversed struct{ src Source }

func (r reversed) Poll() Button {
	switch b := r.src.Poll(); b {
	case Up:
		return Down
	case Down:
		return Up
	default:
		return b
	}
}

func (m merged) Poll() Button {
	got := None
	for _, s := range m {
		if b := s.Poll(); got == None {
			got = b
		}
	}
	return got
}

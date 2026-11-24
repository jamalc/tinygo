//go:build tinygo

// Package buttons reads a board's on-board UP/DOWN buttons as an input.Source.
// The buttons are wired active-low (pressed = pin low) and use the chip's
// internal pull-ups. Each board file supplies New (its pin pair); boards
// without mapped buttons return a disabled Source whose Poll is always None,
// so an app's button stepping is simply inert there (see buttons_other.go).
//
// Poll is edge-triggered and debounced by time, not by poll cadence:
// a press fires only after at least debounceTime of continuous release,
// so callers may poll as fast as they like (the app loops sample every
// few milliseconds to catch quick taps) without contact bounce firing
// extra presses, and holding a button doesn't repeat.
package buttons

import (
	"machine"
	"time"

	"github.com/jamalc/tinygo/internal/input"
)

// debounceTime is the continuous-release time required before a new
// press is accepted. Tactile-switch bounce is ~1-5 ms; human re-presses
// are 100 ms+, so 30 ms rejects all bounce without losing real taps.
const debounceTime = 30 * time.Millisecond

// button tracks one debounced active-low pin.
type button struct {
	pin        machine.Pin
	pressed    bool      // debounced state
	releasedAt time.Time // start of the current released stretch
}

// pressEdge reports a debounced press edge: a release→press transition
// preceded by at least debounceTime of continuous release. Bounce on
// press (the released blips are too short to re-arm) and on release
// (the pressed blips follow too little release) both fail that test.
func (b *button) pressEdge() bool {
	raw := !b.pin.Get() // active low
	switch {
	case raw && !b.pressed:
		b.pressed = true
		return time.Since(b.releasedAt) >= debounceTime
	case !raw && b.pressed:
		b.pressed = false
		b.releasedAt = time.Now()
	}
	return false
}

// Source is the UP/DOWN pair as an input.Source. A zero-value (disabled)
// Source polls only None — used on boards without mapped buttons so it never
// touches real GPIOs (the zero pin would alias a HUB75 line).
type Source struct {
	up, down button
	enabled  bool
}

// newPins configures the given active-low button pins (internal pull-ups) and
// returns an enabled Source. Board files call this from their New.
func newPins(up, down machine.Pin) *Source {
	up.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	down.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	now := time.Now()
	return &Source{
		up:      button{pin: up, releasedAt: now},
		down:    button{pin: down, releasedAt: now},
		enabled: true,
	}
}

// disabled returns a Source that always polls None. For boards whose buttons
// aren't mapped yet, so apps using button stepping still build and run.
func disabled() *Source { return &Source{} }

// Poll implements input.Source. If both buttons fire on the same poll,
// Down wins. A disabled Source always returns None without reading GPIOs.
func (s *Source) Poll() input.Button {
	if !s.enabled {
		return input.None
	}
	up := s.up.pressEdge()
	down := s.down.pressEdge()
	switch {
	case down:
		return input.Down
	case up:
		return input.Up
	default:
		return input.None
	}
}

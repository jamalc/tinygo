//go:build tinygo

package encoder

import (
	"machine"
	"time"

	"tinygo.org/x/drivers/seesaw"

	"github.com/jamalc/tinygo/internal/input"
)

// address is where the I2C QT Rotary Encoder breakout answers by default. Its
// solder jumpers can move it up to 0x3D for stacking; nothing here needs that.
const address = 0x36

// busFrequency is the I2C speed the board files configure — 400 kHz fast mode,
// the same speed internal/accel runs the shared bus at, so whichever package
// configures it last leaves it how the other expects.
const busFrequency = 400 * machine.KHz

// readDelay is the pause the seesaw needs between being asked for a register
// and being read back, while its firmware fills the buffer. The driver's
// default is 100ms — tuned for soil sensors, and ruinous at 60 fps. Adafruit's
// own Arduino library uses 250µs for this breakout; 1ms leaves margin and still
// costs a sixteenth of a frame.
const readDelay = time.Millisecond

// The seesaw hardware IDs a probe may find: SAMD09 (0x55) or ATtiny817 (0x87,
// what the rotary encoder breakouts actually carry). The driver checks these
// itself but only inside SoftReset, which sleeps a full second — see New.
const (
	hwIDSAMD09  = 0x55
	hwIDTiny8x7 = 0x87
)

// buttonMask is the knob's push-switch in the seesaw's 32-bit GPIO pin mask:
// the breakout wires it to the seesaw's own pin 24, active low. The mask goes
// on the wire big-endian, so bit 24 lands in the first byte.
var buttonMask = [4]byte{0x01, 0x00, 0x00, 0x00}

// Source is a seesaw rotary encoder as user input, reporting the knob's spin
// through input.TiltSource and its push-switch through input.Source — one
// adapter, both ports, like the simulator keyboard.
//
// A zero-value (disabled) Source reports dead level and None forever — that's
// what New returns when no encoder answers the probe, so the app can test
// Enabled and fall back to another input without a special case in its loop.
type Source struct {
	dev     *seesaw.Device
	t       tracker
	wasDown bool // the switch's state at the previous Poll, for edge detection
	enabled bool
}

// newDevice probes for the encoder on an already-configured I2C bus. Board
// files call this from their New.
//
// The probe reads the seesaw's hardware ID rather than calling the driver's
// SoftReset: a missing device NACKs the read immediately, where SoftReset
// unconditionally sleeps a second waiting for a reset that nobody heard. The
// ID check also confirms a *seesaw* answered, not just something at 0x36.
func newDevice(bus *machine.I2C) *Source {
	dev := seesaw.New(bus)
	dev.Address = address
	dev.ReadDelay = readDelay

	id, err := dev.ReadRegister(seesaw.ModuleStatusBase, seesaw.FunctionStatusHwId)
	if err != nil || (id != hwIDSAMD09 && id != hwIDTiny8x7) {
		return disabled()
	}

	// Throw away whatever rotation accumulated between power-on and now, so
	// the game doesn't open with the paddle lunging to account for it.
	if _, err := dev.GetEncoderPosition(0, true); err != nil {
		return disabled()
	}

	// The push-switch: input with the seesaw's internal pull-up, which is
	// dir-clear + pull-enable + level-set in the seesaw idiom (on an input pin,
	// the level register selects the pull's direction). Best-effort — if these
	// don't take, the knob still spins and only the click sits dead.
	dev.Write(seesaw.ModuleGpioBase, seesaw.FunctionGpioDirclrBulk, buttonMask[:])
	dev.Write(seesaw.ModuleGpioBase, seesaw.FunctionGpioPullenset, buttonMask[:])
	dev.Write(seesaw.ModuleGpioBase, seesaw.FunctionGpioBulkSet, buttonMask[:])

	return &Source{dev: dev, enabled: true}
}

// disabled returns a Source that reports level without touching a bus.
func disabled() *Source { return &Source{} }

// Enabled reports whether a real encoder answered the probe. Apps use it to
// decide whether to fall back to another input source.
func (s *Source) Enabled() bool { return s.enabled }

// Tilt implements input.TiltSource: the knob's spin since the last poll,
// presented as a lean.
func (s *Source) Tilt() input.Tilt {
	if !s.enabled {
		return input.Tilt{}
	}
	delta, err := s.dev.GetEncoderPosition(0, true)
	if err != nil {
		// A dropped read reads as no rotation; the smoothing rides over it.
		delta = 0
	}
	return s.t.update(delta)
}

// Poll implements input.Source: a Click on each press of the knob's switch.
// Edge-triggered — holding the knob down doesn't repeat. There is no debounce
// beyond the poll cadence itself: at the game's 60 Hz, the ~16ms between polls
// already outlasts tactile-switch bounce, and the one app listening treats a
// double-fire as a no-op anyway.
func (s *Source) Poll() input.Button {
	if !s.enabled {
		return input.None
	}

	var buf [4]byte
	if err := s.dev.Read(seesaw.ModuleGpioBase, seesaw.FunctionGpioBulk, buf[:]); err != nil {
		// A dropped read says nothing about the switch; leave the edge state
		// alone so the press fires on the next clean read instead of vanishing.
		return input.None
	}
	down := buf[0]&buttonMask[0] == 0 // active low

	fired := down && !s.wasDown
	s.wasDown = down
	if fired {
		return input.Click
	}
	return input.None
}

// pageReadInterval is the fastest Pager touches the bus. The clock samples its
// input every 5ms to catch quick button taps, and each seesaw read sleeps
// readDelay, so reading on every sample would spend a fifth of the loop in I2C.
// 25ms is still faster than a hand can click through detents.
const pageReadInterval = 25 * time.Millisecond

// Pager reads the knob as page navigation: one input.Down per detent clockwise,
// one input.Up per detent counter-clockwise. It's a separate input.Source from
// Source's own Poll (which reports the push-switch as a Click) because the two
// apps want different things from the same knob. The rotation counter is shared,
// so use one or the other, not both. A Pager of a disabled Source polls None.
type Pager struct {
	s    *Source
	p    pager
	next time.Time // earliest time of the next bus read
}

// Pager returns the knob as a page-stepping input.Source. See Pager.
func (s *Source) Pager() *Pager {
	return &Pager{s: s}
}

// Poll implements input.Source.
func (p *Pager) Poll() input.Button {
	if !p.s.enabled {
		return input.None
	}
	now := time.Now()
	if now.Before(p.next) {
		return input.None
	}
	p.next = now.Add(pageReadInterval)
	delta, err := p.s.dev.GetEncoderPosition(0, true)
	if err != nil {
		return input.None // a dropped read loses nothing: the seesaw keeps counting
	}
	return p.p.step(delta)
}

// Compile-time assertions: one adapter, both ports, and the pager.
var (
	_ input.TiltSource = (*Source)(nil)
	_ input.Source     = (*Source)(nil)
	_ input.Source     = (*Pager)(nil)
)

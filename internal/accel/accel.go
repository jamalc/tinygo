//go:build tinygo

package accel

import (
	"machine"
	"time"

	"tinygo.org/x/drivers/lis3dh"

	"github.com/jamalc/tinygo/internal/input"
)

// The LIS3DH on both Adafruit MatrixPortal boards is strapped to 0x19, not the
// 0x18 the driver defaults to.
const address = 0x19

// busFrequency is the I2C speed the board files configure. 400 kHz — fast mode —
// which the LIS3DH supports and which keeps a per-frame sensor read short. It
// lives here rather than in each board file so the two can't drift apart; the
// pins differ per board, the speed doesn't.
const busFrequency = 400 * machine.KHz

// calibrationSamples is how many readings are averaged into the resting vector
// at startup. Enough to average out sensor noise, few enough to be instant.
const calibrationSamples = 32

// Source is the accelerometer as user input, reporting the board's lean through
// input.TiltSource.
//
// A zero-value (disabled) Source reports dead level forever — that's what boards
// without an accelerometer get, so every app still builds and runs on them (see
// accel_other.go).
type Source struct {
	dev     lis3dh.Device
	t       *tracker
	enabled bool
}

// newDevice brings up the LIS3DH on an already-configured I2C bus and calibrates
// rest to however the board is sitting right now. Board files call this from
// their New.
//
// A sensor that doesn't answer yields a disabled Source rather than an error:
// the panel is the only output this firmware has, and refusing to boot a game
// because its controller is missing would be worse than letting it run inert.
func newDevice(bus *machine.I2C, o orientation) *Source {
	dev := lis3dh.New(bus)
	if err := dev.Configure(lis3dh.Config{Address: address}); err != nil {
		return disabled()
	}
	if !dev.Connected() {
		return disabled()
	}
	// ±2g gives the finest resolution, and a hand-held panel never sees more.
	dev.SetRange(lis3dh.RANGE_2_G)
	dev.SetDataRate(lis3dh.DATARATE_100_HZ)

	s := &Source{dev: dev, enabled: true}
	s.t = newTracker(o, s.rest())
	return s
}

// disabled returns a Source that reports level and None without touching a bus.
func disabled() *Source { return &Source{} }

// Enabled reports whether there is a real sensor behind this Source. Apps use it
// to say so on screen rather than leaving the player wondering why the board
// isn't responding.
func (s *Source) Enabled() bool { return s.enabled }

// read takes one sample, in µg.
func (s *Source) read() [3]int32 {
	x, y, z, err := s.dev.ReadAcceleration()
	if err != nil {
		return [3]int32{}
	}
	return [3]int32{x, y, z}
}

// rest averages a short burst of samples into a resting vector.
func (s *Source) rest() [3]int32 {
	var sum [3]int64
	for i := 0; i < calibrationSamples; i++ {
		r := s.read()
		for a := 0; a < 3; a++ {
			sum[a] += int64(r[a])
		}
		time.Sleep(2 * time.Millisecond)
	}
	return [3]int32{
		int32(sum[0] / calibrationSamples),
		int32(sum[1] / calibrationSamples),
		int32(sum[2] / calibrationSamples),
	}
}

// Calibrate makes however the board is being held right now the new level.
func (s *Source) Calibrate() {
	if s.enabled {
		s.t.calibrate(s.rest())
	}
}

// Raw returns one unfiltered sensor reading in µg, and the resting vector it is
// measured against. For cmd/acceldiag, which exists to work out a board's
// orientation mapping — the one thing in this package that cannot be determined
// without the hardware in hand.
func (s *Source) Raw() (now, rest [3]int32) {
	if !s.enabled {
		return [3]int32{}, [3]int32{}
	}
	return s.read(), s.t.ref
}

// Tilt implements input.TiltSource.
func (s *Source) Tilt() input.Tilt {
	if !s.enabled {
		return input.Tilt{}
	}
	return s.t.update(s.read())
}

// Compile-time assertion.
var _ input.TiltSource = (*Source)(nil)

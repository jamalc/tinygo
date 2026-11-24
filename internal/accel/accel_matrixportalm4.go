//go:build tinygo && matrixportal_m4

package accel

import "machine"

// New brings up the Matrix Portal M4's on-board LIS3DH. It hangs off I2C0
// (SERCOM5, SDA=D5/PB31, SCL=D6/PB30) — the same bus as the STEMMA QT port,
// which is why the sensor is strapped to 0x19 and leaves 0x18 free for whatever
// you plug in there. The target names those pins already, so the defaults stand.
func New() *Source {
	bus := machine.I2C0
	if err := bus.Configure(machine.I2CConfig{Frequency: busFrequency}); err != nil {
		return disabled()
	}
	return newDevice(bus, m4Orientation)
}

// m4Orientation maps the sensor's axes onto the panel's.
//
// Verified on hardware: the sensor's Y axis runs down the screen rather than up
// it, so `down` is +Y (negating it rolls the marble uphill). If you remount the
// panel or port to another board, `make acceldiag` prints the live axes and
// tells you what to put here.
var m4Orientation = orientation{
	right: axis{index: 0, sign: 1}, // leaning right raises sensor +X
	down:  axis{index: 1, sign: 1}, // leaning toward the bottom raises sensor +Y
}

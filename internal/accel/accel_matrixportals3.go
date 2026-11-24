//go:build tinygo && esp32s3

package accel

import "machine"

// MatrixPortal S3 I2C. The esp32s3-generic target has no board-specific pin
// names, so the STEMMA QT bus is named here: SDA on GPIO16 and SCL on GPIO17,
// per Adafruit's board definition. The on-board LIS3DH shares it.
const (
	s3SDA = machine.Pin(16)
	s3SCL = machine.Pin(17)
)

// New brings up the MatrixPortal S3's on-board LIS3DH, which shares the STEMMA
// QT bus and is strapped to 0x19.
func New() *Source {
	bus := machine.I2C0
	if err := bus.Configure(machine.I2CConfig{
		Frequency: busFrequency,
		SDA:       s3SDA,
		SCL:       s3SCL,
	}); err != nil {
		return disabled()
	}
	return newDevice(bus, s3Orientation)
}

// s3Orientation maps the sensor's axes onto the panel's.
//
// UNVERIFIED: inherited from the M4's measured mapping, not measured on this
// board. Two Adafruit MatrixPortals of the same form factor are a decent prior,
// but they are different layouts and the sensor may sit at a different angle.
// Run `make acceldiag BOARD=matrixportal-s3` and correct this from what it
// prints.
var s3Orientation = orientation{
	right: axis{index: 0, sign: 1},
	down:  axis{index: 1, sign: 1},
}

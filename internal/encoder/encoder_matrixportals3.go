//go:build tinygo && esp32s3

package encoder

import "machine"

// MatrixPortal S3 I2C. The esp32s3-generic target has no board-specific pin
// names, so the STEMMA QT bus is named here: SDA on GPIO16 and SCL on GPIO17,
// per Adafruit's board definition — the same pair internal/accel names for the
// on-board LIS3DH, duplicated because each package owns its own bus bring-up.
const (
	s3SDA = machine.Pin(16)
	s3SCL = machine.Pin(17)
)

// New probes for the encoder on the MatrixPortal S3's STEMMA QT port.
func New() *Source {
	bus := machine.I2C0
	if err := bus.Configure(machine.I2CConfig{
		Frequency: busFrequency,
		SDA:       s3SDA,
		SCL:       s3SCL,
	}); err != nil {
		return disabled()
	}
	return newDevice(bus)
}

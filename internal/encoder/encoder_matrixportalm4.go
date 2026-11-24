//go:build tinygo && matrixportal_m4

package encoder

import "machine"

// New probes for the encoder on the Matrix Portal M4's STEMMA QT port, which
// hangs off I2C0 (SERCOM5, SDA=D5/PB31, SCL=D6/PB30) — the same bus as the
// on-board LIS3DH, which is strapped to 0x19 precisely so the port's guests can
// keep their own addresses. The target names the pins already, so the defaults
// stand.
func New() *Source {
	bus := machine.I2C0
	if err := bus.Configure(machine.I2CConfig{Frequency: busFrequency}); err != nil {
		return disabled()
	}
	return newDevice(bus)
}

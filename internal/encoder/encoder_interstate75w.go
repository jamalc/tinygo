//go:build tinygo && rp2350

package encoder

import "machine"

// Interstate 75 W Qw/ST connector: SDA on GP20 and SCL on GP21, which the RP2350
// routes to I2C0. These must be named explicitly. The pico2-w target's I2C0
// defaults are GP4/GP5, which on this board are HUB75 G2 and B2 (the lower half's
// green and blue; see internal/hub75's pin map). Configuring I2C there takes those
// lines from the PIO, and a plugged-in breakout's pull-ups then hold them high,
// turning the bottom half of the panel solid green+blue (yellow on a BGR panel).
const (
	i75SDA = machine.GPIO20
	i75SCL = machine.GPIO21
)

// New probes for the encoder on the Interstate 75 W's Qw/ST connector. This is
// the board the encoder matters most on: with no on-board accelerometer, the knob
// is the first analog input the Interstate has ever had.
func New() *Source {
	bus := machine.I2C0
	if err := bus.Configure(machine.I2CConfig{
		Frequency: busFrequency,
		SDA:       i75SDA,
		SCL:       i75SCL,
	}); err != nil {
		return disabled()
	}
	return newDevice(bus)
}

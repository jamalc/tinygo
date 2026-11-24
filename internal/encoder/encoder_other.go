//go:build tinygo && !matrixportal_m4 && !esp32s3 && !rp2350

package encoder

// New returns a disabled Source. A board this package has no I2C wiring for
// can't be probed, so the games still build and run there — the knob just never
// answers, and the app falls back the same way it does when one isn't plugged
// in.
func New() *Source { return disabled() }

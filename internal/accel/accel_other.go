//go:build tinygo && !matrixportal_m4 && !esp32s3

package accel

// New returns a disabled Source. The Interstate 75 W (and any other board
// without an on-board accelerometer) has no sensor to read, so the tilt games
// still build and run there — they just sit level. Apps can check Enabled and
// say as much on screen.
func New() *Source { return disabled() }

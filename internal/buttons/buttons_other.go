//go:build tinygo && !matrixportal_m4 && !rp2350 && !esp32s3

package buttons

// New returns a disabled Source: this board's buttons aren't mapped, so apps
// that step on buttons still build and run (their stepping is simply inert).
func New() *Source { return disabled() }

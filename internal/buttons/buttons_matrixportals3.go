//go:build tinygo && esp32s3

package buttons

import "machine"

// New maps the Adafruit MatrixPortal S3's UP (GP6) and DOWN (GP7) buttons. Both are
// active-low (grounded when pressed); newPins enables the internal pull-ups.
func New() *Source { return newPins(machine.Pin(6), machine.Pin(7)) }

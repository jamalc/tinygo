//go:build tinygo && rp2350

package buttons

import "machine"

// New maps the Interstate 75 W's user buttons A and B (A -> Up, B -> Down).
// A = GP14, B = GP15: the two GPIOs just above the HUB75 connector (GP0..GP13).
// B was confirmed on hardware; A pairs with it on the adjacent pin. (BOOT and
// RESET are the flash/reset buttons, not user input.)
func New() *Source { return newPins(machine.Pin(14), machine.Pin(15)) }

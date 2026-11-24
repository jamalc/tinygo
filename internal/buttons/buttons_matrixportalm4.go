//go:build tinygo && matrixportal_m4

package buttons

import "machine"

// New maps the Matrix Portal M4's on-board UP and DOWN buttons.
func New() *Source { return newPins(machine.BUTTON_UP, machine.BUTTON_DOWN) }

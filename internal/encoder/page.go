package encoder

import "github.com/jamalc/tinygo/internal/input"

// detentsPerPage is how far the knob turns to move one page. One detent per
// page is what a clicky knob promises: each click you feel is one page. Raise
// it if a nudge pages too eagerly.
const detentsPerPage = 1

// pager turns encoder deltas into page steps: clockwise is input.Down (the next
// page), counter-clockwise input.Up. Like tracker, it is plain arithmetic so it
// is unit-tested on the host; Pager in encoder.go feeds it from the device.
type pager struct {
	acc int32 // detents turned toward the next step, signed; |acc| < detentsPerPage between steps
}

// step folds one delta into the pager and reports at most one page step. A
// spin of several pages between reads still moves just one page, and the excess
// is dropped rather than queued: the clock takes one press per tick, so a backlog
// would keep paging after the knob had stopped.
func (p *pager) step(delta int32) input.Button {
	p.acc += direction * delta
	switch {
	case p.acc >= detentsPerPage:
		p.acc = 0
		return input.Down
	case p.acc <= -detentsPerPage:
		p.acc = 0
		return input.Up
	}
	return input.None
}

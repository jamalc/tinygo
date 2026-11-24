package encoder

import (
	"testing"

	"github.com/jamalc/tinygo/internal/input"
)

func TestPagerStep(t *testing.T) {
	cases := []struct {
		name   string
		deltas []int32
		want   []input.Button
	}{
		{"still", []int32{0, 0}, []input.Button{input.None, input.None}},
		{"clockwise pages forward", []int32{1, 1}, []input.Button{input.Down, input.Down}},
		{"counter-clockwise pages back", []int32{-1}, []input.Button{input.Up}},
		{"fast spin moves one page, excess dropped", []int32{5, 0}, []input.Button{input.Down, input.None}},
		{"reversal steps immediately", []int32{1, -1}, []input.Button{input.Down, input.Up}},
	}
	for _, c := range cases {
		var p pager
		for i, d := range c.deltas {
			if got := p.step(d); got != c.want[i] {
				t.Errorf("%s: step %d (delta %d) = %v, want %v", c.name, i, d, got, c.want[i])
			}
		}
	}
}

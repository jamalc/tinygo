package input

import "testing"

// script is a Source that replays a fixed sequence, counting its polls.
type script struct {
	seq   []Button
	polls int
}

func (s *script) Poll() Button {
	b := None
	if s.polls < len(s.seq) {
		b = s.seq[s.polls]
	}
	s.polls++
	return b
}

func TestMerge(t *testing.T) {
	a := &script{seq: []Button{None, Up, None}}
	b := &script{seq: []Button{Down, Down, None}}
	m := Merge(a, b)
	for i, want := range []Button{Down, Up, None} {
		if got := m.Poll(); got != want {
			t.Errorf("poll %d = %v, want %v", i, got, want)
		}
	}
	if a.polls != 3 || b.polls != 3 {
		t.Errorf("every source must be polled each time; got a=%d b=%d", a.polls, b.polls)
	}
}

func TestReverse(t *testing.T) {
	r := Reverse(&script{seq: []Button{Up, Down, Click, None}})
	for i, want := range []Button{Down, Up, Click, None} {
		if got := r.Poll(); got != want {
			t.Errorf("poll %d = %v, want %v", i, got, want)
		}
	}
}

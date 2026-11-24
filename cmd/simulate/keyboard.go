package main

import (
	"math"
	"sync"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"

	"github.com/jamalc/tinygo/internal/input"
)

// keyboard adapts the arrow keys and space bar to both input ports, standing in
// for the device's UP/DOWN buttons, the tilt of its accelerometer, and the
// click of a plugged-in encoder knob:
//
//   - as an input.Source, an up/down *press* is one discrete control (clock,
//     stress), and space is a Click (breakout's serve);
//   - as an input.TiltSource, the arrows *held down* are a lean (marble,
//     breakout) — all four of them, including left and right.
//
// Ebiten input may only be read from the Update loop, so capture() runs there.
// Presses reach Poll() through a small buffered channel; the held lean is a
// value the render goroutine reads under a lock, since it is a level rather than
// an event and dropping a stale one costs nothing.
type keyboard struct {
	events chan input.Button

	mu   sync.RWMutex
	lean input.Tilt
}

func newKeyboard() *keyboard {
	return &keyboard{events: make(chan input.Button, 8)}
}

// keys maps each arrow key to the lean it contributes, in screen coordinates
// (+X east, +Y south), and to the discrete control it reports if it has one.
//
// Left and right have no button: the discrete port covers stepping through a
// list, which is a one-dimensional idea, so there is nothing for them to mean
// there. They still lean the board, which is how marble and breakout are played.
var keys = []struct {
	key    ebiten.Key
	button input.Button // None for keys that only lean
	dx, dy float32
}{
	{ebiten.KeyArrowUp, input.Up, 0, -1},
	{ebiten.KeyArrowDown, input.Down, 0, 1},
	{ebiten.KeyArrowLeft, input.None, -1, 0},
	{ebiten.KeyArrowRight, input.None, 1, 0},
	{ebiten.KeySpace, input.Click, 0, 0}, // the encoder knob's click: serves the ball
}

// capture reads the keyboard. Call once per Ebiten Update tick.
func (k *keyboard) capture() {
	var lean input.Tilt
	for _, m := range keys {
		if m.button != input.None && inpututil.IsKeyJustPressed(m.key) {
			k.send(m.button)
		}
		if ebiten.IsKeyPressed(m.key) {
			lean.X += m.dx
			lean.Y += m.dy
		}
	}

	// input.Tilt promises a magnitude of at most 1, so holding two keys leans
	// diagonally rather than leaning half again as hard as either key alone.
	// Opposite keys held together cancel, which is the right answer too.
	if mag := float32(math.Hypot(float64(lean.X), float64(lean.Y))); mag > 1 {
		lean.X /= mag
		lean.Y /= mag
	}

	k.mu.Lock()
	k.lean = lean
	k.mu.Unlock()
}

func (k *keyboard) send(b input.Button) {
	select {
	case k.events <- b:
	default: // buffer full: drop, the user is mashing faster than the loop ticks
	}
}

// Poll implements input.Source.
func (k *keyboard) Poll() input.Button {
	select {
	case b := <-k.events:
		return b
	default:
		return input.None
	}
}

// Tilt implements input.TiltSource.
func (k *keyboard) Tilt() input.Tilt {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.lean
}

// Compile-time assertions: one adapter, both ports.
var (
	_ input.Source     = (*keyboard)(nil)
	_ input.TiltSource = (*keyboard)(nil)
)

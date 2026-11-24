//go:build tinygo

// Command acceldiag works out how a board's accelerometer is mounted relative to
// its panel, which is the one thing internal/accel cannot know without the
// hardware in hand.
//
// It prints the live sensor reading over serial and draws the lean on the panel
// as a dot, so you can check the mapping both ways at once:
//
//	make acceldiag [BOARD=matrixportal-m4]
//
// To read it: hold the board however you want "level" to be, wait for the
// calibration line, then lean it and watch the DEV columns — the deviation from
// rest. Lean the board to the RIGHT and note which axis moves and which way; that
// axis and sign is the `right` field of the board's orientation. Then lean it
// toward the BOTTOM of the screen for the `down` field. Put both in
// internal/accel/accel_<board>.go and reflash.
//
// The dot on the panel is the lean as currently mapped. When the mapping is
// right, the dot moves the way you lean; when it isn't, it'll move sideways or
// backwards, and the serial output tells you what to change it to.
package main

import (
	_ "embed"
	"fmt"
	"image/color"
	"time"

	"github.com/jamalc/tinygo/internal/accel"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/hub75"
)

var (
	dot   = color.RGBA{255, 255, 255, 255}
	cross = color.RGBA{34, 44, 92, 255}  // the level marker
	dead  = color.RGBA{238, 53, 46, 255} // shown when no sensor answered
	guide = color.RGBA{0, 147, 60, 255}  // the edges the dot travels between
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	a := accel.New()
	w, h := panel.Size()
	cx, cy := w/2, h/2

	if !a.Enabled() {
		// No sensor: say so on the panel and stop, rather than drawing a dot
		// that will never move and letting it look like a mapping problem.
		for {
			panel.Clear()
			for x := int16(0); x < w; x++ {
				panel.SetPixel(x, cy, dead)
			}
			panel.Display()
			println("acceldiag: no accelerometer on this board")
			time.Sleep(time.Second)
		}
	}

	println("acceldiag: hold the board level; calibrating...")
	a.Calibrate()
	println("acceldiag: calibrated. Lean the board and read the DEV columns.")
	println("           lean RIGHT      -> the axis that moves is `right`")
	println("           lean toward the BOTTOM -> that axis is `down`")
	println()
	println("      raw x       y       z |     dev x       y       z |  tilt X     Y")

	for {
		now, rest := a.Raw()
		t := a.Tilt()

		fmt.Printf("%9d%8d%8d | %8d%8d%8d | %6.2f%6.2f\n",
			now[0]/1000, now[1]/1000, now[2]/1000,
			(now[0]-rest[0])/1000, (now[1]-rest[1])/1000, (now[2]-rest[2])/1000,
			t.X, t.Y)

		panel.Clear()
		// The level marker, and the edges the dot swings between.
		panel.SetPixel(cx, cy, cross)
		for x := int16(0); x < w; x++ {
			panel.SetPixel(x, 0, guide)
			panel.SetPixel(x, h-1, guide)
		}
		// The lean, as currently mapped. Full lean reaches the edge.
		panel.SetPixel(cx+int16(t.X*float32(cx-1)), cy+int16(t.Y*float32(cy-1)), dot)
		panel.Display()

		time.Sleep(80 * time.Millisecond)
	}
}

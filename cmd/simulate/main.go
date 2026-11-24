// Command simulate is a desktop application to simulate the clock display and
// test the clock, docks, conway, stress, bloom, marble, and breakout apps.
//
// It uses the Ebiten game library to create a window and render the display
// output from those packages. It also captures the arrow keys, standing in for
// the device's buttons (clock, stress) and, for the games, for the tilt of the
// board's accelerometer; space stands in for the encoder knob's click, which is
// what serves breakout's ball.
//
// Usage:
//
//	go run ./cmd/simulate [-config path/to/config.txt] (clock|docks|conway|stress|bloom|marble|breakout)
package main

import (
	"flag"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/jamalc/tinygo/internal/bloom"
	"github.com/jamalc/tinygo/internal/breakout"
	"github.com/jamalc/tinygo/internal/clock"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/conway"
	"github.com/jamalc/tinygo/internal/docks"
	"github.com/jamalc/tinygo/internal/marble"
	"github.com/jamalc/tinygo/internal/simulator"
	"github.com/jamalc/tinygo/internal/stress"
)

var (
	configPath = flag.String("config", "cmd/simulate/config.txt", "path to config file")
)

// apps are the display apps the simulator can run, and the values accepted as
// its positional argument.
var apps = []string{"clock", "docks", "conway", "stress", "bloom", "marble", "breakout"}

// inputCapturer is anything that reads Ebiten input once per Update tick.
type inputCapturer interface {
	capture()
}

// game implements the Ebiten Game interface, holding the display, keyboard
// input, and render scale for the running app.
type game struct {
	title string
	disp  *simulator.Display
	scale int
	img   *ebiten.Image
	pix   []byte       // reused each frame: RGBA bytes for WritePixels
	leds  *ledRenderer // non-nil when scale is large enough for an LED look
	input inputCapturer
}

// Update polls the keyboard once per tick. It implements ebiten.Game.
func (g *game) Update() error {
	if g.input != nil {
		g.input.capture()
	}
	return nil
}

// Draw copies the simulator's pixel buffer into an Ebiten image and blits it
// to the screen, scaled by g.scale.
func (g *game) Draw(screen *ebiten.Image) {
	if g.leds != nil {
		g.leds.draw(screen, g.disp)
		return
	}
	pixels := g.disp.CopyPixels()
	for i, c := range pixels {
		g.pix[i*4] = c.R
		g.pix[i*4+1] = c.G
		g.pix[i*4+2] = c.B
		g.pix[i*4+3] = 0xff
	}
	g.img.WritePixels(g.pix)
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(float64(g.scale), float64(g.scale))
	screen.DrawImage(g.img, op)
}

// Layout reports the logical screen size: the display dimensions scaled by
// g.scale.
func (g *game) Layout(_, _ int) (int, int) {
	return g.disp.Width() * g.scale, g.disp.Height() * g.scale
}

func main() {
	scale := flag.Int("scale", 8, "pixel scale factor (default 8 → 512×256 for a 64×32 panel)")
	flag.Parse()

	if *scale < 0 {
		log.Fatalf("--scale must be >= 0, got %d", *scale)
	}

	app := flag.Arg(0)
	if app == "" {
		app = "clock"
	}
	if !slices.Contains(apps, app) {
		log.Fatalf("unknown app %q (valid: %s)", app, strings.Join(apps, ", "))
	}

	// Layered config: New parses the embedded device config
	// (internal/config/config.txt), then Load overlays the simulate file —
	// keys present there win, absent keys inherit the device values.
	cfg := config.New()
	if err := cfg.Load(*configPath); err != nil {
		log.Fatalf("load config: %v", err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		log.Fatal("config must specify width and height")
	}

	display := simulator.New(cfg.Width, cfg.Height)

	g := &game{
		disp:  display,
		scale: *scale,
		img:   ebiten.NewImage(cfg.Width, cfg.Height),
		pix:   make([]byte, cfg.Width*cfg.Height*4),
	}
	// Below ~3x there aren't enough pixels per cell to draw a convincing dot,
	// so keep the plain block-scaled path for tiny scales.
	if *scale >= 3 {
		g.leds = newLEDRenderer(*scale)
	}

	switch app {
	case "clock":
		kb := newKeyboard()
		g.input = kb
		g.title = "Subway Clock"
		go clock.Run(cfg, display, kb)
	case "docks":
		g.title = "Citi Bike Docks"
		go docks.Run(cfg, display)
	case "conway":
		g.title = "Conway's Game of Life"
		go conway.Run(cfg, display)
	case "stress":
		kb := newKeyboard()
		g.input = kb
		g.title = "HUB75 Stress"
		go stress.Run(display, cfg.Width, cfg.Height, kb)
	case "bloom":
		g.title = "Bloom"
		go bloom.Run(cfg, display)
	case "marble":
		kb := newKeyboard()
		g.input = kb
		g.title = "Tilt Maze"
		go marble.Run(cfg, display, kb)
	case "breakout":
		kb := newKeyboard()
		g.input = kb
		g.title = "Breakout"
		go breakout.Run(cfg, display, kb, kb)
	}

	ebiten.SetWindowSize(cfg.Width**scale, cfg.Height**scale)
	ebiten.SetWindowTitle(g.title + fmt.Sprintf(" — %s (%dx%d @ %dx)", app, cfg.Width, cfg.Height, *scale))
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeDisabled)

	if err := ebiten.RunGame(g); err != nil {
		log.Fatal(err)
	}
}

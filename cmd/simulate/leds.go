package main

import (
	"image/color"
	"math"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/jamalc/tinygo/internal/simulator"
)

// ledRenderer draws a simulator display so the pixels read as discrete LEDs:
// round, slightly glowing dots separated by dark gaps, with an additive bloom
// halo so bright LEDs bleed into their neighbours the way a real HUB75 panel
// does behind its diffuser.
//
// Two sprites are precomputed once at construction:
//
//   - dot:  a crisp, white, anti-aliased disc the size of one cell. It carries
//     the LED shape in its (premultiplied) alpha, so drawing it with a per-LED
//     ColorScale tints it to the pixel colour.
//   - glow: a larger, soft radial falloff drawn additively and centred on the
//     LED, producing the bloom that spills past the cell boundary.
type ledRenderer struct {
	scale int
	dot   *ebiten.Image
	glow  *ebiten.Image
	// glowOffset positions the (larger) glow sprite so its centre lands on the
	// centre of the cell.
	glowOffset float64
}

// newLEDRenderer builds the dot and glow sprites for a given cell scale. A
// scale below ~3 is too small for a convincing dot, so callers should fall
// back to plain block scaling there.
func newLEDRenderer(scale int) *ledRenderer {
	dot := makeDot(scale)
	glowSize := scale * 3
	glow := makeGlow(glowSize)
	return &ledRenderer{
		scale:      scale,
		dot:        dot,
		glow:       glow,
		glowOffset: float64(scale-glowSize) / 2,
	}
}

// draw renders the display's current pixels onto screen. Off (black) pixels are
// skipped entirely, leaving the dark gaps that sell the LED look.
func (r *ledRenderer) draw(screen *ebiten.Image, disp *simulator.Display) {
	screen.Fill(color.Black)

	pixels := disp.CopyPixels()
	w := disp.Width()
	s := float64(r.scale)

	// Bloom pass first, so the crisp dots sit on top of their own halos.
	for i, c := range pixels {
		if c.R == 0 && c.G == 0 && c.B == 0 {
			continue
		}
		x := float64(i%w)*s + r.glowOffset
		y := float64(i/w)*s + r.glowOffset
		op := &ebiten.DrawImageOptions{Blend: ebiten.BlendLighter}
		op.GeoM.Translate(x, y)
		op.ColorScale.Scale(float32(c.R)/255, float32(c.G)/255, float32(c.B)/255, 1)
		screen.DrawImage(r.glow, op)
	}

	for i, c := range pixels {
		if c.R == 0 && c.G == 0 && c.B == 0 {
			continue
		}
		x := float64(i%w) * s
		y := float64(i/w) * s
		op := &ebiten.DrawImageOptions{}
		op.GeoM.Translate(x, y)
		op.ColorScale.Scale(float32(c.R)/255, float32(c.G)/255, float32(c.B)/255, 1)
		screen.DrawImage(r.dot, op)
	}
}

// makeDot builds a white, anti-aliased disc filling a size×size cell, leaving a
// small dark margin so adjacent LEDs don't touch. The shape lives in the
// premultiplied alpha (every channel = coverage), so a later ColorScale tints
// it cleanly.
func makeDot(size int) *ebiten.Image {
	pix := make([]byte, size*size*4)
	center := float64(size-1) / 2
	// Outer edge of the disc; the gap between this and the cell edge is the
	// dark grid line. Inner radius is where the solid core starts to soften.
	outer := float64(size) * 0.46
	inner := outer * 0.6
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)-center, float64(y)-center)
			a := 1 - smoothstep(inner, outer, d)
			b := byte(a * 255)
			j := (y*size + x) * 4
			pix[j], pix[j+1], pix[j+2], pix[j+3] = b, b, b, b
		}
	}
	img := ebiten.NewImage(size, size)
	img.WritePixels(pix)
	return img
}

// makeGlow builds a soft radial halo used for the additive bloom pass. It peaks
// well below full brightness so overlapping halos accumulate gently rather than
// blowing out to white.
func makeGlow(size int) *ebiten.Image {
	pix := make([]byte, size*size*4)
	center := float64(size-1) / 2
	sigma := float64(size) * 0.16
	const peak = 0.35
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := math.Hypot(float64(x)-center, float64(y)-center)
			a := peak * math.Exp(-(d*d)/(2*sigma*sigma))
			b := byte(a * 255)
			j := (y*size + x) * 4
			pix[j], pix[j+1], pix[j+2], pix[j+3] = b, b, b, b
		}
	}
	img := ebiten.NewImage(size, size)
	img.WritePixels(pix)
	return img
}

// smoothstep returns a smooth 0→1 ramp as x moves from edge0 to edge1, clamped
// outside that range.
func smoothstep(edge0, edge1, x float64) float64 {
	if edge1 == edge0 {
		if x < edge0 {
			return 0
		}
		return 1
	}
	t := (x - edge0) / (edge1 - edge0)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return t * t * (3 - 2*t)
}

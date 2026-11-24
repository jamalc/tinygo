// Package bloom is a fireworks showcase built to make a HUB75 panel look
// brilliant rather than to test it. It leans on framebuffer read-back
// (GetPixel) to paint glowing, decaying trails, so it drives any Display that
// can be read as well as written — the on-device hub75.Device or the host
// simulator.Display — and the same animation runs on hardware and in the
// Ebiten simulator.
//
// Why read-back makes it shine:
//
//   - Dark tones. Every frame the whole framebuffer is read back, scaled down a
//     hair, and written again. That feedback paints glowing trails that fade
//     smoothly all the way down through the raw 1..8 range — the deep end of the
//     gamma curve. The trails ARE the dark-tone demo.
//   - Color accuracy. Bursts are drawn in saturated HSV and blended additively;
//     overlapping sparks bloom toward white through the full 8-bit-per-channel
//     gamut.
//   - Speed / efficiency. A full-panel read-modify-write plus a few hundred
//     particles of float physics, every frame. Double-buffered, so it never
//     tears.
package bloom

import (
	"image/color"
	"time"

	"github.com/jamalc/tinygo/internal/config"
)

// Display is the subset of a panel bloom drives. Unlike the other apps' it
// also reads the framebuffer (GetPixel) — the read-back feature the showcase is
// built around. Both hub75.Device and simulator.Display satisfy it.
type Display interface {
	SetPixel(x, y int16, c color.RGBA)
	GetPixel(x, y int16) color.RGBA
	Size() (int16, int16)
	Display() error
}

// Tunables, all in per-frame units (the driver paces us at the refresh rate).
const (
	gravity   = 0.018 // downward pull on every spark — gentle, so trails linger and decay
	drag      = 0.985 // velocity bleed, so bursts settle instead of flying off
	trailFade = 232   // framebuffer feedback multiplier /256 (~0.906); lower = shorter trails
	launchGap = 22    // min frames between rocket launches
)

const (
	maxParticles = 260
	maxRockets   = 4
)

type particle struct {
	x, y, vx, vy float32
	life         float32 // 1 at birth → 0 dead
	hue          uint8
	active       bool
}

type rocket struct {
	x, y, vx, vy float32
	target       float32 // burst altitude
	hue          uint8
	active       bool
}

// show holds one animation's state: the field dimensions, the particle and
// rocket pools, and the PRNG. Bundling it here keeps the package free of mutable
// globals, so two shows could run side by side without trampling each other.
type show struct {
	w, h    int
	rng     uint32 // xorshift32 state
	parts   [maxParticles]particle
	rockets [maxRockets]rocket
}

// newShow sizes a show to w×h and seeds the PRNG. The seed is fixed (there is no
// clock on the MCU); the long-period mixing still gives every firework its own
// colour, height and spread.
func newShow(w, h int) *show {
	return &show{w: w, h: h, rng: 0x9e3779b9}
}

// rnd advances the xorshift32 PRNG — a tiny, allocation-free generator.
func (s *show) rnd() uint32 {
	s.rng ^= s.rng << 13
	s.rng ^= s.rng >> 17
	s.rng ^= s.rng << 5
	return s.rng
}

func (s *show) unit() float32 { return float32(s.rnd()&0xffff) / 65536.0 } // 0..1

func (s *show) between(a, b float32) float32 { return a + (b-a)*s.unit() }

// Run animates fireworks forever on d, sizing the field from the display
// dimensions. It is intentionally thin glue over the showcase logic, so it is
// verified by build, not unit tests.
func Run(cfg config.Config, d Display) {
	wi, hi := d.Size()
	s := newShow(int(wi), int(hi))

	cooldown := 0
	for frame := 0; ; frame++ {
		time.Sleep(20 * time.Millisecond)
		// Feedback decay: the previous frame, dimmed. This is the read-back showcase — and
		// what carves every glowing trail down into the dark end of the gamma curve.
		for y := int16(0); y < hi; y++ {
			for x := int16(0); x < wi; x++ {
				c := d.GetPixel(x, y)
				if c.R == 0 && c.G == 0 && c.B == 0 {
					continue
				}
				d.SetPixel(x, y, color.RGBA{
					R: fade(c.R),
					G: fade(c.G),
					B: fade(c.B),
					A: 255,
				})
			}
		}

		// Launch a new rocket when the sky has room and the cooldown has elapsed.
		if cooldown <= 0 {
			if r := s.freeRocket(); r != nil {
				r.active = true
				r.x = s.between(float32(s.w)*0.2, float32(s.w)*0.8)
				r.y = float32(s.h - 1)
				r.vx = s.between(-0.08, 0.08)
				r.vy = s.between(-0.72, -0.52)
				r.target = s.between(3, float32(s.h)*0.45)
				r.hue = uint8(s.rnd())
				cooldown = launchGap + int(s.rnd()%24)
			}
		}
		cooldown--

		// Rockets: climb, leave a hot trail (via feedback), then burst at altitude.
		for i := range s.rockets {
			r := &s.rockets[i]
			if !r.active {
				continue
			}
			r.vy += gravity
			r.x += r.vx
			r.y += r.vy
			s.plotAdd(d, r.x, r.y, hsv(r.hue, 40, 255)) // near-white hot head
			if r.vy >= 0 || r.y <= r.target {
				s.burst(r.x, r.y, r.hue)
				r.active = false
			}
		}

		// Sparks: ballistic, cooling as they fall. Brightness tracks life so they redden and
		// dim into the trail rather than vanishing.
		for i := range s.parts {
			p := &s.parts[i]
			if !p.active {
				continue
			}
			p.vy += gravity
			p.vx *= drag
			p.vy *= drag
			p.x += p.vx
			p.y += p.vy
			p.life -= 0.007
			if p.life <= 0 || p.y >= float32(s.h) {
				p.active = false
				continue
			}
			v := uint8(p.life * 255)
			s.plotAdd(d, p.x, p.y, hsv(p.hue, 230, v))
		}

		d.Display()
	}
}

// burst scatters a ring of sparks from (x,y) around a base hue, with a spread of speeds so
// the shell looks full rather than a clean circle.
func (s *show) burst(x, y float32, baseHue uint8) {
	n := 28 + int(s.rnd()%22)
	for i := 0; i < n; i++ {
		p := s.freeParticle()
		if p == nil {
			return
		}
		ang := s.between(0, 6.2832)
		spd := s.between(0.25, 1.5)
		p.active = true
		p.x, p.y = x, y
		p.vx = cos(ang) * spd
		p.vy = sin(ang) * spd
		p.life = s.between(0.7, 1.0)
		p.hue = baseHue + uint8(s.rnd()%40) - 20 // tight hue band per firework
	}
}

func (s *show) freeParticle() *particle {
	for i := range s.parts {
		if !s.parts[i].active {
			return &s.parts[i]
		}
	}
	return nil
}

func (s *show) freeRocket() *rocket {
	for i := range s.rockets {
		if !s.rockets[i].active {
			return &s.rockets[i]
		}
	}
	return nil
}

// plotAdd blends a colour additively into the grid (read-modify-write through GetPixel), so
// overlapping sparks bloom toward white instead of overwriting.
func (s *show) plotAdd(d Display, fx, fy float32, c color.RGBA) {
	x, y := int(fx+0.5), int(fy+0.5)
	if x < 0 || x >= s.w || y < 0 || y >= s.h {
		return
	}
	o := d.GetPixel(int16(x), int16(y))
	d.SetPixel(int16(x), int16(y), color.RGBA{
		R: addClamp(o.R, c.R),
		G: addClamp(o.G, c.G),
		B: addClamp(o.B, c.B),
		A: 255,
	})
}

// fade scales a channel toward zero with a floor subtract, so trails actually reach black
// instead of asymptoting at a dim residue.
func fade(c uint8) uint8 {
	v := uint16(c) * trailFade >> 8
	if v > 0 {
		v--
	}
	return uint8(v)
}

func addClamp(a, b uint8) uint8 {
	s := uint16(a) + uint16(b)
	if s > 255 {
		return 255
	}
	return uint8(s)
}

// hsv converts hue/sat/val (all 0..255) to RGB with the standard six-sector method — integer
// math, no float, fast enough to call per spark per frame.
func hsv(hh, s, v uint8) color.RGBA {
	if s == 0 {
		return color.RGBA{R: v, G: v, B: v, A: 255}
	}
	region := hh / 43
	rem := uint16(hh-region*43) * 6
	p := uint8(uint16(v) * (255 - uint16(s)) / 255)
	q := uint8(uint16(v) * (255 - uint16(s)*rem/255) / 255)
	t := uint8(uint16(v) * (255 - uint16(s)*(255-rem)/255) / 255)
	switch region {
	case 0:
		return color.RGBA{R: v, G: t, B: p, A: 255}
	case 1:
		return color.RGBA{R: q, G: v, B: p, A: 255}
	case 2:
		return color.RGBA{R: p, G: v, B: t, A: 255}
	case 3:
		return color.RGBA{R: p, G: q, B: v, A: 255}
	case 4:
		return color.RGBA{R: t, G: p, B: v, A: 255}
	default:
		return color.RGBA{R: v, G: p, B: q, A: 255}
	}
}

// sin/cos: a small lookup avoids pulling in float64 math.Sin per spawn. 64-entry quarter-ish
// table, linear enough for spark directions. Input is radians 0..2π.
var sinT = [65]float32{
	0.0000, 0.0245, 0.0491, 0.0736, 0.0980, 0.1224, 0.1467, 0.1710,
	0.1951, 0.2191, 0.2430, 0.2667, 0.2903, 0.3137, 0.3369, 0.3599,
	0.3827, 0.4052, 0.4276, 0.4496, 0.4714, 0.4929, 0.5141, 0.5350,
	0.5556, 0.5758, 0.5957, 0.6152, 0.6344, 0.6532, 0.6716, 0.6895,
	0.7071, 0.7242, 0.7410, 0.7572, 0.7730, 0.7883, 0.8032, 0.8176,
	0.8315, 0.8449, 0.8577, 0.8701, 0.8819, 0.8932, 0.9040, 0.9142,
	0.9239, 0.9330, 0.9415, 0.9495, 0.9569, 0.9638, 0.9700, 0.9757,
	0.9808, 0.9853, 0.9892, 0.9925, 0.9952, 0.9973, 0.9988, 0.9997,
	1.0000,
}

func sin(rad float32) float32 {
	const twoPi = 6.2831853
	for rad < 0 {
		rad += twoPi
	}
	for rad >= twoPi {
		rad -= twoPi
	}
	// Map into the quarter table by symmetry.
	quarter := rad / (twoPi / 4) // 0..4
	q := int(quarter)
	frac := quarter - float32(q)
	switch q {
	case 0:
		return lerpSin(frac)
	case 1:
		return lerpSin(1 - frac)
	case 2:
		return -lerpSin(frac)
	default:
		return -lerpSin(1 - frac)
	}
}

func cos(rad float32) float32 { return sin(rad + 6.2831853/4) }

// lerpSin samples sinT for a quarter-phase fraction (0..1) with linear interpolation.
func lerpSin(frac float32) float32 {
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	pos := frac * 64
	i := int(pos)
	if i >= 64 {
		return sinT[64]
	}
	return sinT[i] + (sinT[i+1]-sinT[i])*(pos-float32(i))
}

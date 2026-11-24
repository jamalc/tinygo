// Package stress drives worst-case test patterns at a HUB75 panel to
// verify shift-register data timing and signal integrity.
//
// The patterns target several distinct failure mechanisms:
//
//   - Data setup time: the RGB lines must be stable before each clock edge
//     (the hub75 driver emits each pixel as a data byte followed by the
//     same byte with the clock raised, so data leads the edge by one DMA
//     beat). Stressed by pixel-to-pixel transitions along the shift
//     direction (x). Sparse apps like conway and clock barely exercise it;
//     the stripe/checker/noise patterns flip the data lines on every clock
//     instead.
//   - Simultaneous switching: alternating white/black pixels swing all six
//     RGB lines together on every pixel — the worst ground-bounce case
//     (the stripe and checker patterns; a solid field holds them static).
//   - Bit-plane (BCM) timing: dim values exercise only the low-order planes,
//     which the full-brightness patterns never touch ("dim green stripes").
//   - Row addressing: adjacent scan rows differing maximally stresses address
//     settling and OE blanking ("horizontal stripes", "half split").
//   - Visual color correctness: SMPTE split-field bars make a wrong channel or
//     a mis-set black level obvious to the eye ("smpte bars"), and a full HSV
//     field sweeps the entire color solid — hue across x, saturation and value
//     down y — to show off the panel's 8-bit color range ("spectrum").
//   - Bit-depth smoothness: slow-moving sine fields in hue and brightness
//     ("plasma") band and crawl visibly when the configured plane count is
//     too low for the gradient, and "depth split" renders the same dim
//     fade at 8-bit and 4-bit input resolution on the two panel halves
//     for a direct comparison.
//   - Dimming quality: "breathe" alternates software (pixel-scaled) and
//     hardware (SetBrightness) dimming stints on one scene — software
//     stints band at the dim end on every driver, hardware stints stay
//     smooth where the capability exists and hold steady where it
//     doesn't.
//   - Showpieces: animated demoscene effects (fire, ripple, tunnel, starfield,
//     comet, munch, xor plaid, julia, mandelbrot, radar) that are eye-candy
//     first but double as wide color-space and motion coverage — dense palettes
//     that exercise the bit-planes, and full-frame motion that makes tears or
//     stuck pixels plain.
package stress

import (
	"image/color"
	"math"
	"time"

	"github.com/jamalc/tinygo/internal/input"
)

var (
	black   = color.RGBA{0, 0, 0, 255}
	white   = color.RGBA{255, 255, 255, 255}
	red     = color.RGBA{255, 0, 0, 255}
	green   = color.RGBA{0, 255, 0, 255}
	blue    = color.RGBA{0, 0, 255, 255}
	yellow  = color.RGBA{255, 255, 0, 255}
	cyan    = color.RGBA{0, 255, 255, 255}
	magenta = color.RGBA{255, 0, 255, 255}
)

// dimLevel is the 8-bit green value for the dim stripe. It looks high for
// "dim", but the driver applies a steep gamma-2.6 curve into its native
// depth: at the default 8 bit-planes every 8-bit input below 24 collapses
// to native 0 (a blank panel). 72 maps to native level 10 of 255, lighting
// only low bit-planes — the shortest, most timing-sensitive OE windows.
const dimLevel = 72

// Display is the subset of a panel these patterns drive. The on-device
// hub75.Device satisfies it.
type Display interface {
	SetPixel(x, y int16, c color.RGBA)
	Display() error
	Clear()
}

// Pattern is a named full-screen test pattern. At reports the color of pixel
// (x, y) on the given animation frame; static patterns ignore frame.
//
// Bright, when non-nil, requests a global hardware brightness for the
// frame (0-255). It is honored only when the display implements
// SetBrightness (the hub75.Brightness capability); on displays
// without it the request is silently skipped — which is itself the
// comparison the "breathe" pattern visualizes.
type Pattern struct {
	Name   string
	At     func(x, y, frame int) color.RGBA
	Bright func(frame int) uint8
}

// brightnessSetter is the optional dimming capability some panels
// expose (matches hub75.Brightness without importing it, keeping this
// package portable to the simulator).
type brightnessSetter interface {
	SetBrightness(b uint8)
}

// verticalStripe returns on for even columns and black for odd columns,
// producing 1-pixel-wide vertical stripes. Because the panel shifts pixels out
// along x, this toggles the RGB data lines on every clock — the maximum
// setup-time pressure.
func verticalStripe(on color.RGBA, x int) color.RGBA {
	if x%2 == 0 {
		return on
	}
	return black
}

// horizontalStripe returns on for even rows and black for odd rows, making
// adjacent scan rows differ to stress row-address settling and OE blanking.
func horizontalStripe(on color.RGBA, y int) color.RGBA {
	if y%2 == 0 {
		return on
	}
	return black
}

// checker returns on when x+y is even and black otherwise, toggling the data
// lines on both axes.
func checker(on color.RGBA, x, y int) color.RGBA {
	if (x+y)%2 == 0 {
		return on
	}
	return black
}

// scrollStripe is verticalStripe with its phase advanced by frame, so every
// column position experiences both transitions over time — no column gets a
// permanently "lucky" stable sample.
func scrollStripe(on color.RGBA, x, frame int) color.RGBA {
	return verticalStripe(on, x+frame)
}

const (
	tearBarWidth = 4 // bar thickness in pixels
	tearStep     = 4 // pixels the bar advances per frame; bigger = more tear
)

// tearBar is the vsync-tear probe: a high-contrast white bar of tearBarWidth
// swept tearStep pixels per frame across an otherwise black field. Because the
// whole frame is rewritten each Display, a double-buffered scanout shows a clean
// solid vertical line (it only ever reads a completed frame) while a
// single-buffered one tears — rows scanned early show the bar at its old x and
// later rows at its new x, breaking the line into a moving staircase.
func tearBar(x, w, frame int) color.RGBA {
	barX := (frame * tearStep) % w
	if x >= barX && x < barX+tearBarWidth {
		return white
	}
	return black
}

// whiteMagenta holds R and B high on every column and toggles only green
// (white vs magenta), isolating the green line as the lone variable on an
// otherwise fully loaded, switching bus.
func whiteMagenta(x int) color.RGBA {
	if x%2 == 0 {
		return white
	}
	return magenta
}

// dimGreenStripe is a green stripe at dimLevel, so corruption shows up in the
// low-order bit-planes that full-brightness patterns never exercise.
func dimGreenStripe(x int) color.RGBA {
	return verticalStripe(color.RGBA{0, dimLevel, 0, 255}, x)
}

// halfSplit lights the top sub-panel (y < h/2) white and leaves the bottom
// black, driving the r1g1b1 group hard while r2g2b2 stays idle.
func halfSplit(y, h int) color.RGBA {
	if y < h/2 {
		return white
	}
	return black
}

// noise is a deterministic per-pixel hash whose channels are each fully on or
// off. Re-evaluated every frame it acts as a fuzzer, hitting aggressor
// combinations the structured patterns never produce — without carrying any
// RNG state across the portable/device boundary.
func noise(x, y, frame int) color.RGBA {
	h := uint32(x)*73856093 ^ uint32(y)*19349663 ^ uint32(frame)*83492791
	h ^= h >> 13
	h *= 0x5bd1e995
	h ^= h >> 15
	ch := func(bit uint) uint8 {
		if h&(1<<bit) != 0 {
			return 255
		}
		return 0
	}
	return color.RGBA{ch(0), ch(1), ch(2), 255}
}

// scaleColor dims c uniformly to the given level (0 = off, 255 = full),
// preserving its hue. Each channel scales independently, so zero channels stay
// zero.
func scaleColor(c color.RGBA, level uint8) color.RGBA {
	s := func(v uint8) uint8 { return uint8(uint16(v) * uint16(level) / 255) }
	return color.RGBA{s(c.R), s(c.G), s(c.B), 255}
}

// ramp scales base from off (at x=0) to full intensity (at x=w-1) across the
// panel width, sweeping every brightness level. This exercises the full set of
// BCM bit-planes and exposes gamma banding — coverage the all-on and low-plane
// patterns miss. Only base's non-zero channels light, so a colored base stays
// that color.
//
// The dim end sits below the panel's representable floor and reads as black
// (the steep gamma curve crushes the lowest inputs to native 0); how many
// columns that covers depends on the configured bit depth. The ramp stays
// linear in 8-bit space so it makes no bit-depth assumption.
func ramp(base color.RGBA, x, w int) color.RGBA {
	if w <= 1 {
		return base
	}
	return scaleColor(base, uint8(uint16(x)*255/uint16(w-1)))
}

// rgbRamps stacks a red, green, and blue ramp as three equal horizontal bands,
// so each channel's full intensity range — gamma, banding, plane timing — can
// be compared side by side on one screen.
func rgbRamps(x, y, w, h int) color.RGBA {
	switch y * 3 / h {
	case 0:
		return ramp(red, x, w)
	case 1:
		return ramp(green, x, w)
	default:
		return ramp(blue, x, w)
	}
}

// wheel maps a position around the hue circle (0..255) to a fully saturated
// color using integer math: red at 0, green at ~85, blue at ~170, with the
// secondaries (yellow ~43, cyan ~128, magenta ~213) between them.
//
// It walks the hue circle in six segments, holding one channel at full 255 and
// one at 0 while the third ramps — so at every position one channel is off and
// one is fully on. That keeps the secondaries at full intensity: a naive
// crossfade between adjacent primaries would pass through half-lit midpoints
// (yellow as 127,127,0), which the driver's steep gamma curve then crushes to a
// dim, muddy band — the primaries stay vivid but the colors between them wash
// out.
func wheel(pos uint8) color.RGBA {
	p := uint16(pos) * 6
	f := uint8(p % 256) // ramp across the current sixth of the wheel
	switch p / 256 {
	case 0:
		return color.RGBA{255, f, 0, 255} // red → yellow
	case 1:
		return color.RGBA{255 - f, 255, 0, 255} // yellow → green
	case 2:
		return color.RGBA{0, 255, f, 255} // green → cyan
	case 3:
		return color.RGBA{0, 255 - f, 255, 255} // cyan → blue
	case 4:
		return color.RGBA{f, 0, 255, 255} // blue → magenta
	default:
		return color.RGBA{255, 0, 255 - f, 255} // magenta → red
	}
}

// hsv tints a fully-saturated hue color (from wheel) by a saturation and a
// value, in integer math. Saturation desaturates each channel toward white
// (s=255 keeps the pure hue, s=0 yields a neutral gray); value then scales the
// result toward black. It lets a pattern reach the interior of the color solid
// — pastels, shades, and grays — that wheel alone (always one channel off, one
// full on) can never produce.
func hsv(c color.RGBA, s, v uint8) color.RGBA {
	f := func(ch uint8) uint8 {
		d := 255 - uint16(s)*(255-uint16(ch))/255 // desaturate toward white
		return uint8(uint16(v) * d / 255)         // then scale by value
	}
	return color.RGBA{f(c.R), f(c.G), f(c.B), 255}
}

// spectrum is a full HSV color field, the widest spread of distinct 8-bit
// colors of any pattern here. Hue sweeps across x and drifts with the frame, so
// every column visits every hue over time; y walks the other two axes — the top
// half fades from white (at the top edge) down to fully-saturated color at the
// midline, the bottom half fades that color down to black. Where wheel and
// rainbow only ride the saturated rim of the color wheel, this fills its
// interior too. The smooth horizontal hue sweep and vertical fades make a fine
// 8-bit showcase: too few bit-planes turn the continuous gradients into visible
// steps that crawl as the field drifts.
func spectrum(x, y, w, h, frame int) color.RGBA {
	base := wheel(uint8(x*256/w + frame))
	if h <= 1 {
		return base
	}
	half := h / 2
	if y < half {
		// Top: saturation rises 0..255, white at the top edge down to full color.
		return hsv(base, uint8(uint16(y)*255/uint16(half)), 255)
	}
	// Bottom: value falls 255..0, full color at the midline down to black.
	return hsv(base, 255, uint8(255-uint16(y-half)*255/uint16(h-half)))
}

// rainbow is a 2D color-space sweep: hue runs across x and brightness down y
// (full at the top, fading to black at the bottom edge). It covers far more of
// the color space than any single gradient and is the best at surfacing channel
// mixing and crosstalk.
func rainbow(x, y, w, h int) color.RGBA {
	base := wheel(uint8(x * 256 / w))
	if h <= 1 {
		return base
	}
	return scaleColor(base, uint8(255-uint16(y)*255/uint16(h-1)))
}

// colorwash lights every pixel every frame with a drifting plaid of the three
// channels — R follows x, G follows y, B follows x+y, all sliding with the frame
// counter. Because no pixel or channel is ever off for long, it doubles as a
// smoke test: a dead pixel, stuck channel, or wiring fault shows up immediately
// against the moving field.
func colorwash(x, y, frame int) color.RGBA {
	return color.RGBA{
		R: uint8(x*4 + frame),
		G: uint8(y*8 - frame),
		B: uint8((x+y)*2 + frame),
		A: 255,
	}
}

const (
	rainTail = 12 // lit trail length, in pixels, behind each drop head
	rainGap  = 16 // dark gap added to each column's cycle so drops are spaced out
)

// rainHash is a small integer hash used to give each column its own fall speed
// and starting phase (and to twinkle the trail), so the rain looks irregular
// without carrying any RNG state across frames or the portable/device boundary
// — the same stateless trick the noise pattern uses.
func rainHash(v uint32) uint32 {
	v *= 2654435761
	v ^= v >> 15
	v *= 0x2c1b3c6d
	v ^= v >> 12
	return v
}

// matrixRain is the "digital rain" effect: green drops fall down each column,
// a near-white head leading a fading green tail, over a black field. It carries
// no state — each column's speed and phase come from hashing x, and the head's
// position at the current frame is pure arithmetic, so any pixel is a closed
// form of (x, y, frame).
//
// Each column cycles through a virtual length of h+rainTail+rainGap: the extra
// span is the dark gap between successive drops, and the per-column speed (a
// few half-pixels per frame) keeps neighbouring columns out of lockstep. A lit
// trail pixel occasionally dims for one frame — a per-cell, per-frame hash —
// which reads as the glyphs flickering and changing in the original effect.
//
// As a test pattern it is a sparse, mostly-green moving field: it leans on the
// green line and the low-order bit-planes (the dim tail), and any motion tear
// or stuck pixel stands out plainly against the black background.
func matrixRain(x, y, h, frame int) color.RGBA {
	hsh := rainHash(uint32(x))
	period := h + rainTail + rainGap
	speed := 1 + int(hsh%3) // 1..3 half-pixels per frame
	headY := (frame*speed/2 + int(hsh>>8)%period) % period
	dist := headY - y
	if dist < 0 || dist > rainTail {
		return black // ahead of the head, or past the end of the tail
	}
	if dist == 0 {
		return color.RGBA{180, 255, 180, 255} // bright white-green head
	}
	level := uint8(255 - dist*255/rainTail) // tail fades with distance behind the head
	if rainHash(uint32(x)*131^uint32(y)*977^uint32(frame)*49297)&3 == 0 {
		level /= 3 // brief dropout — a glyph blinking as it changes
	}
	return color.RGBA{0, level, 0, 255}
}

// sine is a 256-entry sine lookup table, output 1..255 centered on 128:
// one full period per 256 phase steps. Integer phase arithmetic wraps
// naturally through the uint8 index, so callers just add scaled
// coordinates and frame counters.
var sine [256]uint8

// exp2tab is the fractional part of a smooth exponential zoom: exp2tab[i] =
// 4096 * 2^(-i/256) in Q12, falling from 4096 (1.0) to ~2048 (0.5) across one
// octave. Combined with an integer shift for the whole-octave part, it gives a
// continuous geometric zoom from an integer phase, no float math at runtime.
var exp2tab [256]uint16

func init() {
	for i := range sine {
		sine[i] = uint8(128 + 127*math.Sin(float64(i)*(2*math.Pi/256)))
	}
	for i := range exp2tab {
		exp2tab[i] = uint16(4096 * math.Exp2(-float64(i)/256))
	}
}

// plasma is the classic demoscene effect: phase-shifted sine fields
// summed per pixel select a hue, and a fourth, slower field breathes
// the brightness. The result is smooth gradients in hue and brightness
// that drift continuously — the showcase for high bit depth. With too
// few planes the dim regions collapse into visible bands that crawl
// with the animation; with enough planes the field looks continuous.
//
// The brightness floor of 30 is deliberate: it sits just above the
// 8-plane gamma crush threshold (input 24), so the trough of the
// breathing field reads as a faint glow at full depth but as a
// sweeping dead band on any shallower config.
func plasma(x, y, frame int) color.RGBA {
	t := frame * 4
	a := int(sine[uint8(x*8+t)])
	b := int(sine[uint8(y*12-t)])
	c := int(sine[uint8((x+y)*5+t/2)])
	// Sum and wrap (mod 256) rather than average: averaging clusters the hue
	// around the middle of the wheel (the central-limit effect of summing sines),
	// leaving the reds barely visible. Wrapping the sum sweeps the full spectrum.
	hue := uint8(a + b + c)
	bright := uint8(30 + int(sine[uint8(x*3+y*3-t/2)])*225/255)
	return scaleColor(wheel(hue), bright)
}

// depthSplit drifts the same dim brightness wave across both halves of
// the panel: the top half keeps the full 8-bit input range, the bottom
// half is quantized to 16 input levels (4-bit resolution). The wave
// lives in the dim range (inputs 0..120) where the gamma curve spreads
// levels thinnest — exactly where extra bit-planes pay off. With a
// high plane count the top half breathes smoothly while the bottom
// shows discrete bands marching in step; if both halves band alike,
// the configured plane count — not the input — is the bottleneck.
func depthSplit(x, y, h, frame int) color.RGBA {
	v := uint8(int(sine[uint8(x*4-frame)]) * 120 / 255)
	if y >= h/2 {
		v &^= 0x0F // 16 input levels, as a 4-bit source would deliver
	}
	return color.RGBA{v, v, v, 255}
}

// breathFrames is one dimming mode's stint: a full sine breath
// (~3.4 s at the loop cadence). The breathe pattern alternates
// software and hardware dimming every stint, so both fit comfortably
// inside one auto-advance hold.
const breathFrames = 48

// breathLevel follows a full sine cycle per stint, dipping near black
// where dimming artifacts live.
func breathLevel(frame int) uint8 {
	return sine[uint8(frame*256/breathFrames)]
}

// breatheHardware reports whether the frame is in a hardware-dimming
// stint (odd stints) rather than a software-dimming one (even stints).
func breatheHardware(frame int) bool {
	return (frame/breathFrames)%2 == 1
}

// breatheScene is the reference image the breath dims: saturated hue
// bars with a grayscale ramp footer, so both color banding and
// dim-end crush have somewhere to show.
func breatheScene(x, y, w, h int) color.RGBA {
	if y >= h-h/4 {
		return ramp(white, x, w)
	}
	return wheel(uint8(x * 256 / w))
}

// breathe compares the two ways to dim a panel by alternating them on
// the same breathing scene:
//
//   - Software stints scale the pixel values before the gamma curve —
//     available on every driver, but the dim end collapses into the
//     curve's lowest levels: visible banding and color crush near the
//     bottom of each breath.
//   - Hardware stints draw full pixel values and ask the driver to dim
//     via SetBrightness (the hub75.Brightness capability): full color
//     resolution at every level, so the breath stays smooth — on
//     displays that implement it. On one without it the scene simply
//     holds steady at full brightness for the stint, making the
//     capability gap itself visible.
//
// The right-edge marker column is drawn at full white in both stints:
// software dimming spares it (steady marker = software stint), while
// hardware dimming breathes it along with everything else.
func breathe(x, y, w, h, frame int) color.RGBA {
	if x == w-1 {
		return white
	}
	c := breatheScene(x, y, w, h)
	if breatheHardware(frame) {
		return c // Run dims globally via Pattern.Bright
	}
	return scaleColor(c, breathLevel(frame))
}

// breatheBright is the breathe pattern's hardware-dimming request:
// full brightness during software stints, the breath level during
// hardware stints.
func breatheBright(frame int) uint8 {
	if breatheHardware(frame) {
		return breathLevel(frame)
	}
	return 255
}

// SMPTE EG 1-1990 split-field color bars, matching the standard reproduction
// (Wikimedia "SMPTE_Color_Bars.svg", a 672×504 grid). Colors and segment
// widths below are taken straight from that file.
var (
	barGray    = color.RGBA{192, 192, 192, 255} // 75% white
	barYellow  = color.RGBA{192, 192, 0, 255}
	barCyan    = color.RGBA{0, 192, 192, 255}
	barGreen   = color.RGBA{0, 192, 0, 255}
	barMagenta = color.RGBA{192, 0, 192, 255}
	barRed     = color.RGBA{192, 0, 0, 255}
	barBlue    = color.RGBA{0, 0, 192, 255}

	plugeMinusI = color.RGBA{0, 33, 76, 255}  // -I chroma  (#00214c)
	plugePlusQ  = color.RGBA{50, 0, 106, 255} // +Q chroma  (#32006a)
	plugeBlack  = color.RGBA{19, 19, 19, 255} // picture black / PLUGE reference (#131313)
	plugeDark   = color.RGBA{9, 9, 9, 255}    // below-black PLUGE bar (#090909)
	plugeLight  = color.RGBA{29, 29, 29, 255} // above-black PLUGE bar (#1d1d1d)
)

// barColors is the seven top bars, left to right by descending luminance.
var barColors = []color.RGBA{barGray, barYellow, barCyan, barGreen, barMagenta, barRed, barBlue}

// castellation is the reverse-blue strip under the top bars: each colored
// segment is a top bar's blue content, over the picture-black field.
var castellation = []color.RGBA{barBlue, plugeBlack, barMagenta, plugeBlack, barCyan, plugeBlack, barGray}

// smpteBars renders SMPTE split-field color bars on a w×h panel. The row picks
// the zone, so every band is a known color and a wrong channel or a mis-set
// black level is obvious:
//
//   - Top         (0 .. 2/3 h): the seven 75% color bars.
//   - Castellation(2/3 .. 3/4 h): the reverse-blue strip.
//   - PLUGE       (3/4 .. h): -I, 100% white, +Q, then the PLUGE pulse
//     (below-black | black | above-black) for setting the black level.
func smpteBars(x, y, w, h int) color.RGBA {
	bw := w / 7
	if bw < 1 {
		bw = 1
	}
	col := x / bw
	if col > 6 {
		col = 6
	}
	switch {
	case y*3 < h*2: // top: seven 75% bars
		return barColors[col]
	case y*4 < h*3: // castellation: reverse-blue strip
		return castellation[col]
	}
	// PLUGE row: -I, white and +Q each span 5/28 of the width; the pulse sits
	// at 5/7..6/7 as three 1/21-wide below/at/above-black bars on picture black.
	switch {
	case x*28 < 5*w:
		return plugeMinusI
	case x*28 < 10*w:
		return white
	case x*28 < 15*w:
		return plugePlusQ
	case x*21 < 15*w:
		return plugeBlack
	case x*21 < 16*w:
		return plugeDark
	case x*21 < 17*w:
		return plugeBlack
	case x*21 < 18*w:
		return plugeLight
	default:
		return plugeBlack
	}
}

// isqrt is the integer square root (floor) by Newton's method, used by the
// radial showpieces to get a pixel's distance from a center without floats.
func isqrt(n int) int {
	if n <= 0 {
		return 0
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

// atan2u is an integer atan2 returning the angle as 0..255 around the full
// circle (the same hue-style scale wheel takes): 0 along +x, 64 at +y, and so
// on. It uses the octant approximation — one divide plus quadrant fix-ups — so
// the radial showpieces can read a pixel's bearing without floats or a real
// atan. Accuracy is a couple of degrees, plenty for choosing a color or ring.
func atan2u(y, x int) uint8 {
	ax, ay := x, y
	if ax < 0 {
		ax = -ax
	}
	if ay < 0 {
		ay = -ay
	}
	var a int
	if ay <= ax {
		if ax == 0 {
			return 0 // origin: angle undefined, pick 0
		}
		a = ay * 32 / ax // 0..32 over the lower half-octant
	} else {
		a = 64 - ax*32/ay // 32..64 over the upper half-octant
	}
	if x < 0 {
		a = 128 - a // left half-plane
	}
	if y < 0 {
		a = 256 - a // below the axis
	}
	return uint8(a)
}

// firePalette maps a heat value (0..255) through the classic fire ramp:
// black to red as the low channel, red to yellow as green climbs, then
// yellow to white as blue fills in. Walking all three channels in turn
// across the full range makes it a dense workout for the bit-planes.
func firePalette(heat uint8) color.RGBA {
	switch {
	case heat < 85:
		return color.RGBA{heat * 3, 0, 0, 255} // black → red
	case heat < 170:
		return color.RGBA{255, (heat - 85) * 3, 0, 255} // red → yellow
	default:
		return color.RGBA{255, 255, (heat - 170) * 3, 255} // yellow → white
	}
}

// fire is the demoscene flame effect: flickering tongues licking up from the
// bottom edge and cooling to black as they rise. It carries no per-frame heat
// buffer — the heat at each pixel is a closed form of (x, y, frame).
//
// Three sine octaves, finer across x than y and scrolling upward fast, give
// turbulence shaped into vertical tongues. A cooling term that grows with
// height is then subtracted: at the floor every flicker shows, but higher up
// only the strongest turbulence stays above zero, so the flames taper into
// licking tips instead of a flat gradient. The fire palette sweeps all three
// channels, so it doubles as a busy bit-plane and gamma exercise on a moving
// image.
func fire(x, y, h, frame int) color.RGBA {
	if h <= 1 {
		return firePalette(255)
	}
	t1 := int(sine[uint8(x*10+y*3-frame*10)])
	t2 := int(sine[uint8(x*19-y*2-frame*7)])
	t3 := int(sine[uint8(x*7+y*5-frame*13)])
	turb := (t1 + t2 + t3) / 3
	// Cooling rises toward the top, so weak turbulence dies low and only strong
	// turbulence licks high — the flame-tongue shape.
	cool := (h - 1 - y) * 255 / (h - 1)
	heat := min(max(turb*2-cool, 0), 255)
	return firePalette(uint8(heat))
}

// ripple is two wave sources spreading concentric rings that interfere — the
// textbook two-slit pattern, rendered as moving water. Each source's rings come
// from a sine of distance with the phase pulled outward by the frame; summing
// the two gives the interference fringes, which sweep and beat as the sources
// drift. The brightness is one long, smooth sine gradient, so it is a sensitive
// bit-depth probe: too few planes break the fringes into hard contour steps.
func ripple(x, y, w, h, frame int) color.RGBA {
	// Two sources drifting around the panel so the fringes never settle.
	ax := w/2 + (int(sine[uint8(frame*2)])-128)*w/400
	ay := h/2 + (int(sine[uint8(frame*3+64)])-128)*h/400
	bx := w/2 + (int(sine[uint8(frame*2+128)])-128)*w/400
	by := h/2 + (int(sine[uint8(frame*3+192)])-128)*h/400
	r1 := isqrt((x-ax)*(x-ax) + (y-ay)*(y-ay))
	r2 := isqrt((x-bx)*(x-bx) + (y-by)*(y-by))
	v := (int(sine[uint8(r1*8-frame*6)]) + int(sine[uint8(r2*8-frame*6)])) / 2
	return scaleColor(color.RGBA{0, 128, 255, 255}, uint8(v)) // azure water
}

// tunnel is the demoscene "fly into a tunnel" effect. Each pixel's polar
// coordinates index a virtual wall texture: the angle around the center picks a
// hue (rotating with the frame) and a depth coordinate of 1/r gives rings that
// rush past as the frame advances — radius maps to distance, so the rings bunch
// up toward the center exactly as perspective demands. Brightness fades to dark
// at the center (the far end of the tunnel), selling the depth. The continuous
// hue and brightness gradients in motion make it a strong 8-bit showcase.
func tunnel(x, y, w, h, frame int) color.RGBA {
	dx := x - w/2
	dy := y - h/2
	r := isqrt(dx*dx + dy*dy)
	a := int(atan2u(dy, dx))
	depth := 1 << 12 // r==0: the vanishing point, effectively infinitely deep
	if r > 0 {
		depth = 4096 / r
	}
	// Keep the depth scale gentle: large multipliers make the per-pixel phase
	// step jump by most of a ring, so rings snap plateau-to-plateau (shrink and
	// grow) instead of gliding. 4096/r steps ~14/256 per pixel at the rim — one
	// smooth, continuous flow — with ~7 rings bunching toward the center.
	hue := wheel(uint8(a*3 + depth/4 + frame)) // angle, with a gentle depth spiral
	rings := sine[uint8(depth-frame*3)]        // rings rushing past, accelerating at the rim
	maxr := isqrt((w/2)*(w/2) + (h/2)*(h/2))   // panel corner: brightest, the tunnel mouth
	fade := r * 255 / maxr
	return scaleColor(hue, uint8(int(rings)*fade/255))
}

// starTrail is each warp streak's radial motion-blur length, in pixels.
const starTrail = 6

// starfield is the "warp speed" effect: stars streaking outward from a bright
// core at the center, each leaving a short motion-blur trail pointing back
// inward. It is stateless — the screen is carved into angular rays, and each
// ray (hashed off its index) carries a single star whose radius is a closed
// form of the frame, recycling through a dark gap so neighbouring rays stay out
// of step. A star brightens as it flies out toward the rim, so the field reads
// as accelerating depth. Sparse and bright on black, it makes a stuck pixel or
// a motion tear jump out.
func starfield(x, y, w, h, frame int) color.RGBA {
	dx := x - w/2
	dy := y - h/2
	r := isqrt(dx*dx + dy*dy)
	if r == 0 {
		return white // the core the stars stream out of
	}
	ray := uint32(atan2u(dy, dx)) >> 2 // 64 angular rays around the circle
	hsh := rainHash(ray)
	maxr := isqrt((w/2)*(w/2) + (h/2)*(h/2))
	period := maxr + starTrail + 6                      // travel plus a dark gap before recycling
	sr := (frame*(2+int(hsh%4)) + int(hsh>>5)) % period // this ray's star radius
	d := sr - r                                         // distance from the head back along the trail
	if d < 0 || d > starTrail {
		return black // ahead of the star, or past the end of the tail
	}
	base := min(sr*255/maxr, 255)                      // dim near the core, bright out at the rim
	level := uint8(base * (starTrail - d) / starTrail) // fade along the trail
	return color.RGBA{level, level, level, 255}
}

const (
	cometCount = 3  // number of comets on screen
	cometTrail = 14 // tail length, in trailing frames
)

// comet glides a few colored comets along Lissajous orbits, each dragging a
// fading tail. It is stateless: a comet's head position is sine(freq*frame) in
// x and y, and its tail is the very same closed form evaluated at recent frames
// (frame-k), dimming with k — no stored history. Each comet gets a different
// x:y frequency ratio (so the looping paths differ) and its own hue. Where two
// trails cross, the brighter pixel wins. Sparse bright points on black: an easy
// read on dropped frames or a stuck pixel.
func comet(x, y, w, h, frame int) color.RGBA {
	best := black
	bestLevel := 0
	for i := 0; i < cometCount; i++ {
		fx := 2 + i   // 2, 3, 4
		fy := 3 + i*2 // 3, 5, 7 — different Lissajous ratios per comet
		for k := 0; k < cometTrail; k++ {
			s := frame - k
			cx := w/2 + (int(sine[uint8(fx*s+i*50)])-128)*(w-2)/256
			cy := h/2 + (int(sine[uint8(fy*s+i*90+64)])-128)*(h-2)/256
			if x == cx && y == cy {
				level := 255 * (cometTrail - k) / cometTrail
				if level > bestLevel {
					bestLevel = level
					best = scaleColor(wheel(uint8(i*85+s/2)), uint8(level))
				}
			}
		}
	}
	return best
}

// munch is the PDP-1 "munching squares" classic: light the pixels where
// (x XOR y) falls below a moving threshold and the iconic notched, self-similar
// square boundary munches its way across the screen. The threshold rides a
// triangle wave so the lit region grows then recedes — a continuous breath
// rather than a hard snap back — and the surviving pixels are tinted by their
// XOR value, turning the field into a shifting plaid.
func munch(x, y, frame int) color.RGBA {
	t := frame & 127
	if t >= 64 {
		t = 127 - t // triangle wave: 0 → 63 → 0, region grows then recedes
	}
	v := (x ^ y) & 63
	if v <= t {
		return wheel(uint8(v * 4)) // tint by the XOR value
	}
	return black
}

// xorPlaid fills every pixel with the hue of (x XOR y), scrolling the whole
// palette with the frame. The XOR field is self-similar — nested plaid blocks
// that halve and repeat — so it is a striking, cheap full-color texture, and
// because every pixel is lit and changing it also smoke-tests for dead pixels
// and stuck channels.
func xorPlaid(x, y, frame int) color.RGBA {
	return wheel(uint8((x ^ y) + frame))
}

// juliaIters is the escape-iteration cap: more detail in the fractal filaments,
// but the per-pixel cost is the priciest pattern here, so keep it modest.
const juliaIters = 24

// julia renders an animated Julia set in Q9 fixed-point (1<<9 = 1.0), no floats.
// Each pixel maps to a point on the complex plane and iterates z = z² + c until
// it escapes |z| > 2; the iteration count picks a hue (points that never escape
// are the black interior). The constant c orbits a small circle with the frame,
// so the fractal continuously morphs and buds. It is the heaviest pattern in the
// set — up to juliaIters complex multiplies per pixel — and a deep, colorful
// gradient workout for the bit-planes.
func julia(x, y, w, h, frame int) color.RGBA {
	if w < 2 || h < 2 {
		return black // the coordinate mapping divides by w-1 and h-1
	}
	const s = 9    // fixed-point fractional bits
	span := 3 << s // view 3.0 wide, i.e. each axis spans [-1.5, 1.5]
	zx := x*span/(w-1) - span/2
	zy := y*span/(h-1) - span/2
	// c orbits a circle of radius ~0.7 — the band of c that yields the most
	// intricate, connected Julia shapes.
	cx := (int(sine[uint8(frame+64)]) - 128) * 360 / 128
	cy := (int(sine[uint8(frame)]) - 128) * 360 / 128
	i := 0
	for ; i < juliaIters; i++ {
		x2 := (zx * zx) >> s
		y2 := (zy * zy) >> s
		if x2+y2 > 4<<s { // |z| > 2: escaped
			break
		}
		xy := (zx * zy) >> s
		zx = x2 - y2 + cx
		zy = 2*xy + cy
	}
	if i == juliaIters {
		return black // inside the set
	}
	return wheel(uint8(i*256/juliaIters + frame)) // hue by escape time, palette drifting
}

const (
	// mandelIters caps the escape iterations. The Mandelbrot boundary needs
	// more than Julia — especially zoomed in, where the filaments are thin —
	// and this is the costliest pattern in the set.
	mandelIters = 40
	mandelLoop  = 512 // frames per in-and-out zoom cycle (~25 s at the loop cadence)
	mandelOct   = 6   // octaves at the deepest point (2^6 = 64×)
)

// mandelbrot renders the namesake fractal in Q12 fixed-point (1<<12 = 1.0), no
// floats, animated as a slow breathing zoom toward the seahorse valley on the
// set's boundary: it dives smoothly in to 64× then pulls back out, continuously.
// The zoom is geometric (equal magnification per frame, the way a real zoom
// reads), not linear, so the in and out feel even. True infinite zoom is
// impossible here — Q12 has ~12 fractional bits, so 64× stays comfortably above
// the precision floor where pixels would quantize into flat blocks; going much
// deeper would need int64 (slow on the M0) or arbitrary precision.
//
// Each pixel is a point c; z = z² + c iterates from zero until it escapes
// |z| > 2, and the escape count picks a hue (the never-escaping interior is
// black). The deep, finely-banded color makes it a heavy bit-plane workout —
// watch frame rate on slower targets.
func mandelbrot(x, y, w, h, frame int) color.RGBA {
	const s = 12                // fixed-point fractional bits
	const cx0, cy0 = -3051, 463 // ≈ (-0.745, 0.113): the seahorse valley
	// Triangle-wave geometric zoom: span = 3.0 * 2^(-e), with e ramping
	// 0..mandelOct..0 across the cycle so the view dives in then back out. The
	// whole-octave part is an integer shift; the fraction comes from exp2tab, so
	// the zoom stays continuous rather than stepping per octave.
	p := frame % mandelLoop
	if p >= mandelLoop/2 {
		p = mandelLoop - p // 0 → mandelLoop/2 → 0
	}
	e := p * mandelOct * 256 / (mandelLoop / 2)
	span := ((3 << s) >> (e >> 8)) * int(exp2tab[e&255]) >> 12
	cr := cx0 + (x-w/2)*span/w
	ci := cy0 + (y-h/2)*span/w
	zr, zi := 0, 0
	i := 0
	for ; i < mandelIters; i++ {
		zr2 := (zr * zr) >> s
		zi2 := (zi * zi) >> s
		if zr2+zi2 > 4<<s { // |z| > 2: escaped
			break
		}
		zi = 2*((zr*zi)>>s) + ci
		zr = zr2 - zi2 + cr
	}
	if i == mandelIters {
		return black // inside the set
	}
	return wheel(uint8(i*8 + frame)) // hue by escape time, palette drifting
}

// radarTrail is the angular length of the fading sweep tail, of 256 around the
// circle (110 ≈ 155°).
const radarTrail = 110

// radar is a rotating-sweep field — a radar scope crossed with an RGB bias-light
// test sequence. A beam sweeps around the center; each pixel flares as the beam
// crosses its bearing, then fades as the beam moves on, so a bright leading edge
// drags a long tail around the circle. The sweep hue advances with the frame and
// the tail is colored by how long ago it was swept, so the trail smears through
// the spectrum — a full RGB exercise. Faint concentric range rings keep the
// scope grid visible between sweeps.
func radar(x, y, w, h, frame int) color.RGBA {
	dx := x - w/2
	dy := y - h/2
	a := int(atan2u(dy, dx))
	d := (frame*4 - a) & 255 // angular distance behind the sweeping beam (wraps 0..255)
	ring := 0
	if isqrt(dx*dx+dy*dy)%8 == 0 {
		ring = 40 // faint range rings, always present
	}
	if d < radarTrail {
		sweep := 255 * (radarTrail - d) / radarTrail // bright at the beam, fading down the tail
		if sweep >= ring {
			// Tail hue is the hue as it was when swept (the beam moves 4 units/frame,
			// so d angular units back is d/2 of hue ago) — a spectrum smear.
			return scaleColor(wheel(uint8(frame*2-d/2)), uint8(sweep))
		}
	}
	return scaleColor(wheel(uint8(frame*2)), uint8(ring))
}

// buildPatterns returns the full pattern sequence for a w×h panel. The order
// runs from the most diagnostic patterns toward visual references and baselines,
// grouped by the failure mechanism each one targets (see the package doc):
// channel isolation, worst-case switching, row addressing, bit-plane/bit-depth,
// then visual references and solid baselines. Patterns that depend on panel size
// or animation frame capture w, h here.
func buildPatterns(w, h int) []Pattern {
	return []Pattern{
		// Showpieces: animated eye-candy that doubles as wide-coverage color and
		// motion exercise — dense palettes for the bit-planes, moving fields that
		// make tears and stuck pixels obvious.
		{Name: "smpte bars", At: func(x, y, f int) color.RGBA { return smpteBars(x, y, w, h) }},
		{Name: "matrix rain", At: func(x, y, f int) color.RGBA { return matrixRain(x, y, h, f) }},
		{Name: "fire", At: func(x, y, f int) color.RGBA { return fire(x, y, h, f) }},
		{Name: "ripple", At: func(x, y, f int) color.RGBA { return ripple(x, y, w, h, f) }},
		{Name: "tunnel", At: func(x, y, f int) color.RGBA { return tunnel(x, y, w, h, f) }},
		{Name: "starfield", At: func(x, y, f int) color.RGBA { return starfield(x, y, w, h, f) }},
		{Name: "comet", At: func(x, y, f int) color.RGBA { return comet(x, y, w, h, f) }},
		{Name: "munch", At: func(x, y, f int) color.RGBA { return munch(x, y, f) }},
		{Name: "xor plaid", At: func(x, y, f int) color.RGBA { return xorPlaid(x, y, f) }},
		{Name: "julia", At: func(x, y, f int) color.RGBA { return julia(x, y, w, h, f) }},
		{Name: "mandelbrot", At: func(x, y, f int) color.RGBA { return mandelbrot(x, y, w, h, f) }},
		{Name: "radar", At: func(x, y, f int) color.RGBA { return radar(x, y, w, h, f) }},

		// Channel isolation: one data line as the lone variable — the most
		// sensitive per-channel setup-time check (green especially).
		{Name: "red stripes", At: func(x, y, f int) color.RGBA { return verticalStripe(red, x) }},
		{Name: "green stripes", At: func(x, y, f int) color.RGBA { return verticalStripe(green, x) }},
		{Name: "blue stripes", At: func(x, y, f int) color.RGBA { return verticalStripe(blue, x) }},
		{Name: "green loaded", At: func(x, y, f int) color.RGBA { return whiteMagenta(x) }},

		// Worst-case switching: all six RGB lines flipping every clock — the
		// maximum setup-time and simultaneous-switching (ground-bounce) pressure.
		{Name: "white stripes", At: func(x, y, f int) color.RGBA { return verticalStripe(white, x) }},
		{Name: "scroll stripes", At: func(x, y, f int) color.RGBA { return scrollStripe(white, x, f) }},
		{Name: "white checker", At: func(x, y, f int) color.RGBA { return checker(white, x, y) }},
		{Name: "noise", At: func(x, y, f int) color.RGBA { return noise(x, y, f) }},

		// Row addressing: adjacent scan rows differing maximally, stressing
		// address settling and OE blanking.
		{Name: "horizontal stripes", At: func(x, y, f int) color.RGBA { return horizontalStripe(white, y) }},
		{Name: "half split", At: func(x, y, f int) color.RGBA { return halfSplit(y, h) }},

		// Bit-plane / bit-depth: dim values and gradients that exercise the
		// low-order BCM planes and surface gamma banding.
		{Name: "dim green stripes", At: func(x, y, f int) color.RGBA { return dimGreenStripe(x) }},
		{Name: "green gradient", At: func(x, y, f int) color.RGBA { return ramp(green, x, w) }},
		{Name: "rgb ramps", At: func(x, y, f int) color.RGBA { return rgbRamps(x, y, w, h) }},
		{Name: "plasma", At: func(x, y, f int) color.RGBA { return plasma(x, y, f) }},
		{Name: "depth split", At: func(x, y, f int) color.RGBA { return depthSplit(x, y, h, f) }},
		{Name: "breathe", At: func(x, y, f int) color.RGBA { return breathe(x, y, w, h, f) }, Bright: breatheBright},

		// Visual references & smoke tests: full-color fields to confirm channel
		// correctness and catch dead pixels at a glance.
		{Name: "spectrum", At: func(x, y, f int) color.RGBA { return spectrum(x, y, w, h, f) }},
		{Name: "rainbow", At: func(x, y, f int) color.RGBA { return rainbow(x, y, w, h) }},
		{Name: "colorwash", At: func(x, y, f int) color.RGBA { return colorwash(x, y, f) }},

		// Buffer integrity: a bar swept full-frame each redraw — a clean solid
		// line when double-buffered, a torn staircase when not (see tearBar).
		{Name: "tear bar", At: func(x, y, f int) color.RGBA { return tearBar(x, w, f) }},

		// Baseline: a solid field, the static reference.
		{Name: "full white", At: func(x, y, f int) color.RGBA { return white }},
	}
}

// frameInterval paces animated patterns (scroll, noise). Static patterns
// redraw identically each tick, which is harmless.
const frameInterval = 50 * time.Millisecond

// pollInterval is how often the input source is sampled while waiting
// out frameInterval. Sampling only once per frame loses input: rapid
// taps shorter than the frame period merge into what looks like one
// continuous hold (no release is ever sampled between them), so only
// the first registers. A few milliseconds resolves individual taps.
const pollInterval = 5 * time.Millisecond

// step maps a navigation button to a sequencer direction: Up advances to the
// next pattern, Down goes back to the previous one, and None holds.
func step(b input.Button) int {
	switch b {
	case input.Up:
		return +1
	case input.Down:
		return -1
	default:
		return 0
	}
}

// wrapIndex reduces i into [0, n) with wrap-around in both directions, so
// stepping past either end lands on the opposite end.
func wrapIndex(i, n int) int {
	i %= n
	if i < 0 {
		i += n
	}
	return i
}

// sequencer decides which pattern is on screen. Each frame it advances one
// step (+1/-1) toward whichever pattern the user navigates to. There is no
// auto-advance: a pattern stays on screen until the user steps off it.
type sequencer struct {
	n   int
	idx int
}

func newSequencer(n int) *sequencer {
	return &sequencer{n: n}
}

// tick applies a manual navigation step (+1 next, -1 previous, 0 holds). It
// returns the pattern index to display and whether the pattern just changed, so
// the caller can reset the animation frame counter.
func (s *sequencer) tick(step int) (idx int, changed bool) {
	if step == 0 {
		return s.idx, false
	}
	s.idx = wrapIndex(s.idx+step, s.n)
	return s.idx, true
}

// Run draws each pattern across the whole w×h display and holds it until the
// user navigates — there is no auto-advance, so a pattern can be studied as long
// as needed. in (if non-nil) steps with the Up/Down buttons: Up = next pattern,
// Down = previous. Watch for per-pixel color corruption (classically the green
// channel dropping or smearing) — that's a timing margin failing.
func Run(d Display, w, h int, in input.Source) {
	patterns := buildPatterns(w, h)
	seq := newSequencer(len(patterns))
	frame := 0
	bright, hasBright := d.(brightnessSetter)
	for {
		p := patterns[seq.idx]
		d.Clear()
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				d.SetPixel(int16(x), int16(y), p.At(x, y, frame))
			}
		}
		// Apply the pattern's hardware-dimming request, when both the
		// pattern asks and the display can; setting 255 otherwise keeps
		// patterns isolated from each other's brightness.
		if hasBright {
			level := uint8(255)
			if p.Bright != nil {
				level = p.Bright(frame)
			}
			bright.SetBrightness(level)
		}
		d.Display()

		// Wait out the frame interval while sampling input every few
		// milliseconds (see pollInterval), accumulating navigation so a
		// press/tap landing mid-frame isn't lost.
		s := 0
		deadline := time.Now().Add(frameInterval)
		for {
			if in != nil {
				s += step(in.Poll())
			}
			if !time.Now().Before(deadline) {
				break
			}
			time.Sleep(pollInterval)
		}
		if _, changed := seq.tick(s); changed {
			frame = 0
		} else {
			frame++
		}
	}
}

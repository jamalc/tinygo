package stress

import (
	"image/color"
	"math"
	"testing"

	"github.com/jamalc/tinygo/internal/input"
)

// nativeLevel models the hub75 driver's SetPixel pipeline: an 8-bit
// channel is mapped through the gamma-2.6 curve into the configured
// number of bit-planes (see device.init in internal/hub75; this
// float model matches the driver's integer math away from exact .5
// boundaries). Dim values must survive this to light anything — at the
// default 8 planes the curve maps every 8-bit input below 24 to native
// 0, i.e. a blank panel.
func nativeLevel(v8 uint8, planes int) uint8 {
	maxv := float64(uint32(1)<<planes - 1)
	return uint8(math.Round(maxv * math.Pow(float64(v8)/255, 2.6)))
}

func TestVerticalStripeTogglesEveryColumn(t *testing.T) {
	on := color.RGBA{0, 255, 0, 255}
	if got := verticalStripe(on, 0); got != on {
		t.Errorf("column 0: got %v, want %v (on)", got, on)
	}
	if got := verticalStripe(on, 1); got != black {
		t.Errorf("column 1: got %v, want %v (black)", got, black)
	}
	if got := verticalStripe(on, 2); got != on {
		t.Errorf("column 2: got %v, want %v (on)", got, on)
	}
}

func TestCheckerTogglesOnBothAxes(t *testing.T) {
	on := color.RGBA{255, 255, 255, 255}
	cases := []struct {
		x, y int
		want color.RGBA
	}{
		{0, 0, on},
		{1, 0, black},
		{0, 1, black},
		{1, 1, on},
	}
	for _, c := range cases {
		if got := checker(on, c.x, c.y); got != c.want {
			t.Errorf("checker(%d,%d): got %v, want %v", c.x, c.y, got, c.want)
		}
	}
}

func TestHorizontalStripeTogglesEveryRow(t *testing.T) {
	on := color.RGBA{255, 255, 255, 255}
	if got := horizontalStripe(on, 0); got != on {
		t.Errorf("row 0: got %v, want on", got)
	}
	if got := horizontalStripe(on, 1); got != black {
		t.Errorf("row 1: got %v, want black", got)
	}
}

// scrollStripe is verticalStripe with the phase advanced by frame, so a given
// column flips parity from one frame to the next.
func TestScrollStripeShiftsPhaseWithFrame(t *testing.T) {
	on := color.RGBA{255, 255, 255, 255}
	if got := scrollStripe(on, 0, 0); got != on {
		t.Errorf("col 0 frame 0: got %v, want on", got)
	}
	if got := scrollStripe(on, 0, 1); got != black {
		t.Errorf("col 0 frame 1: got %v, want black (phase shifted)", got)
	}
}

// whiteMagenta holds R and B high and toggles only green, so green is the lone
// variable on an otherwise fully loaded bus.
func TestWhiteMagentaTogglesOnlyGreen(t *testing.T) {
	even := whiteMagenta(0)
	odd := whiteMagenta(1)
	if even != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("even column: got %v, want white", even)
	}
	if odd != (color.RGBA{255, 0, 255, 255}) {
		t.Errorf("odd column: got %v, want magenta", odd)
	}
	if even.R != odd.R || even.B != odd.B {
		t.Errorf("R/B must stay loaded across columns: even=%v odd=%v", even, odd)
	}
}

// dimGreenStripe must be dim green only — and, crucially, dim enough to sit in
// the low bit-planes yet bright enough to survive the driver's shift+gamma to a
// nonzero native level. A value that gamma-crushes to native 0 leaves the panel
// blank, which is the bug this guards against.
func TestDimGreenStripeStaysVisibleInLowPlanes(t *testing.T) {
	c := dimGreenStripe(0)
	if c.R != 0 || c.B != 0 {
		t.Errorf("dim green leaked into R/B: %v", c)
	}
	if dimGreenStripe(1) != black {
		t.Errorf("odd column should be black, got %v", dimGreenStripe(1))
	}
	n := nativeLevel(c.G, 8) // cmd runs at the default 8 bit-planes
	if n == 0 {
		t.Fatalf("dim green %d maps to native level 0 at 8 planes: blank panel", c.G)
	}
	if n > 15 {
		t.Errorf("dim green native level %d not in the low planes (want 1..15)", n)
	}
}

func TestHalfSplitTopBright(t *testing.T) {
	h := 4
	for y := 0; y < 2; y++ {
		if halfSplit(y, h) == black {
			t.Errorf("top half row %d should be lit", y)
		}
	}
	for y := 2; y < 4; y++ {
		if halfSplit(y, h) != black {
			t.Errorf("bottom half row %d should be black", y)
		}
	}
}

// noise is a deterministic per-pixel hash whose channels are full-on or
// full-off, so it can be a fuzzer without carrying RNG state.
func TestNoiseIsDeterministicBinaryAndVaries(t *testing.T) {
	if noise(3, 4, 5) != noise(3, 4, 5) {
		t.Error("noise must be deterministic for the same inputs")
	}
	seen := map[color.RGBA]bool{}
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			c := noise(x, y, 0)
			for _, ch := range []uint8{c.R, c.G, c.B} {
				if ch != 0 && ch != 255 {
					t.Fatalf("noise channel not binary at (%d,%d): %v", x, y, c)
				}
			}
			seen[c] = true
		}
	}
	if len(seen) < 2 {
		t.Error("noise produced only one color across 64 pixels")
	}
}

// The sine table is the basis of plasma's phase math: one full period
// across 256 steps, swinging 1..255 around the 128 midpoint.
func TestSineTableShape(t *testing.T) {
	if sine[0] != 128 {
		t.Errorf("sine[0] = %d, want 128 (midpoint)", sine[0])
	}
	if sine[64] != 255 {
		t.Errorf("sine[64] = %d, want 255 (peak)", sine[64])
	}
	if sine[192] != 1 {
		t.Errorf("sine[192] = %d, want 1 (trough)", sine[192])
	}
	for i := 1; i < 64; i++ { // rising quarter
		if sine[i] < sine[i-1] {
			t.Fatalf("sine not monotonic on the rising quarter at %d", i)
		}
	}
}

// plasma must be deterministic (it's a test pattern), animate over
// frames, vary across the panel within a frame, and stay fully
// saturated: wheel always zeroes one channel and scaleColor preserves
// zeros, so every plasma pixel has at least one dark channel.
func TestPlasmaAnimatedSaturatedAndVaries(t *testing.T) {
	if plasma(3, 4, 5) != plasma(3, 4, 5) {
		t.Error("plasma must be deterministic for the same inputs")
	}

	spatial := map[color.RGBA]bool{}
	moved := false
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			c := plasma(x, y, 0)
			if c.R != 0 && c.G != 0 && c.B != 0 {
				t.Fatalf("plasma(%d,%d,0) = %v is not saturated (no channel off)", x, y, c)
			}
			spatial[c] = true
			if c != plasma(x, y, 8) {
				moved = true
			}
		}
	}
	if len(spatial) < 16 {
		t.Errorf("plasma frame 0 produced only %d distinct colors over 256 pixels", len(spatial))
	}
	if !moved {
		t.Error("plasma did not change between frames 0 and 8")
	}
}

// depthSplit must show the identical wave on both halves — except the
// bottom is quantized to 16 input levels while the top keeps all 256.
func TestDepthSplitQuantizesOnlyBottomHalf(t *testing.T) {
	const h = 32
	topLevels := map[uint8]bool{}
	for x := 0; x < 64; x++ {
		for f := 0; f < 64; f += 8 {
			top := depthSplit(x, 0, h, f)
			bot := depthSplit(x, h-1, h, f)
			if top.R != top.G || top.G != top.B {
				t.Fatalf("depthSplit not grayscale at x=%d: %v", x, top)
			}
			if bot.R != top.R&^0x0F {
				t.Fatalf("bottom half at x=%d f=%d: got %d, want top %d quantized to %d",
					x, f, bot.R, top.R, top.R&^0x0F)
			}
			topLevels[top.R] = true
		}
	}
	// The top half must actually exercise fine levels (more than the 8
	// coarse steps the bottom is limited to within the 0..120 range).
	if len(topLevels) <= 8 {
		t.Errorf("top half produced only %d distinct levels; wave too coarse", len(topLevels))
	}
}

// breathe must alternate dimming modes per stint: software stints
// return pixel-scaled values with the hardware request pinned at 255;
// hardware stints return full values with the request following the
// breath. The right-edge marker stays full white in both.
func TestBreatheAlternatesDimmingModes(t *testing.T) {
	const w, h = 64, 32
	softFrame := breathFrames / 4 // mid-breath, software stint
	hardFrame := breathFrames + breathFrames/4

	if breatheHardware(softFrame) || !breatheHardware(hardFrame) {
		t.Fatal("stint phase math broken")
	}

	// Software stint: scene scaled by the breath level, request 255.
	scene := breatheScene(10, 5, w, h)
	if got, want := breathe(10, 5, w, h, softFrame), scaleColor(scene, breathLevel(softFrame)); got != want {
		t.Errorf("software stint pixel = %v, want scaled %v", got, want)
	}
	if breatheBright(softFrame) != 255 {
		t.Errorf("software stint hardware request = %d, want 255", breatheBright(softFrame))
	}

	// Hardware stint: full pixel values, request follows the breath.
	if got := breathe(10, 5, w, h, hardFrame); got != scene {
		t.Errorf("hardware stint pixel = %v, want full %v", got, scene)
	}
	if got, want := breatheBright(hardFrame), breathLevel(hardFrame); got != want {
		t.Errorf("hardware stint request = %d, want breath level %d", got, want)
	}

	// Marker column: full white in both stints.
	for _, f := range []int{softFrame, hardFrame} {
		if breathe(w-1, 0, w, h, f) != white {
			t.Errorf("marker column not full white at frame %d", f)
		}
	}
}

// The breath must actually dip near black — that's where the two
// dimming methods look most different.
func TestBreatheLevelSpansTheRange(t *testing.T) {
	lo, hi := uint8(255), uint8(0)
	for f := 0; f < breathFrames; f++ {
		v := breathLevel(f)
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	if lo > 8 || hi < 250 {
		t.Errorf("breath range [%d, %d], want ~[1, 255]", lo, hi)
	}
}

// Only breathe requests hardware brightness; every other pattern must
// leave Bright nil so the runner restores full brightness.
func TestOnlyBreatheRequestsBrightness(t *testing.T) {
	for _, p := range buildPatterns(64, 32) {
		if (p.Bright != nil) != (p.Name == "breathe") {
			t.Errorf("pattern %q: Bright non-nil = %v", p.Name, p.Bright != nil)
		}
	}
}

// TestSMPTEBars checks the split-field bars against the reference SVG grid
// (Wikimedia "SMPTE_Color_Bars.svg", 672×504): top bars y<336, castellation
// 336..378, PLUGE row y>=378.
func TestSMPTEBars(t *testing.T) {
	const w, h = 672, 504
	cases := []struct {
		name string
		x, y int
		want color.RGBA
	}{
		// Top: seven 75% bars at x = 0,96,...,576.
		{"top gray", 0, 0, color.RGBA{192, 192, 192, 255}},
		{"top yellow", 96, 0, color.RGBA{192, 192, 0, 255}},
		{"top cyan", 192, 0, color.RGBA{0, 192, 192, 255}},
		{"top green", 288, 0, color.RGBA{0, 192, 0, 255}},
		{"top magenta", 384, 0, color.RGBA{192, 0, 192, 255}},
		{"top red", 480, 0, color.RGBA{192, 0, 0, 255}},
		{"top blue", 576, 0, color.RGBA{0, 0, 192, 255}},
		// Castellation (y in 336..378): reverse-blue strip over picture black.
		{"castle blue", 0, 350, color.RGBA{0, 0, 192, 255}},
		{"castle gap", 96, 350, color.RGBA{19, 19, 19, 255}},
		{"castle magenta", 192, 350, color.RGBA{192, 0, 192, 255}},
		{"castle cyan", 384, 350, color.RGBA{0, 192, 192, 255}},
		{"castle gray", 576, 350, color.RGBA{192, 192, 192, 255}},
		// PLUGE row (y>=378): -I, white, +Q, then the below/at/above-black pulse.
		{"pluge -I", 0, 400, color.RGBA{0, 33, 76, 255}},
		{"pluge white", 120, 400, color.RGBA{255, 255, 255, 255}},
		{"pluge +Q", 240, 400, color.RGBA{50, 0, 106, 255}},
		{"pluge field", 400, 400, color.RGBA{19, 19, 19, 255}},
		{"pluge below-black", 480, 400, color.RGBA{9, 9, 9, 255}},
		{"pluge reference", 512, 400, color.RGBA{19, 19, 19, 255}},
		{"pluge above-black", 544, 400, color.RGBA{29, 29, 29, 255}},
		{"pluge tail", 600, 400, color.RGBA{19, 19, 19, 255}},
	}
	for _, c := range cases {
		if got := smpteBars(c.x, c.y, w, h); got != c.want {
			t.Errorf("%s: smpteBars(%d,%d) = %v, want %v", c.name, c.x, c.y, got, c.want)
		}
	}
}

// The green-channel stripe is the prime suspect for the setup-time bug, so it
// must isolate green: only the G component is ever non-zero.
func TestGreenStripePatternIsolatesGreenChannel(t *testing.T) {
	var p Pattern
	for _, pat := range buildPatterns(8, 8) {
		if pat.Name == "green stripes" {
			p = pat
		}
	}
	if p.At == nil {
		t.Fatal("no \"green stripes\" pattern found")
	}
	for x := 0; x < 8; x++ {
		c := p.At(x, 0, 0)
		if c.R != 0 || c.B != 0 {
			t.Errorf("green stripes at x=%d leaked into R/B: %v", x, c)
		}
	}
}

func TestRampSpansFullRange(t *testing.T) {
	if got := ramp(white, 0, 256); got != black {
		t.Errorf("x=0: got %v, want black (level 0)", got)
	}
	if got := ramp(white, 255, 256); got != white {
		t.Errorf("x=max: got %v, want white (full level)", got)
	}
	if got := ramp(white, 128, 256); got.R != 128 {
		t.Errorf("x=128: got R=%d, want 128 (mid level)", got.R)
	}
	if ramp(white, 50, 256).R >= ramp(white, 200, 256).R {
		t.Error("ramp must increase with x")
	}
}

// A colored base ramps only its own channels, so the gradient stays that color.
func TestRampKeepsBaseColor(t *testing.T) {
	for x := 0; x < 256; x++ {
		c := ramp(green, x, 256)
		if c.R != 0 || c.B != 0 {
			t.Fatalf("green ramp leaked into R/B at x=%d: %v", x, c)
		}
	}
	if got := ramp(green, 255, 256); got != green {
		t.Errorf("green ramp at full: got %v, want green", got)
	}
}

// wheelSlop is how far a channel may sit from a vertex of the hue circle.
//
// wheel divides 256 positions into six segments, and 6 doesn't divide 256: a
// segment is 42⅔ positions, so only red (0) and cyan (128) land on a vertex
// exactly. The other four vertices fall between positions and the nearest one
// misses by a few counts — wheel(85) is {1,255,0}, not pure green. Asserting
// equality there would be asserting something the arithmetic can't deliver.
const wheelSlop = 5

// near reports whether c is within wheelSlop of want on every channel.
func near(c, want color.RGBA) bool {
	d := func(a, b uint8) int {
		if a > b {
			return int(a) - int(b)
		}
		return int(b) - int(a)
	}
	return d(c.R, want.R) <= wheelSlop && d(c.G, want.G) <= wheelSlop && d(c.B, want.B) <= wheelSlop
}

// wheel walks the full hue circle with integer math: red at 0, green at ~85,
// blue at ~170, the secondaries between them, and exactly one channel off at
// every step (full saturation).
func TestWheelHitsPrimariesAndStaysSaturated(t *testing.T) {
	// The two vertices that divide 256 evenly are exact.
	if wheel(0) != red {
		t.Errorf("wheel(0) = %v, want exactly red", wheel(0))
	}
	if wheel(128) != cyan {
		t.Errorf("wheel(128) = %v, want exactly cyan", wheel(128))
	}

	// The rest land within wheelSlop of their vertex.
	for _, tc := range []struct {
		pos  uint8
		want color.RGBA
		name string
	}{
		{43, yellow, "yellow"},
		{85, green, "green"},
		{170, blue, "blue"},
		{213, magenta, "magenta"},
	} {
		if got := wheel(tc.pos); !near(got, tc.want) {
			t.Errorf("wheel(%d) = %v, want within %d of %s %v", tc.pos, got, wheelSlop, tc.name, tc.want)
		}
	}

	for pos := 0; pos < 256; pos++ {
		c := wheel(uint8(pos))
		if c.R != 0 && c.G != 0 && c.B != 0 {
			t.Fatalf("wheel(%d) = %v is not fully saturated (no channel off)", pos, c)
		}
	}
}

// rgbRamps stacks a red, green, and blue ramp as three horizontal bands so each
// channel's full intensity range can be compared side by side.
func TestRgbRampsStacksPerChannelBands(t *testing.T) {
	w, h := 256, 3
	if got := rgbRamps(255, 0, w, h); got != red {
		t.Errorf("top band full: got %v, want red", got)
	}
	if got := rgbRamps(255, 1, w, h); got != green {
		t.Errorf("middle band full: got %v, want green", got)
	}
	if got := rgbRamps(255, 2, w, h); got != blue {
		t.Errorf("bottom band full: got %v, want blue", got)
	}
	if got := rgbRamps(0, 0, w, h); got != black {
		t.Errorf("left edge: got %v, want black (level 0)", got)
	}
}

// rainbow is a 2D field: hue across x, brightness down y (bright at top, off at
// the bottom edge).
func TestRainbowIsHueByBrightness(t *testing.T) {
	w, h := 256, 32
	if got := rainbow(0, 0, w, h); got != red {
		t.Errorf("top-left (hue 0, full bright): got %v, want red", got)
	}
	if got := rainbow(10, h-1, w, h); got != black {
		t.Errorf("bottom edge should fade to black: got %v", got)
	}
	if rainbow(0, 0, w, h) == rainbow(w/2, 0, w, h) {
		t.Error("hue must change across x")
	}
}

func TestWrapIndex(t *testing.T) {
	cases := []struct{ i, n, want int }{
		{0, 3, 0},
		{1, 3, 1},
		{3, 3, 0},  // off the top wraps to start
		{-1, 3, 2}, // off the bottom wraps to end
		{-3, 3, 0},
		{4, 3, 1},
	}
	for _, c := range cases {
		if got := wrapIndex(c.i, c.n); got != c.want {
			t.Errorf("wrapIndex(%d, %d) = %d, want %d", c.i, c.n, got, c.want)
		}
	}
}

// Without a navigation step the pattern holds forever — there is no
// auto-advance.
func TestSequencerHoldsWithoutInput(t *testing.T) {
	s := newSequencer(3)
	for i := 0; i < 100; i++ {
		if idx, changed := s.tick(0); idx != 0 || changed {
			t.Fatalf("tick %d: idx=%d changed=%v, want 0,false (no auto-advance)", i, idx, changed)
		}
	}
}

func TestSequencerManualStep(t *testing.T) {
	s := newSequencer(3)

	if idx, changed := s.tick(+1); idx != 1 || !changed {
		t.Errorf("next: idx=%d changed=%v, want 1,true", idx, changed)
	}
	if idx, changed := s.tick(-1); idx != 0 || !changed {
		t.Errorf("prev: idx=%d changed=%v, want 0,true", idx, changed)
	}
	if idx, changed := s.tick(-1); idx != 2 || !changed {
		t.Errorf("prev wraps: idx=%d changed=%v, want 2,true", idx, changed)
	}
}

func TestStepMapsButtonToDirection(t *testing.T) {
	tests := []struct {
		name string
		b    input.Button
		want int
	}{
		{"up advances", input.Up, +1},
		{"down goes back", input.Down, -1},
		{"none holds", input.None, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := step(tt.b); got != tt.want {
				t.Errorf("step(%v) = %d, want %d", tt.b, got, tt.want)
			}
		})
	}
}

// These tests cover the package's pure policy — the parts of internal/hub75 with no machine
// dependency: Config.Validate, scaleGamma, resolveBrightness, and the shared grid (exercised
// through the host Device that New returns off-target). They build with NO build tag, so
// `go test ./internal/hub75` runs them against the host backend (hub75_host.go) on a dev
// machine. The per-chip encoders and the register/DMA scanout are deliberately NOT tested
// here: they are tied to wiring and silicon and are verified live on the panel (plus
// cmd/stress / cmd/bench). Keeping the host tests to pure policy is what avoids an exploding
// set of build tags.
package hub75

import (
	"image/color"
	"math"
	"testing"
)

func TestValidate(t *testing.T) {
	base := Config{Width: 64, Height: 32, ColorDepth: 8}
	tests := []struct {
		name string
		mut  func(*Config)
		want error
	}{
		{"valid", func(*Config) {}, nil},
		{"valid min depth", func(c *Config) { c.ColorDepth = 1 }, nil},
		{"valid max depth", func(c *Config) { c.ColorDepth = 8 }, nil},
		{"zero width", func(c *Config) { c.Width = 0 }, ErrBadGeometry},
		{"negative width", func(c *Config) { c.Width = -1 }, ErrBadGeometry},
		{"zero height", func(c *Config) { c.Height = 0 }, ErrBadGeometry},
		{"odd height", func(c *Config) { c.Height = 31 }, ErrBadGeometry},
		{"zero depth", func(c *Config) { c.ColorDepth = 0 }, ErrBadGeometry},
		{"depth too deep", func(c *Config) { c.ColorDepth = 9 }, ErrBadGeometry},
		{"too many scan rows", func(c *Config) { c.Height = 66 }, ErrTooManyRows}, // 33 > maxScanRows
		{"max scan rows ok", func(c *Config) { c.Height = 64 }, nil},              // 32 == maxScanRows
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mut(&c)
			if got := c.Validate(); got != tt.want {
				t.Errorf("Validate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScaleGamma(t *testing.T) {
	// 8-bit: input 255 maps to full scale (255), input 0 to 0, and the curve never decreases.
	g8 := scaleGamma(8)
	if g8[0] != 0 {
		t.Errorf("scaleGamma(8)[0] = %d, want 0", g8[0])
	}
	if g8[255] != 255 {
		t.Errorf("scaleGamma(8)[255] = %d, want 255", g8[255])
	}
	for i := 1; i < 256; i++ {
		if g8[i] < g8[i-1] {
			t.Fatalf("scaleGamma(8) not monotonic at %d: %d < %d", i, g8[i], g8[i-1])
		}
	}

	// 1-bit: full scale is 1, so every entry is 0 or 1 and the top maps to 1.
	g1 := scaleGamma(1)
	if g1[255] != 1 {
		t.Errorf("scaleGamma(1)[255] = %d, want 1", g1[255])
	}
	for i, v := range g1 {
		if v > 1 {
			t.Fatalf("scaleGamma(1)[%d] = %d, want 0 or 1", i, v)
		}
	}
}

func TestResolveBrightness(t *testing.T) {
	// Zero means "unset" and defaults to full; any non-zero value is kept verbatim.
	for _, tt := range []struct{ in, want uint8 }{{0, 255}, {1, 1}, {128, 128}, {255, 255}} {
		if got := resolveBrightness(tt.in); got != tt.want {
			t.Errorf("resolveBrightness(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Width: 0, Height: 32, ColorDepth: 8}); err != ErrBadGeometry {
		t.Errorf("New(bad geometry) err = %v, want %v", err, ErrBadGeometry)
	}
}

func TestGridReadWrite(t *testing.T) {
	d, err := New(Config{Width: 64, Height: 32, ColorDepth: 8})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if w, h := d.Size(); w != 64 || h != 32 {
		t.Errorf("Size() = %d,%d, want 64,32", w, h)
	}

	want := color.RGBA{R: 10, G: 20, B: 30, A: 255}
	d.SetPixel(5, 7, want)
	if got := d.GetPixel(5, 7); got != want {
		t.Errorf("GetPixel(5,7) = %v, want %v", got, want)
	}

	// Clear turns it back off.
	d.Clear()
	if got := d.GetPixel(5, 7); got != (color.RGBA{A: 255}) {
		t.Errorf("after Clear, GetPixel(5,7) = %v, want black", got)
	}
}

func TestGridOutOfBounds(t *testing.T) {
	d, err := New(Config{Width: 64, Height: 32, ColorDepth: 8})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Out-of-bounds writes are ignored (must not panic); out-of-bounds reads are black.
	for _, p := range [][2]int16{{-1, 0}, {0, -1}, {64, 0}, {0, 32}, {1000, 1000}} {
		d.SetPixel(p[0], p[1], color.RGBA{R: 255, A: 255})
		if got := d.GetPixel(p[0], p[1]); got != (color.RGBA{A: 255}) {
			t.Errorf("GetPixel(%d,%d) = %v, want black", p[0], p[1], got)
		}
	}
}

// holdScale picks the lit hold that makes the panel's light follow the brightness setting
// perceptually: full is untouched, and each setting gives (b/255)^2.2 of full light once the
// fixed blanked time is counted in.
func TestHoldScale(t *testing.T) {
	const ratio = 1
	if got := holdScale(255, ratio); got != 1<<16 {
		t.Fatalf("holdScale(255) = %d, want %d (full is the plain hold)", got, 1<<16)
	}
	// light is the lit duty cycle relative to full's: a hold scaled by x is lit x·r of every
	// x·r+1 units, where full is lit r of r+1.
	light := func(b uint8) float64 {
		x := float64(holdScale(b, ratio)) / (1 << 16)
		return (x * ratio / (x*ratio + 1)) / (float64(ratio) / (ratio + 1))
	}
	for _, b := range []uint8{1, 30, 64, 120, 128, 200, 254} {
		want := math.Pow(float64(b)/255, brightnessGamma)
		if got := light(b); math.Abs(got-want) > 0.001 {
			t.Errorf("brightness %d: light %.4f of full, want %.4f", b, got, want)
		}
	}
	// Half the setting is about a fifth of the light, which looks about half as bright; the
	// old linear hold scale left it at two thirds, which barely looks dimmer.
	if got := light(128); got < 0.2 || got > 0.23 {
		t.Errorf("brightness 128: light %.3f of full, want ~0.22", got)
	}
	prev := uint32(0)
	for b := 1; b <= 255; b++ {
		s := holdScale(uint8(b), ratio)
		if s < prev {
			t.Fatalf("holdScale(%d) = %d < holdScale(%d) = %d: not monotonic", b, s, b-1, prev)
		}
		prev = s
	}
	if got := holdScale(0, ratio); got != 0 {
		t.Errorf("holdScale(0) = %d, want 0 (off)", got)
	}
}

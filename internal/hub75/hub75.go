// Package hub75 drives HUB75 RGB LED matrix panels, with one scanout engine per chip selected
// at build time. This file is the shared floor and the public surface: the one concrete Device
// (the framebuffer grid + the build-selected engine), the Panel contract applications draw
// against, New/Display/SetBrightness, the flat Config and its Validate, the validation errors,
// and the gamma/brightness helpers — all pure policy with no machine dependency, so it always
// compiles. Each per-chip file provides just the engine: the silicon-specific buffers, encode,
// and present/refresh protocol (see the engine doc below).
//
// Engine selection is a build-time decision keyed on the target's chip tag. Exactly one engine
// `type engine` compiles per build:
//
//	hub75_samd51_evsys.go  //go:build atsamd51 && !atsamd51_isr  evsys (M4 default): free-running
//	                                                             byte chain, OE on an EVSYS-linked
//	                                                             TCC timer (zero ISR)
//	hub75_samd51_isr.go    //go:build atsamd51 && atsamd51_isr   isr (M4, -tags=atsamd51_isr):
//	                                                             DMA stream + a per-window ISR
//	                                                             conductor on TC0/TC1
//	hub75_rp2350.go        //go:build rp2350                     pio:  PIO state machine + DMA word stream
//	hub75_esp32s3.go       //go:build esp32s3                    lcd:  LCD_CAM i8080 + GDMA descriptor holds
//	hub75_host.go          //go:build (none of the above)        a no-hardware stub for host build/tests
//
// Every engine is offloaded: the refresh runs on dedicated silicon (DMA, PIO, LCD_CAM), not on
// the CPU. The M4 (atsamd51) is the one chip with two of them; its shared pin map + DMAC
// descriptor substrate lives in hub75_samd51.go, compiled with either DMA engine. The engines
// are deliberately distinct implementations of one axis — how much of the refresh schedule is
// materialized into memory versus computed live by dedicated silicon. Measured results live in
// this package's README.md.
package hub75

import (
	"errors"
	"image/color"
	"math"
	"runtime"
)

// Validation errors shared across drivers. Each names the invariant it guards; drivers return
// these from Validate and New so callers see one set regardless of which driver is built. A
// driver may keep its own error for a failure truly unique to its hardware.
var (
	ErrBadGeometry = errors.New("hub75: invalid geometry (width/height positive, height even, ColorDepth in range)")
	ErrTooManyRows = errors.New("hub75: too many scan rows for the chip's address lines")
	ErrTooBig      = errors.New("hub75: buffers exceed the RAM budget; reduce ColorDepth or Width")
	ErrConfigured  = errors.New("hub75: a device is already configured")
	ErrTimeout     = errors.New("hub75: timed out waiting on hardware")
)

// display is the drawing contract a *Device satisfies: the tinygo.org/x/drivers Displayer
// (Size / SetPixel / Display) plus Clear and SetBrightness. It is unexported — apps draw
// against their own interface that *Device satisfies structurally; this exists only as a
// compile-time guard that Device keeps implementing the full contract.
type display interface {
	Size() (x, y int16)
	SetPixel(x, y int16, c color.RGBA)
	GetPixel(x, y int16) color.RGBA
	Display() error
	Clear()
	SetBrightness(b uint8)
}

var _ display = (*Device)(nil)

// Device is the one concrete HUB75 driver type: the framebuffer (dimensions + the raw-RGB
// pixel store) plus the build-selected engine — the silicon-specific scanout for this target.
// The public surface (the Display contract) is written once here. Device owns ONLY what the
// draw API needs (width/height for Size and bounds, pix for the pixels); every parameter the
// engine needs — scan rows, bit-planes, the gamma curve, brightness, timing knobs — is the
// engine's own, derived from Config in engine.start. The one thing that crosses the boundary
// is pix, handed to the engine per frame (the engine reads it but never holds a reference back
// into Device).
type Device struct {
	width, height int
	pix           []rgb // framebuffer the engine encodes each present; in panel wire order (see bgr)
	bgr           bool  // BGR panel: SetPixel stores R/B swapped so every engine emits the right line
	eng           engine
}

// engine is the hardware backend, defined by whichever per-chip file the build tag selects
// (hub75_samd51.go / hub75_rp2350.go / hub75_esp32s3.go, or hub75_host.go off-target). It is
// a concrete type — exactly one compiles per build, so no interface or generics are needed —
// and it owns its configuration (geometry, gamma, brightness, timing knobs) plus the buffers,
// the encode, and the present/refresh protocol. Its contract:
//
//	start(cfg Config, pix []rgb) error
//	    Derive the engine's configuration from cfg, size and allocate its buffers (layering any
//	    engine-specific limit checks like ErrTooBig on top of cfg.Validate), encode the initial
//	    (black) pix, and start the hardware refreshing. Returns ErrConfigured if the singleton
//	    engine is already claimed.
//	present(pix []rgb) error
//	    Encode pix into the engine's buffer(s) and make it live (the per-driver double- or
//	    single-buffer present). Called from Display.
//	setBrightness(b uint8)
//	    Adopt the new brightness (rebuild a timing schedule, mark a buffer dirty, or just store
//	    it for the next encode). Called from SetBrightness.
//
// A per-chip file may also hang board-specific public methods on *Device; they compile only
// in that build, where the engine has the backing method.

// New validates cfg, allocates the framebuffer, and starts the build-selected engine (handing
// it cfg and the initial pixels). After it returns the panel is refreshing and the Device is
// ready to draw on.
func New(cfg Config) (*Device, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	d := &Device{
		width:  cfg.Width,
		height: cfg.Height,
		bgr:    cfg.BGR,
		pix:    make([]rgb, cfg.Width*cfg.Height),
	}
	if err := d.eng.start(cfg, d.pix); err != nil {
		return nil, err
	}
	return d, nil
}

// Size reports the panel dimensions.
func (d *Device) Size() (int16, int16) { return int16(d.width), int16(d.height) }

// SetPixel stores a pixel in the framebuffer; the engine applies gamma when it encodes. Visible
// on the next Display. Out-of-bounds coordinates are ignored. On a BGR panel R and B are swapped
// here — once, in the shared store — so every engine emits the color channels on the lines the
// panel actually wires them to, without any engine needing to know the panel's channel order.
// (The simulator uses its own Display type, so it is unaffected and shows true RGB.)
func (d *Device) SetPixel(x, y int16, c color.RGBA) {
	if x < 0 || int(x) >= d.width || y < 0 || int(y) >= d.height {
		return
	}
	p := rgb{c.R, c.G, c.B}
	if d.bgr {
		p.R, p.B = p.B, p.R
	}
	d.pix[int(y)*d.width+int(x)] = p
}

// GetPixel reads back the raw (un-gamma-corrected) RGB stored for a pixel — the value last
// written by SetPixel, not what the panel emits after gamma. The framebuffer is the source of
// truth, so this is a cheap store read; out-of-bounds coordinates read as black. It lets an
// app treat the framebuffer as state (decay trails, blends, cellular rules) without keeping a
// shadow buffer.
func (d *Device) GetPixel(x, y int16) color.RGBA {
	if x < 0 || int(x) >= d.width || y < 0 || int(y) >= d.height {
		return color.RGBA{A: 255}
	}
	p := d.pix[int(y)*d.width+int(x)]
	if d.bgr {
		p.R, p.B = p.B, p.R // undo the wire-order swap so callers still see true RGB
	}
	return color.RGBA{R: p.R, G: p.G, B: p.B, A: 255}
}

// Clear turns every pixel off, visible on the next Display.
func (d *Device) Clear() {
	for i := range d.pix {
		d.pix[i] = rgb{}
	}
}

// Display hands the current pixels to the engine to encode and present (see engine.present).
func (d *Device) Display() error { return d.eng.present(d.pix) }

// SetBrightness sets global brightness, 0 (off) to 255 (full); the engine adopts it.
func (d *Device) SetBrightness(b uint8) { d.eng.setBrightness(b) }

// DriverName reports the build-selected engine's short name ("evsys", "isr", "pio", "lcd", or
// "host"). It is the engine that actually compiled, so it can't disagree with what's running —
// useful for logging/benchmarks. The name is declared alongside each engine in its per-chip
// file.
func DriverName() string { return driverName }

// Config describes the panel and the look — not the silicon. It is deliberately small: only
// what a caller genuinely chooses. Pixel clock, plane on-times, and other refresh timing are
// NOT here; each engine owns its own timing (a safe default plus a clearly-marked "Tuning"
// const block in its per-chip file), the same way Adafruit Protomatter and Pimoroni's
// Interstate75 keep timing internal. To retune an engine, edit those consts and re-flash.
type Config struct {
	Width        int   // total chain width in pixels
	Height       int   // one panel's pixel height (even; scan = Height/2)
	ColorDepth   int   // bit-planes per channel (1..8)
	Brightness   uint8 // initial global brightness 0..255; the zero value means full (see resolveBrightness)
	DoubleBuffer bool  // double-buffer so Display swaps tear-free at a frame boundary
	BGR          bool  // panel is BGR-wired: swap R and B channels. Default false (RGB). Per-panel, not per-board.
}

const (
	maxPlanes = 8 // ColorDepth ceiling — bitplane storage is [8]uint8 across the drivers
	// maxScanRows is the universal scan-row ceiling Validate enforces: the HUB75 address lines
	// (A-E) can select at most 32 row pairs, i.e. 64-tall panels. The drivers whose buffers are
	// dynamic (esp32s3, pio, lcd) accept the whole range and fail with ErrTooBig only if the RAM
	// budget can't hold it. The M4 evsys engine, whose descriptor pool is static, layers a tighter
	// 16-row limit on top (samdMaxScanRows) — see hub75_samd51.go.
	maxScanRows = 32
)

// Validate checks the geometry invariants every hub75 driver shares: a positive width, a
// positive even height of at most maxScanRows scan rows (64-tall panels), and a color depth in
// 1..8. This is the whole of the universal contract. Each driver layers its engine-specific
// limits on top in start — the descriptor pools and RAM budgets that differ per engine (returning
// ErrTooBig / ErrTooManyRows).
func (c Config) Validate() error {
	if !(c.Width > 0 && c.Height > 0 && c.Height%2 == 0 && c.ColorDepth >= 1 && c.ColorDepth <= maxPlanes) {
		return ErrBadGeometry
	}
	if c.Height/2 > maxScanRows {
		return ErrTooManyRows
	}
	return nil
}

// DefaultConfig returns the default configuration every engine accepts: a single 64x32 panel,
// 8-bit color, full brightness, double-buffered. Timing is internal to each engine (see the
// per-chip file's TUNING block), so the default is the same for all of them and lives here.
//
// Note this is a 64×32 default: apps overlay their own geometry (see config.HUB75), and
// ColorDepth 8 + DoubleBuffer does NOT fit every engine at every size — 128×64 exceeds pio's
// budget and is tight on lcd. Engines report that as ErrTooBig from New.
func DefaultConfig() Config {
	return Config{Width: 64, Height: 32, ColorDepth: 8, Brightness: 255, DoubleBuffer: true}
}

// rgb is one raw (un-gamma-corrected) pixel held in a Device's framebuffer. The engine applies
// gamma when it encodes a frame. It is a type ALIAS for the anonymous struct rather than a
// defined type, so a framebuffer can be handed to an engine declaring the same shape without a
// copy or an unsafe conversion.
type rgb = struct{ R, G, B uint8 }

// scaleGamma returns the gamma-2.6 curve scaled to a driver's native bit depth: 8-bit input
// mapped to (2^planes - 1) full scale, rounding to nearest. Call once at New; nothing
// per-pixel.
func scaleGamma(planes int) [256]uint8 {
	var t [256]uint8
	maxv := uint32(1)<<planes - 1
	for i := 0; i < 256; i++ {
		t[i] = uint8((uint32(gamma26[i])*maxv + 127) / 255)
	}
	return t
}

// gamma26 is the 8-bit gamma-2.6 reference curve: round(255·(i/255)^2.6). LED output is
// linear in duty while human brightness perception is compressive; 2.6 is in the standard
// LED-matrix range (2.2-2.8). Precomputed so drivers stay integer-only. Regenerate with:
//
//	for i := 0; i < 256; i++ {
//		fmt.Printf("%d, ", int(math.Round(255*math.Pow(float64(i)/255, 2.6))))
//	}
var gamma26 = [256]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3,
	3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 5, 6, 6, 6, 6, 7,
	7, 7, 8, 8, 8, 9, 9, 9, 10, 10, 10, 11, 11, 11, 12, 12,
	13, 13, 13, 14, 14, 15, 15, 16, 16, 17, 17, 18, 18, 19, 19, 20,
	20, 21, 21, 22, 22, 23, 24, 24, 25, 25, 26, 27, 27, 28, 29, 29,
	30, 31, 31, 32, 33, 34, 34, 35, 36, 37, 38, 38, 39, 40, 41, 42,
	42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56, 57,
	58, 59, 60, 61, 62, 63, 64, 65, 66, 68, 69, 70, 71, 72, 73, 75,
	76, 77, 78, 80, 81, 82, 84, 85, 86, 88, 89, 90, 92, 93, 94, 96,
	97, 99, 100, 102, 103, 105, 106, 108, 109, 111, 112, 114, 115, 117, 119, 120,
	122, 124, 125, 127, 129, 130, 132, 134, 136, 137, 139, 141, 143, 145, 146, 148,
	150, 152, 154, 156, 158, 160, 162, 164, 166, 168, 170, 172, 174, 176, 178, 180,
	182, 184, 186, 188, 191, 193, 195, 197, 199, 202, 204, 206, 209, 211, 213, 215,
	218, 220, 223, 225, 227, 230, 232, 235, 237, 240, 242, 245, 247, 250, 252, 255,
}

// resolveBrightness treats a zero Config.Brightness as unset and defaults it to full (255),
// so a Config literal that omits Brightness still lights the panel. Start a panel dark with
// an explicit SetBrightness(0) after New.
func resolveBrightness(b uint8) uint8 {
	if b == 0 {
		return 255
	}
	return b
}

// brightnessGamma is the exponent between a brightness setting and the light it asks for: eyes
// judge brightness roughly as light^(1/2.2), so this makes the setting track how bright the
// panel looks. Half the setting looks about half as bright, which a linear scale doesn't: there,
// cutting the light by a third barely registers.
const brightnessGamma = 2.2

// holdScale is the fraction of each plane's full-brightness lit hold to show at brightness b, in
// 1/65536ths, for an engine whose lit holds run ratio× its blanked shift+latch time at full
// brightness (the pio engine's holdDutyRatio).
//
// Such an engine dims by shortening only its lit holds; the blanked shift time stays. So its
// light, the lit duty cycle, doesn't fall in proportion to the hold: at ratio 1, halving every
// hold drops the light to 2/3, not 1/2. holdScale undoes that, picking the hold that gives the
// light b asks for, (b/255)^brightnessGamma of full. Full duty is r/(r+1); a hold scaled by x
// gives xr/(xr+1); setting that to t·r/(r+1) and solving gives x = t / (r+1 - r·t).
func holdScale(b uint8, ratio int) uint32 {
	if b == 255 {
		return 1 << 16
	}
	t := math.Pow(float64(b)/255, brightnessGamma)
	r := float64(ratio)
	return uint32(t/(r+1-r*t)*(1<<16) + 0.5)
}

// waitSwap spins until done reports the engine has adopted the requested buffer, yielding
// between polls, and returns ErrTimeout if the swap never lands within maxSpins. The
// double-buffered engines call it from present, then flip their front/back roles on success.
func waitSwap(done func() bool) error {
	for i := 0; ; i++ {
		if done() {
			return nil
		}
		if i >= maxSpins {
			return ErrTimeout
		}
		runtime.Gosched()
	}
}

// maxSpins bounds a Device's wait for the double-buffer frame-boundary swap, so a stalled
// engine can't hang the caller.
const maxSpins = 4_000_000

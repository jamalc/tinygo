//go:build atsamd51 && atsamd51_isr

// SAMD51 backend (Adafruit Matrix Portal M4) — the isr engine: DMA-streamed pixel data paced
// by a per-window interrupt "conductor" (the ISR this engine is named for). TC0 paces the byte
// beats DMA writes to PORTB; TC1 overflow ends each display window and fires windowTick, which
// latches the shifted row, advances the address, and sets the next window's OE on-time. Costs a
// short per-row ISR (~2.5 µs) and otherwise no steady-state CPU. (Contrast the default evsys
// engine, which takes the CPU out of the loop entirely via the Event System.)
//
// Opt-in alternative to evsys: build with -tags=atsamd51_isr (the makefile's DRIVER=isr). The
// shared pin map + DMAC descriptor layout live in hub75_samd51.go.
//
// The six color pins, CLK, and ADDR_A share PORTB's low byte, so one DMA byte beat drives them
// all; ADDR_A's value is baked into the bitstream (see encode). ADDR_B-E and LAT/OE are the
// high byte, driven by the conductor.
package hub75

import (
	"unsafe"

	"device/sam"
	"runtime/interrupt"
	"runtime/volatile"
)

// Bit positions inside the DMA-written PORTB low byte.
const (
	clkBit   = 6 // PB06
	addrABit = 7 // PB07: address line A, baked into every bitstream byte because it shares the
	//              DMA-written byte. While step (r, p) shifts, the panel displays row r-1 if
	//              p == 0, else row r, and bit 7 of every byte must carry that displayed row's A.
)

// PORTB high-byte masks the conductor drives directly (OE, LAT, ADDR_B-E).
const (
	oeMask    uint32 = 1 << 12
	latMask   uint32 = 1 << 14
	addrBMask uint32 = 1 << 8
	addrCMask uint32 = 1 << 9
	addrDMask uint32 = 1 << 15
	addrEMask uint32 = 1 << 13
)

// driverName is this build's engine name, exposed via hub75.DriverName.
const driverName = "isr"

// engine is isr's byte-stream backend: DMA-streamed pixels with a per-window ISR conductor. It
// owns its config and the two wire bitstreams; Device hands it the pixels to encode each
// present. Only one may run per program (one DMA channel, TC0/TC1, the HUB75 pins).
type engine struct {
	// Configuration, derived from Config in start (the engine's own copy of what it needs to
	// encode — Device does not hold these).
	width, rowPairs, planes int
	bytesPerRow             int
	gamma                   [256]uint8 // gamma-2.6 curve scaled to planes bits
	beatPeriod, baseHold    int

	// Two wire bitstreams (double-buffered). The DMA reads bufs[front]; present encodes
	// bufs[back] and the conductor adopts it at the next frame boundary, flipping the roles.
	bufs         [2][]uint8
	front, back  int
	doubleBuffer bool
}

// start derives the engine config from cfg, sizes and allocates the bitstream(s), encodes the
// initial (black) pix, and starts the backend (DMA channel 0 + TC0/TC1 + the conductor). After
// it returns the panel refreshes. Returns ErrConfigured if the singleton engine is already
// claimed.
func (e *engine) start(cfg Config, pix []rgb) error {
	if cfg.Height/2 > samdMaxScanRows {
		return ErrTooManyRows // keep both M4 engines to one geometry envelope (see hub75_samd51.go)
	}
	e.width = cfg.Width
	e.rowPairs = cfg.Height / 2
	e.planes = cfg.ColorDepth
	e.bytesPerRow = bytesPerRow(cfg.Width)
	e.gamma = scaleGamma(cfg.ColorDepth)
	e.beatPeriod = beatPeriodTicks // tuning const (see TUNING below)
	e.baseHold = baseHoldTicks     // tuning const (see TUNING below)
	e.doubleBuffer = cfg.DoubleBuffer

	streamLen := e.rowPairs * e.planes * e.bytesPerRow
	e.bufs[0] = make([]uint8, streamLen)
	e.encode(pix, e.bufs[0]) // black, with valid control bits
	if e.doubleBuffer {
		e.bufs[1] = make([]uint8, streamLen)
		e.encode(pix, e.bufs[1])
		e.front, e.back = 0, 1
	}
	if err := e.startHardware(); err != nil {
		return err
	}
	e.setBrightness(resolveBrightness(cfg.Brightness))
	return nil
}

// present rebuilds the back bitstream from pix (gamma applied here) and presents it tear-free:
// the conductor switches to it at the next frame boundary and present waits (bounded) for the
// swap, flipping the roles. Single-buffer rebuilds the live stream in place and returns at once.
func (e *engine) present(pix []rgb) error {
	e.encode(pix, e.bufs[e.back])
	if !e.doubleBuffer {
		return nil
	}
	e.swap(e.back)
	if err := waitSwap(e.swapDone); err != nil {
		return err
	}
	e.front, e.back = e.back, e.front
	return nil
}

// setBrightness sets global panel brightness, 0 (off) to 255 (full), via the conductor's
// duty-cycle cutoff (CC1). BCM color ratios are preserved and the image data is untouched.
func (e *engine) setBrightness(b uint8) { brightnessReg.Set(uint32(b)) }

// swap asks the conductor to adopt buffer `to` at the next frame boundary.
func (e *engine) swap(to int) { pendingStreamAddr.Set(ptr(unsafe.Pointer(&e.bufs[to][0]))) }

// swapDone reports whether the conductor has adopted the requested buffer (it clears the
// pending address at the frame boundary).
func (e *engine) swapDone() bool { return pendingStreamAddr.Get() == 0 }

// encode rebuilds the whole wire stream s from the raw-RGB framebuffer pix in one write-only
// pass. Loop order is row -> column -> plane: each pixel's read and gamma lookup happen once,
// then fan out across the planes. Each byte carries its control bits (baked ADDR_A, CLK) with
// both half-pixels' plane bits; the trailing idle byte parks CLK low through the latch. A black
// pix yields just the control bits, the valid initial frame.
func (e *engine) encode(pix []rgb, s []uint8) {
	// Per-plane row constants, precomputed once per row. ColorDepth is validated to <= 8, so
	// fixed-size arrays avoid a per-row allocation.
	var base [8]uint8 // baked ADDR_A for this (row, plane)
	var off [8]int    // byte offset of this (row, plane)'s chunk
	for r := 0; r < e.rowPairs; r++ {
		for p := 0; p < e.planes; p++ {
			// While plane 0 of row r shifts, the panel still displays row r-1.
			displayed := r
			if p == 0 {
				displayed = (r + e.rowPairs - 1) % e.rowPairs
			}
			base[p] = uint8((displayed & 1) << addrABit)
			off[p] = (r*e.planes + p) * e.bytesPerRow
			s[off[p]+e.width*2] = base[p] // trailing idle byte: CLK low, ADDR_A held
		}
		top := pix[r*e.width : (r+1)*e.width]                         // R1/G1/B1 -> bits 0..2
		bot := pix[(r+e.rowPairs)*e.width : (r+e.rowPairs+1)*e.width] // R2/G2/B2 -> bits 3..5
		for x := 0; x < e.width; x++ {
			tp, bp := top[x], bot[x]
			tr, tg, tb := e.gamma[tp.R], e.gamma[tp.G], e.gamma[tp.B]
			br, bg, bb := e.gamma[bp.R], e.gamma[bp.G], e.gamma[bp.B]
			for p := 0; p < e.planes; p++ {
				v := base[p]
				v |= ((tr >> p) & 1) << 0
				v |= ((tg >> p) & 1) << 1
				v |= ((tb >> p) & 1) << 2
				v |= ((br >> p) & 1) << 3
				v |= ((bg >> p) & 1) << 4
				v |= ((bb >> p) & 1) << 5
				i := off[p] + x*2
				s[i] = v
				s[i+1] = v | (1 << clkBit)
			}
		}
	}
}

// ----------------------------------------------------------------------------
// SCANOUT HARDWARE (DMA channel 0 + TC0/TC1 + the conductor ISR)
// ----------------------------------------------------------------------------
// The engine is a hardware SINGLETON — one DMA channel, two timers, one set of pins — so its
// running state and the DMAC-visible descriptors are package-level rather than engine fields:
// the DMAC reads the descriptor from a fixed 16-byte-aligned address, the conductor ISR reads
// the geometry snapshot from here on its hot path, and the `started` guard rejects a second
// start with ErrConfigured.

const dmaTrigSrcTC0OVF = 0x2C // DMAC trigger source: TC0 overflow

//go:align 16
var dmaDescriptors [1]descriptor

//go:align 16
var dmaWriteback [1]descriptor

var (
	started bool
	curStep uint32

	// Geometry snapshot for the conductor (uint32 to avoid casts in the hot path).
	geoPlanes      uint32
	geoBytesPerRow uint32
	geoSteps       uint32
	geoLit         [8]uint32 // per-plane OE on-time (exact BCM weight)
	geoWindow      [8]uint32 // per-plane display window (>= row shift time)
	geoBeatPeriod  uint16
)

var (
	curStreamAddr     volatile.Register32
	pendingStreamAddr volatile.Register32
	brightnessReg     volatile.Register32
)

// startHardware snapshots the geometry, claims DMA channel 0 + TC0/TC1 + the HUB75 pins, and
// starts refresh from buffer 0. Returns ErrConfigured if the singleton engine is already
// claimed.
func (e *engine) startHardware() error {
	if started {
		return ErrConfigured
	}
	geoPlanes = uint32(e.planes)
	geoBytesPerRow = uint32(e.bytesPerRow)
	geoSteps = uint32(e.rowPairs * e.planes)
	lit, window := e.planeTimings()
	copy(geoLit[:], lit)
	copy(geoWindow[:], window)
	geoBeatPeriod = uint16(e.beatPeriod)

	initCycleCounter()
	configurePins()
	curStreamAddr.Set(ptr(unsafe.Pointer(&e.bufs[0][0])))
	pendingStreamAddr.Set(0)
	brightnessReg.Set(255)
	if err := configureDMA(); err != nil {
		return err
	}
	if err := configureTimers(); err != nil {
		return err
	}
	started = true
	return nil
}

func rowAddrMasks(r uint32) (set, clr uint32) {
	if r&0b00010 != 0 {
		set |= addrBMask
	} else {
		clr |= addrBMask
	}
	if r&0b00100 != 0 {
		set |= addrCMask
	} else {
		clr |= addrCMask
	}
	if r&0b01000 != 0 {
		set |= addrDMask
	} else {
		clr |= addrDMask
	}
	if r&0b10000 != 0 {
		set |= addrEMask
	} else {
		clr |= addrEMask
	}
	return
}

// windowTick is the conductor: TC1 overflow ends each display window; CC1 match ends the OE
// on-time early when dimmed. It latches the just-shifted row, advances the address, sets the
// next window's timings, and re-arms the DMA chunk.
func windowTick(interrupt.Interrupt) {
	tc := sam.TC1_COUNT16
	port := &sam.PORT.GROUP[1]

	flags := tc.INTFLAG.Get()

	// CC1 match: the dimming cutoff — end this window's OE on-time.
	if flags&sam.TC_COUNT16_INTFLAG_MC1 != 0 {
		tc.INTFLAG.SetBits(sam.TC_COUNT16_INTFLAG_MC1)
		port.OUTSET.Set(oeMask)
		if flags&sam.TC_COUNT16_INTFLAG_OVF == 0 {
			return
		}
	}
	tc.INTFLAG.SetBits(sam.TC_COUNT16_INTFLAG_OVF)

	step := curStep
	row := step / geoPlanes
	plane := step % geoPlanes

	// Window and OE on-time for the plane about to be latched. The window is floored at the row
	// shift time so the next chunk always fits; the on-time carries the exact BCM weight (scaled
	// by global brightness) and, when shorter than the window, is ended early by the CC1 match —
	// that gap is what lets low planes be finer than the shift time (see planeTimings).
	window := geoWindow[plane]
	tc.CC[0].Set(uint16(window))
	cut := geoLit[plane]
	if br := brightnessReg.Get(); br < 255 {
		cut = cut * br >> 8
	}
	// The counter runs during this interrupt; a cutoff that lands before we return would be
	// missed and flash full brightness.
	if cut < litFloorTicks {
		cut = litFloorTicks
	}
	if cut >= window {
		tc.CC[1].Set(0xFFFF) // above the period: never matches
	} else {
		tc.CC[1].Set(uint16(cut))
	}

	// Blank -> row address -> latch -> unblank: the classic order. Setting the address first
	// gives the row decoders the whole latch pulse to settle. The settle/LAT-hold delays exist
	// because 120 MHz GPIO badly outpaces the panel's latch/decoder chips; ~200 ns clears their
	// minimums with margin and costs nothing against the shortest display window.
	port.OUTSET.Set(oeMask)
	set, clr := rowAddrMasks(row)
	if row&1 != 0 {
		set |= 1 << addrABit
	} else {
		clr |= 1 << addrABit
	}
	port.OUTSET.Set(set)
	port.OUTCLR.Set(clr)
	delayCycles(12) // ~100 ns address settle
	port.OUTSET.Set(latMask)
	delayCycles(24) // ~200 ns LAT hold
	port.OUTCLR.Set(latMask)
	port.OUTCLR.Set(oeMask)

	// Advance; at the frame boundary, honor a pending stream switch.
	next := step + 1
	if next == geoSteps {
		next = 0
		if pa := pendingStreamAddr.Get(); pa != 0 {
			curStreamAddr.Set(pa)
			pendingStreamAddr.Set(0)
		}
	}
	curStep = next

	volatile.StoreUint32(&dmaDescriptors[0].Srcaddr,
		curStreamAddr.Get()+(next+1)*geoBytesPerRow) // SRCINC: end address
	sam.DMAC.CHANNEL[0].CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE)
}

func configureDMA() error {
	sam.MCLK.AHBMASK.SetBits(sam.MCLK_AHBMASK_DMAC_)

	dmac := sam.DMAC
	dmac.CTRL.ClearBits(sam.DMAC_CTRL_DMAENABLE)
	dmac.CTRL.SetBits(sam.DMAC_CTRL_SWRST)
	if err := waitSync(func() bool { return !dmac.CTRL.HasBits(sam.DMAC_CTRL_SWRST) }); err != nil {
		return err
	}
	dmac.BASEADDR.Set(ptr(unsafe.Pointer(&dmaDescriptors[0])))
	dmac.WRBADDR.Set(ptr(unsafe.Pointer(&dmaWriteback[0])))

	portBOut := ptr(unsafe.Pointer(&sam.PORT.GROUP[1].OUT.Reg))

	desc := &dmaDescriptors[0]
	desc.Btctrl = btValid | btSrcInc
	desc.Btcnt = uint16(geoBytesPerRow)
	desc.Dstaddr = portBOut
	desc.Descaddr = 0 // stop after each chunk; the conductor re-arms
	desc.Srcaddr = curStreamAddr.Get() + geoBytesPerRow
	curStep = 0

	ch := &dmac.CHANNEL[0]
	ch.CHCTRLA.Set((dmaTrigSrcTC0OVF << sam.DMAC_CHANNEL_CHCTRLA_TRIGSRC_Pos) |
		(sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_BURST << sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_Pos) |
		(sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_SINGLE << sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_Pos))

	dmac.CTRL.Set(sam.DMAC_CTRL_DMAENABLE |
		sam.DMAC_CTRL_LVLEN0 | sam.DMAC_CTRL_LVLEN1 |
		sam.DMAC_CTRL_LVLEN2 | sam.DMAC_CTRL_LVLEN3)
	ch.CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE) // arm chunk 0
	return nil
}

func configureTimers() error {
	sam.MCLK.APBAMASK.SetBits(sam.MCLK_APBAMASK_TC0_ | sam.MCLK_APBAMASK_TC1_)
	sam.GCLK.PCHCTRL[9].Set(sam.GCLK_PCHCTRL_CHEN |
		(sam.GCLK_PCHCTRL_GEN_GCLK0 << sam.GCLK_PCHCTRL_GEN_Pos))

	tc0 := sam.TC0_COUNT16
	tc0.CTRLA.SetBits(sam.TC_COUNT16_CTRLA_SWRST)
	if err := waitSync(func() bool { return !tc0.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_SWRST) }); err != nil {
		return err
	}
	tc0.CTRLA.Set((sam.TC_COUNT16_CTRLA_MODE_COUNT16 << sam.TC_COUNT16_CTRLA_MODE_Pos) |
		(sam.TC_COUNT16_CTRLA_PRESCALER_DIV1 << sam.TC_COUNT16_CTRLA_PRESCALER_Pos))
	tc0.WAVE.Set(sam.TC_COUNT16_WAVE_WAVEGEN_MFRQ << sam.TC_COUNT16_WAVE_WAVEGEN_Pos)
	tc0.CC[0].Set(geoBeatPeriod)
	if err := waitSync(func() bool { return !tc0.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_CC0) }); err != nil {
		return err
	}

	tc1 := sam.TC1_COUNT16
	tc1.CTRLA.SetBits(sam.TC_COUNT16_CTRLA_SWRST)
	if err := waitSync(func() bool { return !tc1.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_SWRST) }); err != nil {
		return err
	}
	tc1.CTRLA.Set((sam.TC_COUNT16_CTRLA_MODE_COUNT16 << sam.TC_COUNT16_CTRLA_MODE_Pos) |
		(sam.TC_COUNT16_CTRLA_PRESCALER_DIV1 << sam.TC_COUNT16_CTRLA_PRESCALER_Pos))
	tc1.WAVE.Set(sam.TC_COUNT16_WAVE_WAVEGEN_MFRQ << sam.TC_COUNT16_WAVE_WAVEGEN_Pos)
	tc1.CC[0].Set(uint16(geoWindow[0]))
	tc1.CC[1].Set(0xFFFF)
	if err := waitSync(func() bool { return !tc1.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_CC0) }); err != nil {
		return err
	}
	tc1.INTFLAG.SetBits(sam.TC_COUNT16_INTFLAG_OVF | sam.TC_COUNT16_INTFLAG_MC1)
	tc1.INTENSET.SetBits(sam.TC_COUNT16_INTENSET_OVF | sam.TC_COUNT16_INTENSET_MC1)

	in := interrupt.New(sam.IRQ_TC1, windowTick)
	in.SetPriority(0x40)
	in.Enable()

	tc0.CTRLA.SetBits(sam.TC_COUNT16_CTRLA_ENABLE)
	if err := waitSync(func() bool { return !tc0.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_ENABLE) }); err != nil {
		return err
	}
	tc1.CTRLA.SetBits(sam.TC_COUNT16_CTRLA_ENABLE)
	return waitSync(func() bool { return !tc1.SYNCBUSY.HasBits(sam.TC_COUNT16_SYNCBUSY_ENABLE) })
}

// ----------------------------------------------------------------------------
// DWT cycle counter: 120 MHz free-running counter, 8.3 ns resolution. Architecturally fixed
// Cortex-M addresses, hence raw pointers. Used only for the conductor's sub-µs signal delays.
// ----------------------------------------------------------------------------

var (
	regDEMCR     = (*volatile.Register32)(unsafe.Pointer(uintptr(0xE000EDFC)))
	regDWTCtrl   = (*volatile.Register32)(unsafe.Pointer(uintptr(0xE0001000)))
	regDWTCyccnt = (*volatile.Register32)(unsafe.Pointer(uintptr(0xE0001004)))
)

func initCycleCounter() {
	regDEMCR.SetBits(1 << 24) // TRCENA
	regDWTCyccnt.Set(0)
	regDWTCtrl.SetBits(1 << 0) // CYCCNTENA
}

func cycles() uint32 { return regDWTCyccnt.Get() }

// delayCycles busy-waits n CPU cycles (8.3 ns each at 120 MHz) on the DWT counter. For
// sub-microsecond signal timing inside the conductor only.
func delayCycles(n uint32) {
	t0 := cycles()
	for cycles()-t0 < n {
	}
}

// ----------------------------------------------------------------------------
// Timing policy
// ----------------------------------------------------------------------------

// bytesPerRow is CLK-low + CLK-high bytes per pixel, plus one trailing idle byte that returns
// CLK low after the last pixel — without it CLK parks high through the latch, which some panel
// driver chips latch unreliably (worst on a sagging USB rail, but the CLK-low park is correct
// at nominal voltage too).
func bytesPerRow(width int) int { return width*2 + 1 }

// Conductor timing floors, in 120 MHz ticks (the lower bounds shiftFloor/planeTimings enforce).
const (
	// litFloorTicks is the minimum OE on-time (~3 µs): the CC1 cutoff that ends a lit period must
	// not land before the conductor ISR has returned, or the match is missed and the row flashes
	// at full window length.
	litFloorTicks = 360

	// isrMarginTicks (~10 µs) budgets the gap between a window starting and its chunk's first DMA
	// beat: the chunk is armed at the END of the conductor, whose worst case is ~3.5 µs plus entry
	// latency behind other handlers, and the first beat then waits for a TC0 overflow. Sized
	// generously — a short window latches a partially-shifted row (sporadic ghost rows).
	isrMarginTicks = 1200
)

// shiftFloor is the minimum display window in 120 MHz ticks: the wire time to shift one row
// chunk, plus 10% bus margin, plus the ISR/arming budget. A row must finish shifting within the
// previous plane's window, or it latches incomplete (shifted rows).
func (e *engine) shiftFloor() int {
	wire := e.bytesPerRow * e.beatPeriod
	return wire + wire/10 + isrMarginTicks + e.beatPeriod
}

// planeTimings returns each plane's OE on-time (lit) and display window in 120 MHz ticks. Lit
// times carry the exact BCM weights (baseHold << p); windows are the same values floored at
// shiftFloor. When a plane's lit time is below the floor, OE is ended early within the window by
// the CC1 match and the remainder is blanked — decoupling the two is what allows high plane
// counts (lit times shrink below the row shift time while windows stay small for refresh rate).
func (e *engine) planeTimings() (lit, window []uint32) {
	floor := uint32(e.shiftFloor())
	lit = make([]uint32, e.planes)
	window = make([]uint32, e.planes)
	for p := 0; p < e.planes; p++ {
		l := uint32(e.baseHold) << p
		w := l
		if w < floor {
			w = floor
		}
		lit[p], window[p] = l, w
	}
	return lit, window
}

// ─── TUNING ─── isr's timing knobs. Change them here and re-flash; nothing in Config controls
// timing. Tuned, hardware-verified at 64x32, 8 planes (~2.5 MHz pixel clock, ~71 Hz, ~87% duty;
// see README.md). Constants, not a geometry formula: the pixel clock is a hardware limit and
// the window floor makes baseHold↔duty nonlinear (so a pio/lcd-style duty target doesn't fit).
const (
	// beatPeriodTicks is the TC0 compare: one byte shifts every beatPeriodTicks+1 ticks of the
	// 120 MHz clock ⇒ pixel clock 60/(beatPeriodTicks+1) MHz (24 ≈ 2.4 MHz). Higher = slower,
	// safer shift (more margin for marginal panels/power); lower = faster (risk of smearing).
	beatPeriodTicks = 24

	// baseHoldTicks is plane-0's OE on-time in 120 MHz ticks; plane p is lit baseHoldTicks<<p.
	// Windows are floored at the row shift time (planeTimings), so low planes sit below it and
	// the remainder is blanked. Higher = brighter / lower refresh; lower = faster / dimmer.
	baseHoldTicks = 360
)

// DefaultConfig lives in hub75.go (shared across engines). isr's timing is internal — see the
// TUNING consts above.

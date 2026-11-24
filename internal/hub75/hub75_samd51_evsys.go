//go:build atsamd51 && !atsamd51_isr

// SAMD51 backend (Adafruit Matrix Portal M4) — the EVSYS-linked engine. The M4's default HUB75
// engine; build the alternative ISR-conductor engine with -tags=atsamd51_isr (hub75_samd51_isr.go).
// The shared pin map + DMAC descriptor layout live in hub75_samd51.go.
//
// A free-running byte-stream data plane, with OE pulled out of the stream and driven by
// a hardware timer (TCC3/WO[0] on PB12), retriggered via the Event System off the DMA's
// per-window block event — so OE pulses in hardware, paced by a compare, with zero ISR. The
// per-plane OE on-time (the BCM weighting) is streamed into TCC3.CCBUF0 by a SECOND DMA
// channel (channel 1), one WORD beat per window, off a sibling EVSYS channel on the same
// block event, keeping channel 0 at 4 descriptors/window (16 KB pool). The design's lab
// lineage is condensed in the repo README ("History"); measurements are in this package's README.md.
//
// Pin map (PORTB bits 0-15):
//
//	PB00-05 R1 G1 B1 R2 G2 B2   PB06 CLK   PB07 ADDR_A
//	PB08 ADDR_B   PB09 ADDR_C   PB12 OE    PB13 ADDR_E
//	PB14 LAT      PB15 ADDR_D
//
// The subtle parts (all confirmed on hardware; the OE-timer/EVSYS path has no host
// coverage and rests on the apps + the cmd/stress worst-case sweep):
//
//   - TWO EVSYS channels share the DMAC-CH0 block event: channel 0 is ASYNC (retriggers the
//     OE pulse — a TCC retrigger needs no clock); channel 1 is RESYNCHRONIZED + EDGSEL
//     rising with a GCLK (a DMAC trigger user samples the event on a clock). See
//     configureEvsysLink / configureCCEvsysLink.
//   - The channel-1 EVSYS user is EVSYS_ID_USER_DMAC_CH_1 = 6, not the low index it looks
//     like (the USER table is PORT_EV_0..3 = 1..4, THEN DMAC_CH_*). See ccEvUser.
//   - CHEVCTRL (event in/out) is enable-protected on both channels: arm it before enabling.
//   - Ch1's CCBUF0 write lands one window behind the retrigger that consumes it, so the
//     schedule is rotated left by one (buildCCSchedule) and the first two windows seeded
//     directly (configureOETimer). A future clock/window change could shift the rotation.
package hub75

import (
	"unsafe"

	"device/sam"
	"runtime/volatile"
)

// ----------------------------------------------------------------------------
// BIT PACKING (PORTB low byte)
// ----------------------------------------------------------------------------
// A data-segment beat is a single BYTE driving PORTB's low half: pixel data on bits 0-5
// (R1 G1 B1 R2 G2 B2), CLK on 6, ADDR_A on 7. The high byte (LAT on 14, ADDR_B-E on
// 8/9/15/13) is set by the backend's HWORD prep beat. OE (PB12) is NOT in the stream — it
// is a timer waveform output.

const (
	bCLK = 1 << 6  // CLK on PB06 (data-segment low byte)
	bLAT = 1 << 14 // LAT on PB14 (high byte)
)

// driverName is this build's engine name, exposed via hub75.DriverName.
const driverName = "evsys"

// engine is evsys's byte-stream backend: a free-running byte-stream refresh with OE on an
// EVSYS-linked hardware timer. It owns its config and the stream buffers; Device hands it the
// pixels to encode each present. Only one may run per program (one DMA channel / one output
// port / one OE timer).
type engine struct {
	// Configuration, derived from Config in start (the engine's own copy of what it needs to
	// encode — Device does not hold these).
	width, scanRows, planes int
	gamma                   [256]uint8 // gamma-2.6 curve scaled to planes bits
	brightness              uint8
	beats                   int
	baseHold                int
	oeBaseTicks             int // plane-0 OE on-time in TCC ticks (from the oeOnTicksBase tuning const)

	// Two pixel streams (double-buffered). The backend refreshes from bufs[front];
	// present encodes bufs[back], makes it live, and flips the roles. Layout per buffer is
	// [scanRow][plane][beat], flattened; beats are BYTES (PORTB low byte only). OE is NOT
	// in the stream — it is the timer's pin.
	bufs         [2][]uint8
	front, back  int
	doubleBuffer bool
}

// start derives the engine config from cfg, sizes and allocates the stream buffer(s), encodes
// the initial (black) pix, and starts the backend (DMA chain + OE timer + EVSYS link). Returns
// ErrConfigured if the singleton engine is already claimed.
func (e *engine) start(cfg Config, pix []rgb) error {
	if cfg.Height/2 > samdMaxScanRows {
		return ErrTooManyRows // descPool is static, sized to samdMaxScanRows (see hub75_samd51.go)
	}
	e.width = cfg.Width
	e.scanRows = cfg.Height / 2
	e.planes = cfg.ColorDepth
	e.gamma = scaleGamma(cfg.ColorDepth)
	e.brightness = resolveBrightness(cfg.Brightness)
	e.beats = cfg.Width * 2
	e.baseHold = baseHoldBeats    // tuning const (see TUNING below)
	e.oeBaseTicks = oeOnTicksBase // tuning const (see TUNING below)
	e.doubleBuffer = cfg.DoubleBuffer

	streamLen := e.scanRows * e.planes * e.beats // one byte per beat
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
	e.refreshSchedule()
	return nil
}

// present rebuilds the back stream from pix and presents it. Single-buffer rebuilds in place.
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

// setBrightness adopts the new brightness and rebuilds the per-plane on-time schedule. The Ch1
// descriptor reads ccSched live, so the change takes effect over the next frame; the OE timer's
// per-window compare schedule scales every plane by the same factor.
func (e *engine) setBrightness(b uint8) {
	e.brightness = b
	e.refreshSchedule()
}

// refreshSchedule rebuilds the (rotated) per-plane on-time schedule from the current
// brightness, once the engine is running. start calls it after the hardware is up; a brightness
// change calls it via setBrightness.
func (e *engine) refreshSchedule() {
	if !started {
		return
	}
	e.buildCCSchedule()
}

// swap requests that buffer `to` become live at the next frame boundary, by pointing the
// live chain's tail at the other chain's head.
func (e *engine) swap(to int) {
	want = to
	if to == live {
		return
	}
	tail := &descPool[live][chainLen[live]-1]
	volatile.StoreUint32(&tail.Descaddr, ringHead(to))
}

// swapDone reports whether the engine has adopted the requested buffer (the writeback shows
// the DMAC now running inside the wanted chain), and restores the old chain's self-loop.
func (e *engine) swapDone() bool {
	if want == live {
		return true
	}
	next := volatile.LoadUint32(&wrbDesc[0].Descaddr)
	lo := ringHead(want)
	hi := ptr(unsafe.Pointer(&descPool[want][chainLen[want]-1]))
	if next < lo || next > hi {
		return false
	}
	old := live
	tail := &descPool[old][chainLen[old]-1]
	volatile.StoreUint32(&tail.Descaddr, ringHead(old))
	live = want
	return true
}

// encode writes the whole frame into buf: for each scan row, the top half (grid row r) drives
// bits 0-2 and the bottom half (row r+scanRows) bits 3-5, across all planes, two byte beats
// per column (gamma applied here; OE is not in the stream — the timer owns it). A black pix
// yields just the control bits (ADDR_A + CLK), the valid initial frame.
func (e *engine) encode(pix []rgb, buf []uint8) {
	for r := 0; r < e.scanRows; r++ {
		top := pix[r*e.width : (r+1)*e.width]
		bot := pix[(r+e.scanRows)*e.width : (r+e.scanRows+1)*e.width]
		a := addrA(r) // bit 7, baked into every data beat
		for col := 0; col < e.width; col++ {
			tp, bp := top[col], bot[col]
			tr, tg, tb := e.gamma[tp.R], e.gamma[tp.G], e.gamma[tp.B]
			br, bg, bb := e.gamma[bp.R], e.gamma[bp.G], e.gamma[bp.B]
			for k := 0; k < e.planes; k++ {
				ku := uint(k)
				data := (tr>>ku)&1<<0 |
					(tg>>ku)&1<<1 |
					(tb>>ku)&1<<2 |
					(br>>ku)&1<<3 |
					(bg>>ku)&1<<4 |
					(bb>>ku)&1<<5
				b := data | a
				i := (r*e.planes+k)*e.beats + col*2
				buf[i] = b
				buf[i+1] = b | bCLK
			}
		}
	}
}

// oeOnTicks is plane k's OE-low on-time at the engine's current brightness, in timer ticks:
// e.oeBaseTicks<<k scaled by brightness/255, clamped to [1, 0xFFFF]. e.oeBaseTicks is plane
// 0's on-time (the oeOnTicksBase tuning const); plane k is lit e.oeBaseTicks<<k — the BCM
// weighting, the timer-side twin of the display window's baseHold<<k. Its ceiling (each plane's
// on-time must end before its window's next latch, or OE bleeds the row into the next —
// ghosting) is documented on the oeOnTicksBase TUNING const.
func (e *engine) oeOnTicks(plane int) uint32 {
	on := (e.oeBaseTicks << uint(plane)) * int(e.brightness) / 255
	if on < 1 {
		on = 1
	}
	if on > 0xFFFF {
		on = 0xFFFF
	}
	return uint32(on)
}

// buildCCSchedule fills ccSched with each plane's OE on-time (oeOnTicks), ROTATED LEFT BY
// ONE for the channel-1 phase: the value the Ch1 descriptor writes during window w lands
// in CCBUF0 after that window's retrigger has already loaded the previous value, so it
// serves window w+1. Hence ccSched[i] = oeOnTicks((i+1) % planes). configureOETimer seeds
// the first two windows directly. The rotation and those seeds are one mechanism — a
// clock/window change moves both together (see the file header).
func (e *engine) buildCCSchedule() {
	for i := range ccSched {
		ccSched[i] = e.oeOnTicks((i + 1) % e.planes)
	}
}

// startHardware builds the refresh structure, configures the OE timer + EVSYS link, and starts
// the engine with buffer 0 live. Returns ErrConfigured if the singleton engine is already
// claimed.
func (e *engine) startHardware() error {
	if started {
		return ErrConfigured
	}
	latchWord = make([][2]uint16, e.scanRows)
	windowWord = make([]uint16, e.scanRows)
	for r := 0; r < e.scanRows; r++ {
		ctrl := addrBits(r) // addressed, OE absent (timer owns it), LAT low
		latchWord[r] = [2]uint16{ctrl | bLAT, ctrl}
		windowWord[r] = ctrl
	}
	ccSched = make([]uint32, e.planes)
	e.buildCCSchedule()
	e.buildChain(0)
	if e.doubleBuffer {
		e.buildChain(1)
	}
	live, want = 0, 0

	// rootDesc[0] is the channel-0 BASEADDR entry: a clone of the live chain head, used
	// once at the initial trigger. The chain then runs from descPool[0][0] forever (the
	// last window's descaddr self-loops to ringHead(0)), so the swap logic — which
	// rewrites tails to ringHead(other) — is unchanged.
	rootDesc[0] = descPool[0][0]
	e.buildCCDescriptor() // rootDesc[1]: the channel-1 CC streamer

	configurePins()
	if err := e.configureOETimer(); err != nil { // TCC3 drives OE (PB12); seeds the first windows
		return err
	}
	configureEvsysLink()                   // EVSYS ch0: DMAC-CH0 event → TCC3 retrigger (OE)
	configureCCEvsysLink()                 // EVSYS ch1: DMAC-CH0 event → DMAC-CH1 trigger (CC advance)
	if err := configureDMA(); err != nil { // arms both channels; ch0 sw-triggered, ch1 event-driven
		return err
	}
	started = true
	return nil
}

// buildChain wires one self-looping chain for stream buffer ci — four descriptors per
// (row, plane) at stride descsPerWindow: prep (HWORD: address + LAT low), data segment
// (byte beats, incrementing), latch pulse (HWORD; emits the block event that drives both
// the OE retrigger and the Ch1 CC advance), display window (HWORD, fixed source,
// baseHold<<k beats). Channel 1 carries the CC schedule (see buildCCDescriptor).
func (e *engine) buildChain(ci int) {
	dst := ptr(unsafe.Pointer(&sam.PORT.GROUP[1].OUT.Reg)) // &OUT.Reg == low byte too
	buf := e.bufs[ci]
	ch := &descPool[ci]
	idx := func(r, k int) int { return (r*e.planes + k) * descsPerWindow }

	for r := 0; r < e.scanRows; r++ {
		for k := 0; k < e.planes; k++ {
			base := idx(r, k)
			pre := &ch[base+0]
			dat := &ch[base+1]
			lat := &ch[base+2]
			win := &ch[base+3]

			nr, nk := r, k+1
			if nk == e.planes {
				nr, nk = (r+1)%e.scanRows, 0
			}
			next := &ch[idx(nr, nk)] // last (row,plane) wraps to (0,0): self-loop

			// prep: one HWORD write of the high byte (row addressed, LAT low, OE
			// absent) so the byte data segment can leave the high byte untouched.
			pre.Btctrl = btValid | btHWord
			pre.Btcnt = 1
			pre.Srcaddr = ptr(unsafe.Pointer(&windowWord[r]))
			pre.Dstaddr = dst
			pre.Descaddr = ptr(unsafe.Pointer(dat))

			// data: byte beats (BEATSIZE clear = BYTE) to OUT's low byte; END addr
			// for an incrementing byte source advances one byte per beat.
			seg := &buf[(r*e.planes+k)*e.beats]
			dat.Btctrl = btValid | btSrcInc
			dat.Btcnt = uint16(e.beats)
			dat.Srcaddr = ptr(unsafe.Pointer(seg)) + uint32(e.beats)
			dat.Dstaddr = dst
			dat.Descaddr = ptr(unsafe.Pointer(lat))

			// latch: pulse LAT; emit a block event on completion. EVSYS fans it to
			// the OE timer RETRIGGER (ch0) AND the CC channel's trigger (ch1).
			lat.Btctrl = btValid | btHWord | btSrcInc | btEvtOut
			lat.Btcnt = 2
			lat.Srcaddr = ptr(unsafe.Pointer(&latchWord[r][0])) + 2*2
			lat.Dstaddr = dst
			lat.Descaddr = ptr(unsafe.Pointer(win))

			// display window: reserve baseHold<<k beats. Fixed source (address held,
			// LAT low). The OE pulse inside it is the timer's, not these writes.
			win.Btctrl = btValid | btHWord
			win.Btcnt = uint16(e.baseHold << uint(k))
			win.Srcaddr = ptr(unsafe.Pointer(&windowWord[r]))
			win.Dstaddr = dst
			win.Descaddr = ptr(unsafe.Pointer(next))
		}
	}
	chainLen[ci] = e.scanRows * e.planes * descsPerWindow
}

// buildCCDescriptor wires channel 1's single self-looping descriptor: it streams ccSched
// (planes WORD entries, incrementing) into TCC3.CCBUF0, one beat per window event. With
// btcnt = planes, a block completes every planes beats = one scan row, then the self-loop
// reloads the descriptor (source resets to ccSched[0]) — matching the k=0…planes-1
// cadence per row. The schedule is frame-independent, so this one descriptor serves both
// frame chains.
func (e *engine) buildCCDescriptor() {
	cc := &rootDesc[1]
	cc.Btctrl = btValid | btWord | btSrcInc
	cc.Btcnt = uint16(e.planes)
	cc.Srcaddr = ptr(unsafe.Pointer(&ccSched[0])) + uint32(e.planes*4) // END addr, WORD = 4 B
	cc.Dstaddr = ptr(unsafe.Pointer(&sam.TCC3.CCBUF[0].Reg))
	cc.Descaddr = ptr(unsafe.Pointer(&rootDesc[1])) // self-loop
}

// configureOETimer is the TCC3 setup, except the initial on-times are seeded
// from oeOnTicks directly (not ccSched[0], which is rotated): CC0 = window 0's
// on-time (plane 0), CCBUF0 = window 1's (plane 1), so the first two windows are correct
// before the Ch1 stream takes over. ⚠️ The CCBUF0 seed pairs with buildCCSchedule's
// rotation — change one and the other must move with it.
func (e *engine) configureOETimer() error {
	muxOEToTimer()

	sam.MCLK.APBCMASK.SetBits(sam.MCLK_APBCMASK_TCC3_)
	sam.GCLK.PCHCTRL[sam.PCHCTRL_GCLK_TCC3].Set(
		(sam.GCLK_PCHCTRL_GEN_GCLK0 << sam.GCLK_PCHCTRL_GEN_Pos) | sam.GCLK_PCHCTRL_CHEN)

	tcc := sam.TCC3
	tcc.CTRLA.ClearBits(sam.TCC_CTRLA_ENABLE)
	if err := waitSync(func() bool { return !tcc.SYNCBUSY.HasBits(sam.TCC_SYNCBUSY_ENABLE) }); err != nil {
		return err
	}
	tcc.CTRLA.SetBits(sam.TCC_CTRLA_SWRST)
	if err := waitSync(func() bool { return !tcc.SYNCBUSY.HasBits(sam.TCC_SYNCBUSY_SWRST) }); err != nil {
		return err
	}

	tcc.WAVE.Set(sam.TCC_WAVE_WAVEGEN_NPWM << sam.TCC_WAVE_WAVEGEN_Pos)
	tcc.DRVCTRL.SetBits(sam.TCC_DRVCTRL_INVEN0) // WO0 rests high = OE blanked
	tcc.PER.Set(0xFFFF)
	tcc.CC[0].Set(e.oeOnTicks(0))               // window 0 (plane 0)
	tcc.CCBUF[0].Set(e.oeOnTicks(1 % e.planes)) // window 1 (plane 1), loaded at event 0

	tcc.EVCTRL.Set((sam.TCC_EVCTRL_EVACT0_RETRIGGER << sam.TCC_EVCTRL_EVACT0_Pos) | sam.TCC_EVCTRL_TCEI0)
	tcc.CTRLBSET.Set(sam.TCC_CTRLBSET_ONESHOT)

	tcc.CTRLA.SetBits(sam.TCC_CTRLA_ENABLE)
	return waitSync(func() bool { return !tcc.SYNCBUSY.HasBits(sam.TCC_SYNCBUSY_ENABLE) })
}

// ----------------------------------------------------------------------------
// SCANOUT HARDWARE (DMA chain + OE timer + EVSYS bring-up)
// ----------------------------------------------------------------------------
// The engine is a hardware SINGLETON — one DMA channel, one output port, one OE timer — so its
// running state and the DMAC-visible buffers below are package-level rather than engine fields:
// the DMAC reads descriptors and source words from fixed, 16-byte-aligned addresses, and the
// `started` guard rejects a second start with ErrConfigured. (Per-frame pixel state lives on
// the Device's grid; only the engine's fixed-address scanout structures live here.)

// descsPerWindow is 4: prep, data, latch, display window (the CC schedule rides on channel 1,
// not on a fifth descriptor). The static descPool below is sized to the M4's own ceilings
// (samdMaxScanRows*maxPlanes windows), so every config that start admits (it rejects a taller
// panel with ErrTooManyRows) fits without allocation.
const descsPerWindow = 4

// descPool holds channel 0's two frame chains (one self-looping chain per buffer).
//
//go:align 16
var descPool [2][samdMaxScanRows * maxPlanes * descsPerWindow]descriptor

// rootDesc is the DMAC's per-channel first-descriptor table at BASEADDR: rootDesc[0] is
// channel 0's entry (a one-time clone of the live chain head — the steady-state head
// stays descPool[0][0], reached via the self-loop), rootDesc[1] is channel 1's single
// self-looping CC-streamer descriptor.
//
//go:align 16
var rootDesc [2]descriptor

// wrbDesc is the DMAC's per-channel writeback table at WRBADDR (one slot per channel
// used). wrbDesc[0] is channel 0's in-flight descriptor (read by swapDone).
//
//go:align 16
var wrbDesc [2]descriptor

var (
	started  bool
	chainLen [2]int
	live     int
	want     int

	latchWord  [][2]uint16
	windowWord []uint16

	// ccSched is the per-plane OE on-time (TCC ticks), one entry per plane, ROTATED for
	// the channel-1 phase (see buildCCSchedule). Streamed into TCC3.CCBUF0 by the Ch1
	// descriptor, one beat per window. Shared by both frame chains; rewritten by
	// refreshSchedule.
	ccSched []uint32
)

// EVSYS IDs (DS60001507 Ch.31). device/sam re-exports none of
// these, hence the literals.
const (
	oeEvGen  = 34 // EVSYS_ID_GEN_DMAC_CH_0  (DMAC channel 0 block event) — shared generator
	oeEvUser = 36 // EVSYS_ID_USER_TCC3_EV_0 (TCC3 event input 0)
	// ccEvUser: EVSYS_ID_USER_DMAC_CH_1 = 6 (the DMAC channel-1 event/trigger input).
	// The SAMD51 USER table is PORT_EV_0..3 = 1..4, THEN DMAC_CH_0..7 = 5..12 — not the low
	// indices, so CH_1 is 6, not 1. Confirmed in framework-cmsis-atmel samd51 evsys.h and
	// cross-checked by the known-good oeEvUser (TCC3_EV_0 = 36) and oeEvGen (DMAC_CH_0 = 34).
	// (An id of 1 = PORT_EV_0: the event reaches the EVSYS channel — EVD sets — but never the
	// DMAC trigger; the symptom is a uniformly dim panel.)
	ccEvUser = 6
)

func ringHead(r int) uint32 { return ptr(unsafe.Pointer(&descPool[r][0])) }

func muxOEToTimer() {
	const oePin = 12
	const funcF = 0x5
	pg := &sam.PORT.GROUP[1]
	pg.PINCFG[oePin].SetBits(sam.PORT_GROUP_PINCFG_PMUXEN)
	v := pg.PMUX[oePin/2].Get()
	v = v&^uint8(0xF<<sam.PORT_GROUP_PMUX_PMUXE_Pos) | uint8(funcF<<sam.PORT_GROUP_PMUX_PMUXE_Pos)
	pg.PMUX[oePin/2].Set(v)
}

// configureEvsysLink is the OE link: DMAC-CH0 block event → EVSYS channel 0
// (ASYNC) → TCC3 RETRIGGER.
func configureEvsysLink() {
	sam.MCLK.APBBMASK.SetBits(sam.MCLK_APBBMASK_EVSYS_)

	const evCh = 0
	sam.EVSYS.CHANNEL[evCh].CHANNEL.Set(
		(oeEvGen << sam.EVSYS_CHANNEL_CHANNEL_EVGEN_Pos) |
			(sam.EVSYS_CHANNEL_CHANNEL_PATH_ASYNCHRONOUS << sam.EVSYS_CHANNEL_CHANNEL_PATH_Pos))
	sam.EVSYS.USER[oeEvUser].Set(evCh + 1)
}

// configureCCEvsysLink routes the SAME DMAC-CH0 block event to a SECOND EVSYS
// channel whose user is DMAC channel 1's trigger input, so each window event advances the
// CC stream by one beat. Multiple EVSYS channels may share one generator.
//
// PATH = RESYNCHRONIZED (not the OE link's ASYNC). Confirmed on the panel: with ASYNC the
// CC channel never transferred (ch1 stayed idle and CCBUF0 frozen at the
// seed, so every plane got one on-time and the panel was uniformly dim) even though the
// same async event retriggered the TCC fine. A DMAC trigger user samples the event on a
// clock, so it needs a clocked path; the TCC retrigger does not. RESYNCHRONIZED needs a
// GCLK on the EVSYS channel (EVSYS channel n ⇒ PCHCTRL_GCLK_EVSYSn; channel 1 ⇒ index 12)
// and edge detection on the event pulse (EDGSEL rising = one trigger per block event).
func configureCCEvsysLink() {
	const evChCC = 1
	// Clock EVSYS channel 1 from GCLK0 (120 MHz) for the resync path; the EVSYS APB clock
	// is already enabled by configureEvsysLink.
	sam.GCLK.PCHCTRL[sam.PCHCTRL_GCLK_EVSYS1].Set(
		(sam.GCLK_PCHCTRL_GEN_GCLK0 << sam.GCLK_PCHCTRL_GEN_Pos) | sam.GCLK_PCHCTRL_CHEN)
	sam.EVSYS.CHANNEL[evChCC].CHANNEL.Set(
		(oeEvGen << sam.EVSYS_CHANNEL_CHANNEL_EVGEN_Pos) |
			(sam.EVSYS_CHANNEL_CHANNEL_PATH_RESYNCHRONIZED << sam.EVSYS_CHANNEL_CHANNEL_PATH_Pos) |
			(sam.EVSYS_CHANNEL_CHANNEL_EDGSEL_RISING_EDGE << sam.EVSYS_CHANNEL_CHANNEL_EDGSEL_Pos))
	sam.EVSYS.USER[ccEvUser].Set(evChCC + 1)
}

// configureDMA resets the DMAC, points BASEADDR/WRBADDR at the two-entry per-channel
// tables, then arms BOTH channels: channel 0 as the self-looping data
// transaction (one software trigger runs it forever), channel 1 as an event-triggered
// single-burst CC streamer (one WORD beat per window event, no software trigger). EVIE on
// channel 1 (like EVOE on channel 0) is armed BEFORE the channel is enabled — CHEVCTRL is
// enable-protected.
func configureDMA() error {
	sam.MCLK.AHBMASK.SetBits(sam.MCLK_AHBMASK_DMAC_)

	dmac := sam.DMAC
	dmac.CTRL.ClearBits(sam.DMAC_CTRL_DMAENABLE)
	dmac.CTRL.SetBits(sam.DMAC_CTRL_SWRST)
	if err := waitSync(func() bool { return !dmac.CTRL.HasBits(sam.DMAC_CTRL_SWRST) }); err != nil {
		return err
	}

	dmac.BASEADDR.Set(ptr(unsafe.Pointer(&rootDesc[0])))
	dmac.WRBADDR.Set(ptr(unsafe.Pointer(&wrbDesc[0])))
	dmac.CTRL.SetBits(sam.DMAC_CTRL_LVLEN0 | sam.DMAC_CTRL_LVLEN1 |
		sam.DMAC_CTRL_LVLEN2 | sam.DMAC_CTRL_LVLEN3)
	dmac.CTRL.SetBits(sam.DMAC_CTRL_DMAENABLE)

	// Channel 0: the data transaction.
	ch0 := &dmac.CHANNEL[0]
	ch0.CHCTRLA.ClearBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE)
	if err := waitSync(func() bool { return !ch0.CHCTRLA.HasBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE) }); err != nil {
		return err
	}
	ch0.CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_SWRST)
	if err := waitSync(func() bool { return !ch0.CHCTRLA.HasBits(sam.DMAC_CHANNEL_CHCTRLA_SWRST) }); err != nil {
		return err
	}
	ch0.CHCTRLA.Set((sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_TRANSACTION << sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_Pos) |
		(sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_SINGLE << sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_Pos))
	ch0.CHPRILVL.Set(0)
	ch0.CHEVCTRL.SetBits(uint8(sam.DMAC_CHANNEL_CHEVCTRL_EVOE |
		(sam.DMAC_CHANNEL_CHEVCTRL_EVOMODE_DEFAULT << sam.DMAC_CHANNEL_CHEVCTRL_EVOMODE_Pos)))
	ch0.CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE)

	// Channel 1: the CC streamer, event-triggered (one beat per window event).
	ch1 := &dmac.CHANNEL[1]
	ch1.CHCTRLA.ClearBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE)
	if err := waitSync(func() bool { return !ch1.CHCTRLA.HasBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE) }); err != nil {
		return err
	}
	ch1.CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_SWRST)
	if err := waitSync(func() bool { return !ch1.CHCTRLA.HasBits(sam.DMAC_CHANNEL_CHCTRLA_SWRST) }); err != nil {
		return err
	}
	// TRIGSRC=DISABLE (events drive it, not a peripheral), TRIGACT=BURST + BURSTLEN=1 →
	// one beat per trigger.
	ch1.CHCTRLA.Set((sam.DMAC_CHANNEL_CHCTRLA_TRIGSRC_DISABLE << sam.DMAC_CHANNEL_CHCTRLA_TRIGSRC_Pos) |
		(sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_BURST << sam.DMAC_CHANNEL_CHCTRLA_TRIGACT_Pos) |
		(sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_SINGLE << sam.DMAC_CHANNEL_CHCTRLA_BURSTLEN_Pos))
	ch1.CHPRILVL.Set(0)
	// Event input as a transfer trigger; armed before enable (enable-protected).
	ch1.CHEVCTRL.SetBits(uint8(sam.DMAC_CHANNEL_CHEVCTRL_EVIE |
		(sam.DMAC_CHANNEL_CHEVCTRL_EVACT_TRIG << sam.DMAC_CHANNEL_CHEVCTRL_EVACT_Pos)))
	ch1.CHCTRLA.SetBits(sam.DMAC_CHANNEL_CHCTRLA_ENABLE)

	dmac.SWTRIGCTRL.SetBits(1 << 0) // start channel 0's endless transaction
	return nil
}

// addrA returns ADDR_A (PB07), the only address line in PORTB's low byte; it is baked into
// every data beat so the byte writes carry it.
func addrA(r int) uint8 {
	if r&1 != 0 {
		return 1 << 7
	}
	return 0
}

// addrBits returns the address-line bits selecting row pair r (A..E on PB07,08,09,15,13).
func addrBits(r int) uint16 {
	var a uint16
	if r&1 != 0 {
		a |= 1 << 7 // A
	}
	if r&2 != 0 {
		a |= 1 << 8 // B
	}
	if r&4 != 0 {
		a |= 1 << 9 // C
	}
	if r&8 != 0 {
		a |= 1 << 15 // D
	}
	if r&16 != 0 {
		a |= 1 << 13 // E
	}
	return a
}

// ─── TUNING ─── evsys's timing knobs. Change them here and re-flash; nothing in Config
// controls timing. They are a tuned, hardware-verified pair (see README.md), not a
// geometry formula like pio/lcd's autoBaseHold, because the evsys window/OE timing is a
// hand-tuned coupling rather than a duty-cycle target.
const (
	// baseHoldBeats is plane-0's display window in DMA beats; plane k reserves baseHoldBeats<<k.
	// The window just has to be long enough to contain that plane's OE pulse (see the ceiling
	// on oeOnTicksBase). Bigger = more headroom but lower refresh.
	baseHoldBeats = 8

	// oeOnTicksBase is plane-0's OE-low on-time in TCC timer ticks; plane k is lit
	// oeOnTicksBase<<k (the BCM weight), scaled by Brightness at runtime. Higher = brighter.
	// CEILING: keep it below ~baseHoldBeats × (TCC ticks per DMA beat, ≈6 on the M4), or the
	// brightest planes' pulse overruns its window and ghosts into the next row (no host check —
	// it only shows on the panel).
	oeOnTicksBase = 48
)

// DefaultConfig lives in hub75.go (shared across engines). evsys's timing is internal — see the
// TUNING consts above.

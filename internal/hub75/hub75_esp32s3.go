//go:build esp32s3

// ESP32-S3 backend (Adafruit Matrix Portal S3) — the LCD_CAM engine.
//
// Drives HUB75 panels with zero CPU involvement during refresh, using the ESP32-S3's LCD_CAM
// peripheral (i8080 parallel mode) + GDMA. The engine owns the present protocol (the Device's
// grid is the framebuffer), feeding LCD_CAM a stream of 16-bit words via a GDMA descriptor
// chain. LCD_CAM clocks each
// word onto a 13-bit parallel bus while its pixel clock (LCD_PCLK) drives the HUB75 shift
// clock. The encode and present protocol are its own; the gamma curve and brightness/geometry
// helpers come from the shared hub75.go floor.
//
// WHY LCD_CAM. LCD_CAM is an i8080/parallel master: GDMA streams words through it and it
// emits one bus word per PCLK forever (lcd_always_out_en + a circular descriptor chain). The
// PCLK pin IS the HUB75 CLK, so it is one stream word per shifted column — no separate clock
// toggling.
//
// LOGICAL BUS, ROUTED PINS. Each 16-bit word carries the 13 HUB75 control/data lines on a
// fixed LOGICAL layout (bits below); the LCD_DATA_OUT0..12 signals route to whatever GPIOs
// the board wires through the GPIO matrix (the esp32s3 backend owns that map). So this
// encoder is entirely board-agnostic: it never names a GPIO, only logical bus bits. The
// HUB75 CLK is not in the word — it is LCD_PCLK, a separate signal.
//
// DESCRIPTOR HOLDS — the RAM trick. The BCM display time (plane k must dwell 2^k longer than
// plane 0) cannot be count-encoded the way the pio driver does it: LCD_CAM clocks exactly one
// word per PCLK. The naive answer materializes every hold beat as a word, and at deep color
// that dwarfs RAM (~Width·planes + BaseHold·2^planes words). Instead this driver MATERIALIZES
// only the per-pixel shift data, and expresses the holds as GDMA descriptors that REPEAT a
// tiny shared buffer:
//
//   - shift buffer: per (row, plane), Width shift words + a latch word, carrying the actual
//     pixel bits. This is the only thing re-encoded each Display.
//   - hold buffers: just two distinct values per scan row — addr (OE low, lit) and addr|OE
//     (blanked) — each stored as a short run of identical words (holdRun long), shared by
//     every hold of that row.
//   - plan: an ordered, circular list of segments. Per (row, plane): one shift segment, then
//     ceil(lit/holdRun) descriptors pointing at the row's lit buffer and ceil(blank/holdRun)
//     at its blank buffer. A hold of thousands of beats costs a handful of 12-byte descriptors
//     instead of thousands of words. The esp32s3 backend turns the plan into the GDMA chain.
//
// So a deep, bright frame fits in a fraction of the materialized RAM, and Display only
// rewrites the shift words (the holds are static descriptors) — far less per-frame work.
// The plane dwell (holdDutyRatio) sets duty; the lit/blank split lives in the plan, so changing
// brightness rebuilds the (cheap) plan, not the buffers.
//
// BUFFERING. Single-buffered by default: the backend refreshes from the one shift buffer in
// place (the holds are shared), so Display can tear; the win is RAM. With Config.DoubleBuffer it
// runs two shift buffers and two GDMA chains and swaps tear-free at a frame boundary — present
// encodes the idle buffer, then relinks the live chain's tail to the idle chain's head so the
// GDMA switches between frames, never mid-frame (see present/swapToBack).
package hub75

import (
	"device/esp"
	"machine"
	"runtime/volatile"
	"unsafe"
)

// Logical bus-bit positions within a stream word. These are NOT GPIO numbers: they are the
// LCD_DATA_OUT0..12 signal indices, which the esp32s3 backend routes to the board's GPIOs
// through the matrix. The six RGB data lines are the low bits so a plane packs into bits 0-5.
// The HUB75 CLK is absent here — it is LCD_PCLK, driven by the peripheral, not a bus bit.
const (
	bR1 = 1 << 0 // LCD_DATA_OUT0  -> R1
	bG1 = 1 << 1 // LCD_DATA_OUT1  -> G1
	bB1 = 1 << 2 // LCD_DATA_OUT2  -> B1
	bR2 = 1 << 3 // LCD_DATA_OUT3  -> R2
	bG2 = 1 << 4 // LCD_DATA_OUT4  -> G2
	bB2 = 1 << 5 // LCD_DATA_OUT5  -> B2
	// address A..E on LCD_DATA_OUT6..10 (bits 6-10)
	bLAT = 1 << 11 // LCD_DATA_OUT11 -> LAT
	bOE  = 1 << 12 // LCD_DATA_OUT12 -> OE (active low: bit SET = output blanked)
)

// latWords is the number of words spent finishing the latch after a shifted row. LAT itself
// is asserted ON the last column's shift word (see encodeShift): with LCD_CAM every word is a
// PCLK pulse, so a separate LAT word would be one extra shift — latching the register one
// position too far (image shifted a column left, rightmost dark). Asserting LAT on the final
// column makes the latch coincide with the last shift; this one trailing blanked word (OE
// high, LAT low) holds it before the display block.
const latWords = 1

// holdRun is the length (words) of each shared hold buffer, and so the most words a single
// hold descriptor emits. A hold of N words becomes ceil(N/holdRun) descriptors pointing at
// the buffer (the last one reads a prefix — every word is identical). Bigger holdRun means
// fewer descriptors but larger shared buffers; 64 balances the two (see package doc).
const holdRun = 64

// seg is one entry in the descriptor plan: emit the first `words` words of `data`. A shift
// segment's data is that (row,plane)'s slice of the shift buffer (words == its full length);
// a hold segment's data is a shared hold buffer, read as a prefix of `words` identical words.
// The esp32s3 backend turns each seg into one GDMA descriptor (&data[0], words*2 bytes).
type seg struct {
	data  []uint16
	words int
}

// driverName is this build's engine name, exposed via hub75.DriverName.
const driverName = "lcd"

// engine is lcd's descriptor-hold backend: it materializes only the per-pixel shift words and
// expresses the BCM holds as GDMA descriptors over tiny shared buffers. It owns its config and
// the shift/hold buffers + descriptor plan; Device hands it the pixels to encode each present.
// Only one may run per program (one LCD_CAM / one GDMA chain / one output bus).
type engine struct {
	// Configuration, derived from Config in start (the engine's own copy of what it needs to
	// encode — Device does not hold these).
	width, scanRows, planes int
	gamma                   [256]uint8 // gamma-2.6 curve scaled to planes bits
	brightness              uint8
	baseHold                int // plane-0 display words; plane k displays baseHold<<k

	shiftLen  int         // words per (row,plane) shift segment: width + latWords
	doubleBuf bool        // Config.DoubleBuffer: run two shift buffers + chains, swap tear-free
	shiftBuf  [2][]uint16 // materialized shift data; [1] used only when doubleBuf
	holds     [][]uint16  // 2*scanRows shared hold buffers: [2r]=lit (addr), [2r+1]=blank (addr|OE)
	plan      [2][]seg    // circular descriptor plan per shift buffer; [1] only when doubleBuf

	// front is the chain (and shift buffer) the GDMA is scanning; back is the idle one present
	// encodes into. Equal when single-buffered; swapped by swapToBack at a frame boundary.
	front, back     int
	brightnessDirty bool // set by setBrightness; present rebuilds the plan + GDMA chain
}

// start derives the engine config from cfg, sizes and allocates the shift + hold buffers,
// builds the initial plan from pix, and starts the hardware refreshing. After it returns the
// panel refreshes with no CPU. Returns ErrTooBig if the buffers exceed the RAM budget, or
// ErrConfigured if the singleton engine is already claimed.
func (e *engine) start(cfg Config, pix []rgb) error {
	e.width = cfg.Width
	e.scanRows = cfg.Height / 2
	e.planes = cfg.ColorDepth
	e.gamma = scaleGamma(cfg.ColorDepth)
	e.brightness = resolveBrightness(cfg.Brightness)
	e.baseHold = autoBaseHold(e.width, e.planes)
	e.shiftLen = e.width + latWords
	e.doubleBuf = cfg.DoubleBuffer

	if e.ramBytes() > maxBufferBytes {
		return ErrTooBig
	}

	// Shared per-row hold buffers (constant — they depend only on the row address).
	e.holds = make([][]uint16, 2*e.scanRows)
	for r := 0; r < e.scanRows; r++ {
		addr := addrBits(r)
		lit := make([]uint16, holdRun)
		blank := make([]uint16, holdRun)
		for i := 0; i < holdRun; i++ {
			lit[i] = addr         // OE low -> panel lit
			blank[i] = addr | bOE // blanked
		}
		e.holds[2*r] = lit
		e.holds[2*r+1] = blank
	}

	// One shift buffer + GDMA chain when single-buffered, two when double-buffered.
	bufs := 1
	if e.doubleBuf {
		bufs = 2
	}
	for c := 0; c < bufs; c++ {
		e.shiftBuf[c] = make([]uint16, e.scanRows*e.planes*e.shiftLen)
		e.encodeShift(pix, c) // black, with valid control bits
		e.buildPlan(c)
		e.buildDescriptors(c)
	}
	e.front, e.back = 0, bufs-1 // single: 0/0; double: front 0, back 1

	return e.startHardware()
}

// present encodes pix into the shift words (gamma applied here); the holds are static
// descriptors, so nothing else changes per frame.
//
// Single-buffered, it re-encodes the one live buffer in place — the refresh may read a
// half-written frame (tear), the documented RAM-for-tear trade. Double-buffered, it encodes the
// idle back buffer and hands off to swapToBack, which switches the GDMA to it at a frame
// boundary so the panel never reads a partial frame.
//
// A brightness change reshapes the lit/blank split, so it rebuilds the plan(s) and chain(s):
// single-buffered via apply (a brief blank); double-buffered by rebuilding both chains, the back
// cleanly and the live front in place (a one-frame glint at worst; brightness changes are
// rare, and setBrightness drops no-op re-assertions).
func (e *engine) present(pix []rgb) error {
	if !e.doubleBuf {
		e.encodeShift(pix, 0)
		if e.brightnessDirty {
			e.buildPlan(0)
			e.brightnessDirty = false
			e.apply(0)
		}
		return nil
	}

	e.encodeShift(pix, e.back)
	if e.brightnessDirty {
		e.buildPlan(e.back)
		e.buildDescriptors(e.back)
		e.buildPlan(e.front)
		e.buildDescriptors(e.front)
		e.brightnessDirty = false
	}
	return e.swapToBack()
}

// swapToBack makes the freshly-encoded back chain live without tearing. It relinks the live
// front chain's tail descriptor to the back chain's head: the GDMA reads a descriptor's .next
// only at the boundary between descriptors, so it follows the relink between frames, never
// mid-frame. It then waits until the GDMA reports completing a full back-chain frame (its EOF
// descriptor is the last consumed), at which point the back buffer is live and the old front is
// idle; it restores the old front's self-loop for its next turn and adopts the swap.
func (e *engine) swapToBack() error {
	// The chain arrays are over-allocated to maxPlanLen; only len(plan) descriptors are linked,
	// so the live tail (the suc_eof descriptor that loops the chain) is at plan length - 1, NOT
	// the last allocated slot.
	fc := descChain[e.front]
	bc := descChain[e.back]
	fcTail := &fc[len(e.plan[e.front])-1]
	bcTailAddr := uint32(uintptr(unsafe.Pointer(&bc[len(e.plan[e.back])-1]))) & descAddrMask

	setNext(fcTail, &bc[0]) // front tail -> back head: GDMA enters back at the boundary

	dma := esp.DMA
	err := waitSwap(func() bool {
		return dma.GetOUT_EOF_DES_ADDR_CH0()&descAddrMask == bcTailAddr
	})

	setNext(fcTail, &fc[0]) // restore the idle old-front chain's self-loop
	e.front, e.back = e.back, e.front
	return err
}

// setBrightness adopts the new brightness and marks the plan dirty: brightness lives in the
// plan's lit/blank split, so it takes effect on the next present, which rebuilds the plan and
// the GDMA chain. BCM color ratios are preserved (every plane's lit fraction scales together).
//
// A no-op change is dropped: rebuilding the plan stops and restarts the continuous GDMA
// outlink (see apply), so doing it every frame — as a caller that re-asserts the same
// brightness each frame would — keeps restarting the scan before the panel finishes a refresh
// cycle, and the display reads as blank. Only an actual change pays that cost.
func (e *engine) setBrightness(b uint8) {
	if b == e.brightness {
		return
	}
	e.brightness = b
	e.brightnessDirty = true
}

// encodeShift writes the per-pixel shift words into e.shift. For each (row, plane) it packs
// that plane's column bits (gamma applied), blanked (OE high), with LAT asserted on the last
// column (so the latch lands with the final shift — see latWords) and a trailing blanked word
// holding the latch. The top half (grid row r) drives bits 0-2, the bottom half (r+scanRows)
// bits 3-5; the row address sits in bits 6-10 of every word.
func (e *engine) encodeShift(pix []rgb, c int) {
	shift := e.shiftBuf[c]
	for r := 0; r < e.scanRows; r++ {
		addr := addrBits(r)
		for k := 0; k < e.planes; k++ {
			i := (r*e.planes + k) * e.shiftLen
			for col := 0; col < e.width; col++ {
				bits := e.planeBits(pix, r, k, col) | addr | bOE
				if col == e.width-1 {
					bits |= bLAT // latch on the last column
				}
				shift[i+col] = bits
			}
			shift[i+e.width] = addr | bOE // trailing latch-hold word (LAT low)
		}
	}
}

// buildPlan rebuilds the circular descriptor plan for the current brightness. Per (row, plane)
// it emits one shift segment (that plane's slice of the shift buffer), then the display block
// as hold runs: the first lit = (baseHold<<k)*brightness/255 words from the row's lit buffer,
// the rest from its blank buffer, each split into holdRun-sized descriptors. The shift words
// carry the pixels; the holds carry the BCM dwell.
func (e *engine) buildPlan(c int) {
	bright := int(e.brightness)
	shift := e.shiftBuf[c]
	plan := e.plan[c][:0]
	for r := 0; r < e.scanRows; r++ {
		litBuf := e.holds[2*r]
		blankBuf := e.holds[2*r+1]
		for k := 0; k < e.planes; k++ {
			off := (r*e.planes + k) * e.shiftLen
			plan = append(plan, seg{shift[off : off+e.shiftLen], e.shiftLen})

			block := e.baseHold << uint(k)
			lit := block * bright / 255
			plan = appendRuns(plan, litBuf, lit)
			plan = appendRuns(plan, blankBuf, block-lit)
		}
	}
	e.plan[c] = plan
}

// planeBits packs bit `k` of each gamma-corrected channel at (row, col) into a word's low
// bits: top half (grid row r) on bits 0-2, bottom half (row r+scanRows) on bits 3-5.
func (e *engine) planeBits(pix []rgb, r, k, col int) uint16 {
	tp := pix[r*e.width+col]
	bp := pix[(r+e.scanRows)*e.width+col]
	ku := uint(k)
	return uint16((e.gamma[tp.R]>>ku)&1)*bR1 |
		uint16((e.gamma[tp.G]>>ku)&1)*bG1 |
		uint16((e.gamma[tp.B]>>ku)&1)*bB1 |
		uint16((e.gamma[bp.R]>>ku)&1)*bR2 |
		uint16((e.gamma[bp.G]>>ku)&1)*bG2 |
		uint16((e.gamma[bp.B]>>ku)&1)*bB2
}

// maxPlanLen is the worst-case number of segments over any brightness: per (row, plane) one
// shift segment plus the hold segments for a full block split into holdRun runs, with the +1
// and the doubling bounding any lit/blank partition (each side rounds up to a partial run).
// The backend sizes its descriptor array to this once, so a brightness rebuild refills in
// place and never reallocates.
func (e *engine) maxPlanLen() int {
	// Per (row, plane): one shift segment, plus the segments the block splits into. The block
	// is partitioned into a lit run and a blank run by brightness, and for a+b = block,
	//
	//	ceil(a/holdRun) + ceil(b/holdRun)  ≤  ceil(block/holdRun) + 1
	//
	// with the bound tight when the split straddles a run boundary. So the +1 below is exactly
	// the partition slack, and holdSegs is already the worst case over every brightness — it
	// must NOT be doubled again (the plan is the dominant RAM term at high holdDutyRatio, so a
	// redundant 2× here is kilobytes of always-resident descriptors).
	holdSegs := 0
	for k := 0; k < e.planes; k++ {
		holdSegs += (e.baseHold<<uint(k)+holdRun-1)/holdRun + 1
	}
	return e.scanRows * (e.planes + holdSegs)
}

// ramBytes estimates the driver's total buffer footprint: the shift words, the shared hold
// buffers, the GDMA descriptors AND the []seg plan they are built from, plus the hold slice
// headers. Used for the ErrTooBig guard.
//
// It is deliberately a slight OVER-estimate — it sizes the plan at maxPlanLen (the
// brightness-independent worst case) while the live plan is usually shorter. Erring high is
// the right direction for a guard.
//
// It is NOT a fit guarantee: it counts this driver only, so an app that also brings up WiFi
// can exhaust the heap well inside this budget. cmd/clock and cmd/bench print the measured
// driver heap at startup — trust that over this estimate, and if the two diverge, this
// function has stopped counting something.
func (e *engine) ramBytes() int {
	bufs := 1
	if e.doubleBuf {
		bufs = 2 // two shift buffers, two descriptor chains, two plans
	}
	shift := e.scanRows * e.planes * e.shiftLen
	holds := 2 * e.scanRows * holdRun

	// Both per-segment structures scale with the plan, and both are real: descChain is the
	// GDMA descriptors (12 B) and e.plan is the []seg they are built from (16 B: a 12-byte
	// slice header plus an int, on 32-bit). Counting only the descriptors would understate
	// the total by roughly a fifth — more at high holdDutyRatio, which is exactly where the
	// guard is supposed to bite.
	perSeg := 12 + int(unsafe.Sizeof(seg{}))
	segs := e.maxPlanLen() * perSeg * bufs

	// The per-row hold slice headers: e.holds is [][]uint16 with 2*scanRows entries.
	headers := 2 * e.scanRows * int(unsafe.Sizeof([]uint16(nil)))

	return (shift*bufs+holds)*2 + segs + headers
}

// buildDescriptors lays chain c's circular GDMA list over e.plan[c]: one descriptor per segment
// (a tiny shift slice or a prefix of a shared hold buffer), owner=DMA, suc_eof on the last, the
// last .next looping back to the first. Each chain's array is sized to maxPlanLen() once, so a
// brightness rebuild refills it in place without reallocating. Every segment is at most
// holdRun (or shiftLen) words — far under the 4092-byte descriptor cap — so none needs
// splitting. The seg data slices live in e.shiftBuf / e.holds, which the live engine keeps alive.
func (e *engine) buildDescriptors(c int) {
	if descChain[c] == nil {
		descChain[c] = make([]dmaDesc, e.maxPlanLen())
	}
	dc := descChain[c]
	plan := e.plan[c]
	n := len(plan)
	for i := 0; i < n; i++ {
		s := plan[i]
		bytes := uint32(s.words * 2)
		dw0 := bytes | bytes<<12 | descOwnerDMA
		if i == n-1 {
			dw0 |= descSucEOF
		}
		dc[i].dw0 = dw0
		dc[i].buffer = uint32(uintptr(unsafe.Pointer(&s.data[0])))
		nxt := &dc[0]
		if i < n-1 {
			nxt = &dc[i+1]
		}
		dc[i].next = uint32(uintptr(unsafe.Pointer(nxt)))
	}
}

// apply rebuilds the GDMA chain after a brightness change reshaped e.plan (the lit/blank
// split moved). It stops the outlink, refills the descriptors in place (the array is pre-sized
// to maxPlanLen, so no realloc), and restarts from the head — a brief blank during the rebuild,
// fine for an occasional brightness change. The LCD keeps running (always_out) and re-fills
// from the new chain. The per-frame shift rewrite needs none of this: those descriptors point
// at the same shift buffer, whose contents present just overwrites.
func (e *engine) apply(c int) {
	dma := esp.DMA
	dma.SetOUT_LINK_CH0_OUTLINK_STOP(1)
	e.buildDescriptors(c)
	dma.SetOUT_LINK_CH0_OUTLINK_ADDR(uint32(uintptr(unsafe.Pointer(&descChain[c][0]))) & 0xfffff)
	dma.SetOUT_LINK_CH0_OUTLINK_START(1)
}

// startHardware ungates LCD_CAM + GDMA, builds the descriptor chain, and begins refreshing,
// returning ErrConfigured if the singleton engine is already claimed.
func (e *engine) startHardware() error {
	if started {
		return ErrConfigured
	}
	started = true

	// Ungate LCD_CAM and GDMA, release their resets.
	sys := esp.SYSTEM
	sys.SetPERIP_CLK_EN1_LCD_CAM_CLK_EN(1)
	sys.SetPERIP_RST_EN1_LCD_CAM_RST(0)
	sys.SetPERIP_CLK_EN1_DMA_CLK_EN(1)
	sys.SetPERIP_RST_EN1_DMA_RST(0)

	routePins()
	configLCDCAM()
	startDMA()
	startLCD()
	return nil
}

// ----------------------------------------------------------------------------
// SCANOUT HARDWARE (LCD_CAM + GDMA bring-up)
// ----------------------------------------------------------------------------
// The engine is a hardware SINGLETON — one LCD_CAM, one GDMA channel, one output bus — so the
// descriptor chains (descChain) are package-level vars: GDMA reads them from fixed addresses and
// they must never be GC'd, and the `started` guard rejects a second start with ErrConfigured.
// Per-frame pixel state lives on the Device's grid.

// --- Tunables ---
const (
	// pixClkDiv divides the 160 MHz module clock to LCD_PCLK: /16 = 10 MHz.
	pixClkDiv = 16

	// ckOutEdge selects which PCLK edge the panel samples data on (0 or 1). HUB75 latches on
	// CLK's rising edge; if columns appear shifted by one, flip this on hardware.
	ckOutEdge = 0

	// periSelLCD is the GDMA peripheral-select value for LCD_CAM (SOC_GDMA_TRIG_PERIPH_LCD0).
	periSelLCD = 5
)

// --- GPIO-matrix signal indices (ESP32-S3, from soc/gpio_sig_map.h) ---
const (
	sigLCDData0 = 133 // LCD_DATA_OUT0_IDX; OUT1..15 are consecutive
	sigLCDPCLK  = 154 // LCD_PCLK_IDX
)

// clkGPIO is the HUB75 CLK pin; LCD_PCLK routes here.
const clkGPIO = 2

// busGPIO maps each logical bus bit (LCD_DATA_OUTi, i == the bit position used by the
// encoder in lcd.go) to the Adafruit MatrixPortal S3's HUB75 GPIO. Index = logical bit:
// 0-5 RGB, 6-10 address A-E, 11 LAT, 12 OE. The matrix lets these be any GPIOs, so the
// encoder stays board-agnostic and only this table knows the wiring.
var busGPIO = [13]uint8{
	42, 41, 40, // R1 G1 B1
	38, 39, 37, // R2 G2 B2
	45, 36, 48, 35, 21, // A B C D E
	47, // LAT
	14, // OE
}

// dmaDesc is the GDMA linked-list descriptor (dma_descriptor_t). dw0 packs
// size:12 | length:12 | reserved:4 | err_eof:1 | reserved:1 | suc_eof:1 | owner:1.
// buffer and next are physical addresses (internal SRAM, where the Go heap lives).
type dmaDesc struct {
	dw0    uint32
	buffer uint32
	next   uint32
}

const (
	descOwnerDMA = 1 << 31
	descSucEOF   = 1 << 30
)

var (
	started   bool
	descChain [2][]dmaDesc // live GDMA chains; package vars so never GC'd. [1] only when doubleBuf.
)

// descAddrMask is the low-20-bit window the GDMA outlink uses to address descriptors in
// internal SRAM (512 KB fits in 19 bits), so those bits identify a descriptor uniquely — enough
// to compare OUT_EOF_DES_ADDR against a known chain tail in swapToBack.
const descAddrMask = 0xfffff

// setNext points descriptor d's GDMA link at target (the descriptor's address).
func setNext(d, target *dmaDesc) {
	d.next = uint32(uintptr(unsafe.Pointer(target)))
}

// routeOut connects an LCD output signal to a GPIO through the matrix: the pad is made a
// GPIO output (IO_MUX + output-enable via machine), then its FUNCn_OUT_SEL_CFG is pointed at
// `signal`. oenSel (bit 10) forces the pad's output-enable from GPIO_ENABLE (set by the
// Configure above) rather than the peripheral, so the line is always driven.
func routeOut(gpio uint8, signal int) {
	machine.Pin(gpio).Configure(machine.PinConfig{Mode: machine.PinOutput})
	const oenSel = 1 << 10
	reg := (*volatile.Register32)(unsafe.Add(unsafe.Pointer(&esp.GPIO.FUNC0_OUT_SEL_CFG), uintptr(gpio)*4))
	reg.Set(uint32(signal) | oenSel)
}

func routePins() {
	for bit, gpio := range busGPIO {
		routeOut(gpio, sigLCDData0+bit)
	}
	routeOut(clkGPIO, sigLCDPCLK)
}

func configLCDCAM() {
	lc := esp.LCD_CAM

	lc.SetLCD_USER_LCD_RESET(1) // self-clearing peripheral reset

	// Clock: PLL_F160M / pixClkDiv, PCLK == module clock (equ_sysclk).
	lc.SetLCD_CLOCK_CLK_EN(1)
	lc.SetLCD_CLOCK_LCD_CLK_SEL(3) // PLL_F160M
	lc.SetLCD_CLOCK_LCD_CLKM_DIV_NUM(pixClkDiv)
	lc.SetLCD_CLOCK_LCD_CLK_EQU_SYSCLK(1)
	lc.SetLCD_CLOCK_LCD_CLKCNT_N(1)
	lc.SetLCD_CLOCK_LCD_CK_OUT_EDGE(ckOutEdge)
	lc.SetLCD_CLOCK_LCD_CK_IDLE_EDGE(0)

	// i8080 parallel, 16-bit bus, DMA-length-driven continuous output.
	lc.SetLCD_CTRL_LCD_RGB_MODE_EN(0)
	lc.SetLCD_USER_LCD_2BYTE_EN(1)
	lc.SetLCD_USER_LCD_DOUT(1)
	lc.SetLCD_USER_LCD_ALWAYS_OUT_EN(1)
	lc.SetLCD_MISC_LCD_BK_EN(1)       // backpressure: stall the FIFO on underrun, don't corrupt
	lc.SetLCD_MISC_LCD_AFIFO_RESET(1) // self-clearing TX-FIFO reset
}

func startDMA() {
	dma := esp.DMA
	dma.SetOUT_CONF0_CH0_OUT_RST(1) // reset pulse
	dma.SetOUT_CONF0_CH0_OUT_RST(0)
	dma.SetOUT_CONF0_CH0_OUT_EOF_MODE(1)
	dma.SetOUT_CONF1_CH0_OUT_CHECK_OWNER(0) // never stop on a consumed descriptor -> loops forever
	dma.SetOUT_PERI_SEL_CH0_PERI_OUT_SEL(periSelLCD)
	dma.SetOUT_LINK_CH0_OUTLINK_ADDR(uint32(uintptr(unsafe.Pointer(&descChain[0][0]))) & 0xfffff)
	dma.SetOUT_LINK_CH0_OUTLINK_START(1)
}

func startLCD() {
	// Let GDMA prefill the TX FIFO before the peripheral starts clocking.
	for i := 0; i < 100000; i++ {
		volatile.LoadUint32((*uint32)(unsafe.Pointer(&descChain[0][0].dw0)))
	}
	lc := esp.LCD_CAM
	lc.SetLCD_USER_LCD_UPDATE(1)
	lc.SetLCD_USER_LCD_START(1)
}

// maxBufferBytes caps the driver's total buffer footprint (shift words + shared hold buffers
// + descriptor plan; see engine.ramBytes) so an absurd plane dwell fails with ErrTooBig instead
// of OOM-ing to a blank panel. The ESP32-S3 has 512 KB internal SRAM.
//
// It is a backstop against the ridiculous, NOT a guarantee that a passing config fits. It
// accounts only for the driver: an app that also brings up WiFi (the clock does) can exhaust
// the heap well under this number. Observed: 128×64×6 at holdDutyRatio 9 needs ~146 KB, passes
// this guard comfortably, and still OOMs the clock. If a config passes here and dies anyway,
// the driver is not the only claimant — lower holdDutyRatio or ColorDepth.
const maxBufferBytes = 256 * 1024

// ─── TUNING ─── lcd's only timing knob. Change it here and re-flash; nothing in Config
// controls timing. (The pixel clock is pixClkDiv, in the Tunables const block above.)
//
// holdDutyRatio trades brightness against refresh. Shifting a row's pixels out is dead time —
// the panel is blanked for those words — and this ratio sets how long the LIT hold runs as a
// multiple of that shift time:
//
//	duty    ≈ ratio/(ratio+1)
//	refresh ≈ pclk / (scanRows · planes · (width+latWords) · (1+ratio))
//
// autoBaseHold works backwards from the first line: plane 0 holds for ratio×shift/(2^planes-1)
// words and plane k holds that <<k, so the BCM weights sum back to ratio× the shift time. That
// division floors, so real duty lands a shade under the ideal — 49.5%, not 50%, at 64×32×8.
//
// At 8 planes and the 10 MHz pixel clock (approximate; the floor above moves these by ~1%):
//
//	ratio   duty    64×32     128×64
//	  1     50%     601 Hz    151 Hz
//	  2     67%     401 Hz    101 Hz
//	  3     75%     300 Hz     76 Hz
//	  4     80%     240 Hz     61 Hz
//	  6     86%     172 Hz     43 Hz
//	  8     89%     134 Hz     34 Hz
//
// Higher = brighter / lower refresh; lower = dimmer / faster. Geometry decides how much room
// there is to spend: 64×32 has refresh to spare anywhere in that range, while 128×64 runs out —
// ratio 4 puts it near 60 Hz, close enough to flicker to show on camera, and worse at deeper
// color.
//
// RAM IS THE HARDER LIMIT, and on a big panel it bites well before refresh does. Descriptor
// holds make a hold beat about ten times cheaper than materializing it as a word (12 bytes per
// holdRun=64 beats, against 2 bytes a beat) — cheaper, NOT free, and still linear in the ratio.
// baseHold scales with it, so BOTH per-segment structures grow ceil((baseHold<<k)/holdRun) per
// plane: the descChain descriptors (12 B each) and the e.plan []seg they are built from (16 B
// each, and append-grown — see maxPlanLen).
//
// Computed for 128×64×6 single-buffered at brightness 200 (not measured — derived from the
// sizing above):
//
//	ratio 1: descChain ~17 KB + plan ~13 KB + buffers ~56 KB ≈ 87 KB
//	ratio 9: descChain ~89 KB + plan ~59 KB + buffers ~56 KB ≈ 206 KB
//
// On the S3's 512 KB that is the difference between fitting alongside WiFi and not: ratio 9
// OOMs the clock on that panel even though it passes the maxBufferBytes guard — the guard is
// sized for the driver alone.
//
// YOU PROBABLY NEED LESS THAN YOU THINK — the duty column reads far scarier than it looks.
// Luminance is linear in duty but the eye is not (perceived lightness goes about as the cube
// root), and these panels are built for daylight, so indoors they are well past the point where
// more duty reads as anything but harder to look at. Dropping 4 → 2 is a ~6% perceptual change;
// 4 → 1 is ~15%, two thirds of a stop — and buys 2.5× the refresh, a big cut in plan RAM, and
// lower average LED current, which is margin on a sagging 5 V rail (a real failure mode on USB
// power here; see the Hardware notes in README.md). Start low and raise it only if the panel
// actually looks dim in the room it lives in.
//
// The one place a low ratio bites is software dimming. autoBaseHold's floor makes plane 0's
// window small — 2 words at 64×32×8 ratio 1, against 8 at ratio 4 — and SetBrightness computes
// lit = block·bright/255, so below ~50% brightness plane 0 rounds to zero and the LSB drops
// out, banding the dark tones. Irrelevant at the default full brightness; worth knowing if you
// dim in software AND run a low ratio.
//
// This is NOT the brightness control. SetBrightness splits each plane's window into lit and
// blanked runs (see buildPlan), dimming without changing the frame length — so brightness
// leaves refresh alone, and this knob does not.
const holdDutyRatio = 1

// autoBaseHold returns the plane-0 display length (words) for holdDutyRatio: lit words ≈ ratio×
// the blanked shift+latch time. plane k displays the result <<k.
func autoBaseHold(width, planes int) int {
	shiftLatchWords := (width + latWords) * planes
	h := holdDutyRatio * shiftLatchWords / ((1 << uint(planes)) - 1)
	if h < 1 {
		h = 1
	}
	return h
}

// appendRuns appends ceil(count/holdRun) segments that emit `count` identical words from buf
// (every word in buf is the same, so a final partial run just reads a prefix). count 0 adds
// nothing.
func appendRuns(plan []seg, buf []uint16, count int) []seg {
	for count > 0 {
		w := count
		if w > holdRun {
			w = holdRun
		}
		plan = append(plan, seg{buf, w})
		count -= w
	}
	return plan
}

// addrBits returns the row-select bits (A..E on LCD_DATA_OUT6..10) for row pair r.
func addrBits(r int) uint16 {
	var a uint16
	if r&1 != 0 {
		a |= 1 << 6
	}
	if r&2 != 0 {
		a |= 1 << 7
	}
	if r&4 != 0 {
		a |= 1 << 8
	}
	if r&8 != 0 {
		a |= 1 << 9
	}
	if r&16 != 0 {
		a |= 1 << 10
	}
	return a
}

// DefaultConfig lives in hub75.go (shared across engines). lcd honors DoubleBuffer: false is
// single-buffered (less RAM, can tear); true runs two shift buffers + GDMA chains and swaps
// tear-free at a frame boundary (see present/swapToBack). Timing (plane dwell, pixel clock) is
// internal — see the TUNING const above (holdDutyRatio) and the Tunables block (pixClkDiv).

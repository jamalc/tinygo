//go:build atsamd51

// Shared SAMD51 substrate for the Matrix Portal M4's two DMA HUB75 engines — evsys (default,
// hub75_samd51_evsys.go) and isr (opt-in with -tags=atsamd51_isr, hub75_samd51_isr.go) —
// exactly one of which compiles alongside this file. They differ in how they schedule the
// refresh (descriptor topology, timers, EVSYS vs a per-window ISR), but they agree on the
// wiring and the DMAC descriptor layout, stated once here.
package hub75

import (
	"machine"
	"unsafe"
)

// descriptor is the SAMD51 DMAC transfer descriptor (DMAC §22.8.2): a fixed 16-byte block
// the DMAC reads from a stable, 16-byte-aligned address. The engines build chains of these;
// the field layout is dictated by hardware, so the struct is exactly the register block — the
// `//go:align 16` lives on the pool variables, not the type.
type descriptor struct {
	Btctrl   uint16
	Btcnt    uint16
	Srcaddr  uint32 // END address for incrementing sources (SAMD51 rule)
	Dstaddr  uint32
	Descaddr uint32 // next descriptor; a self-pointing link closes a loop
}

// BTCTRL bits the engines set (DMAC BTCTRL register). device/sam exposes no BTCTRL bitfield
// constants, so the bit positions are spelled out here once. Not every engine uses every bit
// (btWord/btEvtOut are evsys-only), but the register's layout is one fact in one place.
const (
	btValid  uint16 = 1 << 0  // VALID: descriptor is valid
	btEvtOut uint16 = 1 << 1  // EVOSEL=BLOCK: emit a peripheral event on block-transfer complete
	btHWord  uint16 = 1 << 8  // BEATSIZE=HWORD (16-bit beats)
	btWord   uint16 = 2 << 8  // BEATSIZE=WORD (32-bit beats)
	btSrcInc uint16 = 1 << 10 // SRCINC: source address increments per beat
)

// hub75Pins is the Matrix Portal M4 HUB75 connector mapped onto PORTB bits 0-15:
//
//	PB00-05 R1 G1 B1 R2 G2 B2   PB06 CLK   PB07 ADDR_A
//	PB08 ADDR_B   PB09 ADDR_C   PB12 OE    PB13 ADDR_E
//	PB14 LAT      PB15 ADDR_D
//
// PB10/PB11 are the QSPI flash pins (peripheral-muxed), so DMA halfword writes to OUT bits
// 10-11 never reach their pads.
var hub75Pins = []machine.Pin{
	machine.PB00, machine.PB01, machine.PB02, // R1 G1 B1
	machine.PB03, machine.PB04, machine.PB05, // R2 G2 B2
	machine.PB06,                                           // CLK
	machine.PB07, machine.PB08, machine.PB09, machine.PB15, // ADDR A B C D
	machine.PB13,               // ADDR E
	machine.PB12, machine.PB14, // OE, LAT
}

// configurePins drives every HUB75 pin as an output, low, then parks OE (PB12) high so the
// panel stays blanked until the engine's schedule issues its first unblank. (evsys later
// re-muxes OE/PB12 to TCC3 in configureOETimer; driving it as GPIO here first is harmless.)
func configurePins() {
	for _, p := range hub75Pins {
		p.Configure(machine.PinConfig{Mode: machine.PinOutput})
		p.Low()
	}
	machine.PB12.High()
}

// samdMaxScanRows is the M4's own scan-row ceiling, tighter than the package-wide maxScanRows
// (32). The evsys engine's DMAC descriptor pool is a static, package-level array sized to this
// bound (see descPool in hub75_samd51_evsys.go); raising it to the full 32 would double that
// pool for +16 KB of always-resident RAM on a 192 KB chip. Both DMA engines reject a taller
// panel with ErrTooManyRows in start, so the two agree on the M4's geometry envelope even
// though only evsys is structurally bound by it. The dynamic-buffer drivers (esp32s3, pio, lcd)
// are not subject to this and take 64-tall panels up to their RAM budget.
const samdMaxScanRows = 16

func ptr(p unsafe.Pointer) uint32 { return uint32(uintptr(p)) }

// waitSync bounds a register-sync spin (the driver-design "all loops have fixed bounds" rule);
// a stuck peripheral returns ErrTimeout instead of hanging the program. Shared by both SAMD51
// engines.
func waitSync(ready func() bool) error {
	for i := 0; i < 1_000_000; i++ {
		if ready() {
			return nil
		}
	}
	return ErrTimeout
}

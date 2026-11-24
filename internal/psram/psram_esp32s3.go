//go:build esp32s3

// Package psram brings up the ESP32-S3's external quad-SPI PSRAM, which TinyGo leaves
// completely dark: the runtime's linker script maps only the 512 KB of internal SRAM and the
// (replaced) ROM bootloader never runs the SPIRAM init, so the 2 MB on the MatrixPortal S3 is
// physically wired but un-clocked, un-mapped, and invisible to the CPU. This package is the
// from-scratch bring-up — the work the ESP-IDF 2nd-stage bootloader would normally do — in
// five pieces (each hardware-verified via cmd/psramprobe; README.md in this package covers
// what the mapped 2 MB is for):
//
//	detect    — issue the chip's read-ID (0x9F) over the MSPI manual controller and check
//	            the Known-Good-Die marker. Proves the chip is alive and the command path works.
//	cache     — program the SPI0 cache controller (single-line read 0x0B / write 0x02) and the
//	            separate SRAM clock so cache accesses to the window become PSRAM transactions.
//	            (Single-line keeps the device in serial SPI so it can't strand; QPI is a later
//	            speed pass.)
//	map       — point an MMU entry at PSRAM physical 0 → vaddr 0x3D000000 and enable the cache bus.
//	alloc     — a bump allocator handing out []byte over the mapped window (NOT GC heap).
//	coherency — a Writeback(addr,len) over the EXTMEM DCACHE sync engine so GDMA, which reads
//	            physical PSRAM and bypasses the CPU data cache, sees freshly-encoded frames.
//
// THE BUS. The MSPI has two controllers on the same pins, shared with flash: SPI0 is the
// cache/auto controller (a cache miss in the mapped window becomes a PSRAM burst), SPI1 is the
// manual command controller (we drive it directly to issue init commands). PSRAM sits on chip-
// select 1 (flash is CS0).
//
// WHY NOT CALL ROM. ESP-IDF leans on ROM helpers (esp_rom_spi_cmd_config/start) for the manual
// path. Calling a fixed ROM address from TinyGo means matching the Xtensa windowed-call ABI by
// hand — a gamble that fails silently. Every MSPI register field we need is already named in
// TinyGo's device/esp package, so we reimplement the command engine directly: debuggable, and
// no ABI guesswork.
package psram

import (
	"device/esp"
	"runtime/volatile"
	"unsafe"
)

// PSRAM command opcodes (APMemory APS1604-class quad PSRAM).
const (
	cmdReadID = 0x9F // read 24-bit identification (MFID, KGD, then density/EID bits)

	// Single-line bring-up commands: the device stays in its power-on serial SPI mode, so it can
	// never strand in QPI (which a CPU reset can't clear — only a power cycle can).
	cmdReadFast = 0x0B // serial fast read; 8 dummy cycles (the cache controller's read command)
	cmdWrite    = 0x02 // serial write; 0 dummy (the cache controller's write command)

	// QPI commands — for a later speed pass, once the serial path is proven.
	cmdEnterQPI  = 0x35 // switch the device from serial SPI into QPI (4-line) mode
	cmdReadQuad  = 0xEB // QPI fast read; 6 dummy cycles
	cmdWriteQuad = 0x38 // QPI write; 0 dummy

	cmdExitQPI     = 0xF5 // leave QPI, return to serial SPI (sent AS a 4-line command)
	cmdResetEnable = 0x66 // reset-enable; must precede cmdReset
	cmdReset       = 0x99 // reset device to power-on defaults (serial SPI)
)

const readFastDummy = 8 // dummy cycles after a 0x0B serial fast read, before data

// syncSpinCap bounds the cache-sync completion poll so a misconfigured engine reports (as a
// failed round-trip) instead of hanging the core. A real line-range sync completes in well under
// this many reads; the cap is deliberately generous.
const syncSpinCap = 1 << 20

// kgdGood is the Known-Good-Die marker in ID byte 1: a chip that passed die test reports 0x5D
// there. Anything else (or all-0x00 / all-0xFF) means we are not actually talking to the PSRAM
// — almost always a chip-select routing problem (see cs1GPIO).
const kgdGood = 0x5D

// --- MSPI chip-select routing (the detect-time risk) ---
//
// PSRAM shares every MSPI line with flash except its own chip select (CS1). Flash is already
// running (we execute XIP from it), so the data/clock pads are live; the one thing we must wire
// is SPICS1 → the PSRAM CS pad. For the ESP32-S3's in-package quad PSRAM that pad is GPIO26,
// driven by routing the SPICS1 output signal to it through the GPIO matrix.
//
// This is the single most likely thing to need correcting on first flash: if Detect() comes
// back all-0x00 or all-0xFF, the CS is not reaching the chip. The robust fix is to read the
// pad from eFuse (EFUSE.SPI_PAD_CONFIG_CS1) the way ESP-IDF does; GPIO26 is the standard
// default and the right first guess.
const (
	cs1GPIO       = 26 // ESP32-S3 in-package PSRAM chip-select pad
	sigSPICS1Out  = 6  // SPICS1_OUT_IDX in the S3 GPIO matrix (SPIQ=0,D=1,HD=2,WP=3,CLK=4,CS0=5,CS1=6)
	gpioMatrixOEn = 1 << 10
)

// ID is the decoded PSRAM identification Detect reads. The chip shifts the ID out MSB-first as
// MFID, KGD, then the extended-id/density byte, so in the little-endian Raw word MFID is [7:0],
// KGD is [15:8], and EID is [23:16].
type ID struct {
	Raw      uint32 // the 24 ID bits: MFID in [7:0], KGD in [15:8], EID/density in [23:16]
	MFID     uint8  // manufacturer id (0x0D = AP Memory on the MatrixPortal S3)
	KGD      uint8  // known-good-die marker; must be kgdGood
	EID      uint8  // extended-id byte; density lives in its high bits
	ValidKGD bool   // KGD == kgdGood: we are really talking to the chip
}

// Detect routes the PSRAM chip-select, issues the read-ID command on the SPI1 manual
// controller, and decodes the result. It does NOT enable QPI or map anything — it is a pure
// liveness check. A returned ID with ValidKGD true means the chip answered correctly.
func Detect() ID {
	routeCS1()

	// Try a plain single-line read first. On a cold chip (fresh power-up = serial SPI) this just
	// works, and the chip never sees a 4-line command that could disturb it. Only if it fails do
	// we assume the chip was left in some non-serial mode by a prior run — a CPU reset does NOT
	// reset the continuously-powered PSRAM — and force it back to serial SPI before retrying.
	if id := readID(); id.ValidKGD {
		return id
	}
	resetDevice()
	return readID()
}

// resetDevice forces the PSRAM back to power-on (serial SPI) state from an UNKNOWN mode. A prior
// run may have left it in QPI, and reset-enable(0x66)+reset(0x99) must be issued in the device's
// CURRENT line width — which we don't know. So we send the pair as 4-line commands first (which
// a QPI device understands; after 0x99 it is back in serial SPI), then again as 1-line commands
// (a clean reset for a device that was already serial). A short recovery delay follows 0x99.
func resetDevice() {
	for _, quad := range [...]bool{true, false} {
		spi1CmdLines(quad, cmdResetEnable, 8, 0, 0, 0, nil, nil)
		spi1CmdLines(quad, cmdReset, 8, 0, 0, 0, nil, nil)
	}
	// tRST is sub-microsecond; spin a little to be safe before the next command.
	for i := 0; i < 50000; i++ {
		esp.SPI1.CMD.Get()
	}
}

// readID issues the 0x9F identification command on the single-line SPI1 path and decodes the
// 24-bit reply. The chip shifts MFID, KGD, then the density byte, MSB-first.
func readID() ID {
	var rx [4]byte
	// cmd 0x9F (8 bits), then a 24-bit address phase of zero, no dummy, read 24 bits back.
	spi1Cmd(cmdReadID, 8, 0, 24, 0, nil, rx[:3])

	raw := uint32(rx[0]) | uint32(rx[1])<<8 | uint32(rx[2])<<16
	id := ID{
		Raw:  raw,
		MFID: rx[0], // shifted out first -> Raw[7:0]   (0x0D = AP Memory)
		KGD:  rx[1], // -> Raw[15:8]
		EID:  rx[2], // -> Raw[23:16] (density bits)
	}
	id.ValidKGD = id.KGD == kgdGood
	return id
}

// ErrNotDetected is returned by Enable when the read-ID handshake fails — the chip did not
// answer with a good KGD, so there is no point mapping anything.
var ErrNotDetected = errString("psram: chip not detected (read-id KGD mismatch)")

type errString string

func (e errString) Error() string { return string(e) }

// --- Address window ---
//
// PSRAM is mapped into the ESP32-S3's external-RAM data window [0x3C000000, 0x3E000000), the
// SAME 32 MB window the cached flash rodata (DROM) lives in — the per-page MMU target bit is
// what splits flash pages from PSRAM pages. TinyGo's flash code+rodata sit at the bottom of the
// window, so we map PSRAM high, at 0x3D000000 (16 MB up, MMU entry 256), which no realistic
// program's rodata reaches. That keeps the map LIVE-safe: we never rewrite an entry the running
// code is fetching through.
const (
	psramBase = 0x3D000000  // CPU-visible base of the mapped PSRAM
	psramSize = 2 << 20     // MatrixPortal S3 carries 2 MB (16 Mbit) quad PSRAM
	mmuTable  = 0x600C5000  // base of the 512-entry MMU page table (flat uint32 array)
	mmuVAddrMask = 0x1FFFFFF // 25-bit offset within the 32 MB window
	mmuSPIRAM = 1 << 15     // entry target bit: this page is PSRAM, not flash (VALID == bit-clear)
	pageSize  = 64 << 10    // MMU page granularity
)

// Region is a CPU-addressable view of the mapped PSRAM. It is NOT part of the GC heap — the
// runtime knows nothing about it; callers carve it up themselves (see Allocator).
type Region struct {
	Addr uintptr
	Size int
}

// Bytes returns the region as a byte slice aliasing the mapped window. Writes through it land in
// the data cache; call Writeback before a DMA engine (which bypasses the cache) reads them.
func (r Region) Bytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(r.Addr)), r.Size)
}

// Allocator returns a bump allocator over the region, for carving the PSRAM into long-lived
// buffers. The PSRAM is not GC heap, so these spans are never collected (see alloc.go).
func (r Region) Allocator() *Allocator { return NewAllocator(r.Addr, r.Size) }

// Enable brings the PSRAM all the way up and returns a Region over it: it detects the chip,
// programs the SPI0 cache controller to auto-generate single-line PSRAM read/write transactions,
// then maps it into the data window and invalidates the range. It runs entirely live
// — it never disables the cache, which would hang a flash-XIP program — by touching only
// PSRAM-specific controller registers and a high, unused MMU entry. The device is left in its
// power-on serial SPI mode (no QPI), so it never strands across reboots.
func Enable() (Region, error) {
	id := Detect()
	if !id.ValidKGD {
		return Region{}, ErrNotDetected
	}
	configCachePhases()
	mapRegion(psramBase, 0, psramSize)
	r := Region{Addr: psramBase, Size: psramSize}
	Invalidate(r.Addr, r.Size) // drop any stale cache lines over the freshly-mapped range
	return r, nil
}

// configCachePhases programs the SPI0 cache/auto controller so a cache miss or writeback in the
// mapped window becomes a SINGLE-LINE PSRAM transaction: read = 0x0B + 24-bit address + 8 dummy,
// write = 0x02 + 24-bit address + 0 dummy, all on one data line. This keeps the device in serial
// SPI (no QPI strand). These are the PSRAM-specific (SRAM_*) registers — distinct from the flash
// read path — so writing them does not disturb flash XIP. (QPI is a later speed pass.)
func configCachePhases() {
	s := esp.SPI0

	// Clock the SRAM (PSRAM) path. It has its OWN divider, separate from flash, and at reset it
	// produces no usable clock — so the cache controller's bursts never clock data and the chip
	// drives nothing back (reads come up all-ones). SRAM_CLK shares the flash CLOCK register's
	// layout (SCLKCNT_L/H/N, SCLK_EQU_SYSCLK), so copying the flash clock runs PSRAM at the same
	// rate — safe here since the in-package PSRAM shares the flash's short MSPI traces.
	s.SRAM_CLK.Set(s.CLOCK.Get())

	// Single-line SRAM mode (neither DIO nor QIO).
	s.CACHE_SCTRL.ClearBits(esp.SPI0_CACHE_SCTRL_USR_SRAM_DIO | esp.SPI0_CACHE_SCTRL_USR_SRAM_QIO)

	// Use our explicit read & write commands.
	s.CACHE_SCTRL.SetBits(esp.SPI0_CACHE_SCTRL_CACHE_SRAM_USR_RCMD | esp.SPI0_CACHE_SCTRL_CACHE_SRAM_USR_WCMD)
	s.SRAM_DWR_CMD.Set(uint32(cmdWrite) | 7<<esp.SPI0_SRAM_DWR_CMD_CACHE_SRAM_USR_WR_CMD_BITLEN_Pos)
	s.SRAM_DRD_CMD.Set(uint32(cmdReadFast) | 7<<esp.SPI0_SRAM_DRD_CMD_CACHE_SRAM_USR_RD_CMD_BITLEN_Pos)

	// 24-bit address, 8 read-dummy cycles (0x0B fast read).
	sctrl := s.CACHE_SCTRL.Get()
	sctrl = (sctrl &^ esp.SPI0_CACHE_SCTRL_SRAM_ADDR_BITLEN_Msk) | 23<<esp.SPI0_CACHE_SCTRL_SRAM_ADDR_BITLEN_Pos
	sctrl |= esp.SPI0_CACHE_SCTRL_USR_RD_SRAM_DUMMY
	sctrl = (sctrl &^ esp.SPI0_CACHE_SCTRL_SRAM_RDUMMY_CYCLELEN_Msk) | uint32(readFastDummy-1)<<esp.SPI0_CACHE_SCTRL_SRAM_RDUMMY_CYCLELEN_Pos
	s.CACHE_SCTRL.Set(sctrl)

	// Enable chip-select 1 (PSRAM) for the cache controller. The device package doesn't name
	// SPI0's CS-disable bits, but they sit where SPI1's do — CS1_DIS is bit 1.
	const cs1Dis = 1 << 1
	s.MISC.ClearBits(cs1Dis)
}

// mapRegion points MMU entries at PSRAM physical paddr → CPU vaddr for `size` bytes, one 64 KB
// page per entry. Each entry is (physical page number) | the PSRAM target bit; VALID is the
// bit-clear state, so that is all that's written. The entries are high in the table (vaddr
// 0x3D000000 → entry 256), disjoint from the flash entries the running code uses.
func mapRegion(vaddr, paddr uintptr, size int) {
	entry := (vaddr & mmuVAddrMask) >> 16
	page := paddr >> 16
	for off := 0; off < size; off += pageSize {
		reg := (*volatile.Register32)(unsafe.Pointer(mmuTable + entry*4))
		reg.Set(uint32(page) | mmuSPIRAM)
		entry++
		page++
	}
	// Make sure the data-cache bus is not shut for either core (it isn't, since flash rodata
	// reads through it — but be explicit).
	esp.EXTMEM.DCACHE_CTRL1.ClearBits(esp.EXTMEM_DCACHE_CTRL1_DCACHE_SHUT_CORE0_BUS | esp.EXTMEM_DCACHE_CTRL1_DCACHE_SHUT_CORE1_BUS)
}

// Writeback flushes the data cache for [addr, addr+size) out to physical PSRAM, so a DMA engine
// — which reads PSRAM directly and never sees the CPU's write cache — gets the freshly-written
// bytes. Address and size are rounded to the 16-byte cache line the sync engine works in. This
// is the coherency primitive DMA users must call after encoding into PSRAM.
func Writeback(addr uintptr, size int) { cacheSync(addr, size, esp.EXTMEM_DCACHE_SYNC_CTRL_DCACHE_WRITEBACK_ENA) }

// Invalidate drops the cached copy of [addr, addr+size) so the next CPU read re-fetches from
// physical PSRAM — needed after a DMA engine writes into PSRAM behind the cache's back.
func Invalidate(addr uintptr, size int) { cacheSync(addr, size, esp.EXTMEM_DCACHE_SYNC_CTRL_DCACHE_INVALIDATE_ENA) }

// cacheSync drives the EXTMEM DCACHE sync engine: program the (line-aligned) range, trigger the
// requested op, and spin until SYNC_DONE. It is an APB-register operation, safe to run from
// flash.
func cacheSync(addr uintptr, size int, op uint32) {
	const line = 16
	end := (addr + uintptr(size) + line - 1) &^ (line - 1)
	start := addr &^ (line - 1)

	e := esp.EXTMEM
	e.DCACHE_SYNC_ADDR.Set(uint32(start))
	e.DCACHE_SYNC_SIZE.Set(uint32(end - start))
	// Write exactly the op bit (not OR): clears any stale ENA/SYNC_DONE from a prior call so the
	// poll below waits on THIS op, not a leftover completion flag.
	e.DCACHE_SYNC_CTRL.Set(op)
	// Bounded spin (project convention: never hang on a register wait). A real sync is far under
	// this cap; exceeding it means a misconfig, and best-effort return surfaces as a round-trip
	// mismatch rather than a dead-looking hang.
	for i := 0; i < syncSpinCap; i++ {
		if e.DCACHE_SYNC_CTRL.Get()&esp.EXTMEM_DCACHE_SYNC_CTRL_DCACHE_SYNC_DONE != 0 {
			return
		}
	}
}

// routeCS1 connects the SPICS1 output signal to the PSRAM chip-select pad through the GPIO
// matrix, forcing the pad's output-enable on (oenSel) so the line is always driven. Mirrors the
// routeOut pattern the hub75 esp32s3 backend uses for the LCD bus.
func routeCS1() {
	// Drive-enable the pad (write-1-to-set; cs1GPIO < 32 so it lives in the low ENABLE bank).
	esp.GPIO.ENABLE_W1TS.Set(1 << (cs1GPIO % 32))
	// Point the pad's output mux at the SPICS1 signal.
	reg := (*volatile.Register32)(unsafe.Add(unsafe.Pointer(&esp.GPIO.FUNC0_OUT_SEL_CFG), uintptr(cs1GPIO)*4))
	reg.Set(uint32(sigSPICS1Out) | gpioMatrixOEn)
}

// spi1Cmd issues one manual MSPI transaction on the SPI1 controller against PSRAM (CS1): an
// optional command phase, address phase, dummy cycles, MOSI write, and MISO read. Bit lengths
// are in bits; addr is right-justified and the controller takes the high addrBits. It blocks on
// the USR trigger and copies any read data out of the W0.. data registers.
//
// This is the hand-reimplementation of ESP-IDF's esp_rom_spi_cmd_config + _start for the one
// shape Detect needs. It does not save/restore the flash USER registers (Detect runs once at
// boot before any other MSPI user); a caller that interleaves with flash should back them up.
func spi1Cmd(cmd uint16, cmdBits int, addr uint32, addrBits, dummyBits int, mosi, miso []byte) {
	spi1CmdLines(false, cmd, cmdBits, addr, addrBits, dummyBits, mosi, miso)
}

// spi1CmdLines is spi1Cmd with control over the command-phase line width: quadCmd=false sends
// the command on one line (for a device in serial SPI), quadCmd=true sends it on four (for a
// device already in QPI — only exitQPI needs this). The address/data phases are single-line
// either way, which is all the init commands require.
func spi1CmdLines(quadCmd bool, cmd uint16, cmdBits int, addr uint32, addrBits, dummyBits int, mosi, miso []byte) {
	s := esp.SPI1

	// CTRL line mode. Two cases, both restored before returning:
	//   - single-line (quadCmd=false): clear every dual/quad/dio/qio bit so command+address+read
	//     go out on one line. The bootloader left SPI1 in QIO for flash; without clearing, the
	//     phases go multi-line and a serial-SPI PSRAM sees garbage. (USER's FWRITE_* bits are
	//     already clear since we rebuild USER from zero below.)
	//   - quad command (quadCmd=true): the device is in QPI and only understands 4-line commands,
	//     so keep flash's QIO read config and ADD FCMD_QUAD to put the command phase on 4 lines
	//     too — matching ESP-IDF's "QIO mode + FCMD_QUAD" for exit-QPI.
	ctrl0 := s.CTRL.Get()
	clean := ctrl0 &^ (esp.SPI1_CTRL_FCMD_DUAL | esp.SPI1_CTRL_FCMD_QUAD |
		esp.SPI1_CTRL_FREAD_DUAL | esp.SPI1_CTRL_FREAD_QUAD |
		esp.SPI1_CTRL_FREAD_DIO | esp.SPI1_CTRL_FREAD_QIO)
	if quadCmd {
		clean = ctrl0 | esp.SPI1_CTRL_FCMD_QUAD | esp.SPI1_CTRL_FREAD_QIO
	}
	s.CTRL.Set(clean)
	defer s.CTRL.Set(ctrl0)

	user := uint32(0)

	// Command phase: value in USER2[15:0], (len-1) in USER2[31:28].
	s.USER2.Set(uint32(cmd)&0xFFFF | uint32(cmdBits-1)<<28)
	user |= esp.SPI1_USER_USR_COMMAND

	// Address phase: value left-justified in ADDR, (len-1) in USER1[31:26].
	if addrBits > 0 {
		s.ADDR.Set(addr << (32 - uint(addrBits)))
		u1 := s.USER1.Get() &^ esp.SPI1_USER1_USR_ADDR_BITLEN_Msk
		u1 |= uint32(addrBits-1) << esp.SPI1_USER1_USR_ADDR_BITLEN_Pos
		s.USER1.Set(u1)
		user |= esp.SPI1_USER_USR_ADDR
	}

	// Dummy cycles: (len-1) in USER1[5:0].
	if dummyBits > 0 {
		u1 := s.USER1.Get() &^ esp.SPI1_USER1_USR_DUMMY_CYCLELEN_Msk
		u1 |= uint32(dummyBits - 1)
		s.USER1.Set(u1)
		user |= esp.SPI1_USER_USR_DUMMY
	}

	// MOSI: write payload into the W0.. data registers, set bit length.
	if len(mosi) > 0 {
		writeData(mosi)
		s.MOSI_DLEN.Set(uint32(len(mosi)*8 - 1))
		user |= esp.SPI1_USER_USR_MOSI
	}

	// MISO: just set the expected read length; data is read back after the transfer.
	if len(miso) > 0 {
		s.MISO_DLEN.Set(uint32(len(miso)*8 - 1))
		user |= esp.SPI1_USER_USR_MISO
	}

	s.USER.Set(user)

	// Select PSRAM (CS1 enabled, CS0/flash disabled) for this transaction.
	misc := s.MISC.Get()
	s.MISC.Set((misc | esp.SPI1_MISC_CS0_DIS) &^ esp.SPI1_MISC_CS1_DIS)

	// Fire and wait: USR self-clears when the transaction completes.
	s.CMD.SetBits(esp.SPI1_CMD_USR)
	for s.CMD.Get()&esp.SPI1_CMD_USR != 0 {
	}

	s.MISC.Set(misc) // restore chip-select state

	if len(miso) > 0 {
		readData(miso)
	}
}

// writeData packs a MOSI byte slice little-endian into the SPI1 W0.. data registers.
func writeData(b []byte) {
	for i := 0; i < len(b); i += 4 {
		var w uint32
		for j := 0; j < 4 && i+j < len(b); j++ {
			w |= uint32(b[i+j]) << (8 * uint(j))
		}
		wreg(i / 4).Set(w)
	}
}

// readData copies the SPI1 W0.. data registers back into a MISO byte slice, little-endian.
func readData(b []byte) {
	for i := 0; i < len(b); i += 4 {
		w := wreg(i / 4).Get()
		for j := 0; j < 4 && i+j < len(b); j++ {
			b[i+j] = byte(w >> (8 * uint(j)))
		}
	}
}

// wreg returns the n-th SPI1 data register (W0 at offset 0x58, consecutive 32-bit words).
func wreg(n int) *volatile.Register32 {
	return (*volatile.Register32)(unsafe.Add(unsafe.Pointer(&esp.SPI1.W0), uintptr(n)*4))
}

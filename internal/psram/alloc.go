// Bump allocator over a flat memory span. It is deliberately pure — it knows only a base address
// and a size, not how that memory came to exist — so the same logic serves the ESP32-S3 PSRAM
// region (psram_esp32s3.go wires Region.Allocator to it) and is exercised by host unit tests.
//
// PSRAM is NOT part of the GC heap: the Go runtime never sees it, so nothing here is traced or
// freed by the collector. A bump allocator fits that model — it hands out spans that live for the
// program's duration. There is no per-allocation free; Reset reclaims everything at once.
package psram

import "unsafe"

// Allocator hands out byte spans from a fixed region by bumping a cursor. It is NOT safe for
// concurrent use — it's meant for setup-time allocation of long-lived buffers, not a general heap.
type Allocator struct {
	base uintptr // start of the region (cursor resets here)
	next uintptr // next free address
	end  uintptr // one past the region
}

// NewAllocator returns a bump allocator over [base, base+size).
func NewAllocator(base uintptr, size int) *Allocator {
	return &Allocator{base: base, next: base, end: base + uintptr(size)}
}

// Alloc reserves n bytes aligned up to `align` (a power of two; 0 or 1 means byte-aligned) and
// returns a slice over them, or nil if the region can't satisfy the request. The bytes are NOT
// zeroed — PSRAM powers up with arbitrary contents, so a caller that needs zeros must clear them.
// Align to the 16-byte cache line (or more) for any span a DMA engine will touch.
func (a *Allocator) Alloc(n, align int) []byte {
	if n <= 0 {
		return nil
	}
	if align < 1 {
		align = 1
	}
	start := (a.next + uintptr(align-1)) &^ uintptr(align-1)
	end := start + uintptr(n)
	if end < start || end > a.end { // overflow or out of room
		return nil
	}
	a.next = end
	return unsafe.Slice((*byte)(unsafe.Pointer(start)), n)
}

// Used returns the bytes handed out so far (including alignment padding).
func (a *Allocator) Used() int { return int(a.next - a.base) }

// Free returns the bytes still available (an upper bound — alignment padding on the next Alloc
// may consume a little more).
func (a *Allocator) Free() int { return int(a.end - a.next) }

// Reset drops every allocation at once, returning the whole region to free. Any slices previously
// handed out alias memory that may now be reused — only call when none are live.
func (a *Allocator) Reset() { a.next = a.base }

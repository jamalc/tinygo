//go:build tinygo

// Command psramprobe is the bring-up harness for the ESP32-S3 PSRAM driver (internal/psram).
// It loops on detection (printing the raw ID every time, so a wrong line-mode / stuck-QPI state
// is visible rather than hidden behind an opaque error), and once the chip answers it runs the
// full bring-up plus a physical round-trip test, then idles reporting the result.
//
//	make psramprobe BOARD=matrixportal-s3
//
// NOTE: PSRAM mode state survives a CPU reset (it's volatile but stays powered across a
// `tinygo flash` "Device reset"). To get a truly cold chip, unplug/replug USB.
package main

import (
	"time"
	"unsafe"

	"github.com/jamalc/tinygo/internal/psram"
)

func main() {
	time.Sleep(2 * time.Second) // let the USB-serial monitor attach before the first print

	// Detect loop: print the raw ID until the chip answers correctly. Always showing the bytes
	// makes the failure mode legible (all-0x00, all-0xFF, or a shifted value).
	for {
		id := psram.Detect()
		print("detect: raw=0x")
		printHex(id.Raw, 6)
		print(" mfid=0x")
		printHex(uint32(id.MFID), 2)
		print(" kgd=0x")
		printHex(uint32(id.KGD), 2)
		if id.ValidKGD {
			println(" -> alive")
			break
		}
		println(" -> not yet")
		time.Sleep(time.Second)
	}

	// Chip is alive: bring it all the way up and round-trip through physical PSRAM.
	r, err := psram.Enable()
	if err != nil {
		for {
			println("enable:", err.Error())
			time.Sleep(time.Second)
		}
	}
	print("psram up: ")
	printDec(r.Size / 1024)
	print(" KB @ 0x")
	printHex(uint32(r.Addr), 8)
	println("")

	// Spot-check words spread across the whole 2 MB (offset 0, 1 MB, top) so a half-working map
	// or address aliasing is caught, not just the first page.
	offsets := []int{0, 1 << 20, r.Size - 4}
	ok := true
	for _, off := range offsets {
		p := (*uint32)(unsafe.Pointer(r.Addr + uintptr(off)))
		want := mix(uint32(off))
		*p = want
		psram.Writeback(r.Addr+uintptr(off), 4)
		psram.Invalidate(r.Addr+uintptr(off), 4)
		got := *p
		print("  @0x")
		printHex(uint32(off), 6)
		print(" want=0x")
		printHex(want, 8)
		print(" got=0x")
		printHex(got, 8)
		if got == want {
			println(" ok")
		} else {
			println(" MISMATCH")
			ok = false
		}
	}

	// Carve a buffer out of the region with the bump allocator and round-trip through it,
	// proving the allocator hands out real, usable PSRAM (16-byte aligned for DMA).
	al := r.Allocator()
	buf := al.Alloc(64<<10, 16)
	if buf == nil {
		println("alloc: nil (region too small?)")
		ok = false
	} else {
		base := uintptr(unsafe.Pointer(&buf[0]))
		print("alloc: 64 KB @ 0x")
		printHex(uint32(base), 8)
		print(" aligned16=")
		println(base%16 == 0)
		for i := range buf {
			buf[i] = byte(i*7 + 1)
		}
		psram.Writeback(base, len(buf))
		psram.Invalidate(base, len(buf))
		bad := 0
		for i := range buf {
			if buf[i] != byte(i*7+1) {
				bad++
			}
		}
		print("alloc round-trip: ")
		if bad == 0 {
			println("ok")
		} else {
			print("FAIL bad=")
			printDec(bad)
			println("")
			ok = false
		}
	}

	for {
		if ok {
			println("ALL PASS")
		} else {
			println("FAIL")
		}
		time.Sleep(2 * time.Second)
	}
}

// mix turns an address into a distinctive word so stuck/aliased bits show up.
func mix(x uint32) uint32 { return x*2654435761 ^ 0xA5A5A5A5 }

func printHex(v uint32, digits int) {
	const hex = "0123456789abcdef"
	for i := digits - 1; i >= 0; i-- {
		print(string(hex[(v>>(uint(i)*4))&0xF]))
	}
}

func printDec(v int) {
	if v == 0 {
		print("0")
		return
	}
	var buf [10]byte
	n := 0
	for v > 0 {
		buf[n] = byte('0' + v%10)
		v /= 10
		n++
	}
	for i := n - 1; i >= 0; i-- {
		print(string(buf[i]))
	}
}

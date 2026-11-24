package psram

import (
	"testing"
	"unsafe"
)

// backed returns an allocator over a real heap slice, so spans it hands out are valid to read and
// write (the bump logic is pure, but we want to exercise it against memory we can touch). The
// backing slice is returned so tests can confirm writes land where expected.
func backed(size int) (*Allocator, []byte) {
	buf := make([]byte, size)
	return NewAllocator(uintptr(unsafe.Pointer(&buf[0])), size), buf
}

func TestAllocBumpsAndAccounts(t *testing.T) {
	a, _ := backed(1024)
	if a.Free() != 1024 || a.Used() != 0 {
		t.Fatalf("fresh: used=%d free=%d", a.Used(), a.Free())
	}
	if b := a.Alloc(100, 1); len(b) != 100 {
		t.Fatalf("alloc 100: len=%d", len(b))
	}
	if a.Used() != 100 || a.Free() != 924 {
		t.Fatalf("after 100: used=%d free=%d", a.Used(), a.Free())
	}
}

func TestAllocAligns(t *testing.T) {
	a, _ := backed(4096)
	a.Alloc(1, 1) // bump to an odd offset
	b := a.Alloc(16, 16)
	if got := uintptr(unsafe.Pointer(&b[0])) % 16; got != 0 {
		t.Fatalf("not 16-aligned: off-by %d", got)
	}
}

func TestAllocExhaustionReturnsNil(t *testing.T) {
	a, _ := backed(64)
	if a.Alloc(65, 1) != nil {
		t.Fatal("over-size alloc should be nil")
	}
	if a.Alloc(64, 1) == nil {
		t.Fatal("exact-fit alloc should succeed")
	}
	if a.Alloc(1, 1) != nil {
		t.Fatal("alloc past full should be nil")
	}
}

func TestAllocAlignmentCanExhaust(t *testing.T) {
	a, _ := backed(32)
	a.Alloc(8, 1)            // cursor at +8
	if a.Alloc(32, 16) != nil { // would need padding to +16 then 32 bytes -> past end
		t.Fatal("alloc that overflows after alignment should be nil")
	}
}

func TestAllocWritesAreVisibleInBacking(t *testing.T) {
	a, buf := backed(256)
	b := a.Alloc(4, 1)
	b[0], b[1], b[2], b[3] = 0xDE, 0xAD, 0xBE, 0xEF
	if buf[0] != 0xDE || buf[3] != 0xEF {
		t.Fatalf("write not visible in backing: %x %x", buf[0], buf[3])
	}
}

func TestResetReclaims(t *testing.T) {
	a, _ := backed(128)
	a.Alloc(100, 1)
	a.Reset()
	if a.Used() != 0 || a.Free() != 128 {
		t.Fatalf("after reset: used=%d free=%d", a.Used(), a.Free())
	}
	if a.Alloc(128, 1) == nil {
		t.Fatal("full region should be allocatable after reset")
	}
}

func TestAllocZeroAndNegative(t *testing.T) {
	a, _ := backed(64)
	if a.Alloc(0, 1) != nil || a.Alloc(-5, 1) != nil {
		t.Fatal("non-positive sizes should return nil")
	}
	if a.Used() != 0 {
		t.Fatal("rejected allocs should not move the cursor")
	}
}

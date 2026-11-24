package client

import (
	"fmt"
	"runtime"
)

// TraceAlloc turns on per-fetch allocation tracing. Off by default; cmd/clock
// sets it when it wants to know where a poll's garbage comes from. A phase
// breakdown turns "something allocates a lot" into a line number — see
// the repo README ("Memory on TinyGo") for the investigation this settled.
var TraceAlloc = false

// allocTrace measures allocation between marks. It reads TotalAlloc, which is
// cumulative and monotonic, so a delta is exact no matter when the collector last
// ran — no GC is forced and the measurement does not perturb what it measures.
//
// The cost is not zero: TinyGo's ReadMemStats walks the whole heap bitmap to count
// live blocks, so each mark is O(heap). At a 30s poll that is irrelevant; do not
// put marks in a per-frame path.
//
// Phase names are string constants and the arrays are fixed, so tracing allocates
// nothing until the final Printf. Not safe for concurrent fetches, which matches
// LoadArrivals' existing single-owner contract for *buf.
type allocTrace struct {
	on   bool
	last uint64
	n    int
	name [6]string
	used [6]uint64
}

func startTrace() *allocTrace {
	t := &allocTrace{on: TraceAlloc}
	if t.on {
		t.last = totalAlloc()
	}
	return t
}

// mark records everything allocated since the previous mark under this name.
func (t *allocTrace) mark(name string) {
	if !t.on || t.n >= len(t.name) {
		return
	}
	now := totalAlloc()
	t.name[t.n] = name
	t.used[t.n] = now - t.last
	t.n++
	t.last = now
}

// report prints one line: the phases in order, and the total.
func (t *allocTrace) report(label string) {
	if !t.on || t.n == 0 {
		return
	}
	var total uint64
	fmt.Printf("%s:", label)
	for i := 0; i < t.n; i++ {
		fmt.Printf(" %s %d B |", t.name[i], t.used[i])
		total += t.used[i]
	}
	fmt.Printf(" total %d B\n", total)
}

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

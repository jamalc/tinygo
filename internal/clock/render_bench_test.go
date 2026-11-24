package clock

import "testing"

// These quantify the pageText cache: what a render tick used to compose versus
// what it composes now. The clock ticks at 10 Hz while its data changes once per
// poll, so the old path regenerated identical strings ~300 times between changes.
// Measured 128x64 two-column, the shipping layout.

// What one render tick used to compose, per tick: the page pace (which built
// every headline) plus, per cell, the headline again and the ETA line.
func BenchmarkTickOld(b *testing.B) {
	g := gridFor(128, 64, 2)
	groups := testGroups(g.cells() * 3)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = pagePaceRef(groups, 0, g)
		for j := 0; j < g.cells(); j++ {
			c := g.cell(j)
			_ = groupHeadline(groups[j], c.wide)
			_ = etaText(groups[j].ETAs, c.region())
		}
	}
}

// What a tick composes now: nothing. The page is built once per page/poll change
// and the tick only moves the marquee offset.
func BenchmarkTickCached(b *testing.B) {
	g := gridFor(128, 64, 2)
	groups := testGroups(g.cells() * 3)
	var p pageText
	p.build(groups, 0, g)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range p.cells {
			_ = p.cells[j].etas
			_ = p.cells[j].head
		}
	}
}

// And what a page change costs, which now happens once per page instead of 10x/s.
func BenchmarkPageBuild(b *testing.B) {
	g := gridFor(128, 64, 2)
	groups := testGroups(g.cells() * 3)
	var p pageText
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.build(groups, 0, g)
	}
}

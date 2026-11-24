package clock

import (
	"strconv"
	"testing"

	"github.com/jamalc/tinygo/internal/api"
)

func TestEtaText(t *testing.T) {
	if got := etaText(nil, 100); got != "No service" {
		t.Errorf("empty ETAs: got %q, want %q", got, "No service")
	}
	if got := etaText([]string{}, 100); got != "No service" {
		t.Errorf("zero-len ETAs: got %q, want %q", got, "No service")
	}
	if got := etaText([]string{"3m"}, 100); got != "3m" {
		t.Errorf("with ETAs: got %q, want %q", got, "3m")
	}
}

func TestMarqueeOffset(t *testing.T) {
	// The page's longest line: it sets both the pause offset (its stop name) and
	// the remaining scroll after it, so the pace is sized to this line.
	const period, pause = 120, 45
	p := marqueePace(pause, period-pause)

	if got := marqueeOffset(0, period, pause, p); got != 0 {
		t.Errorf("start: offset(0) = %d, want 0 (held)", got)
	}
	if got := marqueeOffset(marqueeHold-1, period, pause, p); got != 0 {
		t.Errorf("during hold: offset = %d, want 0", got)
	}
	// It never goes backwards and stays within [0, period].
	prev := -1
	for t0 := marqueeHold; t0 < p.cycle; t0++ {
		o := marqueeOffset(t0, period, pause, p)
		if o < 0 || o > period {
			t.Fatalf("offset(%d) = %d, out of [0,%d]", t0, o, period)
		}
		if o < prev {
			t.Errorf("offset(%d) = %d went backwards from %d", t0, o, prev)
		}
		prev = o
	}
	// The whole pause window rests with the headsign at the left edge, and the
	// scroll resumes from there on the tick after.
	for t0 := p.pauseStart; t0 < p.pauseEnd; t0++ {
		if got := marqueeOffset(t0, period, pause, p); got != pause {
			t.Errorf("in pause window: offset(%d) = %d, want %d", t0, got, pause)
		}
	}
	if got := marqueeOffset(p.pauseEnd+1, period, pause, p); got != pause+marqueeSpeed {
		t.Errorf("after pause: offset = %d, want %d (resumed)", got, pause+marqueeSpeed)
	}
	// A line with a shorter stop name reaches its own pause offset before the
	// window opens and waits there for the page.
	const shortPause = 15
	for t0 := marqueeHold + marqueeTicks(shortPause); t0 < p.pauseEnd; t0++ {
		if got := marqueeOffset(t0, period, shortPause, p); got != shortPause {
			t.Errorf("short pause offset at %d: got %d, want %d (waiting)", t0, got, shortPause)
		}
	}
	// A line shorter than the page's longest finishes its lap early and then
	// rests at its home position (offset == its own period) until the cycle wraps.
	const short = 30
	if got := marqueeOffset(p.cycle-1, short, shortPause, p); got != short {
		t.Errorf("short line at cycle end: offset = %d, want %d (rested home)", got, short)
	}
	// A line that fits never scrolls.
	if got := marqueeOffset(p.cycle-1, 0, 0, p); got != 0 {
		t.Errorf("non-scrolling line: offset = %d, want 0", got)
	}
	// Loops each cycle.
	a := marqueeOffset(marqueeHold+1, period, pause, p)
	b := marqueeOffset(marqueeHold+1+p.cycle, period, pause, p)
	if a != b {
		t.Errorf("not periodic: %d vs %d", a, b)
	}
}

// The wide layout has no stop name to scroll off, so its pace has no pause
// window and the marquee behaves as a plain hold-then-scroll lap.
func TestMarqueePaceNoPause(t *testing.T) {
	const period = 120
	p := marqueePace(0, period)
	if p.pauseStart != p.pauseEnd {
		t.Errorf("pause window %d..%d, want empty", p.pauseStart, p.pauseEnd)
	}
	if want := marqueeHold + marqueeTicks(period); p.cycle != want {
		t.Errorf("cycle = %d, want %d", p.cycle, want)
	}
	if got := marqueeOffset(marqueeHold+1, period, 0, p); got != marqueeSpeed {
		t.Errorf("offset after hold = %d, want %d (scrolling)", got, marqueeSpeed)
	}
}

func TestGrid(t *testing.T) {
	// One column on a 128x32: two full-width cells, each wide enough for the
	// fixed stop name plus a marqueed headsign.
	g := gridFor(128, 32, 1)
	if g.cells() != 2 {
		t.Errorf("single column: cells = %d, want 2", g.cells())
	}
	if c := g.cell(1); c.x != 0 || c.right != 128 || c.y != slotHeight || !c.wide {
		t.Errorf("single column cell 1 = %+v, want {0 128 16 true}", c)
	}
	// Two columns on the same panel: four 64px cells, numbered down each column
	// before the next (0,1 fill the left column top to bottom; 2,3 the right),
	// each too narrow for the wide layout. Every cell but the last in its row
	// stops cellGap short of the next column.
	g = gridFor(128, 32, 2)
	if g.cells() != 4 {
		t.Errorf("two columns: cells = %d, want 4", g.cells())
	}
	want := []cell{
		{x: 0, right: 64 - cellGap, y: 0},
		{x: 0, right: 64 - cellGap, y: slotHeight},
		{x: 64, right: 128, y: 0},
		{x: 64, right: 128, y: slotHeight},
	}
	for i, w := range want {
		if got := g.cell(i); got != w {
			t.Errorf("two-column cell %d = %+v, want %+v", i, got, w)
		}
	}
	// A 256px panel split in two still gets the wide layout in each column.
	if c := gridFor(256, 32, 2).cell(0); !c.wide {
		t.Errorf("128px cell of a 256px panel: wide = false, want true")
	}
	// Columns that would leave an unusably narrow cell are dropped.
	if g := gridFor(64, 32, 2); g.cols != 1 || g.cellW != 64 {
		t.Errorf("64px panel, 2 columns: cols = %d, cellW = %d, want 1, 64", g.cols, g.cellW)
	}
	if g := gridFor(128, 32, 0); g.cols != 1 {
		t.Errorf("unset columns: cols = %d, want 1", g.cols)
	}
}

func TestTruncateWords(t *testing.T) {
	// Picopixel widths (measured): "Coney Island"=43px,
	// "Coney Island-Stillwell"=78px, the full string=88px. With a 50px budget
	// the cut should land at the hyphen, not drop the whole hyphenated token.
	tests := []struct {
		s        string
		maxWidth int
		want     string
	}{
		{"Coney Island-Stillwell Av", 50, "Coney Island"},
		{"Coney Island-Stillwell Av", 100, "Coney Island-Stillwell Av"}, // fits whole
		{"Far Rockaway-Mott Av", 50, "Far Rockaway"},
		{"Brighton Beach", 50, "Brighton Beach"},             // 50px, fits exactly
		{"Supercalifragilistic", 10, "Supercalifragilistic"}, // one long word: kept whole
		{"", 50, ""},
	}
	for _, tt := range tests {
		if got := truncateWords(tt.s, tt.maxWidth); got != tt.want {
			t.Errorf("truncateWords(%q, %d) = %q, want %q", tt.s, tt.maxWidth, got, tt.want)
		}
	}
}

// testGroups builds n groups with distinct, deliberately long stop names and
// headsigns, so line 0 overflows its cell and the marquee metrics are non-zero.
func testGroups(n int) []api.FlatArrivalGroup {
	gs := make([]api.FlatArrivalGroup, n)
	for i := range gs {
		gs[i] = api.FlatArrivalGroup{
			ArrivalGroup: api.ArrivalGroup{
				Route:     string(rune('A' + i)),
				Headsign:  "Coney Island-Stillwell Av " + strconv.Itoa(i),
				ETAs:      []string{"2m", "9m", "18m", "28m", "36m", "43m", "51m", "56m"},
				RouteName: "Broadway Express",
			},
			StopID:   "R30N",
			StopName: "Atlantic Av-Barclays Ctr " + strconv.Itoa(i),
		}
	}
	return gs
}

// pagePaceRef is the pace computation as it stood before pageText existed: walk
// the page's cells, build each headline, take the max pause and remainder. Kept
// here as the reference the cached path must agree with — the refactor moved this
// arithmetic into pageText.build to stop recomputing it every tick, and this test
// is what says the move didn't change the answer.
func pagePaceRef(groups []api.FlatArrivalGroup, cursor int, g grid) pace {
	maxPause, maxRemain := 0, 0
	for i := 0; i < g.cells(); i++ {
		idx := cursor + i
		if idx < 0 || idx >= len(groups) {
			continue
		}
		c := g.cell(i)
		period, pause := headlineMetrics(groupHeadline(groups[idx], c.wide), c)
		if pause > maxPause {
			maxPause = pause
		}
		if remain := period - pause; remain > maxRemain {
			maxRemain = remain
		}
	}
	return marqueePace(maxPause, maxRemain)
}

func TestPageBuildMatchesReferencePace(t *testing.T) {
	// Both panel shapes the clock ships on, and both column layouts.
	for _, geo := range []struct {
		name string
		w, h int16
		cols int
	}{
		{"64x32 one column", 64, 32, 1},
		{"128x64 one column", 128, 64, 1},
		{"128x64 two columns", 128, 64, 2},
	} {
		t.Run(geo.name, func(t *testing.T) {
			g := gridFor(geo.w, geo.h, geo.cols)
			groups := testGroups(g.cells() * 3)

			// Every page position, including a short final page and the negative
			// startup sentinel the render loop begins on.
			for _, cursor := range []int{-g.cells(), 0, 1, g.cells(), len(groups) - 1} {
				var p pageText
				p.build(groups, cursor, g)

				if want := pagePaceRef(groups, cursor, g); p.pace != want {
					t.Errorf("cursor %d: pace = %+v, want %+v", cursor, p.pace, want)
				}
			}
		})
	}
}

func TestPageBuildFillsOnlyRealCells(t *testing.T) {
	g := gridFor(128, 64, 2)
	groups := testGroups(g.cells() + 1)

	var p pageText
	p.build(groups, 0, g)
	if len(p.cells) != g.cells() {
		t.Errorf("full page: %d cells, want %d", len(p.cells), g.cells())
	}

	// Short final page: one group left, so one cell, not a page of blanks.
	p.build(groups, g.cells(), g)
	if len(p.cells) != 1 {
		t.Errorf("short final page: %d cells, want 1", len(p.cells))
	}

	// The startup sentinel sits entirely before the data.
	p.build(groups, -g.cells(), g)
	if len(p.cells) != 0 {
		t.Errorf("pre-first page: %d cells, want 0", len(p.cells))
	}
}

func TestPageBuildReusesItsBacking(t *testing.T) {
	// The point of the cache is to stop allocating per tick, so a rebuild must
	// reuse the slice it already has rather than allocate a fresh one.
	g := gridFor(128, 64, 2)
	groups := testGroups(g.cells() * 2)

	var p pageText
	p.build(groups, 0, g)
	first := cap(p.cells)

	for i := 0; i < 20; i++ {
		p.build(groups, 0, g)
	}
	if cap(p.cells) != first {
		t.Errorf("capacity grew from %d to %d across rebuilds; build is reallocating", first, cap(p.cells))
	}
}

func TestPageBuildCarriesCellText(t *testing.T) {
	g := gridFor(128, 64, 1)
	groups := testGroups(g.cells())

	var p pageText
	p.build(groups, 0, g)

	for i, c := range p.cells {
		if c.route != groups[i].Route {
			t.Errorf("cell %d: route %q, want %q", i, c.route, groups[i].Route)
		}
		if want := etaText(groups[i].ETAs, c.cell.region()); c.etas != want {
			t.Errorf("cell %d: etas %q, want %q", i, c.etas, want)
		}
		if c.etas == "" {
			t.Errorf("cell %d: empty ETA line", i)
		}
	}
}

// The LIRR draws its train mark, in teal, and nothing spills past the bullet.
func TestDrawBulletLongRouteStaysInside(t *testing.T) {
	d := newFakePanel(64, 16)
	drawBullet(d, "LIRR", 0, 0)
	for p, c := range d.px {
		if p[0] >= bulletX+bulletSize || p[1] >= bulletY+bulletSize {
			t.Fatalf("pixel %v = %v outside the %dpx bullet", p, c, bulletSize)
		}
	}
	if got := d.at(bulletX+bulletSize/2, bulletY); got != lirrTeal {
		t.Errorf("bullet top = %v, want LIRR teal", got)
	}
}

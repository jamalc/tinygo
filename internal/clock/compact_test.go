package clock

import (
	"image/color"
	"testing"

	"github.com/jamalc/tinygo/internal/api"
)

func TestLayoutFromConfig(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want layoutMode
	}{
		{"", layoutClassic},
		{"classic", layoutClassic},
		{"compact", layoutCompact},
		{"COMPACT", layoutClassic}, // exact match only
		{"nonsense", layoutClassic},
	} {
		if got := layoutFromConfig(tc.in); got != tc.want {
			t.Errorf("layoutFromConfig(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDirectionFromStopID(t *testing.T) {
	for _, tc := range []struct {
		id        string
		hasDir    bool
		northboud bool
	}{
		{"R30N", true, true},
		{"R30S", true, false},
		{"A41N", true, true},
		{"901", false, false}, // no N/S suffix
		{"", false, false},
	} {
		if got := hasDirection(tc.id); got != tc.hasDir {
			t.Errorf("hasDirection(%q) = %v, want %v", tc.id, got, tc.hasDir)
		}
		if got := northbound(tc.id); got != tc.northboud {
			t.Errorf("northbound(%q) = %v, want %v", tc.id, got, tc.northboud)
		}
	}
}

// truncateChars cuts mid-word (unlike truncateWords), and never returns a
// string wider than the limit.
func TestTruncateChars(t *testing.T) {
	if got := truncateChars("DeKalb Av", 1000); got != "DeKalb Av" {
		t.Errorf("fits: got %q, want the whole string", got)
	}
	if got := truncateChars("anything", 0); got != "" {
		t.Errorf("zero width: got %q, want empty", got)
	}
	// A width that lands mid-word keeps the partial word, and the result fits.
	full := "Atlantic Av"
	got := truncateChars(full, textWidth(full)-4)
	if got == full || len(got) >= len(full) {
		t.Errorf("expected a shorter prefix, got %q", got)
	}
	if textWidth(got) > textWidth(full)-4 {
		t.Errorf("truncateChars overflowed: %q is %dpx, limit %d", got, textWidth(got), textWidth(full)-4)
	}
	// It cuts where truncateWords wouldn't: a word-boundary cut of "Atlantic Av"
	// at a sub-"Atlantic" width yields "Atlantic" (first word kept whole), while
	// truncateChars yields a shorter, mid-word prefix.
	narrow := textWidth("Atlan")
	if w := truncateChars(full, narrow); w == truncateWords(full, narrow) {
		t.Errorf("truncateChars should differ from truncateWords here: both %q", w)
	}
}

// The compact grid packs twice as many single-line rows as the classic grid.
func TestCompactGridDoublesRows(t *testing.T) {
	classic := gridFor(64, 32, 1)
	compact := gridForRows(64, 32, 1, compactRowH)
	if classic.cells() != 2 {
		t.Fatalf("classic 64x32: %d cells, want 2", classic.cells())
	}
	if compact.cells() != 4 {
		t.Fatalf("compact 64x32: %d cells, want 4", compact.cells())
	}
	// Rows are stacked 8px apart, top to bottom.
	for i := 0; i < compact.cells(); i++ {
		if got, want := compact.cell(i).y, int16(i)*compactRowH; got != want {
			t.Errorf("compact cell %d: y = %d, want %d", i, got, want)
		}
	}
}

func compactGroup(route, stop, id string, etas ...string) api.FlatArrivalGroup {
	return api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: route, Headsign: "96 St", ETAs: etas},
		StopID:       id, StopName: stop,
	}
}

// buildCompact composes each row's ETAs and direction up front.
func TestBuildCompact(t *testing.T) {
	groups := []api.FlatArrivalGroup{
		compactGroup("Q", "DeKalb Av", "R30N", "1m", "8", "16"),
		compactGroup("R", "DeKalb Av", "R30S", "11m", "19"),
		compactGroup("W", "DeKalb Av", "R30N"), // no arrivals -> No service
	}
	g := gridForRows(64, 32, 1, compactRowH)
	var p pageText
	p.buildCompact(groups, 0, g)

	if len(p.cells) != 3 {
		t.Fatalf("got %d cells, want 3", len(p.cells))
	}
	if p.cells[0].route != "Q" || !p.cells[0].north || !p.cells[0].dir {
		t.Errorf("cell 0: %+v, want route Q northbound", p.cells[0])
	}
	if p.cells[1].north {
		t.Errorf("cell 1 (R30S) should be southbound")
	}
	if p.cells[0].etas == "" {
		t.Errorf("cell 0 should have its ETAs composed: %+v", p.cells[0])
	}
	if p.cells[2].etas != "No service" {
		t.Errorf("cell 2 (no arrivals) etas = %q, want %q", p.cells[2].etas, "No service")
	}
}

// --- a fake panel, to assert what the compact draw actually paints ---

type fakePanel struct {
	w, h int16
	px   map[[2]int16]color.RGBA
}

func newFakePanel(w, h int16) *fakePanel {
	return &fakePanel{w: w, h: h, px: map[[2]int16]color.RGBA{}}
}
func (f *fakePanel) Size() (int16, int16) { return f.w, f.h }
func (f *fakePanel) Display() error       { return nil }
func (f *fakePanel) Clear()               { f.px = map[[2]int16]color.RGBA{} }
func (f *fakePanel) SetPixel(x, y int16, c color.RGBA) {
	if x >= 0 && y >= 0 && x < f.w && y < f.h {
		f.px[[2]int16{x, y}] = c
	}
}
func (f *fakePanel) at(x, y int16) color.RGBA { return f.px[[2]int16{x, y}] }

// The chip paints the route colour, and the arrow's apex row distinguishes up
// from down: a northbound apex sits at the top of the triangle, a southbound one
// at the bottom.
func TestDrawCompactChipAndArrow(t *testing.T) {
	g := gridForRows(64, 32, 1, compactRowH)
	up := compactGroup("Q", "DeKalb Av", "R30N", "1m")
	down := compactGroup("R", "DeKalb Av", "R30S", "2m")

	var p pageText
	p.buildCompact([]api.FlatArrivalGroup{up, down}, 0, g)

	d := newFakePanel(64, 32)
	drawCompactGroup(d, p.cells[0])
	drawCompactGroup(d, p.cells[1])

	// Chip pixel: cell 0 at (compactChipX, cell.y+1) is the Q's colour.
	if got := d.at(compactChipX, p.cells[0].cell.y+1); got != routeColor("Q") {
		t.Errorf("cell 0 chip = %v, want route Q colour %v", got, routeColor("Q"))
	}

	// The apex column is painted in every row of both triangles, so it can't
	// tell up from down. The base edge can: an up arrow's wide base is at the
	// bottom row, a down arrow's at the top. Check the leftmost arrow column —
	// set only on the base row. The arrow hugs the glyph, so its x depends on the
	// route width.
	rx := int16(compactChipX + compactChipSize + compactChipGap)
	upEdge := rx + int16(textWidth(p.cells[0].route)) + compactRouteArrowGap   // Q's arrow left column
	downEdge := rx + int16(textWidth(p.cells[1].route)) + compactRouteArrowGap // R's arrow left column
	blank := color.RGBA{}

	upTopEdge := d.at(upEdge, p.cells[0].cell.y+2)     // up: top row is the apex -> edge blank
	upBotEdge := d.at(upEdge, p.cells[0].cell.y+4)     // up: bottom row is the base -> edge set
	downTopEdge := d.at(downEdge, p.cells[1].cell.y+2) // down: top row is the base -> edge set
	downBotEdge := d.at(downEdge, p.cells[1].cell.y+4) // down: bottom row is the apex -> edge blank

	if upTopEdge != blank || upBotEdge == blank {
		t.Errorf("up arrow base should be at the bottom: top-edge %v, bottom-edge %v", upTopEdge, upBotEdge)
	}
	if downTopEdge == blank || downBotEdge != blank {
		t.Errorf("down arrow base should be at the top: top-edge %v, bottom-edge %v", downTopEdge, downBotEdge)
	}
}

// An LIRR row draws the small train mark, not a chip and "LIRR", then its
// destination's code, then the ETAs.
func TestCompactLIRRRow(t *testing.T) {
	groups := []api.FlatArrivalGroup{
		compactGroup("A", "Jay St", "A41N", "5m"),
		{ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: "Jamaica", ETAs: []string{"12m"}}, StopID: "ATL>JAM", StopName: "Atlantic Terminal"},
	}
	var p pageText
	p.buildCompact(groups, 0, gridForRows(64, 32, 1, compactRowH))
	d := newFakePanel(64, 32)
	drawCompactGroup(d, p.cells[1])
	y := p.cells[1].cell.y
	if got := d.at(compactChipX+1, y); got != lirrTeal {
		t.Errorf("train roof = %v, want LIRR teal", got)
	}
	if p.cells[1].label != "JAM" {
		t.Errorf("label = %q, want JAM", p.cells[1].label)
	}
	// The label is white, between the train and the ETAs; nothing else is.
	start := int16(compactTripInset("JAM"))
	white_ := 0
	for x := int16(compactChipX + len(trainFrontSmall[0])); x < start; x++ {
		for dy := int16(0); dy < compactRowH; dy++ {
			switch c := d.at(x, y+dy); c {
			case white:
				white_++
			case color.RGBA{}:
			default:
				t.Fatalf("pixel (%d,%d) = %v between the train and the ETAs, want only the white label", x, y+dy, c)
			}
		}
	}
	if white_ == 0 {
		t.Error("no label drawn between the train and the ETAs")
	}
	lit := false
	for x := start; x < start+4; x++ {
		for dy := int16(0); dy < compactRowH; dy++ {
			if d.at(x, y+dy) == gold {
				lit = true
			}
		}
	}
	if !lit {
		t.Errorf("no gold ETA at x=%d, just past the label", start)
	}
}

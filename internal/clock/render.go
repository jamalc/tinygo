package clock

import (
	"image/color"
	"strings"

	"tinygo.org/x/tinydraw"
	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/fonts"
)

// Layout geometry, in pixels. Each arrival group occupies one cell of the page
// grid, a slotHeight row within a column: a route bullet at the left, then two
// text rows (line 0, line 1) whose columns start at textX, just past the bullet.
const (
	slotHeight = 16

	bulletX        = 1
	bulletY        = 2
	bulletSize     = 12
	bulletRadius   = 4
	bulletTextX    = 5  // route label x, inset into the bullet
	bulletBaseline = 11 // route label baseline within the bullet

	textX = 14 // line 0 / line 1 text column, just right of the bullet

	// stopNameMax caps the fixed stop name on line 0 of the wide layout, so a
	// long name is word-trimmed and leaves room for the marqueed headsign.
	stopNameMax = 68

	// wideCell is the cell width at which a cell switches from the narrow layout
	// (stop name and headsign sweeping together) to the wide one (stop name held
	// fixed, only the headsign sweeping).
	wideCell = 128

	// cellGap is the blank column left at the right edge of every cell but the
	// last, so a marquee stops short of the next column's bullet.
	cellGap = 0

	// minCell is the narrowest usable cell: a bullet plus enough text region to
	// read a marqueed word at a time. Below it gridFor drops columns instead, so
	// a 64px panel ignores columns: 2 rather than shredding every line.
	minCell = 48
)

// --- Page grid -------------------------------------------------------------

// cell is one arrival group's drawing bounds: the text and bullet start at x,
// nothing is drawn at or past right, and the row's top is y. wide selects the
// layout, from the cell's own width rather than the display's — a 256px panel
// split into two columns still gets the wide layout in each.
type cell struct {
	x, right, y int16
	wide        bool
}

// region returns the width available to text in the cell, past the bullet.
func (c cell) region() int { return int(c.right) - int(c.x) - textX }

// grid is a page's cell layout: cols columns across the display, each divided
// into rows of rowH pixels. Cells are numbered down each column before moving to
// the next, so consecutive groups fill a column top to bottom rather than a row
// left to right.
type grid struct {
	cols, rows int
	cellW      int16
	rowH       int16 // slotHeight for the classic layout, compactRowH for compact
}

// gridFor lays out a w×h display as cols columns of the classic slotHeight rows.
func gridFor(w, h int16, cols int) grid {
	return gridForRows(w, h, cols, slotHeight)
}

// gridForRows is gridFor with an explicit row height, so the compact layout can
// pack twice as many single-line rows into the same panel. It drops columns
// until each is at least minCell wide. A page holds cells() groups.
func gridForRows(w, h int16, cols int, rowH int16) grid {
	if cols < 1 {
		cols = 1
	}
	for cols > 1 && int(w)/cols < minCell {
		cols--
	}
	return grid{cols: cols, rows: int(h) / int(rowH), cellW: w / int16(cols), rowH: rowH}
}

// cells is how many arrival groups one page shows.
func (g grid) cells() int { return g.cols * g.rows }

// cell returns the bounds of the i'th cell, numbered down each column before
// moving to the next.
func (g grid) cell(i int) cell {
	col, row := i/g.rows, i%g.rows
	x := int16(col) * g.cellW
	right := x + g.cellW
	if col < g.cols-1 {
		right -= cellGap
	}
	return cell{x: x, right: right, y: int16(row) * g.rowH, wide: g.cellW >= wideCell}
}

// rowY returns the Picopixel baseline y-coordinate for the given text row.
// Each row is 8px tall; +5 positions the baseline within the row for Picopixel.
func rowY(row int) int16 { return int16(row*8 + 5) }

// textWidth returns the rendered width of s in Picopixel pixels.
func textWidth(s string) int {
	_, w := tinyfont.LineWidth(&tinyfont.Picopixel, s)
	return int(w)
}

// trimEtas trims the ETA list to fit within maxWidth pixels, dropping trailing
// entries.
func trimEtas(etas []string, maxWidth int) string {
	s := strings.Join(etas, " ")
	for textWidth(s) > maxWidth && len(etas) > 0 {
		etas = etas[:len(etas)-1]
		s = strings.Join(etas, " ")
	}
	return s
}

// etaText is the ETA line for a row: the trimmed ETA list, or "No service"
// when the route has no arrivals (empty ETAs).
func etaText(etas []string, maxWidth int) string {
	if len(etas) == 0 {
		return "No service"
	}
	return trimEtas(etas, maxWidth)
}

// truncateWords returns the longest prefix of s that fits within maxWidth
// pixels, cut only at a word (space) or hyphen boundary so names like
// "Coney Island-Stillwell Av" shorten to "Coney Island" rather than mid-word.
// The first segment is kept even if it alone overflows, so the result is never
// empty for a non-empty s.
func truncateWords(s string, maxWidth int) string {
	if textWidth(s) <= maxWidth {
		return s
	}
	// Prefixes only grow wider, so the last one that fits is the longest.
	best := ""
	for i, r := range s {
		if r != ' ' && r != '-' {
			continue
		}
		if seg := strings.TrimRight(s[:i], " -"); textWidth(seg) <= maxWidth {
			best = seg
		} else {
			break
		}
	}
	if best != "" {
		return best
	}
	// Even the first segment overflows; return it whole (it will clip).
	if i := strings.IndexAny(s, " -"); i > 0 {
		return strings.TrimRight(s[:i], " -")
	}
	return s
}

// truncateChars returns the longest prefix of s that fits within maxWidth
// pixels, cut at any character (unlike truncateWords, which cuts only at word
// boundaries). The compact layout uses it so a stop name fills the row to the
// last pixel — "Atlantic Av-Barcl" rather than stopping at "Atlantic". Widths
// are summed per rune, which the bitmap font advances additively; a rounding
// pixel is caught by the caller's clip.
func truncateChars(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if textWidth(s) <= maxWidth {
		return s
	}
	w := 0
	for i, r := range s {
		w += textWidth(string(r))
		if w > maxWidth {
			return s[:i]
		}
	}
	return s
}

// segment is a run of text drawn in one colour. Lines are built from segments so
// differently coloured parts (e.g. a grey stop name and a white headsign) can
// scroll together as a unit.
type segment struct {
	text string
	col  color.RGBA
}

// segmentsWidth returns the combined rendered width of segs in pixels.
func segmentsWidth(segs []segment) int {
	total := 0
	for _, s := range segs {
		total += textWidth(s.text)
	}
	return total
}

// drawSegments draws segs left to right starting at (x, y).
func drawSegments(d Display, segs []segment, x, y int16) {
	for _, s := range segs {
		tinyfont.WriteLine(d, &tinyfont.Picopixel, x, y, s.text, s.col)
		x += int16(textWidth(s.text))
	}
}

// --- Marquee ---------------------------------------------------------------
//
// Pacing is in render ticks (the loop renders ~10/s): a line holds at the start
// for marqueeHold ticks, then scrolls left at a constant marqueeSpeed px/tick
// (the text plus marqueeGap of blank). A line also has a pause offset — the
// pixel at which its headsign reaches the left edge — where it rests again for
// marqueeHeadsignHold ticks before finishing the lap.
//
// All lines on a page share one pace, so they scroll at the same speed and both
// rests are simultaneous: a line that reaches its pause offset (or its lap end)
// early waits there for the slowest before the page moves on together.

const (
	marqueeHold  = 10 // ticks held at the start before scrolling
	marqueeSpeed = 3  // pixels scrolled per tick
	marqueeGap   = 16 // blank gap after the text before it wraps

	// marqueeHeadsignHold is how long every line rests once the slowest has
	// scrolled its stop name off, leaving the headsigns aligned at the left
	// edge long enough to read.
	marqueeHeadsignHold = 20

	// marqueeEndHold is how long the marquee rests at the start once a group
	// cycle is due, so a finished page pauses before swapping rather than
	// cycling the instant it returns home.
	marqueeEndHold = 20
)

// pace is a page's shared marquee timing, in ticks: the cycle length and the
// window [pauseStart, pauseEnd) during which every line rests with its headsign
// at the left edge. pauseStart == pauseEnd when no line on the page has a stop
// name to scroll off (the wide layout, where the headsign starts at the edge).
type pace struct {
	cycle      int
	pauseStart int
	pauseEnd   int
}

// marqueeTicks returns the ticks it takes to scroll px pixels.
func marqueeTicks(px int) int {
	if px <= 0 {
		return 0
	}
	return (px + marqueeSpeed - 1) / marqueeSpeed
}

// marqueePace builds a page's pace from the widest pause offset among its lines
// and the widest remaining scroll after that offset. A page with nothing to
// scroll just gets the hold.
func marqueePace(maxPause, maxRemain int) pace {
	start := marqueeHold + marqueeTicks(maxPause)
	end := start
	if maxPause > 0 {
		end += marqueeHeadsignHold
	}
	return pace{cycle: end + marqueeTicks(maxRemain), pauseStart: start, pauseEnd: end}
}

// marqueePeriod returns the lap length (px) of text of width tw scrolling in a
// region of width region — the text plus the gap — or 0 when it fits and so
// doesn't scroll.
func marqueePeriod(tw, region int) int {
	if tw <= region {
		return 0
	}
	return tw + marqueeGap
}

// marqueeOffset returns how far to scroll left at the given tick within the
// page's shared pace: held at 0 for marqueeHold ticks, then advancing at
// marqueeSpeed px/tick to pause, resting there for the page's pause window,
// then advancing again up to period, where it rests until the cycle wraps.
// A line reaching either rest early pauses there, so lines stay in step;
// period ≡ 0 visually, so the final rest and the wrap are seamless.
func marqueeOffset(tick, period, pause int, p pace) int {
	if period <= 0 {
		return 0
	}
	if pause > period {
		pause = period
	}
	t := tick % p.cycle
	switch {
	case t < marqueeHold:
		return 0
	case t < p.pauseStart:
		if scrolled := (t - marqueeHold) * marqueeSpeed; scrolled < pause {
			return scrolled
		}
		return pause
	case t < p.pauseEnd:
		return pause
	}
	if scrolled := pause + (t-p.pauseEnd)*marqueeSpeed; scrolled <= period {
		return scrolled
	}
	return period
}

// clipX wraps a Display and drops any pixel outside the horizontal range
// [min, max), cropping a line of text to a region so a marquee doesn't bleed
// over the route bullet or off-screen.
type clipX struct {
	d        Display
	min, max int16
}

func (c clipX) Size() (int16, int16) { return c.d.Size() }
func (c clipX) Display() error       { return c.d.Display() }
func (c clipX) Clear()               { c.d.Clear() }

func (c clipX) SetPixel(x, y int16, col color.RGBA) {
	if x < c.min || x >= c.max {
		return
	}
	c.d.SetPixel(x, y, col)
}

// marquee draws segs as one line on baseline y within the region [x, w). When
// they fit they are drawn statically at x; when wider they scroll left at the
// constant speed and loop — a trailing copy one lap behind makes the wrap
// seamless. pause is the offset (px) into segs the line rests at mid-lap;
// tick and the pace come from the page.
func marquee(d Display, segs []segment, pause int, x, y, w int16, tick int, p pace) {
	period := marqueePeriod(segmentsWidth(segs), int(w)-int(x))
	if period == 0 {
		drawSegments(d, segs, x, y)
		return
	}
	clip := clipX{d: d, min: x, max: w}
	off := int16(marqueeOffset(tick, period, pause, p))
	drawSegments(clip, segs, x-off, y)
	drawSegments(clip, segs, x-off+int16(period), y)
}

// --- Arrival groups --------------------------------------------------------

// headline is line 0 of a group: a fixed prefix anchored at the text column,
// then segments that marquee in the space after it. pause is the offset (px)
// into those segments where the headsign begins, so the marquee can rest with
// the headsign at the left edge; it is 0 when nothing precedes the headsign.
type headline struct {
	fixed  []segment
	scroll []segment
	pause  int
}

// groupHeadline builds line 0 for a group in the given layout. Narrow sweeps the
// stop name and headsign together (nothing fixed), pausing once the stop name
// has scrolled off; wide keeps the word-trimmed stop name fixed and sweeps only
// the headsign beside it, which starts at the left edge and so has no mid-lap
// pause. This is the single source of line-0 composition, shared by drawGroup
// and line0Metrics.
func groupHeadline(g api.FlatArrivalGroup, wide bool) headline {
	if wide {
		h := headline{fixed: []segment{{wideStop(g), grey}}}
		if g.Headsign != "" {
			h.scroll = []segment{{g.Headsign, white}}
		}
		return h
	}
	if g.Headsign == "" {
		return headline{scroll: []segment{{g.StopName, grey}}}
	}
	stop := g.StopName + " "
	return headline{
		scroll: []segment{{stop, grey}, {g.Headsign, white}},
		pause:  textWidth(stop),
	}
}

// wideStop is the grey fixed prefix of a wide line 0: the stop name (word-
// trimmed to stopNameMax) plus a separator when a headsign follows.
func wideStop(g api.FlatArrivalGroup) string {
	s := truncateWords(g.StopName, stopNameMax)
	if g.Headsign != "" {
		s += " > "
	}
	return s
}

// headlineMetrics returns the marquee lap length of a built line 0 in the given
// cell and the offset at which it rests mid-lap (both px), or 0, 0 when the line
// fits without scrolling. pageText.build takes the max of each across a page to
// size the shared pace.
//
// It takes an already-built headline rather than a group because build needs both
// the headline and its metrics; composing them separately cost two identical
// groupHeadline calls per cell per tick.
func headlineMetrics(h headline, c cell) (period, pause int) {
	period = marqueePeriod(segmentsWidth(h.scroll), c.region()-segmentsWidth(h.fixed))
	if period == 0 {
		return 0, 0
	}
	if pause = h.pause; pause > period {
		pause = period
	}
	return period, pause
}

// drawBullet draws the route bullet for the group at the cell origin (x, y): one
// character in the route-sign font, which is all the bullet holds. The LIRR
// gets its own mark instead, a train (see trainFront), so it reads as a
// railroad rather than one more subway line. Any other longer route ID shows
// its first letter.
func drawBullet(d Display, route string, x, y int16) {
	if route == "LIRR" {
		drawTrainMark(d, x+bulletX, y+bulletY)
		return
	}
	filledRoundedRectangle(d, x+bulletX, y+bulletY, bulletSize, bulletSize, bulletRadius, routeColor(route))
	if len(route) > 1 {
		route = route[:1]
	}
	tinyfont.WriteLine(d, &fonts.RouteSign, x+bulletTextX, y+bulletBaseline, route, white)
}

// groupText is one cell's drawable content: everything about a group that does
// NOT change at tick rate. Only the marquee's offset does, and that is derived
// from tick at draw time.
//
// Splitting this out is the whole point of pageText — see its doc.
type groupText struct {
	cell  cell
	route string
	head  headline // classic line 0 (fixed prefix + marqueed remainder)
	etas  string

	// Compact-layout fields, unused by the classic layout: north and dir drive
	// the direction arrow; label is an LIRR trip's destination code, "JAM".
	north bool
	dir   bool
	label string
}

// pageText is a rendered page: one groupText per filled cell, plus the marquee
// pace they share. Rebuilt only when the page or the arrivals data changes.
//
// The rebuild-on-change discipline is load-bearing, not a nicety: the render
// loop ticks at 10 Hz while the data changes once per poll, so composing the
// text per tick would regenerate identical strings hundreds of times between
// changes (trimEtas's strings.Join alone runs repeatedly while fitting) — and
// on TinyGo's non-compacting heap that steady churn is what breaks up the
// contiguous blocks later allocations need (see the repo README, "Memory on TinyGo").
type pageText struct {
	cells []groupText
	pace  pace
}

// build refreshes p for the page starting at cursor. It reuses p.cells' backing
// array, so a rebuild allocates only the strings that actually changed.
func (p *pageText) build(groups []api.FlatArrivalGroup, cursor int, g grid) {
	p.cells = p.cells[:0]
	maxPause, maxRemain := 0, 0
	for i := 0; i < g.cells(); i++ {
		idx := cursor + i
		if idx < 0 || idx >= len(groups) {
			continue // short final page (or the pre-first-page startup sentinel)
		}
		c := g.cell(i)
		h := groupHeadline(groups[idx], c.wide)
		period, pause := headlineMetrics(h, c)
		if pause > maxPause {
			maxPause = pause
		}
		if remain := period - pause; remain > maxRemain {
			maxRemain = remain
		}
		p.cells = append(p.cells, groupText{
			cell:  c,
			route: groups[idx].Route,
			head:  h,
			etas:  etaText(groups[idx].ETAs, c.region()),
		})
	}
	p.pace = marqueePace(maxPause, maxRemain)
}

// drawGroup draws one prebuilt cell: the route bullet, line 0 (the fixed prefix
// plus the marqueed remainder), and line 1 (the ETAs). tick and the page's pace
// drive the marquee, and are the only per-tick inputs.
// Callers must call display.Clear() before and display.Display() after.
func drawGroup(d Display, t groupText, tick int, p pace) {
	c := t.cell
	drawBullet(d, t.route, c.x, c.y)

	x := c.x + textX
	drawSegments(d, t.head.fixed, x, rowY(0)+c.y)
	x += int16(segmentsWidth(t.head.fixed))
	marquee(d, t.head.scroll, t.head.pause, x, rowY(0)+c.y, c.right, tick, p)

	tinyfont.WriteLine(d, &tinyfont.Picopixel, c.x+textX, rowY(1)+c.y, t.etas, gold)
}

// trainFront is the LIRR's mark, a 12x12 train seen head-on in place of a
// bullet: # body, w windshield (left unlit), l headlight, r rails. One row per
// string, top first. It's a generic railroad pictogram, not the LIRR's logo.
var trainFront = [bulletSize]string{
	"...######...",
	".##########.",
	".#wwwwwwww#.",
	".#wwwwwwww#.",
	".#wwwwwwww#.",
	".##########.",
	".##########.",
	".#ll####ll#.",
	".##########.",
	"..########..",
	"..r......r..",
	".r........r.",
}

// trainFrontSmall is trainFront cut down to 7x7 for a compact row, which is
// too short for the full mark.
var trainFrontSmall = [7]string{
	".#####.",
	"#wwwww#",
	"#wwwww#",
	"#######",
	"#l###l#",
	".#####.",
	"r.....r",
}

// drawTrainMark draws the LIRR's mark in a bullet's 12x12 square at (x, y): a
// teal train with white headlights on grey rails. The windshield stays dark.
func drawTrainMark(d Display, x, y int16) { drawTrain(d, x, y, trainFront[:]) }

// drawTrain draws a train pixel map (see trainFront) with its top-left at (x, y).
func drawTrain(d Display, x, y int16, rows []string) {
	for ry, row := range rows {
		for rx := 0; rx < len(row); rx++ {
			var c color.RGBA
			switch row[rx] {
			case '#':
				c = lirrTeal
			case 'l':
				c = white
			case 'r':
				c = grey
			default:
				continue
			}
			d.SetPixel(x+int16(rx), y+int16(ry), c)
		}
	}
}

// filledRoundedRectangle draws a filled rounded rectangle of width w and
// height h at (x, y), with corner radius r clamped to fit.
func filledRoundedRectangle(display Display, x int16, y int16, w int16, h int16, r int16, c color.RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	if r < 0 {
		r = 0
	}
	if r > w/2 {
		r = w / 2
	}
	if r > h/2 {
		r = h / 2
	}

	for cy := y + r; cy < y+h-r; cy++ {
		tinydraw.Line(display, x, cy, x+w-1, cy, c)
	}

	// For each corner row dy in [0, r), find the inset = r - u, where u is
	// the largest non-negative integer with u*u + v*v <= r*r and v = r - dy.
	// u is monotonically non-decreasing as dy grows, so the inner loop is
	// amortized O(r) across the whole outer loop.
	u := int16(0)
	for dy := int16(0); dy < r; dy++ {
		v := r - dy
		threshold := r*r - v*v
		for (u+1)*(u+1) <= threshold {
			u++
		}
		inset := r - u
		if 2*inset >= w {
			continue
		}
		tinydraw.Line(display, x+inset, y+dy, x+w-1-inset, y+dy, c)
		tinydraw.Line(display, x+inset, y+h-1-dy, x+w-1-inset, y+h-1-dy, c)
	}
}

// wrapString splits s into lines that each fit within maxWidth pixels when
// drawn with font.
func wrapString(s string, maxWidth uint32, font *tinyfont.Font) []string {
	var lines []string
	var currentLine string
	currentWidth := uint32(0)

	for _, r := range s {
		_, charWidth := tinyfont.LineWidth(font, string(r))
		if currentWidth+charWidth > maxWidth && currentLine != "" {
			lines = append(lines, currentLine)
			currentLine = ""
			currentWidth = 0
		}
		currentLine += string(r)
		currentWidth += charWidth
	}

	if currentLine != "" {
		lines = append(lines, currentLine)
	}

	return lines
}

// message clears the display and draws msg wrapped to the display width in the
// given colour, also echoing it to the console. Info, Warning, and Error are the
// public entry points.
func message(display Display, msg string, c color.RGBA) {
	display.Clear()
	w, _ := display.Size()
	for i, s := range wrapString(msg, uint32(w), &tinyfont.Picopixel) {
		tinyfont.WriteLine(display, &tinyfont.Picopixel, 0, rowY(i), s, c)
	}
	display.Display()
	println(msg)
}

// Info clears the display and draws an informational message.
func Info(display Display, msg string) { message(display, msg, green) }

// Warning clears the display and draws a warning message.
func Warning(display Display, msg string) { message(display, msg, yellow) }

// Error clears the display and draws an error message.
func Error(display Display, msg string) { message(display, msg, grey) }

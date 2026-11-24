package docks

import (
	"image/color"
	"strconv"
	"strings"
	"time"

	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/api"
)

// The board is one fixed screen, a block per station stacked top to bottom.
// On a 128x64 panel with two stations each block gets half the panel, and draws
// big:
//
//	Willoughby St & Fleet St
//	 ██ ██  docks          37 classic
//	  █ ██                  3 e-bike
//	 ██ ██  of 62
//	▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮▮
//
// The open-dock count is the point of the app, so it's the big number, coloured
// by how sure a return is: green with plenty, amber with a few, red with none.
// The bikes are there for context, quieter, at the right. Under them a bar
// shows the whole station, one cell per dock where the width allows: open docks
// green, then classic bikes, e-bikes, and whatever is out of service.
//
// A block too short for that (a 64x32 panel, or more stations) draws compact:
// the name, then one line of numbers ("19 docks", "40 bikes"), then a thin bar.
//
// Nothing animates. The board redraws only when a poll lands or the data's age
// crosses a threshold.
const (
	bigH     = 28 // a big block: name, a gap, the 15px number, a gap, the bar
	bigMinW  = 96 // narrower than this and the big number and bike counts collide
	compactH = 13 // a compact block without its bar
	barredH  = 16 // a compact block with room for its 2px bar

	bigScale = 3 // the big number is Picopixel at 3x: 9x15 digits

	margin = 1 // blank columns at the panel's left and right
)

var (
	nameC    = color.RGBA{140, 140, 145, 255} // station names: readable, quieter than the numbers
	labelC   = color.RGBA{100, 100, 104, 255} // "docks", "of 62", "classic"
	dim      = color.RGBA{70, 70, 72, 255}    // a closed station's dash and name
	stale    = color.RGBA{110, 110, 110, 255} // numbers once the data is too old to trust
	white    = color.RGBA{255, 255, 255, 255}
	plenty   = color.RGBA{108, 190, 69, 255} // open docks: room to spare
	few      = color.RGBA{255, 140, 0, 255}  // open docks: a handful, may be gone when you get there
	none     = color.RGBA{238, 53, 46, 255}  // open docks: nowhere to return
	amber    = color.RGBA{255, 140, 0, 255}  // the staleness dot
	classicC = color.RGBA{90, 90, 96, 255}   // the bar's classic bikes
	ebikeC   = color.RGBA{40, 100, 210, 255} // the bar's e-bikes
	brokenC  = color.RGBA{70, 16, 16, 255}   // the bar's docks and bikes out of service
)

// fewDocks is the most open docks that still count as "a few": a ride or two
// arriving ahead of you could take them.
const fewDocks = 3

// dockColor is the big number's colour for n open docks.
func dockColor(n int) color.RGBA {
	switch {
	case n == 0:
		return none
	case n <= fewDocks:
		return few
	}
	return plenty
}

// staleness is how far the board trusts its data, as in the clock's board.
type staleness uint8

const (
	fresh    staleness = iota
	late               // a poll or two missed: a small dot in the corner
	outdated           // too old to act on: the numbers grey out
)

// outdatedAfter is when the numbers stop being worth acting on. Docks turn over
// faster than trains run late, so this is shorter than the clock's five minutes.
const outdatedAfter = 4 * time.Minute

// board is the last good poll and what was drawn from it.
type board struct {
	w, h     int16
	poll     time.Duration
	stations []api.BikeStation
	lastOK   time.Time

	dirty    bool
	drawnAge staleness
}

func newBoard(w, h int16, poll time.Duration) *board {
	return &board{w: w, h: h, poll: poll, dirty: true}
}

// update takes a landed poll. stations is the fetcher's buffer, which stays
// untouched until the poll after next, by which time update has replaced it.
func (b *board) update(stations []api.BikeStation, now time.Time) {
	b.stations, b.lastOK, b.dirty = stations, now, true
}

func (b *board) age(now time.Time) staleness {
	since := now.Sub(b.lastOK)
	switch {
	case since >= outdatedAfter:
		return outdated
	case since >= 2*b.poll+15*time.Second:
		return late
	}
	return fresh
}

// render draws the board if anything it shows changed, and reports whether it
// did.
func (b *board) render(d Display, now time.Time) bool {
	age := b.age(now)
	if !b.dirty && age == b.drawnAge {
		return false
	}
	b.dirty, b.drawnAge = false, age
	d.Clear()
	b.draw(d, age)
	d.Display()
	return true
}

// layout splits the panel's height among n stations: each block's height, how
// many blocks fit, and whether they draw big.
func layout(w, h int16, n int) (slot int16, shown int, big bool) {
	if n == 0 {
		return 0, 0, false
	}
	slot = h / int16(n)
	switch {
	case slot >= bigH && w >= bigMinW:
		return slot, n, true
	case slot >= compactH:
		return slot, n, false
	}
	// More stations than fit even compact: show the first ones, in config order.
	return compactH, int(h / compactH), false
}

func (b *board) draw(d Display, age staleness) {
	slot, shown, big := layout(b.w, b.h, len(b.stations))
	shared := sharedStreet(b.stations)
	need := int16(compactH)
	switch {
	case big:
		need = bigH
	case slot >= barredH:
		need = barredH
	}
	for i := 0; i < shown && i < len(b.stations); i++ {
		// Centre each block in its slot, so spare rows fall evenly between them.
		y := int16(i)*slot + (slot-need)/2
		s := &b.stations[i]
		if big {
			drawBig(d, s, shared, y, b.w, age == outdated)
		} else {
			drawCompact(d, s, shared, y, b.w, need == barredH, age == outdated)
		}
	}
	if age != fresh {
		// Top-right corner: a 2px dot, clear of every block's text.
		for dy := int16(0); dy < 2; dy++ {
			for dx := int16(0); dx < 2; dx++ {
				d.SetPixel(b.w-2+dx, dy, amber)
			}
		}
	}
}

// drawName draws a station's name along the top of its block at y, fitted to
// the panel. It leaves room at the right for the staleness dot, which shares
// the first block's name line.
func drawName(d Display, s *api.BikeStation, shared string, y, w int16) {
	name := s.Name
	if name == "" {
		name = "Station " + s.ID
	}
	c := nameC
	if !s.Returning {
		c = dim
	}
	write(d, margin, y+4, label(name, shared, int(w-2*margin-3)), c)
}

// docksWord is the label for n open docks.
func docksWord(n int) string {
	if n == 1 {
		return "dock"
	}
	return "docks"
}

// drawBig draws one station's big block with its top at y.
func drawBig(d Display, s *api.BikeStation, shared string, y, w int16, old bool) {
	known := s.Name != ""
	drawName(d, s, shared, y, w)

	// The big number, with its two labels stacked beside it.
	numTop := y + 7
	label, sub := docksWord(s.Docks), "of "+strconv.Itoa(s.Capacity)
	var numW int16
	if s.Returning {
		c := dockColor(s.Docks)
		if old {
			c = stale
		}
		numW = writeBig(d, margin, numTop, strconv.Itoa(s.Docks), c)
	} else {
		numW = writeBig(d, margin, numTop, "-", dim)
		label, sub = "closed", ""
		if !known {
			label = "no data"
		}
	}
	lx := margin + numW + 2
	write(d, lx, numTop+4, label, labelC)
	if sub != "" && s.Capacity > 0 {
		write(d, lx, numTop+14, sub, labelC)
	}

	// The bikes, right-aligned: numbers ending in a column, labels after.
	if s.Renting {
		labelX := w - margin - max(textWidth("classic"), textWidth("e-bike"))
		numR := labelX - 2
		nc := white
		if old {
			nc = stale
		}
		for row, n := range [2]int{s.Classic, s.EBikes} {
			base := numTop + 4 + int16(row)*10
			t := strconv.Itoa(n)
			write(d, numR-textWidth(t), base, t, nc)
			write(d, labelX, base, [2]string{"classic", "e-bike"}[row], labelC)
		}
	}

	drawBar(d, s, margin, y+25, w-2*margin, 3, old)
}

// drawCompact draws one station's compact block with its top at y: the name,
// then the open docks at the left and the bikes at the right, then (with room)
// a 2px bar.
func drawCompact(d Display, s *api.BikeStation, shared string, y, w int16, bar, old bool) {
	drawName(d, s, shared, y, w)

	base := y + 11
	switch {
	case s.Returning:
		c := dockColor(s.Docks)
		if old {
			c = stale
		}
		x := write(d, margin, base, strconv.Itoa(s.Docks), c)
		write(d, x+2, base, docksWord(s.Docks), labelC)
	case s.Name == "":
		write(d, margin, base, "no data", dim)
	default:
		write(d, margin, base, "closed", dim)
	}
	if s.Renting {
		t := strconv.Itoa(s.Classic + s.EBikes)
		lx := w - margin - textWidth("bikes")
		c := white
		if old {
			c = stale
		}
		write(d, lx-2-textWidth(t), base, t, c)
		write(d, lx, base, "bikes", labelC)
	}
	if bar {
		drawBar(d, s, margin, y+13, w-2*margin, 2, old)
	}
}

// drawBar draws the station as a bar of its docks at (x, y), w wide and h
// tall: open docks, classic bikes, e-bikes, then out of service. With room for
// a 1px gap between docks, each dock is its own cell, so the bar reads as the
// rack itself; otherwise the bar is split in proportion.
func drawBar(d Display, s *api.BikeStation, x, y, w, h int16, old bool) {
	open, classic, ebikes := 0, 0, 0
	if s.Returning {
		open = s.Docks
	}
	if s.Renting {
		classic, ebikes = s.Classic, s.EBikes
	}
	total := max(s.Capacity, open+classic+ebikes)
	if total == 0 {
		return
	}
	openC := plenty
	if old {
		openC = stale
	}
	runs := [4]struct {
		n int
		c color.RGBA
	}{{open, openC}, {classic, classicC}, {ebikes, ebikeC}, {total - open - classic - ebikes, brokenC}}

	pitch := (int(w) + 1) / total // a cell and its gap
	done := 0                     // docks drawn so far
	for _, r := range runs {
		for i := 0; i < r.n; i++ {
			var x0, x1 int // the cell's columns, [x0, x1)
			if pitch >= 2 {
				x0 = done * pitch
				x1 = x0 + pitch - 1
			} else {
				x0 = done * int(w) / total
				x1 = (done + 1) * int(w) / total
			}
			for cx := x0; cx < x1; cx++ {
				for cy := int16(0); cy < h; cy++ {
					d.SetPixel(x+int16(cx), y+cy, r.c)
				}
			}
			done++
		}
	}
}

// write draws s in Picopixel with its baseline at base and returns the x just
// past it. Picopixel's capitals and digits fill the five rows ending at base.
func write(d Display, x, base int16, s string, c color.RGBA) int16 {
	tinyfont.WriteLine(d, &tinyfont.Picopixel, x, base, s, c)
	return x + textWidth(s)
}

func textWidth(s string) int16 {
	_, w := tinyfont.LineWidth(&tinyfont.Picopixel, s)
	return int16(w)
}

// scaled is a Display that draws each pixel as a k×k block of another, with
// its origin at (x, y): Picopixel through it makes the big number, crisp and in
// the same face as the rest of the board.
type scaled struct {
	d    Display
	x, y int16
	k    int16
}

func (s *scaled) Size() (int16, int16) { w, h := s.d.Size(); return w / s.k, h / s.k }
func (s *scaled) Display() error       { return nil }
func (s *scaled) SetPixel(x, y int16, c color.RGBA) {
	for dy := int16(0); dy < s.k; dy++ {
		for dx := int16(0); dx < s.k; dx++ {
			s.d.SetPixel(s.x+x*s.k+dx, s.y+y*s.k+dy, c)
		}
	}
}

// big is the one scaled view the board draws through, reused so a redraw
// doesn't allocate one.
var big scaled

// writeBig draws s at bigScale with its top-left at (x, top) and returns its
// width, trailing blank included.
func writeBig(d Display, x, top int16, s string, c color.RGBA) int16 {
	big = scaled{d: d, x: x, y: top, k: bigScale}
	tinyfont.WriteLine(&big, &tinyfont.Picopixel, 0, 4, s, c)
	return textWidth(s) * bigScale
}

// sharedStreet returns the street every station is on, when they're all named
// for one corner of it: "Willoughby St" for "Willoughby St & Ashland Pl" and
// "Willoughby St & Fleet St". It's "" with fewer than two stations, or when
// they don't share one.
func sharedStreet(stations []api.BikeStation) string {
	if len(stations) < 2 {
		return ""
	}
	a, b, ok := strings.Cut(stations[0].Name, " & ")
	if !ok {
		return ""
	}
	for _, cand := range [2]string{a, b} {
		all := true
		for _, s := range stations[1:] {
			x, y, ok := strings.Cut(s.Name, " & ")
			if !ok || (x != cand && y != cand) {
				all = false
				break
			}
		}
		if all {
			return cand
		}
	}
	return ""
}

// label fits a station's name to max pixels. A name that fits is drawn whole.
// One that doesn't drops the street every station shares, if any, since that
// says nothing about which station this is: on a narrow panel the Willoughby St
// stations read "Ashland Pl" and "Fleet St", not two "Willoughby & …"s. Past
// that, fitName shortens it.
func label(name, shared string, max int) string {
	if int(textWidth(name)) <= max {
		return name
	}
	if shared != "" {
		if a, b, ok := strings.Cut(name, " & "); ok {
			switch shared {
			case a:
				name = b
			case b:
				name = a
			}
		}
	}
	return fitName(name, max)
}

// suffixes are the street-type words fitName drops when a name doesn't fit, but
// only at the end of a street's name ("St Edwards St" keeps its saint).
var suffixes = map[string]bool{"St": true, "Pl": true, "Ave": true, "Av": true, "Blvd": true, "Rd": true, "Pkwy": true, "Dr": true}

// fitName returns name, shortened as needed to fit max pixels: first by dropping
// each street's type ("Willoughby St & Fleet St" -> "Willoughby & Fleet"), then
// by trimming characters.
func fitName(name string, max int) string {
	if int(textWidth(name)) <= max {
		return name
	}
	words := strings.Fields(name)
	kept := words[:0:0]
	for i, wd := range words {
		end := i == len(words)-1 || words[i+1] == "&"
		if end && i > 0 && words[i-1] != "&" && suffixes[wd] {
			continue
		}
		kept = append(kept, wd)
	}
	name = strings.Join(kept, " ")
	for len(name) > 0 && int(textWidth(name)) > max {
		name = strings.TrimRight(name[:len(name)-1], " &")
	}
	return name
}

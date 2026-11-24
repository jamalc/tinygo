package clock

import (
	"image/color"
	"strconv"
	"strings"
	"time"

	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/api"
)

// The board layout is one fixed screen for a cluster of nearby stations, built
// for a 128×64 panel. Every route gets one row holding both directions, rows sit
// in 64px-wide columns, and nothing pages, phases, or scrolls:
//
//	■B ▲ 3 11 ▼ 6 14   │ ■2 ▲ 4 12 ▼ 2  9
//	■Q ▲ 2  9 ▼ 4 12   │ ■3 ▲ 8 16 ▼ 5 15
//	■R ▲ 7 18 ▼ 1 11   │ ■4 ▲ 1 10 ▼ 7 17
//	■A ▲ 5 13 ▼ 8 20   │ ■5 ▲ -    ▼ -
//	■C ▲ 9 21 ▼12      │ ■G ▲ 6 18 ▼10 22
//	■F ▲ 4 14 ▼ 3 13   │ ■ATL>JAM    2 22
//	7:42   ■ 35 ϟ4     ■ 20 ϟ1            ·
//
// Each station's name heads its routes, small; headsigns are left
// out, since around a station cluster the route and its arrow already say where
// a train is going. A station's routes stay together where they fit, in the
// config's station order, so every route keeps its spot and you find the Q by
// position, not by reading.
//
// Each direction's numbers follow a small arrow, so a row reads on its own with
// no header to look up to. An LIRR trip ("ATL>JAM") is a station like any
// other: its origin's name on top, then one row with the destination's full
// name where the chip and route letter would go ("Jamaica"), and the next
// departures in the ▼ field's slots, with no arrow.
//
// The footer holds the time of day in the bottom-right corner, with the
// staleness dot just before it.
//
// The only thing that changes the picture is new information: a poll's numbers,
// the minute on the clock, or the data going stale. The board redraws only then.
const (
	boardColW     = 64              // one column: chip, route, ▲ field, ▼ field
	boardMargin   = 0               // blank pixels above the first row and below the last, and the clock
	boardHeadH    = boardMargin - 1 // where the first row starts: a row's own top line is blank, so this puts its text boardMargin from the top
	boardPitch    = 7               // row pitch: a 5px Picopixel line plus a 2px gap
	boardNameH    = 4               // a station's name above its routes, in Tiny3x3: 3px capitals, a descender line, then the next row's blank top line
	boardETAs     = 2               // ETAs shown per direction: the next train, and the one after
	boardTripETAs = 3               // a trip's departures: it has one direction, so the room the ▲ field would take holds a third
	boardMaxETA   = 99              // the two-digit field's limit; nothing this far out is actionable

	boardChipX  = 1
	boardRouteX = 7

	// The ETA fields are right-aligned slots so a column of numbers lines up:
	// boardSlotR[d][i] is the right edge (exclusive) of direction d's i'th ETA,
	// from the column's left. Direction 0 is ▲ (a …N platform), 1 is ▼.
	// A two-digit number is 7px with its trailing blank, so this leaves 5px
	// between a direction's two numbers — wide enough that "10 18" can't read as
	// a time of day, which a 3px gap did. Each arrow sits 2px from its own
	// numbers and 4px from whatever comes before it — the route letter, or the
	// ▲ field's last digit — so the two directions are spaced the same way and
	// each arrow reads as its numbers'. That leaves 2px at the column's right.
	boardSlotGap = 11

	// leaveWindow is how close to "leave now" an ETA has to be to light up: a
	// train you'd catch only by leaving within this many minutes blinks. At 0,
	// only the train exactly the walk time out blinks.
	leaveWindow = 0

	// blinkHalf is how long a hot ETA spends lit, then at 85% brightness, each blink.
	blinkHalf = 500 * time.Millisecond
)

var boardSlotR = [2]int16{26, 52} // first ETA's right edge per direction; each next is boardSlotGap further

// boardArrowX is each direction's inline arrow, from the column's left: 2px
// before a two-digit first number. boardArrowW is its width; it's 3x2, smaller
// than the compact layout's, so two fit in a row.
var boardArrowX = [2]int16{14, 40}

const boardArrowW = 3

var (
	nameC = color.RGBA{140, 140, 145, 255} // station names: readable, quieter than the routes
)

var (
	dim    = color.RGBA{70, 70, 72, 255}    // markers, dashes, a route with no service
	stale  = color.RGBA{110, 110, 110, 255} // ETAs once the data is too old to trust
	amber  = color.RGBA{255, 140, 0, 255}   // the staleness dot
	clockC = grey
)

// numStr holds "0".."99" so drawing a number never allocates — the board redraws
// on a microcontroller with a non-compacting heap.
var numStr [boardMaxETA + 1]string

func init() {
	for i := range numStr {
		numStr[i] = strconv.Itoa(i)
	}
}

// boardKeep is how many arrivals each direction remembers. More than are shown:
// some are closer than the walk, and between polls (or through an outage) the
// soonest ones count down and drop off, so the next ones need to be on hand.
const boardKeep = 6

// boardDir is one direction of a board row: the next arrivals, soonest first,
// as wall-clock Unix seconds.
type boardDir struct {
	at [boardKeep]int64
	n  int8
}

// boardETA is one number as drawn: minutes out, and how to colour it.
type boardETA struct {
	mins int
	hot  bool // within leaveWindow of the walk: leave now for this one
}

// boardRow is one route at one station, both directions merged; or one LIRR
// trip, whose departures sit in the ▼ direction.
type boardRow struct {
	station string // stop ID without its N/S suffix, e.g. "R30"; a trip's whole ID, "ATL>JAM"
	route   string
	trip    bool   // an LIRR trip: one direction, labelled with label
	label   string // a trip's destination, "Jamaica", drawn in place of the chip and route
	name    string // the station's name, from the server; drawn above its first route
	named   bool   // this row is its station's first, with the name above it at nameY
	nameY   int16
	walk    [2]int // minutes to each platform; see config.Walk
	dirs    [2]boardDir
	x, y    int16 // top-left of the row; set by place
	shown   bool  // false when the panel ran out of rows for it
}

// staleness is how far the board trusts its data.
type staleness uint8

const (
	fresh    staleness = iota
	late               // a poll or two missed: a small dot in the corner
	outdated           // too old to trust: the ETAs grey out
)

// board is the board layout's state: the arrivals from the last good poll, and
// what was on screen last time it drew, so it can skip redraws that change
// nothing.
//
// It keeps arrival times, not minutes, and works the minutes out itself at draw
// time against the current wall-clock minute. So every number ticks down at the
// same instant the clock rolls over, and between polls (or through an outage)
// the board keeps counting rather than freezing. A poll's data shows as soon as
// it lands; a number changes then only if the MTA's prediction moved.
type board struct {
	rows []boardRow
	walk map[string]int // platform stop ID -> minutes' walk; see config.Walk

	cols   int   // columns across the panel
	bottom int16 // the y a row's pitch must end by in the clock's column: the footer row's own y, so the last row keeps the usual 2px gap above it
	height int16 // the panel's height, where the other columns end

	// wallOffset turns the local clock into wall time. It's learned from the
	// server's Date header; until then (haveWall false) the local clock stands in
	// so countdowns still work, but no time of day is shown.
	wallOffset time.Duration
	haveWall   bool

	lastOK    time.Time // when the last poll succeeded
	pollEvery time.Duration

	// The state last drawn. A redraw happens only when one of these changes.
	dirty       bool
	drawnMinute int64 // wall-clock Unix minute last drawn
	drawnAge    staleness
	drawnBlink  bool // whether hot ETAs were lit in the last draw
	drawnHot    bool // whether the last draw had any hot ETA, so the blink matters
}

func newBoard(w, h int16, walk map[string]int, poll time.Duration) *board {
	b := &board{walk: walk, pollEvery: poll, dirty: true, drawnMinute: -1}
	b.cols = min(int(w)/boardColW, 8) // layout tracks up to 8 columns
	if b.cols < 1 {
		b.cols = 1
	}
	b.bottom = clockTop(h) - 1     // the last row's text ends 2px over the clock's, as between rows
	b.height = h + 1 - boardMargin // a row's text ends 2px before its pitch does
	return b
}

// stationOf strips a stop ID's N/S platform suffix, so both platforms of a
// station share one row. An ID without one is its own station.
func stationOf(stopID string) string {
	if hasDirection(stopID) {
		return stopID[:len(stopID)-1]
	}
	return stopID
}

// dirOf is the board direction for a platform: 0 (▲) for a northbound or
// directionless stop, 1 (▼) for a southbound one.
func dirOf(stopID string) int {
	if hasDirection(stopID) && !northbound(stopID) {
		return 1
	}
	return 0
}

// parseMins reads a server ETA like "12m" as minutes. ok is false for anything
// that doesn't start with a number.
func parseMins(s string) (int, bool) {
	n, i := 0, 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	return n, i > 0
}

// wall is the board's best idea of the wall-clock time at local time now.
func (b *board) wall(now time.Time) time.Time { return now.Add(b.wallOffset) }

// update replaces the arrivals with a successful poll's, received at local time
// now. date is the server's clock when it wrote the response (zero if it didn't
// say): it sets the wall clock, and it's the instant the groups' Secs count
// from. A server too old to send Secs still works — its rounded minute strings
// count from the same instant, to within half a minute.
func (b *board) update(groups []api.FlatArrivalGroup, now, date time.Time) {
	if !date.IsZero() {
		b.wallOffset = date.Sub(now)
		b.haveWall = true
	}
	sent := b.wall(now).Unix()

	b.rows = b.rows[:0]
	for _, g := range groups {
		st, dir := stationOf(g.StopID), dirOf(g.StopID)
		dest, trip := tripDest(g.StopID)
		if trip {
			st, dir = g.StopID, 1
			if g.Headsign != "" {
				dest = g.Headsign
			}
		}
		r := b.row(st, g.Route)
		r.trip, r.label = trip, fitLabel(dest)
		r.name = fitName(g.StopName)
		r.walk[dir] = b.walk[g.StopID]
		d := &r.dirs[dir]
		exact := len(g.Secs) == len(g.ETAs)
		for i, e := range g.ETAs {
			var at int64
			if exact {
				at = sent + int64(g.Secs[i])
			} else if m, ok := parseMins(e); ok {
				at = sent + int64(m)*60
			} else {
				continue
			}
			d.insert(at)
		}
	}
	b.place()

	b.lastOK = now
	b.dirty = true
}

// tripDest reports whether stopID is an LIRR trip ("ATL>JAM") and gives its
// destination's code, the stand-in label when the server sent no headsign.
func tripDest(stopID string) (dest string, ok bool) {
	for i := 0; i < len(stopID); i++ {
		if stopID[i] == '>' {
			return stopID[i+1:], true
		}
	}
	return "", false
}

// tripSlotR is the right edge of a trip's first departure, from the column's
// left: its last one lands where a route's last ▼ number does, and the rest
// step back boardSlotGap at a time.
const tripSlotR = 52 - boardSlotGap*(boardTripETAs-boardETAs) // 52 is boardSlotR[1]

// fitLabel cuts a trip's destination to what fits before its first
// departure, with the same 3px a route letter leaves before its numbers.
func fitLabel(dest string) string {
	max := tripSlotR - textWidth("22") - 3 - boardChipX
	for len(dest) > 0 && textWidth(dest) > max {
		dest = dest[:len(dest)-1]
	}
	return dest
}

// row finds the station's row for route, appending a new one if there isn't
// one yet. Rows keep the order the server sent them in, which is the config's
// station order and then route order.
func (b *board) row(station, route string) *boardRow {
	for i := range b.rows {
		if b.rows[i].station == station && b.rows[i].route == route {
			return &b.rows[i]
		}
	}
	b.rows = append(b.rows, boardRow{station: station, route: route})
	return &b.rows[len(b.rows)-1]
}

// insert adds an arrival, keeping the soonest boardKeep in order. Groups split
// by headsign can deliver one direction's arrivals out of order, so this sorts
// rather than appends.
func (d *boardDir) insert(at int64) {
	i := int(d.n)
	for i > 0 && d.at[i-1] > at {
		i--
	}
	if i >= boardKeep {
		return
	}
	end := int(d.n)
	if end == boardKeep {
		end-- // the last one falls off
	}
	for j := end; j > i; j-- {
		d.at[j] = d.at[j-1]
	}
	d.at[i] = at
	if int(d.n) < boardKeep {
		d.n++
	}
}

// upcoming reports whether any train is still to come after wall minute minute.
// A route with none either way is drawn dimmed, as not running.
func (d *boardDir) upcoming(minute int64) bool {
	return d.n > 0 && d.at[d.n-1]/60 > minute
}

// etas works out the numbers to draw during wall minute minute (Unix seconds
// / 60): each train's minutes out, skipping any closer than the walk, up to
// want of them.
//
// A train's minutes out is its arrival minute less the current one. Both are
// whole minutes, so the number holds still all minute and drops by one exactly
// when the clock does. It equals the remaining time rounded to the nearest
// minute as of the middle of the minute, so it's never more than about half a
// minute off — no worse than the server's own rounding.
func (d *boardDir) etas(minute int64, walk, want int) (out [boardTripETAs]boardETA, n int) {
	min := walk
	if min < 1 {
		min = 1 // a train due this minute is at the platform: too late
	}
	for i := 0; i < int(d.n) && n < want; i++ {
		m := int(d.at[i]/60 - minute)
		if m < min {
			continue
		}
		if m > boardMaxETA {
			break
		}
		out[n] = boardETA{mins: m, hot: walk > 0 && m-walk <= leaveWindow}
		n++
	}
	return out, n
}

// place gives each row a position. A station starts with its name, then its
// routes; an LIRR trip is a station too, named for where it leaves from.
// Stations always go in config order, down one column and on to the next, so
// each keeps its place from one poll to the next. A station that won't fit in
// what's left of a column moves whole to the next. Only when keeping stations
// whole would push routes off the panel (a diversion adds routes, say) do they
// split, running on from one column into the next, so a station is never lost
// just to keep another one together.
func (b *board) place() {
	for _, idle := range [2]bool{false, true} {
		if b.layout(false, idle) || b.layout(true, idle) {
			return
		}
	}
}

// idle reports whether a row had no trains at all in the last poll: a route
// not running tonight or this weekend. When space runs out these go first, so
// a dimmed row never costs a running one its place.
func (r *boardRow) idle() bool { return r.dirs[0].n == 0 && r.dirs[1].n == 0 }

// clockTop is the clock's top pixel row: its digits' tallest reach above the
// baseline, in whichever clockFont is set.
func clockTop(h int16) int16 {
	up := 0
	for _, g := range clockFont.Glyphs {
		if g.Rune >= '0' && g.Rune <= '9' && -int(g.YOffset) > up {
			up = -int(g.YOffset)
		}
	}
	return clockBase(h) - int16(up) // a glyph's rows start YOffset above the baseline
}

// clockBase is the footer clock's baseline on a panel h tall: boardMargin
// above the bottom edge, Picopixel's descender line included.
func clockBase(h int16) int16 { return h - 1 - boardMargin }

// colBottom is the y a row's pitch must end by in column c. The last column
// shares the footer row with the clock, so it stops there; the others run to
// the panel's foot, leaving the same 1px margin as the top.
func (b *board) colBottom(c int) int16 {
	if c == b.cols-1 {
		return b.bottom
	}
	return b.height
}

// layout places every row and reports whether all of them landed on the panel.
// Without split, a station that won't fit in what's left of a column starts the
// next one. With split, stations fill the columns in order and carry on at the
// top of the next, without repeating the name: reading down one column and
// into the next, the rows still follow their station. With skipIdle, routes with
// no trains are left off (and don't count against "all"); a station left with
// none takes no space at all, name included.
func (b *board) layout(split, skipIdle bool) bool {
	var ends [8]int16 // each column's next free y; only the first b.cols are used
	for c := range ends {
		ends[c] = boardHeadH
	}
	col := 0
	all := true
	for i := 0; i < len(b.rows); {
		j := i
		for j < len(b.rows) && b.rows[j].station == b.rows[i].station {
			j++
		}
		n := 0 // the block's rows that will be placed
		for k := i; k < j; k++ {
			if skip := skipIdle && b.rows[k].idle(); skip {
				b.rows[k].shown, b.rows[k].named = false, false
			} else {
				n++
			}
		}
		if n == 0 {
			i = j
			continue
		}
		named := b.rows[i].name != ""
		nameH := int16(0)
		if named {
			nameH = boardNameH
		}
		if !split && col < b.cols {
			// Kept whole: a station that won't fit in what's left of this
			// column starts the next one, rather than leaving a route or two
			// behind at the bottom of this one.
			if h := nameH + int16(n)*boardPitch; ends[col] > boardHeadH && ends[col]+h > b.colBottom(col) {
				col++
			}
		}
		first := true
		for k := i; k < j; k++ {
			row := &b.rows[k]
			if skipIdle && row.idle() {
				continue
			}
			head := named && first
			first = false
			// A row that won't fit carries on at the top of the next column,
			// unnamed, like a newspaper column. Kept whole, this only happens
			// to a station taller than a whole column.
			if col < b.cols && ends[col]+rowNeed(head, nameH) > b.colBottom(col) {
				col++
			}
			row.shown = col < b.cols
			all = all && row.shown
			if !row.shown {
				continue
			}
			y := ends[col]
			row.named = head
			if head {
				row.nameY = y
				y += nameH
			}
			row.x = int16(col * boardColW)
			row.y = y
			ends[col] = y + boardPitch
		}
		i = j
	}
	return all
}

// rowNeed is the height a row takes: its pitch, and its station's name above
// it if it's the one carrying it.
func rowNeed(head bool, nameH int16) int16 {
	if head {
		return nameH + boardPitch
	}
	return boardPitch
}

// clockFont is the footer clock's font. The right column above it ends 2px
// over the clock's digits, however tall this font makes them (see clockTop).
var clockFont = &tinyfont.Picopixel

// nameFont is the station names' font: Tiny3x3, a 3px face, so a name costs
// 4px of height instead of a full row's 6.
var nameFont = &tinyfont.Tiny3x3a2pt7b

// nameWidth is name's width in nameFont.
func nameWidth(name string) int {
	_, w := tinyfont.LineWidth(nameFont, name)
	return int(w)
}

// fitName shortens a station name to fit across a column: it
// may use the column's last pixel, since the next column starts with its
// chip's margin. A name that's too long first trades its long words for the
// MTA's own short forms ("Atlantic Terminal" -> "Atlantic Term"), and only
// then is cut.
func fitName(name string) string {
	const max = boardColW - boardChipX + 1 // LineWidth counts a trailing blank
	if nameWidth(name) <= max {
		return name
	}
	words := strings.Split(name, " ")
	for i, w := range words {
		for _, ab := range nameAbbrevs {
			if strings.EqualFold(w, ab[0]) {
				if w == strings.ToUpper(w) {
					words[i] = strings.ToUpper(ab[1])
				} else {
					words[i] = ab[1]
				}
			}
		}
	}
	name = strings.Join(words, " ")
	for len(name) > 0 && nameWidth(name) > max {
		name = name[:len(name)-1]
	}
	return name
}

// nameAbbrevs are the short forms fitName falls back on.
var nameAbbrevs = [][2]string{
	{"Terminal", "Term"}, {"Avenue", "Av"}, {"Street", "St"}, {"Station", "Sta"},
	{"Boulevard", "Blvd"}, {"Parkway", "Pkwy"}, {"Center", "Ctr"}, {"Square", "Sq"},
}

// age grades the data's freshness at now. A poll or two can miss without
// worry; past a couple of intervals the corner dot appears, and after five
// minutes the numbers are too old to act on.
func (b *board) age(now time.Time) staleness {
	if b.lastOK.IsZero() {
		return fresh
	}
	since := now.Sub(b.lastOK)
	switch {
	case since >= 5*time.Minute:
		return outdated
	case since >= 2*b.pollEvery+15*time.Second:
		return late
	}
	return fresh
}

// render draws the board if anything visible changed since the last draw, and
// reports whether it did.
func (b *board) render(d Display, now time.Time) bool {
	wall := b.wall(now)
	minute, age := wall.Unix()/60, b.age(now)
	lit := now.UnixNano()/int64(blinkHalf)%2 == 0
	if !b.dirty && minute == b.drawnMinute && age == b.drawnAge && (!b.drawnHot || lit == b.drawnBlink) {
		return false
	}
	b.dirty, b.drawnMinute, b.drawnAge, b.drawnBlink = false, minute, age, lit

	d.Clear()
	b.drawnHot = b.draw(d, wall, age, lit)
	d.Display()
	return true
}

// draw paints the whole board: header markers, every placed row, then the
// footer clock and staleness dot. Hot ETAs are bright only when lit; draw reports
// whether there were any, so render knows to keep blinking them.
func (b *board) draw(d Display, wall time.Time, age staleness, lit bool) (hot bool) {
	minute := wall.Unix() / 60
	for i := range b.rows {
		r := &b.rows[i]
		if !r.shown {
			continue
		}
		if r.named {
			tinyfont.WriteLine(d, nameFont, r.x+boardChipX, r.nameY+3, r.name, nameC)
		}
		if drawBoardRow(d, r, minute, age == outdated, lit) {
			hot = true
		}
	}

	// The clock sits in the bottom-right corner, its last digit lined up with
	// the right column's last numbers; the staleness dot goes just before it,
	// or in the corner itself while there's no time to show.
	w, h := d.Size()
	base := clockBase(h)
	right := w - (boardColW - boardSlotR[1] - boardSlotGap) // where the last column's numbers end
	x := right
	if b.haveWall {
		t := eastern(wall)
		min := t.Hour()*60 + t.Minute()
		hr := (min / 60) % 12
		if hr == 0 {
			hr = 12
		}
		pad := ""
		if min%60 < 10 {
			pad = "0"
		}
		cw := func(s string) int16 { _, w := tinyfont.LineWidth(clockFont, s); return int16(w) }
		x -= cw(numStr[hr]) + cw(":") + cw(pad) + cw(numStr[min%60])
		cx := x
		for _, part := range [4]string{numStr[hr], ":", pad, numStr[min%60]} {
			tinyfont.WriteLine(d, clockFont, cx, base, part, clockC)
			cx += cw(part)
		}
	}
	if age != fresh {
		x -= 3 // a 2px dot and a 1px gap, on top of the clock's leading blank
		for dy := int16(0); dy < 2; dy++ {
			for dx := int16(0); dx < 2; dx++ {
				d.SetPixel(x+dx, base-2+dy, amber)
			}
		}
	}
	return hot
}

// writeAt draws s at (x, baseline) and returns the x just past it.
func writeAt(d Display, x, base int16, s string, c color.RGBA) int16 {
	tinyfont.WriteLine(d, &tinyfont.Picopixel, x, base, s, c)
	return x + int16(textWidth(s))
}

// drawBoardRow draws one route's row as of wall minute minute: the chip and
// route letter, then each direction's ETAs right-aligned in their slots, or a
// dash when nothing catchable is coming. A route with nothing coming either way
// dims entirely, so service that's off for the night or the weekend fades
// instead of disappearing. A hot ETA (leave now for it) blinks: full gold when
// lit, 85% brightness between. drawBoardRow reports whether the row had one.
func drawBoardRow(d Display, r *boardRow, minute int64, old, lit bool) (hot bool) {
	running := r.dirs[0].upcoming(minute) || r.dirs[1].upcoming(minute)
	label := white
	if !running {
		label = dim
	}
	base := r.y + 5
	if r.trip {
		// The destination stands where the route sign would: "Jamaica".
		writeAt(d, r.x+boardChipX, base, r.label, label)
	} else {
		chip := routeColor(r.route)
		if !running {
			chip = dimmed(chip)
		}
		drawChip(d, r.x+boardChipX, r.y+1, chip)
		writeAt(d, r.x+boardRouteX, base, r.route, label)
	}

	for di := range r.dirs {
		if r.trip && di == 0 {
			continue // a trip runs one way; its label has this field
		}
		if !r.trip {
			drawSmallArrow(d, r.x+boardArrowX[di], r.y+2, di == 0, dim)
		}
		want, right := boardETAs, r.x+boardSlotR[di]
		if r.trip {
			want, right = boardTripETAs, r.x+tripSlotR
		}
		etas, n := r.dirs[di].etas(minute, r.walk[di], want)
		if n == 0 {
			writeAt(d, right-int16(textWidth("-")), base, "-", dim)
			continue
		}
		for i := 0; i < n; i++ {
			e := etas[i]
			s := numStr[e.mins]
			c := gold
			if old {
				c = stale // too old to act on, so nothing urges leaving
			} else if e.hot {
				hot = true
				if !lit {
					c = blinkGold
				}
			}
			edge := right + int16(i*boardSlotGap)
			writeAt(d, edge-int16(textWidth(s)), base, s, c)
		}
	}
	return hot
}

// drawSmallArrow draws a 3x2 direction arrow with its top-left at (x, y):
// pointing up for ▲, down for ▼.
func drawSmallArrow(d Display, x, y int16, up bool, c color.RGBA) {
	rows := [2]uint8{0b010, 0b111}
	if !up {
		rows[0], rows[1] = rows[1], rows[0]
	}
	for ry, bits := range rows {
		for rx := int16(0); rx < boardArrowW; rx++ {
			if bits&(0b100>>rx) != 0 {
				d.SetPixel(x+rx, y+int16(ry), c)
			}
		}
	}
}

// dimmed is a route colour at about a quarter brightness: still recognisably
// that line, but clearly not running.
func dimmed(c color.RGBA) color.RGBA {
	return color.RGBA{c.R / 4, c.G / 4, c.B / 4, 255}
}

// eastern converts t to New York local time. TinyGo has no time zone database,
// so this applies the US rule directly: daylight time from 2am on the second
// Sunday of March to 2am on the first Sunday of November.
func eastern(t time.Time) time.Time {
	u := t.UTC()
	y := u.Year()
	start := nthSunday(y, time.March, 2).Add(7 * time.Hour)  // 2am EST = 07:00 UTC
	end := nthSunday(y, time.November, 1).Add(6 * time.Hour) // 2am EDT = 06:00 UTC
	if !u.Before(start) && u.Before(end) {
		return u.In(edt)
	}
	return u.In(est)
}

var (
	est = time.FixedZone("EST", -5*3600)
	edt = time.FixedZone("EDT", -4*3600)
)

// nthSunday is midnight UTC on the n'th Sunday of month m in year y.
func nthSunday(y int, m time.Month, n int) time.Time {
	first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	off := (7 - int(first.Weekday())) % 7
	return first.AddDate(0, 0, off+7*(n-1))
}

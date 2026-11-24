package clock

import (
	"image/color"

	"tinygo.org/x/tinyfont"

	"github.com/jamalc/tinygo/internal/api"
)

// The compact layout puts one arrival group on a single 8px row:
//
//	[▢] Q ▲ 1m 8 16 24
//
// A colour chip, the route glyph, and a direction arrow open each row, with the
// arrow hugging the glyph; the arrivals then start at the same x on every row.
// Nothing scrolls or swaps: the route and its arrow already say which train it
// is, so the row is just the arrivals — in a row half the height of the classic
// layout's, so a 64×32 panel shows four groups instead of two.
const (
	compactRowH = 8

	// The left column flows left: chip, route glyph, arrow, then text, each hugging
	// the last with a small gap. The route glyph and arrow read as a unit ("Q▲"),
	// so nothing is at a reserved column — the text starts a set gap past the
	// arrow, which puts it a hair further left for a narrow route than a wide one.
	compactChipX         = 1
	compactChipSize      = 5
	compactChipGap       = 2 // chip→route
	compactRouteArrowGap = 1 // route glyph→arrow: tight, so they read as a unit
	compactArrowW        = 5
	compactArrowTextGap  = 2 // arrow→text
)

// compactInset is the x offset (from the cell's left) at which the ETAs
// begin for a group on the given route: past the chip, the route glyph (at its
// actual width), the arrow, and the gaps between them. The arrow's slot is
// always reserved, even for a stop with no direction, so a station's rows line
// up.
func compactInset(route string) int {
	return compactChipX + compactChipSize + compactChipGap +
		textWidth(route) + compactRouteArrowGap + compactArrowW + compactArrowTextGap
}

// compactTrainLabelX is where an LIRR row's destination label starts, from the cell's
// left: past the small train mark and a 2px gap, like a route past its chip.
const compactTrainLabelX = compactChipX + 7 + compactChipGap

// compactTripInset is the x offset at which an LIRR row's ETAs begin: past the
// destination label and the same gap a route's arrow leaves before its ETAs, or where
// a one-letter subway route's begin if that's further right, so a short label
// still lines up with the rows around it.
func compactTripInset(label string) int {
	return max(compactTrainLabelX+textWidth(label)+compactArrowTextGap+1, compactInset("A"))
}

// layoutMode selects the on-display arrangement.
type layoutMode uint8

const (
	layoutClassic layoutMode = iota // two-line cell with a route bullet and marquee
	layoutCompact                   // single-line row: chip, route, arrow, ETAs
	layoutBoard                     // one fixed screen, a row per route with both directions
)

// layoutFromConfig maps the config's layout string to a mode, defaulting to
// classic for "", "classic", or any unrecognized value.
func layoutFromConfig(s string) layoutMode {
	switch s {
	case "compact":
		return layoutCompact
	case "board":
		return layoutBoard
	}
	return layoutClassic
}

// northbound reports whether a stop ID is a northbound platform, from the
// trailing N/S suffix the MTA uses on GTFS stop IDs (e.g. "R30N").
func northbound(stopID string) bool {
	return len(stopID) > 0 && stopID[len(stopID)-1] == 'N'
}

// hasDirection reports whether a stop ID carries a N/S direction suffix at all.
// Without one (some shuttles, a few feeds) the compact row leaves the arrow
// column blank rather than guessing.
func hasDirection(stopID string) bool {
	if stopID == "" {
		return false
	}
	last := stopID[len(stopID)-1]
	return last == 'N' || last == 'S'
}

// compactRegion is the text width available to the ETAs in a compact cell,
// past the left column, which depends on the route glyph's width.
func compactRegion(c cell, route string) int {
	return int(c.right) - int(c.x) - compactInset(route)
}

// drawChip draws the route-colour square that opens a compact row.
func drawChip(d Display, x, y int16, col color.RGBA) {
	for dy := int16(0); dy < compactChipSize; dy++ {
		for dx := int16(0); dx < compactChipSize; dx++ {
			d.SetPixel(x+dx, y+dy, col)
		}
	}
}

// drawArrow draws a solid 5×3 direction triangle at (x, y): pointing up for a
// northbound platform, down for southbound. A triangle alone, no stem — at five
// pixels wide a stem makes the glyph read as a plus sign, and its mirror reads
// as a plus too, so up and down would be indistinguishable.
func drawArrow(d Display, x, y int16, up bool, col color.RGBA) {
	rows := [3]uint8{0b00100, 0b01110, 0b11111} // point at top
	for ry := int16(0); ry < 3; ry++ {
		src := ry
		if !up {
			src = 2 - ry // point at bottom for the down arrow
		}
		bits := rows[src]
		for rx := int16(0); rx < compactArrowW; rx++ {
			if bits&(1<<uint(compactArrowW-1-rx)) != 0 {
				d.SetPixel(x+rx, y+ry, col)
			}
		}
	}
}

// drawCompactGroup draws one prebuilt compact row: the chip, route and arrow,
// then the ETAs in gold, clipped to the cell so they can't bleed into the next
// column.
func drawCompactGroup(d Display, t groupText) {
	c := t.cell
	if t.route == "LIRR" {
		// The LIRR's train mark stands in for the chip, route and arrow; the
		// destination's code ("JAM") follows it, then the ETAs.
		drawTrain(d, c.x+compactChipX, c.y, trainFrontSmall[:])
		lx := c.x + compactTrainLabelX
		tinyfont.WriteLine(d, &tinyfont.Picopixel, lx, rowY(0)+c.y, t.label, white)
		tx := c.x + int16(compactTripInset(t.label))
		tinyfont.WriteLine(clipX{d: d, min: tx, max: c.right}, &tinyfont.Picopixel, tx, rowY(0)+c.y, t.etas, gold)
		return
	}
	drawChip(d, c.x+compactChipX, c.y+1, routeColor(t.route))

	rx := c.x + compactChipX + compactChipSize + compactChipGap
	tinyfont.WriteLine(d, &tinyfont.Picopixel, rx, rowY(0)+c.y, t.route, white)

	if t.dir {
		// The arrow hugs the actual glyph width, so it sits right next to the route.
		ax := rx + int16(textWidth(t.route)) + compactRouteArrowGap
		drawArrow(d, ax, c.y+2, t.north, grey)
	}

	tx := c.x + int16(compactInset(t.route))
	clip := clipX{d: d, min: tx, max: c.right}
	tinyfont.WriteLine(clip, &tinyfont.Picopixel, tx, rowY(0)+c.y, t.etas, gold)
}

// buildCompact fills p.cells for a compact page, composing each row's ETAs once
// per page or poll rather than per tick — the same discipline as pageText's
// marquee path.
func (p *pageText) buildCompact(groups []api.FlatArrivalGroup, cursor int, g grid) {
	p.cells = p.cells[:0]
	for i := 0; i < g.cells(); i++ {
		idx := cursor + i
		if idx < 0 || idx >= len(groups) {
			continue
		}
		c := g.cell(i)
		gr := groups[idx]
		region := compactRegion(c, gr.Route)
		label := ""
		if gr.Route == "LIRR" {
			label, _ = tripDest(gr.StopID) // where it goes: "JAM" for ATL>JAM
			region = int(c.right) - int(c.x) - compactTripInset(label)
		}
		p.cells = append(p.cells, groupText{
			cell:  c,
			route: gr.Route,
			etas:  etaText(gr.ETAs, region),
			label: label,
			north: northbound(gr.StopID),
			dir:   hasDirection(gr.StopID),
		})
	}
	p.pace = pace{} // the compact layout doesn't marquee
}

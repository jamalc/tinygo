package clock

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jamalc/tinygo/internal/api"
)

// stationNames are the GTFS names the server sends for the fixture's stations.
var stationNames = map[string]string{"R30": "DeKalb Av", "A41": "Jay St-MetroTech", "234": "Nevins St", "G36": "Fulton St"}

func boardGroup(stop, route string, etas ...string) api.FlatArrivalGroup {
	return api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: route, ETAs: etas},
		StopID:       stop,
		StopName:     stationNames[stationOf(stop)],
	}
}

// downtownBrooklyn is a realistic flat response for DeKalb, Jay St, Fulton and
// Nevins, in the server's order: stations as the config lists them (R30, A41,
// G36, 234), then route, then N before S.
func downtownBrooklyn() []api.FlatArrivalGroup {
	return []api.FlatArrivalGroup{
		boardGroup("R30N", "B", "3m", "11m", "19m", "27m"), boardGroup("R30S", "B", "6m", "14m", "22m"),
		boardGroup("R30N", "Q", "2m", "9m", "17m", "25m"), boardGroup("R30S", "Q", "4m", "12m", "20m", "28m"),
		boardGroup("R30N", "R", "7m", "18m", "29m"), boardGroup("R30S", "R", "1m", "11m", "21m", "31m"),
		boardGroup("A41N", "A", "5m", "13m", "24m", "33m"), boardGroup("A41S", "A", "8m", "20m", "31m"),
		boardGroup("A41N", "C", "9m", "21m", "33m"), boardGroup("A41S", "C", "12m", "24m"),
		boardGroup("A41N", "F", "4m", "14m", "24m", "34m"), boardGroup("A41S", "F", "3m", "13m", "23m", "33m"),
		boardGroup("G36N", "G", "6m", "18m", "30m"), boardGroup("G36S", "G", "10m", "22m", "34m"),
		boardGroup("234N", "2", "4m", "12m", "20m", "28m"), boardGroup("234S", "2", "2m", "9m", "16m", "23m"),
		boardGroup("234N", "3", "8m", "16m", "24m"), boardGroup("234S", "3", "5m", "15m", "25m"),
		boardGroup("234N", "4", "1m", "10m", "19m", "28m"), boardGroup("234S", "4", "7m", "17m", "27m"),
		boardGroup("234N", "5"), boardGroup("234S", "5"), // weekend: no service
	}
}

var t0 = time.Date(2026, 11, 18, 12, 42, 10, 0, time.UTC) // 7:42am in New York

// minuteOf is t's Unix minute, the unit the board counts down in.
func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

// shown is the minutes a route's direction draws during t's minute, for a walk.
func shown(d *boardDir, t time.Time, walk int) []int { return shownN(d, t, walk, boardETAs) }

// shownN is shown for a row that draws want numbers, as a trip does.
func shownN(d *boardDir, t time.Time, walk, want int) []int {
	etas, n := d.etas(minuteOf(t), walk, want)
	out := make([]int, n)
	for i := range out {
		out[i] = etas[i].mins
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBoardMergesDirections(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(downtownBrooklyn(), t0, t0)

	if len(b.rows) != 11 {
		t.Fatalf("got %d rows, want 11 (one per route)", len(b.rows))
	}
	q := b.rows[1]
	if q.station != "R30" || q.route != "Q" {
		t.Fatalf("row 1 = %s/%s, want R30/Q", q.station, q.route)
	}
	if got := shown(&q.dirs[0], t0, 0); !equalInts(got, []int{2, 9}) {
		t.Errorf("Q ▲ = %v, want the first two: 2, 9", got)
	}
	if got := shown(&q.dirs[1], t0, 0); !equalInts(got, []int{4, 12}) {
		t.Errorf("Q ▼ = %v, want 4, 12", got)
	}
	five := b.rows[10]
	if five.route != "5" || five.dirs[0].n != 0 || five.dirs[1].n != 0 {
		t.Errorf("5 should be a no-service row: %+v", five)
	}
}

// Stations stay whole within a column: DeKalb and Jay St fill the left one,
// and Nevins (four routes) moves to the right rather than splitting.
func TestBoardPlacementKeepsStationsTogether(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	// Nine rows down the left column, which runs to the panel's foot; eight
	// in the right, which stops 2px over the clock.
	if b.cols != 2 || (b.colBottom(0)-boardHeadH)/boardPitch != 9 || (b.colBottom(1)-boardHeadH)/boardPitch != 8 {
		t.Fatalf("grid = %d cols × %d/%d rows, want 2 × 9/8", b.cols, (b.colBottom(0)-boardHeadH)/boardPitch, (b.colBottom(1)-boardHeadH)/boardPitch)
	}
	b.update(downtownBrooklyn(), t0, t0)
	col := map[string]int16{}
	for _, r := range b.rows {
		if !r.shown {
			t.Errorf("%s/%s not shown", r.station, r.route)
		}
		if x, ok := col[r.station]; ok && x != r.x {
			t.Errorf("station %s split across columns", r.station)
		}
		col[r.station] = r.x
	}
	// DeKalb, Jay St and Fulton fill the left column; Nevins is too tall for
	// what they leave, so it starts the right one.
	if col["R30"] != 0 || col["A41"] != 0 || col["234"] != boardColW || col["G36"] != 0 {
		t.Errorf("columns = %v, want DeKalb, Jay St and Fulton left, Nevins right", col)
	}
}

// With a walk time, trains you can't reach are dropped, and the one you'd have
// to leave now for lights up.
func TestBoardWalkFiltersAndHighlights(t *testing.T) {
	// Each platform has its own walk: the southbound one is further.
	b := newBoard(128, 64, map[string]int{"R30N": 4, "R30S": 6}, 30*time.Second)
	b.update([]api.FlatArrivalGroup{
		boardGroup("R30N", "Q", "2m", "4m", "9m", "17m", "25m"),
		boardGroup("R30S", "Q", "5m"), // close enough for ▲'s walk, not ▼'s
	}, t0, t0)
	r := &b.rows[0]
	up, n := r.dirs[0].etas(minuteOf(t0), r.walk[0], boardETAs)
	if n != 2 || up[0].mins != 4 || up[1].mins != 9 {
		t.Errorf("▲ = %+v, want 4, 9 (the 2m is too close to make)", up[:n])
	}
	if !up[0].hot || up[1].hot {
		t.Errorf("▲ = %+v, want only the 4 lit (4-4 ≤ %d)", up[:n], leaveWindow)
	}
	if _, n := r.dirs[1].etas(minuteOf(t0), r.walk[1], boardETAs); n != 0 {
		t.Errorf("▼ shows %d, want nothing catchable in a 6-minute walk", n)
	}
	// Only the walk time itself blinks: a 4-minute walk lights a train 4
	// minutes out (above), but not one 5 minutes out.
	b.update([]api.FlatArrivalGroup{boardGroup("R30N", "Q", "5m")}, t0, t0)
	if up, n := r.dirs[0].etas(minuteOf(t0), r.walk[0], boardETAs); n != 1 || up[0].hot {
		t.Errorf("▲ = %+v, want 5 shown but not lit (5-4 > %d)", up[:n], leaveWindow)
	}
}

// A headsign split delivers a direction's ETAs across several groups; the row
// still ends up with the soonest two, in order.
func TestBoardDirInsertSorts(t *testing.T) {
	var d boardDir
	for _, m := range []int64{12, 3, 30, 7, 1, 40, 22, 9} {
		d.insert(m)
	}
	want := []int64{1, 3, 7, 9, 12, 22}
	for i, w := range want {
		if d.at[i] != w {
			t.Fatalf("kept %v, want the soonest %d in order: %v", d.at[:d.n], boardKeep, want)
		}
	}
}

// The board counts down on its own clock: a number holds all minute and drops
// exactly when the minute rolls over, and between polls the next train moves up
// once the first is too close to make.
func TestBoardCountsDownOnTheMinute(t *testing.T) {
	b := newBoard(128, 64, map[string]int{"R30N": 3}, 30*time.Second)
	// Sent at t0 (12:42:10): trains at 12:46:00 and 12:54:30.
	b.update([]api.FlatArrivalGroup{{
		ArrivalGroup: api.ArrivalGroup{Route: "Q", ETAs: []string{"4m", "12m"}, Secs: []int{230, 740}},
		StopID:       "R30N",
	}}, t0, t0)
	d := &b.rows[0].dirs[0]
	for _, tc := range []struct {
		at   time.Time
		want []int
	}{
		{t0, []int{4, 12}},                       // 12:42:10
		{t0.Add(49 * time.Second), []int{4, 12}}, // 12:42:59 — same minute, same numbers
		{t0.Add(50 * time.Second), []int{3, 11}}, // 12:43:00 — both drop together
		{t0.Add(110 * time.Second), []int{10}},   // 12:44:00 — the 46 is 2 away, under the walk
	} {
		if got := shown(d, tc.at, 3); !equalInts(got, tc.want) {
			t.Errorf("at %s: %v, want %v", tc.at.Format("15:04:05"), got, tc.want)
		}
	}
}

// Secs are exact; the minute strings from an older server are the fallback.
// Both count from the response's Date.
func TestBoardPrefersSecs(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	b.update([]api.FlatArrivalGroup{
		{ArrivalGroup: api.ArrivalGroup{Route: "Q", ETAs: []string{"4m"}, Secs: []int{170}}, StopID: "R30N"},
		{ArrivalGroup: api.ArrivalGroup{Route: "R", ETAs: []string{"4m"}}, StopID: "R30N"},
	}, t0, t0)
	if at := b.rows[0].dirs[0].at[0]; at != t0.Unix()+170 {
		t.Errorf("Q at %d, want sent+170 = %d", at, t0.Unix()+170)
	}
	if at := b.rows[1].dirs[0].at[0]; at != t0.Unix()+240 {
		t.Errorf("R at %d, want sent+4m = %d", at, t0.Unix()+240)
	}
}

func TestBoardStaleness(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(downtownBrooklyn(), t0, t0)
	for _, tc := range []struct {
		after time.Duration
		want  staleness
	}{
		{30 * time.Second, fresh},
		{74 * time.Second, fresh},
		{75 * time.Second, late},
		{5 * time.Minute, outdated},
	} {
		if got := b.age(t0.Add(tc.after)); got != tc.want {
			t.Errorf("age after %v = %d, want %d", tc.after, got, tc.want)
		}
	}
}

// The board redraws only when what it shows changes: new data, the clock's
// minute, or the data's age — never just because a tick went by.
func TestBoardRendersOnlyOnChange(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(downtownBrooklyn(), t0, t0)
	d := newFakePanel(128, 64)
	if !b.render(d, t0) {
		t.Fatal("first render should draw")
	}
	if b.render(d, t0.Add(100*time.Millisecond)) {
		t.Error("an idle tick redrew")
	}
	if !b.render(d, t0.Add(50*time.Second)) {
		t.Error("the minute rolled over (7:42 → 7:43) but nothing redrew")
	}
	b.update(downtownBrooklyn(), t0.Add(55*time.Second), t0.Add(55*time.Second))
	if !b.render(d, t0.Add(55*time.Second)) {
		t.Error("new data didn't redraw")
	}
}

// A train to leave now for blinks: its number is bright, then 85% brightness, then
// bright again, and the board redraws for each half-blink only while one is
// showing.
func TestBoardHotETABlinks(t *testing.T) {
	b := newBoard(128, 64, map[string]int{"R30N": 4}, 30*time.Second)
	b.update([]api.FlatArrivalGroup{boardGroup("R30N", "Q", "4m", "9m")}, t0, t0)
	r := &b.rows[0]
	edge := r.x + boardSlotR[0]
	has := func(d *fakePanel, c color.RGBA) bool { // any pixel of c where the 4 goes
		for x := edge - int16(textWidth("4")); x < edge; x++ {
			for y := r.y; y < r.y+boardPitch; y++ {
				if d.at(x, y) == c {
					return true
				}
			}
		}
		return false
	}
	lit := func(d *fakePanel) bool { return has(d, gold) && !has(d, blinkGold) }
	dark := func(d *fakePanel) bool { return has(d, blinkGold) && !has(d, gold) }
	d := newFakePanel(128, 64)
	if !b.render(d, t0) || !lit(d) {
		t.Fatal("hot 4 not drawn on the lit half")
	}
	if b.render(d, t0.Add(blinkHalf/2)) {
		t.Error("redrew mid-half")
	}
	if !b.render(d, t0.Add(blinkHalf)) || !dark(d) {
		t.Error("hot 4 not dimmed on the dark half")
	}
	if !b.render(d, t0.Add(2*blinkHalf)) || !lit(d) {
		t.Error("hot 4 didn't come back")
	}
}

func TestBoardDimsChipForNoService(t *testing.T) {
	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(downtownBrooklyn(), t0, t0)
	d := newFakePanel(128, 64)
	b.render(d, t0)
	for _, r := range b.rows {
		got := d.at(r.x+boardChipX, r.y+1)
		want := routeColor(r.route)
		if r.route == "5" {
			want = dimmed(want)
		}
		if got != want {
			t.Errorf("%s chip = %v, want %v", r.route, got, want)
		}
	}
}

// When keeping stations whole would push routes off the panel, stations split
// across columns instead; the continuation carries on without the name.
func TestBoardSplitsStationsRatherThanDropThem(t *testing.T) {
	station := func(id string, routes ...string) []api.FlatArrivalGroup {
		var out []api.FlatArrivalGroup
		for _, r := range routes {
			out = append(out, api.FlatArrivalGroup{ArrivalGroup: api.ArrivalGroup{Route: r, ETAs: []string{"5m"}}, StopID: id + "N", StopName: id})
		}
		return out
	}
	// Seven routes, six, then two: whole, the second can't share a column with
	// the first and the third fits beside neither.
	var groups []api.FlatArrivalGroup
	groups = append(groups, station("S1", "A", "B", "C", "D", "E", "F", "G")...)
	groups = append(groups, station("S2", "J", "L", "M", "N", "Q", "R")...)
	groups = append(groups, station("S3", "1", "2")...)

	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(groups, t0, t0)
	cols := map[string]map[int16]bool{}
	heads := map[string]int{}
	for _, r := range b.rows {
		if !r.shown {
			t.Errorf("%s/%s not shown", r.station, r.route)
		}
		if cols[r.station] == nil {
			cols[r.station] = map[int16]bool{}
		}
		cols[r.station][r.x] = true
		if r.named {
			heads[r.station]++
		}
		if r.y+boardPitch > b.colBottom(int(r.x/boardColW)) {
			t.Errorf("%s/%s at (%d,%d) runs past its column's foot", r.station, r.route, r.x, r.y)
		}
	}
	if len(cols["S2"]) != 2 || heads["S2"] != 1 {
		t.Errorf("S2 in %d columns with %d names, want split across 2 with one name", len(cols["S2"]), heads["S2"])
	}
	for _, st := range []string{"S1", "S3"} {
		if len(cols[st]) != 1 || heads[st] != 1 {
			t.Errorf("%s in %d columns with %d names, want whole with one", st, len(cols[st]), heads[st])
		}
	}

	// Today's routes fit whole, so nothing splits.
	b.update(downtownBrooklyn(), t0, t0)
	for _, r := range b.rows {
		if r.named && r.station == "A41" && r.x != 0 {
			t.Errorf("Jay St moved to x=%d without needing to", r.x)
		}
	}
}

// When even splitting can't fit every route, the ones not running go first,
// and the running ones (here the LIRR) keep their place.
func TestBoardDropsIdleRoutesFirst(t *testing.T) {
	groups := downtownBrooklyn() // the 5 has no trains
	extra := func(st string, routes ...string) {
		last := 0
		for i, g := range groups {
			if stationOf(g.StopID) == st {
				last = i
			}
		}
		var add []api.FlatArrivalGroup
		for _, r := range routes {
			add = append(add, boardGroup(st+"N", r, "5m", "15m"), boardGroup(st+"S", r, "7m", "17m"))
		}
		groups = append(append(append([]api.FlatArrivalGroup{}, groups[:last+1]...), add...), groups[last+1:]...)
	}
	// A diversion through DeKalb: kept whole, or split, there's too little room
	// for everything, and it's the idle 5's row that makes the difference.
	extra("R30", "D", "N", "W")
	groups = append(groups, api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: "Jamaica", ETAs: []string{"12m"}},
		StopID:       "ATL>JAM", StopName: "Atlantic Terminal",
	})

	b := newBoard(128, 64, nil, 30*time.Second)
	b.update(groups, t0, t0)
	for _, r := range b.rows {
		switch {
		case r.route == "5" && r.shown:
			t.Error("the idle 5 is shown while space is short")
		case r.route != "5" && !r.shown:
			t.Errorf("%s/%s not shown; only the idle 5 should give way", r.station, r.route)
		}
	}

	// With room to spare, an idle route stays, dimmed.
	b.update(downtownBrooklyn(), t0, t0)
	for _, r := range b.rows {
		if r.route == "5" && !r.shown {
			t.Error("the idle 5 is hidden though everything fits")
		}
	}
}

func TestBoardLIRRTrip(t *testing.T) {
	groups := append(downtownBrooklyn(), api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: "Jamaica", ETAs: []string{"2m", "22m", "42m", "62m", "82m"}, Secs: []int{110, 1310, 2510, 3710, 4910}},
		StopID:       "ATL>JAM",
		StopName:     "Atlantic Terminal",
	})
	b := newBoard(128, 64, map[string]int{"ATL>JAM": 9}, 30*time.Second)
	b.update(groups, t0, t0)

	r := &b.rows[len(b.rows)-1]
	if !r.trip || r.label != "Jamaica" || r.station != "ATL>JAM" {
		t.Fatalf("trip row = %+v, want an ATL>JAM trip labelled Jamaica", r)
	}
	// A station of its own, after Fulton in the right column, named for its origin.
	if !r.shown || r.x != boardColW || !r.named || r.name != "Atlantic Terminal" {
		t.Errorf("trip row at x=%d shown=%v named=%v %q, want the right column under Atlantic Terminal", r.x, r.shown, r.named, r.name)
	}
	// A name too wide for a column trades long words for short forms, keeping its case.
	if got := fitName("ATLANTIC TERMINAL"); got != "ATLANTIC TERM" {
		t.Errorf("ATLANTIC TERMINAL = %q, want ATLANTIC TERM", got)
	}
	if got := fitName("Flatbush Avenue Terminal Station"); !strings.HasPrefix(got, "Flatbush Av Term") || nameWidth(got) > boardColW-boardChipX+1 {
		t.Errorf("long mixed-case name = %q, want it abbreviated (Flatbush Av Term…) and fitting", got)
	}
	// Its departures are the ▼ direction, and the walk to Atlantic drops the 2m.
	if r.dirs[0].n != 0 {
		t.Errorf("▲ has %d trains, want none", r.dirs[0].n)
	}
	// Three departures, not a route's two.
	if got := shownN(&r.dirs[1], t0, r.walk[1], boardTripETAs); !equalInts(got, []int{22, 42, 62}) {
		t.Errorf("departures = %v, want [22 42 62] with a 9-minute walk", got)
	}

	d := newFakePanel(128, 64)
	b.render(d, t0)
	// No chip: where it would be there's only the white of "Jamaica".
	lit := 0
	for y := r.y + 1; y <= r.y+5; y++ {
		for x := r.x + boardChipX; x < r.x+boardChipX+compactChipSize; x++ {
			switch c := d.at(x, y); c {
			case white:
				lit++
			case color.RGBA{}:
			default:
				t.Fatalf("pixel (%d,%d) = %v in the chip's place, want only white text", x, y, c)
			}
		}
	}
	if lit == 0 {
		t.Error("nothing drawn where the chip would be, want the start of Jamaica")
	}
	// No dash in the ▲ field: the label is there instead.
	dash := r.x + boardSlotR[0] - int16(textWidth("-"))
	if got := d.at(dash, r.y+3); got == dim {
		t.Errorf("▲ field has a dash at x=%d", dash)
	}

	// Without a headsign the destination's code stands in.
	b.update([]api.FlatArrivalGroup{{ArrivalGroup: api.ArrivalGroup{Route: "LIRR", ETAs: []string{"5m"}}, StopID: "PSN>JAM"}}, t0, t0)
	if r := b.rows[0]; r.label != "JAM" || r.named {
		t.Errorf("headsign-less trip = label %q named %v, want JAM with no name", r.label, r.named)
	}
	if _, ok := tripDest("R30N"); ok {
		t.Error("a subway stop read as a trip")
	}
	// A long destination is cut to end before a two-digit ▼ number.
	if l := fitLabel("Ronkonkoma Station"); l == "" || boardChipX+textWidth(l)+3 > tripSlotR-textWidth("22") {
		t.Errorf("long destination cut to %q, which runs into the ▼ field", l)
	}
}

func TestEastern(t *testing.T) {
	for _, tc := range []struct {
		utc  time.Time
		want string
	}{
		{time.Date(2026, 1, 15, 17, 0, 0, 0, time.UTC), "12:00 EST"},
		{time.Date(2026, 7, 4, 16, 0, 0, 0, time.UTC), "12:00 EDT"},
		// 2026: DST starts Sunday March 8 at 2am EST (07:00 UTC) and ends
		// Sunday November 1 at 2am EDT (06:00 UTC).
		{time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC), "01:59 EST"},
		{time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC), "03:00 EDT"},
		{time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC), "01:59 EDT"},
		{time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC), "01:00 EST"},
	} {
		if got := eastern(tc.utc).Format("15:04 MST"); got != tc.want {
			t.Errorf("eastern(%v) = %s, want %s", tc.utc, got, tc.want)
		}
	}
}

// TestBoardSnapshot writes the sample board to a PNG for eyeballing when
// BOARD_PNG names a path. It isn't an assertion; it's how the layout gets
// looked at without a panel.
func TestBoardSnapshot(t *testing.T) {
	path := os.Getenv("BOARD_PNG")
	if path == "" {
		t.Skip("set BOARD_PNG=out.png to write a snapshot")
	}
	walk := map[string]int{}
	for st, m := range map[string]int{"R30": 3, "A41": 5, "234": 4, "G36": 7} {
		walk[st+"N"], walk[st+"S"] = m, m
	}
	walk["ATL>JAM"] = 9
	b := newBoard(128, 64, walk, 30*time.Second)
	b.update(append(downtownBrooklyn(), api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: "Jamaica", ETAs: []string{"2m", "13m", "33m", "58m"}},
		StopID:       "ATL>JAM",
		StopName:     "Atlantic Terminal",
	}), t0, t0)
	d := newFakePanel(128, 64)
	b.render(d, t0.Add(80*time.Second)) // late: shows the dot
	writePNG(t, d, path)
}

// writePNG renders a fake panel as LEDs at 8x, for looking at a layout.
func writePNG(t *testing.T, d *fakePanel, path string) {
	const scale = 8
	img := image.NewRGBA(image.Rect(0, 0, 128*scale, 64*scale))
	for y := 0; y < 64*scale; y++ {
		for x := 0; x < 128*scale; x++ {
			c := color.RGBA{12, 12, 12, 255} // unlit LED
			if x%scale != 0 && y%scale != 0 {
				if p, ok := d.px[[2]int16{int16(x / scale), int16(y / scale)}]; ok {
					c = p
				}
			} else {
				c = color.RGBA{0, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

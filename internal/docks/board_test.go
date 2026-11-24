package docks

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/simulator"
)

// willoughby is both ends of Willoughby St between Ashland Pl and Fleet St, as
// the server sends them: Ashland nearly full (one dock open), Fleet with room.
func willoughby() []api.BikeStation {
	return []api.BikeStation{
		{ID: "4587.02", Name: "Willoughby St & Ashland Pl", Classic: 11, EBikes: 15, Renting: true, Docks: 1, Capacity: 31, Returning: true},
		{ID: "4628.05", Name: "Willoughby St & Fleet St", Classic: 37, EBikes: 3, Renting: true, Docks: 19, Capacity: 62, Returning: true},
	}
}

var t0 = time.Date(2026, 11, 21, 16, 40, 0, 0, time.UTC)

func drawn(t *testing.T, w, h int, stations []api.BikeStation, now time.Time) *simulator.Display {
	t.Helper()
	d := simulator.New(w, h)
	b := newBoard(int16(w), int16(h), 30*time.Second)
	b.update(stations, t0)
	if !b.render(d, now) {
		t.Fatal("first render drew nothing")
	}
	return d
}

// count is how many pixels in rows [y0, y1) are exactly c.
func count(d *simulator.Display, y0, y1 int, c color.RGBA) int {
	n := 0
	for y := y0; y < y1; y++ {
		for x := 0; x < d.Width(); x++ {
			if d.GetPixel(int16(x), int16(y)) == c {
				n++
			}
		}
	}
	return n
}

// cells is how many separate runs of colour c the bar row y holds: on a bar
// drawn one cell per dock, the docks of that kind.
func cells(d *simulator.Display, y int, c color.RGBA) int {
	n, in := 0, false
	for x := 0; x < d.Width(); x++ {
		on := d.GetPixel(int16(x), int16(y)) == c
		if on && !in {
			n++
		}
		in = on
	}
	return n
}

func TestLayout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		w, h      int16
		n         int
		slot      int16
		shown     int
		big       bool
		wantBlock string
	}{
		{"two on 128x64 draw big", 128, 64, 2, 32, 2, true, ""},
		{"one on 128x64 draws big", 128, 64, 1, 64, 1, true, ""},
		{"two on 64x32 draw compact", 64, 32, 2, 16, 2, false, ""},
		{"two on a narrow tall panel draw compact", 64, 64, 2, 32, 2, false, ""},
		{"three on 128x64 draw compact", 128, 64, 3, 21, 3, false, ""},
		{"too many show what fits", 128, 64, 6, compactH, 4, false, ""},
		{"none", 128, 64, 0, 0, 0, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slot, shown, big := layout(tc.w, tc.h, tc.n)
			if slot != tc.slot || shown != tc.shown || big != tc.big {
				t.Errorf("layout(%d, %d, %d) = %d, %d, %v; want %d, %d, %v", tc.w, tc.h, tc.n, slot, shown, big, tc.slot, tc.shown, tc.big)
			}
		})
	}
}

func TestFitName(t *testing.T) {
	for _, tc := range []struct {
		name string
		max  int
		want string
	}{
		{"Willoughby St & Fleet St", 200, "Willoughby St & Fleet St"},
		{"Willoughby St & Fleet St", 80, "Willoughby & Fleet"},
		{"Myrtle Ave & St Edwards St", 90, "Myrtle & St Edwards"}, // the saint stays
		{"Willoughby St & Ashland Pl", 60, "Willoughby & Ash"},    // trimmed, no dangling "&"
		{"Washington Park", 30, "Washing"},
	} {
		got := fitName(tc.name, tc.max)
		if got != tc.want {
			t.Errorf("fitName(%q, %d) = %q, want %q", tc.name, tc.max, got, tc.want)
		}
		if w := int(textWidth(got)); w > tc.max {
			t.Errorf("fitName(%q, %d) = %q, %dpx wide", tc.name, tc.max, got, w)
		}
	}
}

func TestLabel(t *testing.T) {
	const shared = "Willoughby St"
	if got := sharedStreet(willoughby()); got != shared {
		t.Errorf("sharedStreet = %q, want %q", got, shared)
	}
	other := append(willoughby(), api.BikeStation{Name: "Myrtle Ave & Fleet Pl"})
	if got := sharedStreet(other); got != "" {
		t.Errorf("sharedStreet with a Myrtle station = %q, want none", got)
	}
	if got := sharedStreet(willoughby()[:1]); got != "" {
		t.Errorf("sharedStreet of one station = %q, want none", got)
	}

	for _, tc := range []struct {
		name, shared string
		max          int
		want         string
	}{
		{"Willoughby St & Fleet St", shared, 200, "Willoughby St & Fleet St"}, // fits: whole
		{"Willoughby St & Fleet St", shared, 60, "Fleet St"},                  // the cross street
		{"Willoughby St & Ashland Pl", shared, 60, "Ashland Pl"},
		{"Ashland Pl & Willoughby St", shared, 60, "Ashland Pl"}, // either side
		{"Willoughby St & Ashland Pl", "", 60, "Willoughby & Ash"},
	} {
		if got := label(tc.name, tc.shared, tc.max); got != tc.want {
			t.Errorf("label(%q, %q, %d) = %q, want %q", tc.name, tc.shared, tc.max, got, tc.want)
		}
	}
}

func TestBigBoard(t *testing.T) {
	d := drawn(t, 128, 64, willoughby(), t0)

	// Each block is 28 rows centred in a 32-row slot: Ashland at 2, Fleet at 34.
	// The big number fills rows 7..21 of its block. Ashland's one open dock is
	// amber; Fleet's nineteen are green, and nothing in Fleet's number is amber.
	if n := count(d, 2+7, 2+22, few); n == 0 {
		t.Error("Ashland's 1 open dock isn't drawn amber")
	}
	if n := count(d, 34+7, 34+22, plenty); n == 0 {
		t.Error("Fleet's 19 open docks aren't drawn green")
	}
	if n := count(d, 34+7, 34+22, few); n != 0 {
		t.Errorf("Fleet has %d amber pixels in its number, want none", n)
	}

	// The bars draw one cell per dock: 31 and 62 both leave room for a gap.
	for _, tc := range []struct {
		station  string
		y        int
		open, cl int
		eb, gone int
	}{
		{"Ashland", 2 + 25, 1, 11, 15, 31 - 1 - 26},
		{"Fleet", 34 + 25, 19, 37, 3, 62 - 19 - 40},
	} {
		got := [4]int{cells(d, tc.y, plenty), cells(d, tc.y, classicC), cells(d, tc.y, ebikeC), cells(d, tc.y, brokenC)}
		want := [4]int{tc.open, tc.cl, tc.eb, tc.gone}
		// Adjacent cells of one kind are separate runs only because of the gap,
		// which is the claim: the bar is the rack, a cell per dock.
		if got != want {
			t.Errorf("%s bar cells (open, classic, e-bike, out) = %v, want %v", tc.station, got, want)
		}
	}

	// Fresh data: no staleness dot.
	if c := d.GetPixel(127, 0); c == amber {
		t.Error("staleness dot drawn on fresh data")
	}
}

func TestClosedStation(t *testing.T) {
	stations := willoughby()
	stations[0] = api.BikeStation{ID: "4587.02", Name: "Willoughby St & Ashland Pl", Capacity: 31} // neither renting nor returning
	stations[1] = api.BikeStation{ID: "9999.99"}                                                   // unknown to the server
	d := drawn(t, 128, 64, stations, t0)
	for _, c := range []color.RGBA{plenty, few, none, white} {
		if n := count(d, 0, 64, c); n != 0 {
			t.Errorf("closed and unknown stations drew %d pixels of %v, want none", n, c)
		}
	}
	// The closed station's bar is all out of service: its 31 docks, nothing open.
	if n := cells(d, 2+25, brokenC); n != 31 {
		t.Errorf("closed station's bar has %d out-of-service cells, want 31", n)
	}
}

func TestNoDocksIsRed(t *testing.T) {
	stations := willoughby()
	stations[0].Docks = 0
	d := drawn(t, 128, 64, stations, t0)
	if n := count(d, 2+7, 2+22, none); n == 0 {
		t.Error("a full station's 0 isn't drawn red")
	}
}

func TestCompactBoard(t *testing.T) {
	d := drawn(t, 64, 32, willoughby(), t0)
	// The compact number line sits at rows 7..11 of each 16-row block.
	if n := count(d, 7, 12, few); n == 0 {
		t.Error("Ashland's 1 open dock isn't drawn amber")
	}
	if n := count(d, 16+7, 16+12, plenty); n == 0 {
		t.Error("Fleet's 19 open docks aren't drawn green")
	}
	// 62 docks don't fit 62px with gaps, so Fleet's bar is proportional: 19 of
	// 62 open docks is 19*62/62 = 19 columns, give or take one at each edge.
	if n := count(d, 16+13, 16+14, plenty); n < 18 || n > 20 {
		t.Errorf("Fleet's bar has %d open columns, want about 19", n)
	}
	// Everything stays inside the panel's 64 columns: nothing drawn at row 15 or
	// 31, the gaps under the bars.
	if n := count(d, 15, 16, plenty) + count(d, 31, 32, plenty); n != 0 {
		t.Errorf("%d pixels in the gaps under the bars", n)
	}
}

func TestStaleness(t *testing.T) {
	d := simulator.New(128, 64)
	b := newBoard(128, 64, 30*time.Second)
	b.update(willoughby(), t0)
	b.render(d, t0)

	// Nothing new: no redraw.
	if b.render(d, t0.Add(10*time.Second)) {
		t.Error("redrew with nothing changed")
	}

	// Two polls missed: the dot appears, the numbers keep their colour.
	if !b.render(d, t0.Add(80*time.Second)) {
		t.Fatal("didn't redraw when the data went late")
	}
	if c := d.GetPixel(127, 0); c != amber {
		t.Errorf("corner = %v, want the amber staleness dot", c)
	}
	if n := count(d, 34+7, 34+22, plenty); n == 0 {
		t.Error("late data lost its colour; only outdated data should")
	}

	// Too old to act on: the numbers and open docks grey out. (Rows 0..1 hold
	// the staleness dot, which is amber too.)
	b.render(d, t0.Add(outdatedAfter))
	for _, c := range []color.RGBA{plenty, few} {
		if n := count(d, 2, 64, c); n != 0 {
			t.Errorf("outdated board still has %d pixels of %v", n, c)
		}
	}

	// A fresh poll brings it all back.
	now := t0.Add(outdatedAfter + time.Second)
	b.update(willoughby(), now)
	b.render(d, now)
	if c := d.GetPixel(127, 0); c == amber {
		t.Error("staleness dot survived a fresh poll")
	}
}

// TestSnapshot writes the boards as PNGs, scaled up, for looking at:
//
//	DOCKS_PNG=/tmp/docks go test ./internal/docks -run Snapshot
func TestSnapshot(t *testing.T) {
	prefix := os.Getenv("DOCKS_PNG")
	if prefix == "" {
		t.Skip("set DOCKS_PNG=/path/prefix to write snapshots")
	}
	for _, sz := range [][2]int{{128, 64}, {64, 32}} {
		d := drawn(t, sz[0], sz[1], willoughby(), t0)
		const k = 6
		img := image.NewRGBA(image.Rect(0, 0, sz[0]*k, sz[1]*k))
		for y := 0; y < sz[1]*k; y++ {
			for x := 0; x < sz[0]*k; x++ {
				c := color.RGBA{12, 12, 12, 255} // an unlit LED
				if x%k != 0 && y%k != 0 {
					if p := d.GetPixel(int16(x/k), int16(y/k)); p != (color.RGBA{}) {
						c = p
					}
				}
				img.Set(x, y, c)
			}
		}
		path := prefix + "-" + itoa(sz[0]) + "x" + itoa(sz[1]) + ".png"
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		f.Close()
		t.Log("wrote", path)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

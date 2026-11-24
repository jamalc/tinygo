package arrivals

import (
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jamalc/tinygo/internal/api"
	gtfsstatic "github.com/jamalc/tinygo/internal/gtfs"
)

// apiGroup keeps the assertions readable.
type apiGroup = api.ArrivalGroup

// fakeSource is a test entitySource.
type fakeSource struct{ entities []*gtfs.FeedEntity }

func (f fakeSource) AllEntities() []*gtfs.FeedEntity { return f.entities }

// arrival builds a FeedEntity for one route/trip arriving at stopID in `mins`.
func arrival(routeID, tripID, stopID string, mins int) *gtfs.FeedEntity {
	t := time.Now().Unix() + int64(mins*60)
	return &gtfs.FeedEntity{
		TripUpdate: &gtfs.TripUpdate{
			Trip: &gtfs.TripDescriptor{
				RouteId: proto.String(routeID),
				TripId:  proto.String(tripID),
			},
			StopTimeUpdate: []*gtfs.TripUpdate_StopTimeUpdate{{
				StopId:  proto.String(stopID),
				Arrival: &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(t)},
			}},
		},
	}
}

// staticFixture returns static data with route metadata for A, C, Q, R.
func staticFixture() *gtfsstatic.StaticData {
	s := gtfsstatic.NewStaticData()
	s.Stops["A27"] = "Franklin Av"
	s.Routes["A"] = gtfsstatic.Route{ID: "A", LongName: "8 Avenue", Color: "0039A6"}
	s.Routes["C"] = gtfsstatic.Route{ID: "C", LongName: "8 Avenue Local", Color: "0039A6"}
	s.Routes["Q"] = gtfsstatic.Route{ID: "Q", LongName: "2 Av", Color: "FCCC0A"}
	s.Routes["R"] = gtfsstatic.Route{ID: "R", LongName: "4 Av Local", Color: "FCCC0A"}
	return s
}

// byRoute indexes one stop's Groups by route for assertions.
func byRoute(rows []apiGroup) map[string]apiGroup {
	m := make(map[string]apiGroup, len(rows))
	for _, r := range rows {
		m[r.Route] = r
	}
	return m
}

// req builds a StopRequest with the given pins and mutes (mute wins on overlap,
// mirroring parseStopParam).
func req(id string, pins, mutes []string) StopRequest {
	sr := StopRequest{StopID: id, Pins: map[string]bool{}, Mutes: map[string]bool{}}
	for _, p := range pins {
		sr.Pins[p] = true
	}
	for _, m := range mutes {
		sr.Mutes[m] = true
		delete(sr.Pins, m)
	}
	return sr
}

func newTestEngine(s *gtfsstatic.StaticData, ents ...*gtfs.FeedEntity) *Engine {
	return &Engine{static: s, poller: fakeSource{ents}}
}

func groups(e *Engine, sr StopRequest) map[string]apiGroup {
	return byRoute(e.GetArrivals([]StopRequest{sr}, false, 0)[0].Groups)
}

func TestGetArrivals_NoFilterShowsAllArrivals(t *testing.T) {
	e := newTestEngine(staticFixture(), arrival("A", "t1", "A27N", 3), arrival("C", "t2", "A27N", 7))
	got := groups(e, req("A27N", nil, nil))
	if len(got) != 2 || len(got["A"].ETAs) == 0 || len(got["C"].ETAs) == 0 {
		t.Errorf("no filter should show all arriving routes; got %+v", got)
	}
}

func TestGetArrivals_PinNoServiceWhenAbsent(t *testing.T) {
	// Pin Q at A27; R is arriving but Q is not -> R arrival + Q "No service".
	e := newTestEngine(staticFixture(), arrival("R", "t1", "A27N", 4))
	got := groups(e, req("A27N", []string{"Q"}, nil))
	if r := got["R"]; len(r.ETAs) == 0 {
		t.Errorf("R should be a live arrival; got %+v", r)
	}
	if q, ok := got["Q"]; !ok || len(q.ETAs) != 0 {
		t.Errorf("Q should be a no-service row (empty ETAs); got %+v ok=%v", q, ok)
	}
}

func TestGetArrivals_PinArrivingNotDuplicated(t *testing.T) {
	// Pin Q and Q is arriving -> one live Q row, no extra no-service row.
	e := newTestEngine(staticFixture(), arrival("Q", "t1", "A27N", 2))
	rows := e.GetArrivals([]StopRequest{req("A27N", []string{"Q"}, nil)}, false, 0)[0].Groups
	if len(rows) != 1 || rows[0].Route != "Q" || len(rows[0].ETAs) == 0 {
		t.Errorf("pinned arriving Q should be a single live row; got %+v", rows)
	}
}

func TestGetArrivals_MuteSuppressesArriving(t *testing.T) {
	// Mute R; both Q and R arriving -> only Q.
	e := newTestEngine(staticFixture(), arrival("Q", "t1", "A27N", 3), arrival("R", "t2", "A27N", 5))
	got := groups(e, req("A27N", nil, []string{"R"}))
	if _, ok := got["R"]; ok {
		t.Errorf("muted R must not appear")
	}
	if _, ok := got["Q"]; !ok {
		t.Errorf("Q should still show")
	}
}

func TestGetArrivals_MuteWinsOverPin(t *testing.T) {
	// Q is both pinned and muted -> mute wins: no Q row even though it's absent.
	e := newTestEngine(staticFixture(), arrival("R", "t1", "A27N", 4))
	got := groups(e, req("A27N", []string{"Q"}, []string{"Q"}))
	if _, ok := got["Q"]; ok {
		t.Errorf("Q is muted; should not appear as a no-service row")
	}
}

func TestGetArrivals_HorizonBoundsETAs(t *testing.T) {
	// Q trains at 5, 40 and 80 minutes out.
	e := newTestEngine(staticFixture(),
		arrival("Q", "t1", "A27N", 5),
		arrival("Q", "t2", "A27N", 40),
		arrival("Q", "t3", "A27N", 80),
	)

	for _, tc := range []struct {
		name    string
		horizon time.Duration
		want    []string
	}{
		{"default drops the 80m train", 0, []string{"5m", "40m"}},
		{"30m keeps only the 5m train", 30 * time.Minute, []string{"5m"}},
		{"90m keeps all three", 90 * time.Minute, []string{"5m", "40m", "80m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := e.GetArrivals([]StopRequest{req("A27N", nil, nil)}, false, tc.horizon)
			got := byRoute(rows[0].Groups)["Q"].ETAs
			if len(got) != len(tc.want) {
				t.Fatalf("horizon %v: got %v, want %v", tc.horizon, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("horizon %v: got %v, want %v", tc.horizon, got, tc.want)
					break
				}
			}
		})
	}
}

func TestGetArrivals_BothDirectionsPairsRoutes(t *testing.T) {
	// N runs only northbound, Q both ways. With both platforms requested, N gets a
	// southbound "No service" row so each route shows as an N/S pair.
	s := staticFixture()
	s.Routes["N"] = gtfsstatic.Route{ID: "N", LongName: "Broadway Express", Color: "FCCC0A"}
	e := newTestEngine(s,
		arrival("N", "t1", "R30N", 4),
		arrival("Q", "t2", "R30N", 6),
		arrival("Q", "t3", "R30S", 8),
	)
	res := e.GetArrivals([]StopRequest{req("R30N", nil, nil), req("R30S", nil, nil)}, false, 0)
	north, south := byRoute(res[0].Groups), byRoute(res[1].Groups)
	if len(north) != 2 || len(south) != 2 {
		t.Fatalf("want N and Q on both platforms; north=%+v south=%+v", north, south)
	}
	if g := south["N"]; len(g.ETAs) != 0 || g.Headsign != "Southbound" {
		t.Errorf("southbound N should be a No service row; got %+v", g)
	}
	if len(south["Q"].ETAs) == 0 || len(north["N"].ETAs) == 0 {
		t.Errorf("arriving rows should keep their ETAs; north=%+v south=%+v", north, south)
	}
}

func TestGetArrivals_PairingRespectsMuteAndNeedsBothPlatforms(t *testing.T) {
	ents := []*gtfs.FeedEntity{arrival("N", "t1", "R30N", 4)}
	e := newTestEngine(staticFixture(), ents...)

	// Southbound mutes N: no paired row.
	res := e.GetArrivals([]StopRequest{req("R30N", nil, nil), req("R30S", nil, []string{"N"})}, false, 0)
	if len(res[1].Groups) != 0 {
		t.Errorf("mute should suppress the paired row; got %+v", res[1].Groups)
	}

	// Only one platform requested: nothing is paired.
	res = e.GetArrivals([]StopRequest{req("R30N", nil, nil), req("A27S", nil, nil)}, false, 0)
	if len(res[1].Groups) != 0 {
		t.Errorf("unrelated stop should get no paired rows; got %+v", res[1].Groups)
	}
}

func TestGetArrivals_DropsImminentTrains(t *testing.T) {
	now := arrival("A", "t1", "A27N", 0) // 0s out: would round to 0m
	soon := arrival("A", "t2", "A27N", 4)
	got := groups(newTestEngine(staticFixture(), now, soon), req("A27N", nil, nil))
	if etas := got["A"].ETAs; len(etas) != 1 || etas[0] != "4m" {
		t.Errorf("imminent train should be dropped; got %v", etas)
	}
}

func TestGetArrivals_ExpressVariantMergesWithBaseRoute(t *testing.T) {
	// FX (F express) trips belong in the F group, not a separate FX row.
	e := newTestEngine(staticFixture(), arrival("F", "t1", "A27N", 3), arrival("FX", "t2", "A27N", 6))
	got := groups(e, req("A27N", nil, nil))
	if _, ok := got["FX"]; ok {
		t.Errorf("FX must not get its own row; got %+v", got)
	}
	if f := got["F"]; len(f.ETAs) != 2 {
		t.Errorf("F row should carry both the F and FX trips; got %+v", f)
	}
}

// Secs carries each ETA in seconds, parallel to the minute strings, so a client
// with a clock can count down itself.
func TestGetArrivals_SecsParallelETAs(t *testing.T) {
	e := newTestEngine(staticFixture(), arrival("A", "t1", "A27N", 3), arrival("A", "t2", "A27N", 9))
	a := groups(e, req("A27N", nil, nil))["A"]
	if len(a.Secs) != len(a.ETAs) || len(a.Secs) != 2 {
		t.Fatalf("secs %v should parallel etas %v", a.Secs, a.ETAs)
	}
	for i, want := range []int{180, 540} {
		if d := a.Secs[i] - want; d < -2 || d > 2 {
			t.Errorf("secs[%d] = %d, want about %d", i, a.Secs[i], want)
		}
	}
}

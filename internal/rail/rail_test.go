package rail

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"
)

const stopsCSV = `"stop_id","stop_code","stop_name","stop_lat","stop_lon"
"102","JAM","Jamaica","40.69","-73.80"
"148","NAV","Nostrand Avenue","40.67","-73.94"
"241","ATL","Atlantic Terminal","40.68","-73.97"
`

// stu is one stop on a trip: id, and departure (or arrival when dep is 0).
type stu struct {
	id       string
	arr, dep int64
	skipped  bool
}

func trip(id string, cancelled bool, stops ...stu) *gtfs.FeedEntity {
	tu := &gtfs.TripUpdate{Trip: &gtfs.TripDescriptor{TripId: proto.String(id)}}
	if cancelled {
		tu.Trip.ScheduleRelationship = gtfs.TripDescriptor_CANCELED.Enum()
	}
	for _, s := range stops {
		u := &gtfs.TripUpdate_StopTimeUpdate{StopId: proto.String(s.id)}
		if s.arr != 0 {
			u.Arrival = &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(s.arr)}
		}
		if s.dep != 0 {
			u.Departure = &gtfs.TripUpdate_StopTimeEvent{Time: proto.Int64(s.dep)}
		}
		if s.skipped {
			u.ScheduleRelationship = gtfs.TripUpdate_StopTimeUpdate_SKIPPED.Enum()
		}
		tu.StopTimeUpdate = append(tu.StopTimeUpdate, u)
	}
	return &gtfs.FeedEntity{Id: proto.String(id), TripUpdate: tu}
}

func TestDepartures(t *testing.T) {
	now := int64(1_800_000_000)
	m := func(n int64) int64 { return now + n*60 }
	feed := &gtfs.FeedMessage{Entity: []*gtfs.FeedEntity{
		// Out of order on purpose: the result is sorted.
		trip("later", false, stu{id: "241", dep: m(22)}, stu{id: "148", arr: m(25)}, stu{id: "102", arr: m(43)}),
		trip("next", false, stu{id: "241", dep: m(2)}, stu{id: "102", arr: m(23)}),
		// Inbound: reaches Jamaica first, then Atlantic. Not a trip to Jamaica.
		trip("inbound", false, stu{id: "102", dep: m(5)}, stu{id: "241", arr: m(26)}),
		trip("cancelled", false, stu{id: "241", dep: m(12)}, stu{id: "102", arr: m(33)}),
		// Skips Jamaica (a diversion): doesn't get you there.
		trip("skips", false, stu{id: "241", dep: m(15)}, stu{id: "102", arr: m(36), skipped: true}),
		// Already pulling out, and past the horizon.
		trip("gone", false, stu{id: "241", dep: now + 10}, stu{id: "102", arr: m(21)}),
		trip("far", false, stu{id: "241", dep: m(70)}, stu{id: "102", arr: m(91)}),
		// No departure time at the origin: the arrival stands in.
		trip("arronly", false, stu{id: "241", arr: m(40)}, stu{id: "102", arr: m(61)}),
	}}
	feed.Entity[3] = trip("cancelled", true, stu{id: "241", dep: m(12)}, stu{id: "102", arr: m(33)})

	got := departures(feed, "241", "102", now, 60*60)
	want := []int{2 * 60, 22 * 60, 40 * 60}
	if len(got) != len(want) {
		t.Fatalf("departures = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("departures = %v, want %v", got, want)
		}
	}
}

func TestPollerDepartures(t *testing.T) {
	stops, err := parseStops(strings.NewReader(stopsCSV))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	feed := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{GtfsRealtimeVersion: proto.String("2.0")},
		Entity: []*gtfs.FeedEntity{trip("t", false, stu{id: "241", dep: now + 300}, stu{id: "102", arr: now + 1560})},
	}
	body, err := proto.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()

	p := NewPoller(srv.URL, stops)

	// Before any fetch: the group is there, with no trains.
	if g := p.Departures("ATL", "JAM", time.Hour); len(g.ETAs) != 0 || g.StopID != "ATL>JAM" {
		t.Errorf("before a fetch = %+v, want ATL>JAM with no ETAs", g)
	}
	if err := p.Fetch(); err != nil {
		t.Fatal(err)
	}
	// Codes are case-insensitive, and stop IDs work as well as codes.
	for _, q := range [][2]string{{"ATL", "JAM"}, {"atl", "jam"}, {"241", "102"}} {
		g := p.Departures(q[0], q[1], time.Hour)
		if len(g.ETAs) != 1 || g.ETAs[0] != "5m" || len(g.Secs) != 1 || g.Secs[0] < 295 || g.Secs[0] > 300 {
			t.Errorf("%v: ETAs %v secs %v, want one train in 5m", q, g.ETAs, g.Secs)
		}
		if g.Route != Route || g.StopName != "Atlantic Terminal" || g.Headsign != "Jamaica" || g.StopID != q[0]+">"+q[1] {
			t.Errorf("%v: group = %+v", q, g)
		}
	}
	// An unknown station gives an empty group rather than an error.
	if g := p.Departures("ATL", "XYZ", time.Hour); len(g.ETAs) != 0 {
		t.Errorf("unknown station = %+v, want no ETAs", g)
	}
}

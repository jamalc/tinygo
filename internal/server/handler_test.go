package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/arrivals"
	"github.com/jamalc/tinygo/internal/gtfs"
	"github.com/jamalc/tinygo/internal/realtime"
)

func TestFlatten(t *testing.T) {
	// Two stops, out of name order and with unsorted routes within a stop.
	// Stations keep their request order (Bravo first), not alphabetical.
	byStop := []api.StopArrivals{
		{StopID: "B12N", StopName: "Bravo", Groups: []api.ArrivalGroup{
			{Route: "N", Headsign: "Astoria"},
			{Route: "A", Headsign: "Inwood"},
		}},
		{StopID: "A27N", StopName: "Alpha", Groups: []api.ArrivalGroup{
			{Route: "Q", Headsign: "96 St"},
		}},
	}

	got := flatten(byStop)

	// Request order, then route: Bravo/A, Bravo/N, Alpha/Q.
	want := []struct{ stop, route string }{
		{"Bravo", "A"},
		{"Bravo", "N"},
		{"Alpha", "Q"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].StopName != w.stop || got[i].Route != w.route {
			t.Errorf("group %d: got %s/%s, want %s/%s", i, got[i].StopName, got[i].Route, w.stop, w.route)
		}
	}
	// Stop ID is folded in alongside the name.
	if got[2].StopID != "A27N" {
		t.Errorf("stop ID not folded in: %+v", got[0])
	}
}

func TestFlattenDirection(t *testing.T) {
	// One station, two platforms (same name) sharing routes. Each route's N/S
	// platforms should stay adjacent, northbound (…N) before southbound (…S).
	byStop := []api.StopArrivals{
		{StopID: "R30S", StopName: "DeKalb Av", Groups: []api.ArrivalGroup{
			{Route: "Q", Headsign: "Coney Island"},
			{Route: "B", Headsign: "Brighton Beach"},
		}},
		{StopID: "R30N", StopName: "DeKalb Av", Groups: []api.ArrivalGroup{
			{Route: "Q", Headsign: "96 St"},
			{Route: "B", Headsign: "Bedford Park Blvd"},
		}},
	}

	got := flatten(byStop)

	// Sorted by route, then direction: B(N), B(S), Q(N), Q(S).
	want := []struct{ route, stopID string }{
		{"B", "R30N"},
		{"B", "R30S"},
		{"Q", "R30N"},
		{"Q", "R30S"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Route != w.route || got[i].StopID != w.stopID {
			t.Errorf("group %d: got %s/%s, want %s/%s", i, got[i].Route, got[i].StopID, w.route, w.stopID)
		}
	}
}

func TestFlattenStationOrder(t *testing.T) {
	// A station's platforms listed apart still sort together, at the position of
	// whichever was requested first: R30N, A27N, R30S -> R30 then A27.
	byStop := []api.StopArrivals{
		{StopID: "R30N", StopName: "DeKalb Av", Groups: []api.ArrivalGroup{{Route: "Q"}}},
		{StopID: "A27N", StopName: "Franklin Av", Groups: []api.ArrivalGroup{{Route: "A"}}},
		{StopID: "R30S", StopName: "DeKalb Av", Groups: []api.ArrivalGroup{{Route: "Q"}}},
	}
	got := flatten(byStop)
	want := []string{"R30N", "R30S", "A27N"}
	for i, w := range want {
		if got[i].StopID != w {
			t.Errorf("group %d: got %s, want %s", i, got[i].StopID, w)
		}
	}
}

func TestParseStopParam(t *testing.T) {
	sr := parseStopParam("A27N:Q,!R,!N")
	if sr.StopID != "A27N" {
		t.Errorf("stopID: got %q", sr.StopID)
	}
	if len(sr.Pins) != 1 || !sr.Pins["Q"] {
		t.Errorf("pins: got %v, want {Q}", sr.Pins)
	}
	if len(sr.Mutes) != 2 || !sr.Mutes["R"] || !sr.Mutes["N"] {
		t.Errorf("mutes: got %v, want {R,N}", sr.Mutes)
	}

	// Tokens are upper-cased and mute wins when a route is both.
	both := parseStopParam("r30n:q,!q")
	if len(both.Pins) != 0 || !both.Mutes["Q"] {
		t.Errorf("mute should win: pins=%v mutes=%v", both.Pins, both.Mutes)
	}

	// No colon means no tokens (every arriving route shows).
	bare := parseStopParam("635N")
	if len(bare.Pins) != 0 || len(bare.Mutes) != 0 {
		t.Errorf("bare stop should have no tokens; got pins=%v mutes=%v", bare.Pins, bare.Mutes)
	}
}

func TestParseHorizonParam(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", arrivals.DefaultHorizon, false},
		{"1", time.Minute, false},
		{"90", 90 * time.Minute, false},
		{"180", 180 * time.Minute, false},
		{"181", 0, true},
		{"0", 0, true},
		{"-5", 0, true},
		{"45.5", 0, true},
		{"soon", 0, true},
	} {
		got, err := parseHorizonParam(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("horizon=%q: want error, got %v", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("horizon=%q: unexpected error %v", tc.raw, err)
		} else if got != tc.want {
			t.Errorf("horizon=%q: got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestWriteJSONSendsContentLength pins the property the device client depends on:
// a length header and no chunked encoding.
//
// This is not a cosmetic assertion. Without Content-Length, net/http chunks any
// response larger than its 4 KB buffer, and the TinyGo client then has to grow its
// read buffer by doubling because it cannot know the body size in advance. TinyGo
// rounds a growing slice to the next power of two and never compacts the heap, so
// that doubling is what OOMs the board — on contiguity, not on total free bytes.
// A regression here is invisible on the server and fatal on the device, which is
// exactly the kind of thing worth a test.
func TestWriteJSONSendsContentLength(t *testing.T) {
	// Comfortably past net/http's 4 KB auto-buffer, so a missing length header
	// would show up as chunked rather than being silently absorbed.
	payload := make([]api.FlatArrivalGroup, 400)
	for i := range payload {
		payload[i] = api.FlatArrivalGroup{
			ArrivalGroup: api.ArrivalGroup{
				Route: "Q", Color: "#FCCC0A", RouteName: "Broadway Express",
				Headsign: "96 St-2 Av", ETAs: []string{"2", "9", "17"},
			},
			StopID: "R30N", StopName: "Atlantic Av-Barclays Ctr",
		}
	}

	// A real server, not httptest.NewRecorder: the recorder never runs net/http's
	// transfer-encoding logic, so it would report no chunking whether or not the
	// header was set. Only a real request/response exercises the thing being pinned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, payload)
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) < 4096 {
		t.Fatalf("payload is %d B, too small to exercise net/http's 4 KB chunking threshold", len(body))
	}

	if res.ContentLength < 0 {
		t.Fatal("no Content-Length: the client cannot size its read buffer and will grow it by doubling")
	}
	if int(res.ContentLength) != len(body) {
		t.Errorf("Content-Length = %d, want %d (the body's actual size)", res.ContentLength, len(body))
	}
	for _, e := range res.TransferEncoding {
		if e == "chunked" {
			t.Error("response is chunked; the device client must then de-chunk into a second full-size buffer")
		}
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

type fakeBikes map[string]api.BikeStation

func (f fakeBikes) Stations(ids []string) []api.BikeStation {
	out := make([]api.BikeStation, len(ids))
	for i, id := range ids {
		out[i] = f[id]
		out[i].ID = id
	}
	return out
}

func TestArrivalsBoardFormat(t *testing.T) {
	engine := arrivals.NewEngine(gtfs.NewStaticData(), realtime.NewPoller())
	bikes := fakeBikes{"4628.05": {Classic: 35, EBikes: 4, Renting: true}}

	for _, tc := range []struct {
		name  string
		bikes BikeSource
		want  []api.BikeStation
	}{
		{"with bikes", bikes, []api.BikeStation{{ID: "4628.05", Classic: 35, EBikes: 4, Renting: true}, {ID: "4587.02"}}},
		{"no source", nil, []api.BikeStation{{ID: "4628.05"}, {ID: "4587.02"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			NewHandler(engine, gtfs.NewStaticData(), tc.bikes, nil).RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", "/arrivals?stop=R30N&format=board&bike=4628.05&bike=4587.02", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var got api.Board
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Bikes) != len(tc.want) {
				t.Fatalf("bikes = %+v, want %+v", got.Bikes, tc.want)
			}
			for i := range tc.want {
				if got.Bikes[i] != tc.want[i] {
					t.Errorf("bike %d = %+v, want %+v", i, got.Bikes[i], tc.want[i])
				}
			}
		})
	}

	// No bike= at all still sends an empty list, not null, so a client reusing
	// its decode target never keeps the previous poll's stations.
	mux := http.NewServeMux()
	NewHandler(engine, gtfs.NewStaticData(), bikes, nil).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/arrivals?stop=R30N&format=board", nil))
	if !strings.Contains(rec.Body.String(), `"bikes":[]`) {
		t.Errorf("body = %s, want an empty bikes list", rec.Body)
	}
}

func TestBikes(t *testing.T) {
	engine := arrivals.NewEngine(gtfs.NewStaticData(), realtime.NewPoller())
	fleet := api.BikeStation{Name: "Willoughby St & Fleet St", Classic: 37, EBikes: 3, Renting: true, Docks: 19, Capacity: 62, Returning: true}
	bikes := fakeBikes{"4628.05": fleet}
	fleet.ID = "4628.05"

	for _, tc := range []struct {
		name  string
		bikes BikeSource
		want  []api.BikeStation
	}{
		{"with bikes", bikes, []api.BikeStation{{ID: "4587.02"}, fleet}},
		{"no source", nil, []api.BikeStation{{ID: "4587.02"}, {ID: "4628.05"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			NewHandler(engine, gtfs.NewStaticData(), tc.bikes, nil).RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", "/bikes?station=4587.02&station=4628.05", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("Content-Length") == "" {
				t.Error("no Content-Length: the device reads into one exactly-sized buffer")
			}
			var got []api.BikeStation
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("stations = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("station %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}

	// Asking for nothing is a mistake, not an empty list.
	mux := http.NewServeMux()
	NewHandler(engine, gtfs.NewStaticData(), bikes, nil).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/bikes", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no station= status = %d, want 400", rec.Code)
	}
}

type fakeRail struct{ secs []int }

func (f fakeRail) Departures(from, to string, _ time.Duration) api.FlatArrivalGroup {
	g := api.FlatArrivalGroup{ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: "Jamaica", Secs: f.secs}, StopID: from + ">" + to, StopName: "Atlantic Terminal"}
	for _, s := range f.secs {
		g.ETAs = append(g.ETAs, strconv.Itoa((s+30)/60)+"m")
	}
	return g
}

func TestArrivalsLIRR(t *testing.T) {
	engine := arrivals.NewEngine(gtfs.NewStaticData(), realtime.NewPoller())
	get := func(rail RailSource, query string) (*httptest.ResponseRecorder, []api.FlatArrivalGroup) {
		mux := http.NewServeMux()
		NewHandler(engine, gtfs.NewStaticData(), nil, rail).RegisterRoutes(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/arrivals?"+query, nil))
		var groups []api.FlatArrivalGroup
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
		return rec, groups
	}

	// A lirr= trip alone is enough, and comes back as one flat group after the stops.
	rec, groups := get(fakeRail{secs: []int{120, 1320}}, "stop=R30N&lirr=ATL%3EJAM&format=flat")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	last := groups[len(groups)-1]
	if last.StopID != "ATL>JAM" || last.Route != "LIRR" || strings.Join(last.ETAs, " ") != "2m 22m" || len(last.Secs) != 2 {
		t.Errorf("LIRR group = %+v", last)
	}

	// No rail source: the trip still shows, with no trains.
	if _, groups := get(nil, "lirr=ATL%3EJAM&format=flat"); len(groups) != 1 || groups[0].StopID != "ATL>JAM" || len(groups[0].ETAs) != 0 {
		t.Errorf("without a rail source = %+v, want one empty ATL>JAM group", groups)
	}

	for _, bad := range []string{"lirr=ATL", "lirr=%3EJAM", "lirr=ATL%3E", "lirr=A%3EB%3EC"} {
		if rec, _ := get(nil, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

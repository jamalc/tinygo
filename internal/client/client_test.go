package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/config"
)

func TestLoadArrivals_DecodesFlatAndRequestsFormat(t *testing.T) {
	// The server returns the flat, pre-sorted list (format=flat); the client
	// decodes it as-is rather than flattening on the device.
	body := `[
		{"stop_id":"A27N","stop_name":"Franklin Av","route":"A","color":"0039A6","route_name":"8 Avenue","headsign":"Inwood","etas":["3m","9m"]},
		{"stop_id":"A27N","stop_name":"Franklin Av","route":"C","color":"0039A6","route_name":"8 Avenue Local","headsign":"Northbound","etas":[]},
		{"stop_id":"A27N","stop_name":"Franklin Av","route":"F","color":"FF6319","route_name":"Queens Blvd","headsign":"Coney Is","etas":["2m"]}
	]`
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Write([]byte(body))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.Split(host, ":")
	port := 0
	if len(parts) == 2 {
		port = atoiTest(t, parts[1])
	}
	c := NewClient(config.Config{Host: parts[0], Port: port, Stops: []string{"A27N"}})

	var dst api.Board
	var buf []byte
	date, err := c.LoadArrivals(&dst, &buf)
	groups := dst.Groups
	if err != nil {
		t.Fatalf("LoadArrivals: %v", err)
	}
	// net/http's server always sends a Date header; it's the device's only wall clock.
	if date.IsZero() || time.Since(date) > time.Minute || time.Until(date) > time.Minute {
		t.Errorf("date = %v, want the server's clock (about now)", date)
	}
	if gotQuery.Get("format") != "flat" {
		t.Errorf("client should request format=flat; got query %v", gotQuery)
	}
	if gotQuery.Get("stop") != "A27N" {
		t.Errorf("client should request stop=A27N; got query %v", gotQuery)
	}
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3", len(groups))
	}
	byRoute := map[string]int{}
	for i, g := range groups {
		byRoute[g.Route] = i
	}
	if g := groups[byRoute["C"]]; len(g.ETAs) != 0 {
		t.Errorf("C should be a no-service row (empty etas), got %v", g.ETAs)
	}
	if groups[byRoute["A"]].StopName != "Franklin Av" {
		t.Errorf("stop name not attached: %+v", groups[byRoute["A"]])
	}
}

func TestLoadArrivals_BoardWithBikes(t *testing.T) {
	body := `{"groups":[{"stop_id":"R30N","route":"Q","etas":["3m"],"secs":[170]}],
		"bikes":[{"id":"4628.05","name":"Willoughby St & Fleet St","classic":35,"ebikes":4,"renting":true},
		         {"id":"4587.02","name":"Willoughby St & Ashland Pl","classic":0,"ebikes":0,"renting":false}]}`
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Write([]byte(body))
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c := NewClient(config.Config{Host: host, Port: atoiTest(t, port), Stops: []string{"R30N"}, LIRR: []string{"ATL>JAM"}, Bikes: []string{"4628.05", "4587.02"}})

	// A reused target holding a previous poll's three stations.
	dst := api.Board{Bikes: make([]api.BikeStation, 3)}
	var buf []byte
	if _, err := c.LoadArrivals(&dst, &buf); err != nil {
		t.Fatalf("LoadArrivals: %v", err)
	}
	if gotQuery.Get("format") != "board" || strings.Join(gotQuery["bike"], " ") != "4628.05 4587.02" || gotQuery.Get("lirr") != "ATL>JAM" {
		t.Errorf("query = %v, want format=board, both bike= stations in order, and lirr=ATL>JAM", gotQuery)
	}
	if len(dst.Groups) != 1 || dst.Groups[0].Route != "Q" {
		t.Errorf("groups = %+v, want the one Q group", dst.Groups)
	}
	want := []api.BikeStation{
		{ID: "4628.05", Name: "Willoughby St & Fleet St", Classic: 35, EBikes: 4, Renting: true},
		{ID: "4587.02", Name: "Willoughby St & Ashland Pl"},
	}
	if len(dst.Bikes) != len(want) {
		t.Fatalf("bikes = %+v, want %+v", dst.Bikes, want)
	}
	for i := range want {
		if dst.Bikes[i] != want[i] {
			t.Errorf("bike %d = %+v, want %+v", i, dst.Bikes[i], want[i])
		}
	}
}

func TestLoadStations(t *testing.T) {
	// The second station leaves its counts out entirely, as a server that
	// doesn't know it might.
	body := `[{"id":"4587.02","name":"Willoughby St & Ashland Pl","classic":11,"ebikes":15,"renting":true,"docks":1,"capacity":31,"returning":true},
		{"id":"4628.05"}]`
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		w.Write([]byte(body))
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c := NewBikeClient(config.Config{Host: host, Port: atoiTest(t, port), Stops: []string{"R30N"}, Bikes: []string{"4587.02", "4628.05"}})

	// A reused target holding a previous poll's three stations, every field set.
	old := api.BikeStation{ID: "old", Name: "Old", Classic: 9, EBikes: 9, Renting: true, Docks: 9, Capacity: 9, Returning: true}
	dst := []api.BikeStation{old, old, old}
	var buf []byte
	if _, err := c.LoadStations(&dst, &buf); err != nil {
		t.Fatalf("LoadStations: %v", err)
	}
	if gotPath != "/bikes" || strings.Join(gotQuery["station"], " ") != "4587.02 4628.05" || gotQuery.Has("stop") {
		t.Errorf("request = %s?%v, want /bikes with both station= in order and no stops", gotPath, gotQuery)
	}
	want := []api.BikeStation{
		{ID: "4587.02", Name: "Willoughby St & Ashland Pl", Classic: 11, EBikes: 15, Renting: true, Docks: 1, Capacity: 31, Returning: true},
		{ID: "4628.05"}, // nothing of the old station's survives the fields it left out
	}
	if len(dst) != len(want) {
		t.Fatalf("stations = %+v, want %+v", dst, want)
	}
	for i := range want {
		if dst[i] != want[i] {
			t.Errorf("station %d = %+v, want %+v", i, dst[i], want[i])
		}
	}
}

func atoiTest(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestNewClient_HorizonParam(t *testing.T) {
	for _, tc := range []struct {
		name    string
		horizon time.Duration
		want    string // "" means the param must be absent
	}{
		{"unset defers to the server", 0, ""},
		{"sub-minute defers to the server", 30 * time.Second, ""},
		{"whole minutes", 90 * time.Minute, "90"},
		{"hours convert to minutes", 2 * time.Hour, "120"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(config.Config{Host: "h", Port: 8080, Stops: []string{"A27N"}, Horizon: tc.horizon})
			u, err := url.Parse(c.url)
			if err != nil {
				t.Fatalf("parse %q: %v", c.url, err)
			}
			q := u.Query()
			if tc.want == "" {
				if q.Has("horizon") {
					t.Errorf("horizon %v: want no param, got %q", tc.horizon, q.Get("horizon"))
				}
				return
			}
			if got := q.Get("horizon"); got != tc.want {
				t.Errorf("horizon %v: got %q, want %q", tc.horizon, got, tc.want)
			}
		})
	}
}

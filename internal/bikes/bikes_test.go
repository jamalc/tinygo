package bikes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// feeds serves a tiny GBFS directory: a live station with e-bikes, one that's
// installed and taking returns but not renting, a dead one that hasn't reported
// in months, and one with a status but no short number.
func feeds(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	info := `{"data":{"stations":[
		{"station_id":"uuid-fleet","short_name":"4628.05","name":"Willoughby St & Fleet St","capacity":62},
		{"station_id":"uuid-ashland","short_name":"4587.02","name":"Willoughby St & Ashland Pl","capacity":31},
		{"station_id":"uuid-dead","short_name":"4513.06","name":"DeKalb Ave & Hudson Ave","capacity":0},
		{"station_id":"uuid-noshort","name":"Somewhere"}
	]}}`
	live := now.Add(-2 * time.Minute).Unix()
	status := fmt.Sprintf(`{"data":{"stations":[
		{"station_id":"uuid-fleet","num_bikes_available":39,"num_ebikes_available":4,"num_docks_available":19,"is_installed":1,"is_renting":1,"is_returning":1,"last_reported":%d},
		{"station_id":"uuid-ashland","num_bikes_available":21,"num_ebikes_available":1,"num_docks_available":8,"is_installed":1,"is_renting":0,"is_returning":1,"last_reported":%d},
		{"station_id":"uuid-dead","num_bikes_available":0,"num_ebikes_available":0,"num_docks_available":5,"is_installed":0,"is_renting":0,"is_returning":1,"last_reported":1700000000}
	]}}`, live, live)
	mux := http.NewServeMux()
	mux.HandleFunc("/station_information.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(info)) })
	mux.HandleFunc("/station_status.json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(status)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestStations(t *testing.T) {
	srv := feeds(t, time.Now())
	p := NewPoller(srv.URL + "/")
	if err := p.Fetch(); err != nil {
		t.Fatal(err)
	}
	got := p.Stations([]string{"4628.05", "4587.02", "4513.06", "9999.99"})
	if len(got) != 4 {
		t.Fatalf("got %d stations, want 4", len(got))
	}

	// E-bikes are part of the feed's bike count: 39 bikes with 4 e-bikes is 35 classic.
	if s := got[0]; !s.Renting || s.Classic != 35 || s.EBikes != 4 || s.Name != "Willoughby St & Fleet St" || s.ID != "4628.05" {
		t.Errorf("Fleet St = %+v, want renting 35 classic + 4 e-bikes", s)
	}
	if s := got[0]; !s.Returning || s.Docks != 19 || s.Capacity != 62 {
		t.Errorf("Fleet St = %+v, want returning, 19 of 62 docks open", s)
	}
	// Installed but not renting: no bikes to take, whatever the dock holds, but
	// it still takes bikes back.
	if s := got[1]; s.Renting || s.Classic != 0 || s.EBikes != 0 {
		t.Errorf("Ashland Pl = %+v, want not renting, no bikes", s)
	}
	if s := got[1]; !s.Returning || s.Docks != 8 || s.Capacity != 31 {
		t.Errorf("Ashland Pl = %+v, want returning, 8 of 31 docks open", s)
	}
	// A dead station and an unknown number both come back neither renting nor
	// returning, in place, whatever the feed claims for the dead one's docks.
	for i, id := range []string{"4513.06", "9999.99"} {
		if s := got[2+i]; s.Renting || s.Returning || s.Docks != 0 || s.ID != id {
			t.Errorf("%s = %+v, want not renting or returning", id, s)
		}
	}
}

func TestStationsSilentIsNotRenting(t *testing.T) {
	// A station that says it's renting but last reported two hours ago isn't trusted.
	srv := feeds(t, time.Now().Add(-2*time.Hour))
	p := NewPoller(srv.URL + "/")
	if err := p.Fetch(); err != nil {
		t.Fatal(err)
	}
	if s := p.Stations([]string{"4628.05"})[0]; s.Renting || s.Returning || s.Docks != 0 {
		t.Errorf("silent station = %+v, want not renting or returning", s)
	}
}

func TestFetchKeepsDataOnError(t *testing.T) {
	srv := feeds(t, time.Now())
	p := NewPoller(srv.URL + "/")
	if err := p.Fetch(); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	p.infoAt = time.Time{} // force an info refresh too
	if err := p.Fetch(); err == nil {
		t.Fatal("fetch from a closed server succeeded")
	}
	if s := p.Stations([]string{"4628.05"})[0]; !s.Renting || s.Classic != 35 {
		t.Errorf("after a failed fetch = %+v, want the previous data", s)
	}
}

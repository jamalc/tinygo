// Package rail serves Long Island Rail Road departures between two stations,
// such as Atlantic Terminal to Jamaica. Server-side, native only (used by the
// serve binary).
//
// It's kept apart from the subway engine on purpose. LIRR stop IDs are bare
// numbers that collide with subway ones (LIRR 241 is Atlantic Terminal, subway
// 241 is President St), and a rail question is a trip — trains that leave A and
// later stop at B — not every arrival at one platform.
package rail

import (
	"archive/zip"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/jamalc/tinygo/internal/api"
)

// FeedURL is the MTA's LIRR GTFS-realtime feed. It needs no key.
const FeedURL = "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/lirr%2Fgtfs-lirr"

// Route, Color and RouteName label every rail group. The feed tags trains out
// of Atlantic Terminal as the City Terminal Zone, whose grey reads on a panel
// as a dimmed, not-running line, so the group carries the LIRR's teal instead.
const (
	Route     = "LIRR"
	Color     = "00B2A9"
	RouteName = "Long Island Rail Road"
)

// minETASeconds matches the subway engine: a train under 30s out is pulling
// away, too late to catch, so it isn't shown.
const minETASeconds = 30

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Stops maps the LIRR's station codes ("ATL", "JAM") and stop IDs ("241",
// "102") to stop IDs and names, from the static feed's stops.txt.
type Stops struct {
	ids   map[string]string // stop_code or stop_id -> stop_id
	names map[string]string // stop_id -> stop_name
}

// LoadStops reads stops.txt from the LIRR static GTFS zip at path.
func LoadStops(path string) (*Stops, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "stops.txt" {
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return parseStops(r)
		}
	}
	return nil, fmt.Errorf("%s: no stops.txt", path)
}

func parseStops(r io.Reader) (*Stops, error) {
	cr := csv.NewReader(r)
	head, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	idCol, ok1 := col["stop_id"]
	nameCol, ok2 := col["stop_name"]
	codeCol, hasCode := col["stop_code"]
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("stops.txt: missing stop_id or stop_name")
	}
	s := &Stops{ids: map[string]string{}, names: map[string]string{}}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return s, nil
		}
		if err != nil {
			return nil, err
		}
		id := rec[idCol]
		s.ids[id] = id
		s.names[id] = rec[nameCol]
		if hasCode && rec[codeCol] != "" {
			s.ids[strings.ToUpper(rec[codeCol])] = id
		}
	}
}

// lookup resolves a station code or stop ID.
func (s *Stops) lookup(code string) (id, name string, ok bool) {
	id, ok = s.ids[strings.ToUpper(code)]
	return id, s.names[id], ok
}

// Poller caches the latest LIRR realtime feed and answers trip questions over
// it. Safe for concurrent use by the HTTP handlers and its polling goroutine.
type Poller struct {
	url   string
	stops *Stops

	mu   sync.RWMutex
	feed *gtfs.FeedMessage
}

// NewPoller returns a Poller over the feed at url (FeedURL for the LIRR).
func NewPoller(url string, stops *Stops) *Poller {
	return &Poller{url: url, stops: stops}
}

// Start does a first fetch, then polls every interval on a goroutine. Failures,
// the first included, are logged and retried rather than fatal: the subway
// should come up without the LIRR.
func (p *Poller) Start(interval time.Duration) {
	if err := p.Fetch(); err != nil {
		log.Printf("lirr: %v", err)
	}
	go func() {
		for range time.Tick(interval) {
			if err := p.Fetch(); err != nil {
				log.Printf("lirr: %v", err)
			}
		}
	}()
}

// Fetch refreshes the feed, keeping the previous one on error.
func (p *Poller) Fetch() error {
	resp, err := httpClient.Get(p.url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	msg := &gtfs.FeedMessage{}
	if err := proto.Unmarshal(b, msg); err != nil {
		return err
	}
	p.mu.Lock()
	p.feed = msg
	p.mu.Unlock()
	return nil
}

// Departures returns the trains leaving from that later stop at to, within
// horizon of now, as one group: soonest first, ETAs in the subway's "5m" form
// with Secs alongside. from and to are station codes or stop IDs. The group's
// StopID is the trip as asked ("ATL>JAM"), its StopName the origin's and its
// Headsign the destination's. A station the LIRR doesn't know, or no data yet,
// gives the group no ETAs: the board shows it as not running.
func (p *Poller) Departures(from, to string, horizon time.Duration) api.FlatArrivalGroup {
	g := api.FlatArrivalGroup{
		ArrivalGroup: api.ArrivalGroup{Route: Route, Color: Color, RouteName: RouteName, Headsign: to, ETAs: []string{}},
		StopID:       from + ">" + to,
		StopName:     from,
	}
	fromID, fromName, ok1 := p.stops.lookup(from)
	toID, toName, ok2 := p.stops.lookup(to)
	if !ok1 || !ok2 {
		return g
	}
	g.StopName, g.Headsign = fromName, toName

	p.mu.RLock()
	feed := p.feed
	p.mu.RUnlock()
	if feed == nil {
		return g
	}
	secs := departures(feed, fromID, toID, time.Now().Unix(), int64(horizon/time.Second))
	g.Secs = secs
	for _, s := range secs {
		g.ETAs = append(g.ETAs, fmt.Sprintf("%dm", (s+30)/60))
	}
	return g
}

// departures finds each trip that stops at fromID and then, later in its run,
// at toID, and returns its seconds until leaving fromID, sorted. Cancelled trips
// and skipped stops don't count.
func departures(feed *gtfs.FeedMessage, fromID, toID string, now, maxSecs int64) []int {
	var out []int
	for _, e := range feed.Entity {
		tu := e.GetTripUpdate()
		if tu == nil || tu.GetTrip().GetScheduleRelationship() == gtfs.TripDescriptor_CANCELED {
			continue
		}
		var leave int64
		for _, stu := range tu.StopTimeUpdate {
			if stu.GetScheduleRelationship() == gtfs.TripUpdate_StopTimeUpdate_SKIPPED {
				continue
			}
			switch stu.GetStopId() {
			case fromID:
				if t := stu.GetDeparture().GetTime(); t != 0 {
					leave = t
				} else {
					leave = stu.GetArrival().GetTime()
				}
			case toID:
				if leave == 0 {
					continue // reached to before from: the other direction
				}
				if s := leave - now; s >= minETASeconds && s <= maxSecs {
					out = append(out, int(s))
				}
				leave = 0
			}
		}
	}
	sort.Ints(out)
	return out
}

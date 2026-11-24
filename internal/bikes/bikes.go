// Package bikes polls Citi Bike's public GBFS feeds and serves the bikes and
// open docks at a handful of stations. Server-side, native only (used by the
// serve binary): the full status feed is about a megabyte, far too much for the
// device, so the server keeps it and hands out only the stations asked for.
package bikes

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jamalc/tinygo/internal/api"
)

// DefaultBaseURL is Citi Bike's GBFS 2.3 feed directory. It needs no key.
const DefaultBaseURL = "https://gbfs.lyft.com/gbfs/2.3/bkn/en/"

// infoEvery is how often the station list (names, short numbers, capacities)
// is refreshed.
// It changes when stations open or move, which is rare; the status changes
// every minute.
const infoEvery = time.Hour

// silentAfter is how long a station may go without reporting before it's
// treated as not renting. Live stations report every few minutes; the dead ones
// still listed in the feed haven't reported in months.
const silentAfter = time.Hour

var httpClient = &http.Client{Timeout: 15 * time.Second}

// station is what's kept of a station's status between polls.
type station struct {
	classic, ebikes int
	docks           int
	renting         bool
	returning       bool
	reported        time.Time
}

// info is what's kept of a station's description: it changes only when the
// station is built, moved, or resized.
type info struct {
	name     string
	capacity int
}

// Poller caches the latest Citi Bike station status. It's safe for concurrent
// use by the HTTP handlers and its own polling goroutine.
type Poller struct {
	base string

	mu      sync.RWMutex
	ids     map[string]string // short number -> station_id
	info    map[string]info   // station_id -> name and capacity
	status  map[string]station
	infoAt  time.Time
	updated time.Time // when the status was last fetched
}

// NewPoller returns a Poller reading the feeds under base (DefaultBaseURL for
// Citi Bike).
func NewPoller(base string) *Poller {
	return &Poller{base: base}
}

// Start does a first fetch, then polls every interval on a goroutine. A failed
// fetch, the first included, is logged and retried next tick rather than being
// fatal: bikes are an extra, and the arrivals should come up without them.
func (p *Poller) Start(interval time.Duration) {
	if err := p.Fetch(); err != nil {
		log.Printf("bikes: %v", err)
	}
	go func() {
		for range time.Tick(interval) {
			if err := p.Fetch(); err != nil {
				log.Printf("bikes: %v", err)
			}
		}
	}()
}

// Fetch refreshes the status, and the station list too when it's missing or
// over infoEvery old. On error the previous data is kept.
func (p *Poller) Fetch() error {
	p.mu.RLock()
	needInfo := p.ids == nil || time.Since(p.infoAt) >= infoEvery
	p.mu.RUnlock()

	if needInfo {
		ids, inf, err := p.fetchInfo()
		if err != nil {
			return fmt.Errorf("station_information: %w", err)
		}
		p.mu.Lock()
		p.ids, p.info, p.infoAt = ids, inf, time.Now()
		p.mu.Unlock()
	}

	status, err := p.fetchStatus()
	if err != nil {
		return fmt.Errorf("station_status: %w", err)
	}
	p.mu.Lock()
	p.status, p.updated = status, time.Now()
	p.mu.Unlock()
	return nil
}

// Stations returns the bikes and open docks at each station named by its short
// number, in the order given. A station the feed doesn't know comes back with
// only its ID. One that has gone silent comes back with its name and capacity
// but neither renting nor returning; one that's only doing one of the two has
// the other side's counts zeroed.
func (p *Poller) Stations(shorts []string) []api.BikeStation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]api.BikeStation, len(shorts))
	for i, short := range shorts {
		out[i].ID = short
		id, ok := p.ids[short]
		if !ok {
			continue
		}
		out[i].Name, out[i].Capacity = p.info[id].name, p.info[id].capacity
		s, ok := p.status[id]
		if !ok || now.Sub(s.reported) > silentAfter {
			continue
		}
		if s.renting {
			out[i].Classic, out[i].EBikes, out[i].Renting = s.classic, s.ebikes, true
		}
		if s.returning {
			out[i].Docks, out[i].Returning = s.docks, true
		}
	}
	return out
}

func (p *Poller) fetchInfo() (ids map[string]string, inf map[string]info, err error) {
	body, err := p.get("station_information.json")
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	return parseInfo(body)
}

func (p *Poller) fetchStatus() (map[string]station, error) {
	body, err := p.get("station_status.json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseStatus(body)
}

func (p *Poller) get(name string) (io.ReadCloser, error) {
	resp, err := httpClient.Get(p.base + name)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", name, resp.Status)
	}
	return resp.Body, nil
}

// parseInfo reads station_information.json into short number -> station_id and
// station_id -> name and capacity.
func parseInfo(r io.Reader) (ids map[string]string, inf map[string]info, err error) {
	var feed struct {
		Data struct {
			Stations []struct {
				ID       string `json:"station_id"`
				Short    string `json:"short_name"`
				Name     string `json:"name"`
				Capacity int    `json:"capacity"`
			} `json:"stations"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&feed); err != nil {
		return nil, nil, err
	}
	ids = make(map[string]string, len(feed.Data.Stations))
	inf = make(map[string]info, len(feed.Data.Stations))
	for _, s := range feed.Data.Stations {
		if s.Short != "" {
			ids[s.Short] = s.ID
		}
		inf[s.ID] = info{name: s.Name, capacity: max(s.Capacity, 0)}
	}
	return ids, inf, nil
}

// parseStatus reads station_status.json. num_bikes_available counts every
// rentable bike, e-bikes included, so the classic count is the difference.
// num_docks_available is the open, working docks: where a bike can go back.
func parseStatus(r io.Reader) (map[string]station, error) {
	var feed struct {
		Data struct {
			Stations []struct {
				ID        string `json:"station_id"`
				Bikes     int    `json:"num_bikes_available"`
				EBikes    int    `json:"num_ebikes_available"`
				Docks     int    `json:"num_docks_available"`
				Installed int    `json:"is_installed"`
				Renting   int    `json:"is_renting"`
				Returning int    `json:"is_returning"`
				Reported  int64  `json:"last_reported"`
			} `json:"stations"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&feed); err != nil {
		return nil, err
	}
	out := make(map[string]station, len(feed.Data.Stations))
	for _, s := range feed.Data.Stations {
		e := min(max(s.EBikes, 0), max(s.Bikes, 0))
		out[s.ID] = station{
			classic:   max(s.Bikes, 0) - e,
			ebikes:    e,
			docks:     max(s.Docks, 0),
			renting:   s.Installed == 1 && s.Renting == 1,
			returning: s.Installed == 1 && s.Returning == 1,
			reported:  time.Unix(s.Reported, 0),
		}
	}
	return out, nil
}

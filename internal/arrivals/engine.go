// Package arrivals is the serve backend's engine.
//
// It combines static GTFS data with the realtime feeds into per-stop arrival
// groups.
package arrivals

import (
	"fmt"
	"sort"
	"time"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/gtfs"
	"github.com/jamalc/tinygo/internal/realtime"
)

// maxETAs is the maximum number of ETAs to show per route/headsign group.
const maxETAs = 8

// DefaultHorizon is the lookahead applied when a caller passes a non-positive
// one. Arrivals further out are dropped before grouping, so the horizon — not
// maxETAs — is usually what bounds a group: an hour of real headways holds only
// three or four trains per route.
const DefaultHorizon = time.Hour

// StopRequest is a single stop with optional per-stop route pins and mutes,
// parsed from query params like "A27N:Q,!R". Bare routes are pins (always
// shown, with "No service" when not arriving); "!route" entries are mutes
// (never shown, even when arriving). A route listed as both is treated as
// muted.
type StopRequest struct {
	StopID string
	Pins   map[string]bool // always shown; "No service" when not arriving
	Mutes  map[string]bool // never shown, even when arriving
}

// entitySource supplies realtime feed entities. *realtime.Poller satisfies it;
// tests provide a fake.
type entitySource interface {
	AllEntities() []*gtfsrt.FeedEntity
}

// Engine combines static and realtime data into API-friendly arrival groups.
type Engine struct {
	static *gtfs.StaticData
	poller entitySource
}

// NewEngine returns an Engine backed by the given static data and poller.
func NewEngine(static *gtfs.StaticData, poller *realtime.Poller) *Engine {
	return &Engine{static: static, poller: poller}
}

// tripETA pairs an ETA in seconds with the headsign of that specific trip.
type tripETA struct {
	eta      int
	headsign string
}

// groupKey identifies a group of arrivals. In default mode it's just the
// route; with split=headsign it includes the headsign too.
type groupKey struct {
	Route    string
	Headsign string // empty in default (route-only) mode
}

// GetArrivals returns one StopArrivals per requested stop, each stop's Groups
// sorted by route.
//
// Realtime-first: by default every arriving route at a requested stop is shown.
// A stop's optional filter tunes this — a pinned route is always shown, with a
// "No service" row when it isn't arriving; a muted route is never shown, even
// when arriving. When both platforms of a station are requested (…N and …S), a
// route arriving at either is pinned at both (see pairedPins). If splitByHeadsign is true, arriving routes split into one row
// per headsign.
//
// horizon bounds how far ahead an arrival can be and still appear; a
// non-positive horizon means DefaultHorizon. Widening it past the depth the
// realtime feed actually publishes adds nothing.
func (e *Engine) GetArrivals(stops []StopRequest, splitByHeadsign bool, horizon time.Duration) []api.StopArrivals {
	if horizon <= 0 {
		horizon = DefaultHorizon
	}
	maxETA := int64(horizon / time.Second)
	now := time.Now().Unix()

	stopIndex := make(map[string]StopRequest, len(stops))
	for _, s := range stops {
		stopIndex[s.StopID] = s
	}

	// stopID -> groupKey -> []tripETA, excluding muted routes.
	index := make(map[string]map[groupKey][]tripETA)
	for _, entity := range e.poller.AllEntities() {
		tu := entity.TripUpdate
		if tu == nil {
			continue
		}
		routeID := gtfs.CanonicalRoute(tu.Trip.GetRouteId())
		tripID := tu.Trip.GetTripId()

		for _, stu := range tu.StopTimeUpdate {
			stopID := stu.GetStopId()
			sr, wanted := stopIndex[stopID]
			if !wanted {
				continue
			}
			if sr.Mutes[routeID] {
				continue
			}

			var eta int64
			if stu.Arrival != nil && stu.Arrival.Time != nil {
				eta = *stu.Arrival.Time - now
			} else if stu.Departure != nil && stu.Departure.Time != nil {
				eta = *stu.Departure.Time - now
			} else {
				continue
			}
			// A train under 30s out rounds to 0m: it's at the platform or about
			// to be, too late to catch, so it isn't shown at all.
			if eta < minETASeconds || eta > maxETA {
				continue
			}

			headsign := e.static.GetHeadsign(tripID)
			key := groupKey{Route: routeID}
			if splitByHeadsign {
				key.Headsign = headsign
			}
			if index[stopID] == nil {
				index[stopID] = make(map[groupKey][]tripETA)
			}
			index[stopID][key] = append(index[stopID][key], tripETA{int(eta), headsign})
		}
	}

	result := make([]api.StopArrivals, 0, len(stops))
	for _, sr := range stops {
		result = append(result, api.StopArrivals{
			StopID:   sr.StopID,
			StopName: e.static.GetStopName(sr.StopID),
			Groups:   buildGroups(index[sr.StopID], sr.StopID, pairedPins(sr, stopIndex, index), e.static),
		})
	}
	return result
}

// pairedPins returns sr's pins plus every route arriving at its opposite-direction
// platform, when that platform was requested too. Asking for both R30N and R30S
// means "show this station both ways", so a route running only one way still gets
// its other direction as a "No service" row. That keeps a station's rows in N/S
// pairs, which is what keeps a two-row page from splitting the next route across
// pages. sr's own mutes still win. Returns sr.Pins unchanged when there is nothing
// to add, so the common case allocates nothing.
func pairedPins(sr StopRequest, requested map[string]StopRequest, index map[string]map[groupKey][]tripETA) map[string]bool {
	sib, ok := siblingStop(sr.StopID)
	if !ok {
		return sr.Pins
	}
	if _, wanted := requested[sib]; !wanted {
		return sr.Pins
	}
	var pins map[string]bool
	for key := range index[sib] {
		if sr.Pins[key.Route] || sr.Mutes[key.Route] {
			continue
		}
		if pins == nil {
			pins = make(map[string]bool, len(sr.Pins)+len(index[sib]))
			for r := range sr.Pins {
				pins[r] = true
			}
		}
		pins[key.Route] = true
	}
	if pins == nil {
		return sr.Pins
	}
	return pins
}

// siblingStop returns the opposite-direction platform of a GTFS stop ID
// ("R30N" <-> "R30S"), or false for an ID with no N/S suffix.
func siblingStop(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	base, dir := id[:len(id)-1], id[len(id)-1]
	switch dir {
	case 'N':
		return base + "S", true
	case 'S':
		return base + "N", true
	}
	return "", false
}

// buildGroups turns the per-stop arrival index into display rows, then appends
// a "No service" row (empty ETAs) for every pinned route with no arrivals.
// pins is the stop's set of always-show routes.
func buildGroups(raw map[groupKey][]tripETA, stopID string, pins map[string]bool, static *gtfs.StaticData) []api.ArrivalGroup {
	directionFallback := "Northbound"
	if len(stopID) > 0 && stopID[len(stopID)-1] == 'S' {
		directionFallback = "Southbound"
	}

	var groups []api.ArrivalGroup
	arriving := make(map[string]bool)

	for key, etas := range raw {
		arriving[key.Route] = true
		sort.Slice(etas, func(i, j int) bool { return etas[i].eta < etas[j].eta })

		headsign := key.Headsign // non-empty only in split mode
		if headsign == "" {
			for _, t := range etas {
				if t.headsign != "" {
					headsign = t.headsign
					break
				}
			}
		}
		if headsign == "" {
			headsign = directionFallback
		}

		if len(etas) > maxETAs {
			etas = etas[:maxETAs]
		}
		formatted := make([]string, len(etas))
		secs := make([]int, len(etas))
		for i, t := range etas {
			formatted[i] = fmtETA(t.eta)
			secs[i] = t.eta
		}

		route, _ := static.GetRoute(key.Route)
		groups = append(groups, api.ArrivalGroup{
			Route:     key.Route,
			Color:     route.Color,
			RouteName: route.LongName,
			Headsign:  headsign,
			ETAs:      formatted,
			Secs:      secs,
		})
	}

	// "No service" rows for pinned routes with nothing arriving.
	absent := make([]string, 0, len(pins))
	for r := range pins {
		if !arriving[r] {
			absent = append(absent, r)
		}
	}
	sort.Strings(absent)
	for _, r := range absent {
		route, _ := static.GetRoute(r)
		groups = append(groups, api.ArrivalGroup{
			Route:     r,
			Color:     route.Color,
			RouteName: route.LongName,
			Headsign:  directionFallback,
			ETAs:      nil, // empty = No service
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Route != groups[j].Route {
			return groups[i].Route < groups[j].Route
		}
		return groups[i].Headsign < groups[j].Headsign
	})
	return groups
}

// minETASeconds is the soonest arrival shown. Anything sooner would round to 0m.
const minETASeconds = 30

// fmtETA formats an ETA in seconds as a display string such as "5m", rounding
// to the nearest minute. GetArrivals drops ETAs under minETASeconds, so the
// shortest shown is "1m".
func fmtETA(seconds int) string {
	mins := (seconds + 30) / 60
	return fmt.Sprintf("%dm", mins)
}

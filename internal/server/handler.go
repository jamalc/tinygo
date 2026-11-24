// Package server exposes the HTTP API (/arrivals, /bikes, /stops,
// /stops/search, /health) over the arrivals engine and static GTFS data. Server-side, native
// only (used by the serve binary).
package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jamalc/tinygo/internal/api"
	"github.com/jamalc/tinygo/internal/arrivals"
	"github.com/jamalc/tinygo/internal/gtfs"
)

//go:embed stops.html
var stopsHTML []byte

// Handler serves the HTTP API over an arrivals engine and the static GTFS data.
type Handler struct {
	engine *arrivals.Engine
	static *gtfs.StaticData
	bikes  BikeSource
	rail   RailSource
}

// RailSource answers a lirr= trip: the trains leaving one station that later
// stop at another, as one group. The serve binary passes a *rail.Poller.
type RailSource interface {
	Departures(from, to string, horizon time.Duration) api.FlatArrivalGroup
}

// BikeSource answers the bike= stations of a format=board request and the
// station= stations of /bikes. The serve binary passes a *bikes.Poller.
type BikeSource interface {
	Stations(ids []string) []api.BikeStation
}

// NewHandler returns a Handler backed by the given engine and static data.
// bikes may be nil, in which case every bike station comes back not renting;
// rail may be nil, in which case every lirr= trip comes back with no trains.
func NewHandler(engine *arrivals.Engine, static *gtfs.StaticData, bikes BikeSource, rail RailSource) *Handler {
	return &Handler{engine: engine, static: static, bikes: bikes, rail: rail}
}

// RegisterRoutes registers the API routes on the given ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /arrivals", h.handleArrivals)
	mux.HandleFunc("GET /bikes", h.handleBikes)
	mux.HandleFunc("GET /stops", h.handleStops)
	mux.HandleFunc("GET /stops/search", h.handleStopSearch)
	mux.HandleFunc("GET /health", h.handleHealth)
}

// handleArrivals serves GET /arrivals
//
// Query params:
//
//	stop=<stopID>[:<token>[,<token>...]]   (repeatable)
//	split=headsign                          (optional) split groups by headsign
//	format=flat                             (optional) flat, sorted group list
//	format=board                            (optional) flat list plus bike stations
//	bike=<station number>                   (repeatable) a Citi Bike station, for format=board
//	lirr=<from>><to>                        (repeatable) LIRR trains from one station to another
//	horizon=<minutes>                       (optional) arrival lookahead
//
// A stop's tokens tune which routes show. A bare route is a pin (always shown,
// with "No service" when not arriving); "!route" is a mute (never shown). With
// no tokens, every arriving route is shown.
//
// horizon is how many minutes ahead to look, 1 to maxHorizonMins, defaulting to
// arrivals.DefaultHorizon. It's what usually bounds a group's ETA count, since
// real headways only fit three or four trains per route into an hour.
//
// By default the response is one object per stop ([]api.StopArrivals). With
// format=flat the server folds the stop ID/name into each group and returns a
// single list ([]api.FlatArrivalGroup) with stations in request order, then
// sorted by route and direction — the shape the embedded client renders directly, so it doesn't
// have to rebuild it on the device. format=board wraps that same list in an
// api.Board alongside the bikes at each bike= station, in request order, so a
// device gets both from one request.
//
// A lirr= trip ("ATL>JAM", by LIRR station code or stop ID) adds one group of
// the trains leaving from that later stop at to, after every stop= station.
// Its stop ID is the trip as asked and its route is "LIRR", so it sorts and
// draws as one more station. At least one stop= or lirr= is required.
//
// Examples:
//
//	?stop=A27N                  — every route arriving at Franklin
//	?stop=A27N:C                — all arrivals, plus "No service" for C when down
//	?stop=A27N:C,!A             — pin C (No service if absent), hide A, show rest
//	?stop=A27N&split=headsign   — separate group per headsign
//	?stop=A27N&format=flat      — one flat, sorted list of groups
//	?stop=A27N&horizon=90       — look 90 minutes ahead instead of 60
//	?stop=A27N&format=board&bike=4628.05 — groups plus one Citi Bike station
//	?stop=R30N&lirr=ATL>JAM     — DeKalb northbound, plus Atlantic to Jamaica
func (h *Handler) handleArrivals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	rawStops := q["stop"]
	trips, err := parseTripParams(q["lirr"])
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	if len(rawStops) == 0 && len(trips) == 0 {
		http.Error(w, `{"error":"at least one stop= or lirr= param required"}`, http.StatusBadRequest)
		return
	}

	stops := make([]arrivals.StopRequest, len(rawStops))
	for i, raw := range rawStops {
		stops[i] = parseStopParam(raw)
	}

	horizon, err := parseHorizonParam(q.Get("horizon"))
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}

	splitByHeadsign := q.Get("split") == "headsign"
	byStop := h.engine.GetArrivals(stops, splitByHeadsign, horizon)
	for _, t := range trips {
		g := h.departures(t, horizon)
		byStop = append(byStop, api.StopArrivals{StopID: g.StopID, StopName: g.StopName, Groups: []api.ArrivalGroup{g.ArrivalGroup}})
	}

	switch q.Get("format") {
	case "flat":
		writeJSON(w, flatten(byStop))
		return
	case "board":
		writeJSON(w, api.Board{Groups: flatten(byStop), Bikes: h.bikeStations(q["bike"])})
		return
	}
	writeJSON(w, byStop)
}

// departures answers one lirr= trip, or an empty group when there's no rail
// source, so the trip still shows (as not running) rather than vanishing.
func (h *Handler) departures(t [2]string, horizon time.Duration) api.FlatArrivalGroup {
	if horizon <= 0 {
		horizon = arrivals.DefaultHorizon
	}
	if h.rail == nil {
		return api.FlatArrivalGroup{
			ArrivalGroup: api.ArrivalGroup{Route: "LIRR", Headsign: t[1], ETAs: []string{}},
			StopID:       t[0] + ">" + t[1],
			StopName:     t[0],
		}
	}
	return h.rail.Departures(t[0], t[1], horizon)
}

// parseTripParams parses lirr= values like "ATL>JAM" into from/to pairs. A
// value without exactly one ">" between two names is an error.
func parseTripParams(raw []string) ([][2]string, error) {
	out := make([][2]string, 0, len(raw))
	for _, v := range raw {
		from, to, ok := strings.Cut(strings.TrimSpace(v), ">")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" || strings.Contains(to, ">") {
			return nil, fmt.Errorf("lirr must look like FROM>TO, e.g. ATL>JAM")
		}
		out = append(out, [2]string{from, to})
	}
	return out, nil
}

// handleBikes serves GET /bikes: Citi Bike stations on their own, with no
// arrivals, for the docks app.
//
// Query params:
//
//	station=<station number>   (repeatable, at least one) a station's short number
//
// The response is []api.BikeStation in request order, one per station= even
// when the server doesn't know it (then it comes back with only its ID), so a
// client can match its config to the response by position.
//
// Example:
//
//	?station=4587.02&station=4628.05 — both ends of Willoughby St, Ashland to Fleet
func (h *Handler) handleBikes(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["station"]
	if len(ids) == 0 {
		http.Error(w, `{"error":"at least one station= param required"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, h.bikeStations(ids))
}

// bikeStations looks up the bike stations, never returning nil, so the
// response always carries a "bikes" list even when it's empty.
func (h *Handler) bikeStations(ids []string) []api.BikeStation {
	if h.bikes == nil {
		out := make([]api.BikeStation, len(ids))
		for i, id := range ids {
			out[i].ID = id
		}
		return out
	}
	if out := h.bikes.Stations(ids); out != nil {
		return out
	}
	return []api.BikeStation{}
}

// writeJSON marshals v and writes it with an explicit Content-Length.
//
// The length is the point, not the buffering. json.NewEncoder(w).Encode streams
// straight to the ResponseWriter, and net/http only auto-sets Content-Length when
// the whole response fits its 4 KB buffer — an arrivals payload does not, so it
// would go out chunked. That costs the device client twice over: it must de-chunk into
// a second full-size buffer, and it cannot know how big the body is until it has
// read all of it, so it sizes its read buffer by doubling.
//
// TinyGo's allocator rounds a growing slice up to the next power of two and never
// compacts, so that doubling is what eventually fails — not on total bytes free,
// but on finding one contiguous block twice the size of the last. Sending a length
// lets the client allocate once, exactly, and never grow. Marshalling to a buffer
// here costs one response-sized allocation on a machine that has gigabytes, to
// remove several on one that has kilobytes.
func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encode"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

// flatten folds each stop's ID and name into its groups and returns one sorted
// list: stations in the order they were requested, then route, then direction
// (stop ID). A station is a stop ID minus its N/S suffix, so a station's two
// platforms sort together wherever each was listed, ranked by whichever came
// first. It's the format=flat projection of handleArrivals: the embedded client
// renders this directly, so flattening and sorting here keeps that allocation
// off the device. byStop must be in request order, as GetArrivals returns it.
func flatten(byStop []api.StopArrivals) []api.FlatArrivalGroup {
	rank := make(map[string]int, len(byStop)) // station -> first position requested
	groups := make([]api.FlatArrivalGroup, 0, len(byStop))
	for _, s := range byStop {
		if _, seen := rank[station(s.StopID)]; !seen {
			rank[station(s.StopID)] = len(rank)
		}
		for _, g := range s.Groups {
			groups = append(groups, api.FlatArrivalGroup{
				ArrivalGroup: g,
				StopID:       s.StopID,
				StopName:     s.StopName,
			})
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		ri, rj := rank[station(groups[i].StopID)], rank[station(groups[j].StopID)]
		if ri != rj {
			return ri < rj
		}
		if groups[i].Route != groups[j].Route {
			return groups[i].Route < groups[j].Route
		}
		// Keep a route's platforms adjacent, ordered by direction: the N/S stop
		// ID suffix sorts northbound (…N) before southbound (…S).
		if groups[i].StopID != groups[j].StopID {
			return groups[i].StopID < groups[j].StopID
		}
		return groups[i].Headsign < groups[j].Headsign
	})
	return groups
}

// station returns a GTFS stop ID without its N/S platform suffix ("R30N" ->
// "R30"). An ID with no suffix is its own station.
func station(stopID string) string {
	if n := len(stopID); n > 0 && (stopID[n-1] == 'N' || stopID[n-1] == 'S') {
		return stopID[:n-1]
	}
	return stopID
}

// handleStops serves GET /stops — the embedded HTML stop lookup page.
func (h *Handler) handleStops(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(stopsHTML)
}

// handleStopSearch serves GET /stops/search?q=franklin
func (h *Handler) handleStopSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	writeJSON(w, h.static.SearchStops(q))
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// maxHorizonMins bounds the horizon= param. The realtime feeds don't publish
// trips much beyond an hour or two out, so a larger lookahead only widens the
// window the engine scans without surfacing more trains.
const maxHorizonMins = 180

// parseHorizonParam parses the horizon= param, a lookahead in minutes, into a
// duration. An empty param yields the engine's default. A value that isn't a
// whole number of minutes in [1, maxHorizonMins] is an error rather than a
// silent clamp, so a typo'd horizon doesn't quietly return the wrong window.
func parseHorizonParam(raw string) (time.Duration, error) {
	if raw == "" {
		return arrivals.DefaultHorizon, nil
	}
	mins, err := strconv.Atoi(raw)
	if err != nil || mins < 1 || mins > maxHorizonMins {
		return 0, fmt.Errorf("horizon must be a whole number of minutes from 1 to %d", maxHorizonMins)
	}
	return time.Duration(mins) * time.Minute, nil
}

// parseStopParam parses "A27N:Q,!R" into a StopRequest. The part after ":" is
// a comma-separated list of route tokens: a bare route is a pin (always
// shown), "!route" is a mute (never shown). A route given as both is treated
// as muted. The token portion is optional; "635N" with no colon shows every
// arriving route.
func parseStopParam(raw string) arrivals.StopRequest {
	parts := strings.SplitN(raw, ":", 2)
	sr := arrivals.StopRequest{
		StopID: parts[0],
		Pins:   make(map[string]bool),
		Mutes:  make(map[string]bool),
	}
	if len(parts) == 2 {
		for _, tok := range strings.Split(parts[1], ",") {
			if tok = strings.TrimSpace(tok); tok == "" {
				continue
			}
			if strings.HasPrefix(tok, "!") {
				if r := gtfs.CanonicalRoute(strings.TrimSpace(tok[1:])); r != "" {
					sr.Mutes[r] = true
				}
			} else {
				sr.Pins[gtfs.CanonicalRoute(tok)] = true
			}
		}
		// Mute wins when a route is listed as both.
		for r := range sr.Mutes {
			delete(sr.Pins, r)
		}
	}
	return sr
}

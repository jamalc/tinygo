// Package api defines the data types for the HTTP API responses.
//
// They are shared by both the server and the client.
package api

// ArrivalGroup is a group of arrivals for one route and headsign at a stop.
type ArrivalGroup struct {
	Route     string   `json:"route"`
	Color     string   `json:"color"`
	RouteName string   `json:"route_name"`
	Headsign  string   `json:"headsign"`
	ETAs      []string `json:"etas"`

	// Secs is each ETA in whole seconds from when the response was written,
	// parallel to ETAs. A client that knows the time (the response's Date
	// header) can count down from these itself, so its numbers tick on its own
	// clock's minute rather than whenever a poll lands.
	Secs []int `json:"secs,omitempty"`
}

// FlatArrivalGroup is an ArrivalGroup with the stop ID and name flattened in.
type FlatArrivalGroup struct {
	ArrivalGroup
	StopID   string `json:"stop_id"`
	StopName string `json:"stop_name"`
}

// BikeStation is one Citi Bike station right now: the bikes there to take, and
// the open docks to return one to.
//
// Classic and EBikes add up to every rentable bike there. Renting is false for
// a station that's closed, out of service, or unknown to the server; its bike
// counts are zero then. Returning is the same for the docks side: when it's
// false, Docks is zero. A station can rent without returning or the other way
// round, so the two are separate.
//
// Capacity is the station's dock count, from its station information; it's
// zero for a station the server doesn't know. What's left of it past the bikes
// and open docks is out of service: broken docks and bikes.
type BikeStation struct {
	ID        string `json:"id"` // the station's short number, e.g. "4628.05"
	Name      string `json:"name"`
	Classic   int    `json:"classic"`
	EBikes    int    `json:"ebikes"`
	Renting   bool   `json:"renting"`
	Docks     int    `json:"docks"`
	Capacity  int    `json:"capacity"`
	Returning bool   `json:"returning"`
}

// Board is the format=board response: the format=flat groups, plus the bike
// stations asked for, in request order. Neither field is omitted when empty, so
// a client decoding into a reused value never keeps a previous poll's list.
type Board struct {
	Groups []FlatArrivalGroup `json:"groups"`
	Bikes  []BikeStation      `json:"bikes"`
}

// StopArrivals is the set of arrival groups served at one stop.
type StopArrivals struct {
	StopID   string         `json:"stop_id"`
	StopName string         `json:"stop_name"`
	Groups   []ArrivalGroup `json:"groups"`
}

// ErrorResponse is the JSON body returned for API errors.
type ErrorResponse struct {
	Message string `json:"message"`
}

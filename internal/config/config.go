// Package config parses the flat key: value config files.
//
// Used by the TinyGo apps. Lines starting with # are comments; blank lines are
// skipped. This is separate from the serve backend, which has its own YAML
// config.
package config

import (
	_ "embed"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jamalc/tinygo/internal/hub75"
)

// configData is the config baked into the firmware at build time. The device
// has no filesystem, so config can't be read from disk at runtime — it must be
// embedded. See internal/config.Parse.
//
//go:embed config.txt
var global []byte

// Config holds the settings for the TinyGo apps, parsed
// from a flat key: value config file by Parse.
type Config struct {
	Width        int // logical display width that apps draw to
	Height       int // logical display height that apps draw to
	Columns      int // arrival columns per page; 0 or 1 is one group per row
	ColorDepth   int // bits per color channel; 0 means "use the driver default"
	Brightness   uint8
	DoubleBuffer bool // tear-free double buffering; defaults false (single-buffer) when the key is absent
	BGR          bool // panel wires colors B-G-R instead of R-G-B; defaults false (RGB). A per-panel trait.

	SSID          string
	Passphrase    string
	Host          string
	Port          int
	Stops         []string
	PollInterval  time.Duration
	GroupInterval time.Duration

	// Horizon is how far ahead to ask for arrivals. Zero leaves the choice to
	// the server (an hour). It bounds how many ETAs a row can show long before
	// the display width does: an hour of real headways holds only three or four
	// trains per route.
	Horizon time.Duration

	// Layout selects the on-display arrangement: "" or "classic" is the two-line
	// cell (route bullet, marqueed stop/headsign, ETAs); "compact" is the
	// single-line row (colour chip, route, direction arrow, and the ETAs);
	// "board" is one
	// fixed, unanimated screen with a row per route holding both directions,
	// built for a 128x64 panel. The clock maps the
	// string to its layout; an unrecognized value falls back to classic.
	Layout string

	// Walk is each platform's walking time in minutes, keyed by stop ID, from
	// an "@minutes" on the stop in the stops list ("R30N@5"). The board layout
	// drops trains that arrive sooner than you could get there and lights up the
	// one you'd have to leave now to catch. A platform without one takes its
	// station's other platform's, so "R30N@5 R30S" means five minutes both ways;
	// a station with none counts as zero: nothing dropped, nothing lit.
	Walk map[string]int

	// Bikes is the Citi Bike stations to ask the server about, by their short
	// station number ("4628.05"). The docks app draws them, from the server's
	// /bikes. In the clock, setting any switches the request to format=board,
	// which carries their counts too, though no clock layout draws them yet.
	Bikes []string

	// LIRR is the Long Island Rail Road trips to show, each "FROM>TO" by LIRR
	// station code ("ATL>JAM": Atlantic Terminal to Jamaica), from the lirr key.
	// Like a stop, a trip may take "@minutes", the walk to its origin station,
	// which lands in Walk under the trip ("ATL>JAM").
	LIRR []string
}

func New() Config {
	c := Config{}
	c.Parse(global)
	return c
}

// Load reads and parses a config file from disk. Native builds (the simulate
// binary) use this. Device builds (TinyGo, baremetal) have no filesystem, so
// they embed the config at build time and call Parse directly instead.
func (c *Config) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c.Parse(data)
	return nil
}

// Parse fills cfg from raw key: value config bytes.
func (c *Config) Parse(data []byte) {
	m := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		m[key] = val
	}
	if v, ok := m["ssid"]; ok {
		c.SSID = v
	}
	if v, ok := m["passphrase"]; ok {
		c.Passphrase = v
	}
	if v, ok := m["host"]; ok {
		c.Host = v
	}
	if v, ok := m["port"]; ok {
		c.Port, _ = strconv.Atoi(v)
	}
	// Stops are whitespace-separated. Commas are reserved for a stop's route
	// tokens (e.g. "A42N:A,C"), and stop IDs never contain spaces. A stop's
	// "@minutes" walk time is the device's alone: it's split off into Walk here,
	// so the stop sent to the server is just the ID and its tokens.
	if v, ok := m["stops"]; ok {
		c.Stops, c.Walk = parseStops(v)
	}
	// Trips go after the stops so the walk times merge into the same map.
	if v, ok := m["lirr"]; ok {
		var walk map[string]int
		c.LIRR, walk = parseStops(v)
		if c.Walk == nil {
			c.Walk = make(map[string]int)
		}
		for k, mins := range walk {
			c.Walk[k] = mins
		}
	}
	if v, ok := m["bikes"]; ok {
		c.Bikes = strings.Fields(v)
	}
	if v, ok := m["poll_interval"]; ok {
		c.PollInterval, _ = time.ParseDuration(v)
	}
	if v, ok := m["group_interval"]; ok {
		c.GroupInterval, _ = time.ParseDuration(v)
	}
	// A duration like "90m", matching the other interval keys, even though the
	// server takes the horizon as minutes. The client converts.
	if v, ok := m["horizon"]; ok {
		c.Horizon, _ = time.ParseDuration(v)
	}
	if v, ok := m["layout"]; ok {
		c.Layout = v
	}
	if v, ok := m["width"]; ok {
		c.Width, _ = strconv.Atoi(v)
	}
	if v, ok := m["height"]; ok {
		c.Height, _ = strconv.Atoi(v)
	}
	// Columns splits the display into side-by-side arrival columns: the compact
	// view. Absent (or 1) keeps one full-width group per row; the clock drops
	// columns that would leave a cell too narrow to draw.
	if v, ok := m["columns"]; ok {
		c.Columns, _ = strconv.Atoi(v)
	}
	if v, ok := m["color_depth"]; ok {
		c.ColorDepth, _ = strconv.Atoi(v)
	}
	// Brightness 0 means "unset": HUB75() only overlays a positive value, and the
	// driver treats zero as full (see hub75.resolveBrightness). Out-of-range
	// values are ignored rather than clamped, like every other bad value here.
	if v, ok := m["brightness"]; ok {
		if b, err := strconv.Atoi(v); err == nil && b >= 0 && b <= 255 {
			c.Brightness = uint8(b)
		}
	}
	// DoubleBuffer defaults to false (single-buffer) when the key is absent; an
	// explicit "double_buffer: true" opts into the tear-free frame-boundary swap
	// at the cost of a second buffer. (hub75.DefaultConfig, used by cmd/bench and
	// examples, defaults the other way — to true.)
	if v, ok := m["double_buffer"]; ok {
		c.DoubleBuffer, _ = strconv.ParseBool(v)
	}
	// BGR defaults false (RGB). Set "bgr: true" for a panel whose data lines are wired B-G-R
	// (reds and blues come out swapped otherwise). It varies by panel, not by board.
	if v, ok := m["bgr"]; ok {
		c.BGR, _ = strconv.ParseBool(v)
	}
}

// parseStops splits the stops list on whitespace and takes each stop's walk
// time out of it. The "@minutes" may sit right after the ID or after its route
// tokens ("R30N@5:Q" or "R30N:Q@5"); a malformed one is dropped, like other bad
// values, leaving the stop as if it had none. Platforms without a walk then take
// their sibling's (R30S from R30N).
func parseStops(v string) (stops []string, walk map[string]int) {
	walk = make(map[string]int)
	for _, f := range strings.Fields(v) {
		mins := -1
		if at := strings.IndexByte(f, '@'); at >= 0 {
			end := at + 1 // the token runs to the next ':' or the end
			for end < len(f) && f[end] != ':' {
				end++
			}
			if n, err := strconv.Atoi(f[at+1 : end]); err == nil && n >= 0 {
				mins = n
			}
			f = f[:at] + f[end:]
		}
		if f == "" {
			continue
		}
		stops = append(stops, f)
		if mins >= 0 {
			walk[stopID(f)] = mins
		}
	}
	for _, f := range stops {
		id := stopID(f)
		if _, ok := walk[id]; ok {
			continue
		}
		if sib, ok := sibling(id); ok {
			if mins, ok := walk[sib]; ok {
				walk[id] = mins
			}
		}
	}
	return stops, walk
}

// stopID is a stops-list entry without its route tokens: "R30N:Q,!R" -> "R30N".
func stopID(stop string) string {
	if i := strings.IndexByte(stop, ':'); i >= 0 {
		return stop[:i]
	}
	return stop
}

// sibling is the other platform of a station ("R30N" <-> "R30S"), or false for
// an ID with no N/S suffix.
func sibling(id string) (string, bool) {
	if n := len(id); n > 1 {
		switch id[n-1] {
		case 'N':
			return id[:n-1] + "S", true
		case 'S':
			return id[:n-1] + "N", true
		}
	}
	return "", false
}

// HUB75 maps the app config's display knobs onto a hub75.Config for the
// build-selected driver. It starts from the driver's DefaultConfig (so each
// driver's timing knobs stay at their tuned defaults) and overlays the geometry:
// Width and Height always, ColorDepth only when set (0 keeps the driver default),
// and DoubleBuffer as parsed. The other Config fields (WiFi, stops) are unrelated
// to the panel and ignored.
func (c Config) HUB75() hub75.Config {
	h := hub75.DefaultConfig()
	h.Width = c.Width
	h.Height = c.Height
	if c.ColorDepth > 0 {
		h.ColorDepth = c.ColorDepth
	}
	if c.Brightness > 0 {
		h.Brightness = c.Brightness
	}
	h.DoubleBuffer = c.DoubleBuffer
	h.BGR = c.BGR
	return h
}

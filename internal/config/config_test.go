package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	data := []byte(`# comment
ssid: mynet
passphrase: secret
width: 64
height: 32
host: 10.0.0.5
port: 8080
stops:   A42N:A,C   R30N   233N:1,2,3
poll_interval: 30s
group_interval: 9s
brightness: 180
`)

	var cfg Config
	cfg.Parse(data)

	if cfg.SSID != "mynet" || cfg.Passphrase != "secret" {
		t.Errorf("creds: got %q/%q", cfg.SSID, cfg.Passphrase)
	}
	if cfg.Width != 64 || cfg.Height != 32 {
		t.Errorf("dims: got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Host != "10.0.0.5" || cfg.Port != 8080 {
		t.Errorf("host: got %s:%d", cfg.Host, cfg.Port)
	}
	// Stops split on whitespace; commas stay inside a stop's route filter.
	want := []string{"A42N:A,C", "R30N", "233N:1,2,3"}
	if len(cfg.Stops) != len(want) {
		t.Fatalf("stops: got %v", cfg.Stops)
	}
	for i := range want {
		if cfg.Stops[i] != want[i] {
			t.Errorf("stops[%d]: got %q want %q", i, cfg.Stops[i], want[i])
		}
	}
	if cfg.PollInterval != 30*time.Second || cfg.GroupInterval != 9*time.Second {
		t.Errorf("intervals: got %v/%v", cfg.PollInterval, cfg.GroupInterval)
	}
	if cfg.Brightness != 180 {
		t.Errorf("brightness: got %d", cfg.Brightness)
	}
}

// TestExampleFile guards the shipped example against keys the parser doesn't
// understand — most easily broken by an inline comment, which becomes part of
// the value and fails the field's conversion silently.
func TestExampleFile(t *testing.T) {
	data, err := os.ReadFile("config.example.txt")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	cfg.Parse(data)

	if cfg.Width != 64 || cfg.Height != 32 {
		t.Errorf("dims: got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.ColorDepth != 8 {
		t.Errorf("color_depth: got %d", cfg.ColorDepth)
	}
	if cfg.Brightness != 255 {
		t.Errorf("brightness: got %d", cfg.Brightness)
	}
	if cfg.Host == "" || strings.ContainsAny(cfg.Host, " #") {
		t.Errorf("host: got %q", cfg.Host)
	}
	if cfg.Port != 8080 {
		t.Errorf("port: got %d", cfg.Port)
	}
	if len(cfg.Stops) == 0 {
		t.Errorf("stops: got %v", cfg.Stops)
	}
	if cfg.PollInterval == 0 || cfg.GroupInterval == 0 {
		t.Errorf("intervals: got %v/%v", cfg.PollInterval, cfg.GroupInterval)
	}
}

// A stop's "@minutes" is split off into Walk, before or after its route tokens,
// and a platform without one takes its sibling's.
func TestParseStopsWalk(t *testing.T) {
	var cfg Config
	cfg.Parse([]byte("stops: R30N@5 R30S A41N:A,C@7 A41S@8 234N@x 234S G36N\n"))

	wantStops := []string{"R30N", "R30S", "A41N:A,C", "A41S", "234N", "234S", "G36N"}
	if strings.Join(cfg.Stops, " ") != strings.Join(wantStops, " ") {
		t.Errorf("stops = %v, want %v (no @ sent to the server)", cfg.Stops, wantStops)
	}
	wantWalk := map[string]int{
		"R30N": 5, "R30S": 5, // R30S inherits
		"A41N": 7, "A41S": 8, // each platform its own
		// 234N's "@x" is malformed: dropped, so neither Nevins platform has one
	}
	if len(cfg.Walk) != len(wantWalk) {
		t.Fatalf("walk = %v, want %v", cfg.Walk, wantWalk)
	}
	for k, v := range wantWalk {
		if cfg.Walk[k] != v {
			t.Errorf("walk[%s] = %d, want %d", k, cfg.Walk[k], v)
		}
	}
}

func TestParseLIRR(t *testing.T) {
	var c Config
	c.Parse([]byte("stops: R30N@3 R30S\nlirr: ATL>JAM@9 PSN>JAM\n"))
	if strings.Join(c.LIRR, " ") != "ATL>JAM PSN>JAM" {
		t.Errorf("LIRR = %v, want both trips without walk times", c.LIRR)
	}
	want := map[string]int{"R30N": 3, "R30S": 3, "ATL>JAM": 9}
	if len(c.Walk) != len(want) {
		t.Fatalf("Walk = %v, want %v", c.Walk, want)
	}
	for k, v := range want {
		if c.Walk[k] != v {
			t.Errorf("Walk[%s] = %d, want %d", k, c.Walk[k], v)
		}
	}
}

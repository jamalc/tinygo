// Command serve is the main server for the subway clock application.
//
// It loads GTFS data, starts a realtime poller for the MTA feeds, and serves
// arrival information over HTTP.
//
// Usage:
//
//	go run ./cmd/serve [-config path/to/config.yaml]
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jamalc/tinygo/internal/arrivals"
	"github.com/jamalc/tinygo/internal/bikes"
	"github.com/jamalc/tinygo/internal/gtfs"
	"github.com/jamalc/tinygo/internal/rail"
	"github.com/jamalc/tinygo/internal/realtime"
	"github.com/jamalc/tinygo/internal/server"

	"gopkg.in/yaml.v3"
)

var (
	configPath = flag.String("config", "cmd/serve/config.yaml", "path to config file")
)

type config struct {
	// RealtimeInterval is the number of seconds between MTA API polls.
	RealtimeInterval int `yaml:"realtime_interval"`

	// GTFSSubway is the path to the GTFS subway feed zip file.
	GTFSSubway string `yaml:"gtfs_subway"`

	// GTFSSupplemented is the path to the GTFS supplemented feed zip, which
	// adds data such as trip updates.
	GTFSSupplemented string `yaml:"gtfs_supplemented"`

	// BikesInterval is the number of seconds between Citi Bike status polls.
	// Zero (or absent) means 60, the feed's own refresh rate; negative turns
	// bikes off.
	BikesInterval int `yaml:"bikes_interval"`

	// GTFSLIRR is the path to the LIRR static GTFS zip, for its station codes
	// and names. Empty turns the LIRR off.
	GTFSLIRR string `yaml:"gtfs_lirr"`
}

// load reads the config from the specified YAML file path.
func (c *config) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return yaml.NewDecoder(f).Decode(c)
}

func main() {
	flag.Parse()

	cfg := config{}
	if err := cfg.load(*configPath); err != nil {
		log.Fatal("config:", err)
	}

	static := gtfs.NewStaticData()
	log.Println("Loading GTFS data...")
	if err := static.Load(cfg.GTFSSubway, cfg.GTFSSupplemented); err != nil {
		log.Fatal("GTFS:", err)
	}
	log.Println("GTFS loaded")

	log.Println("Starting realtime poller...")
	poller := realtime.NewPoller()
	if err := poller.Start(time.Duration(cfg.RealtimeInterval) * time.Second); err != nil {
		log.Fatal("realtime poller:", err)
	}
	log.Println("Realtime poller ready")

	engine := arrivals.NewEngine(static, poller)

	// Citi Bike is an extra: a failed first fetch is logged, not fatal, and
	// stations read as not renting until a poll succeeds.
	var bikeSrc server.BikeSource
	if cfg.BikesInterval >= 0 {
		every := time.Duration(cfg.BikesInterval) * time.Second
		if every == 0 {
			every = time.Minute
		}
		bp := bikes.NewPoller(bikes.DefaultBaseURL)
		bp.Start(every)
		bikeSrc = bp
		log.Println("Citi Bike poller started")
	}

	// The LIRR, like Citi Bike, is optional: without its static zip, or if that
	// fails to load, lirr= trips come back with no trains.
	var railSrc server.RailSource
	if cfg.GTFSLIRR != "" {
		stops, err := rail.LoadStops(cfg.GTFSLIRR)
		if err != nil {
			log.Printf("LIRR off: %v", err)
		} else {
			rp := rail.NewPoller(rail.FeedURL, stops)
			rp.Start(time.Duration(cfg.RealtimeInterval) * time.Second)
			railSrc = rp
			log.Println("LIRR poller started")
		}
	}

	mux := http.NewServeMux()
	server.NewHandler(engine, static, bikeSrc, railSrc).RegisterRoutes(mux)

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe("0.0.0.0:8080", mux))
}

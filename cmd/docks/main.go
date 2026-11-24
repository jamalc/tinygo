//go:build tinygo

// Command docks is the TinyGo firmware for the Citi Bike dock board: the open
// docks, and the bikes, at the stations in the embedded config's bikes: list.
//
// It connects to WiFi and polls the serve backend's /bikes endpoint (see
// internal/docks). Set the server address and the stations in config.txt.
//
// Usage:
//
//	make docks [BOARD=...]
package main

import (
	_ "embed"

	"github.com/jamalc/tinygo/internal/clock"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/docks"
	"github.com/jamalc/tinygo/internal/hub75"
	"github.com/jamalc/tinygo/internal/wifi"
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	clock.Info(panel, "Connecting to WiFi")
	if err := wifi.Connect(cfg.SSID, cfg.Passphrase); err != nil {
		clock.Error(panel, "Failed to connect to WiFi: "+err.Error())
		return
	}

	docks.Run(cfg, panel)
}
